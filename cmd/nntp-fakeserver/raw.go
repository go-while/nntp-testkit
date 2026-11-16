package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
	"github.com/go-while/nntp-testkit/internal/inflate"
)

// recorder holds what raw mode saw (the fakeserver records the other modes; a raw step reads
// the socket itself, so the fakeserver sees no lines).
type recorder struct {
	mu         sync.Mutex
	lines      []string
	mismatches []string
	articles   [][]string
	articleRaw [][]byte // the blocks as read, without the "." line (articles_raw)
	notes      []string
	counts     map[string]int
}

func (r *recorder) line(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
	if r.counts == nil {
		r.counts = make(map[string]int)
	}
	var cmd string
	if f := strings.Fields(s); len(f) > 0 {
		cmd = strings.ToUpper(f[0])
	}
	r.counts[cmd]++
}

func (r *recorder) mismatch(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mismatches = append(r.mismatches, s)
}

func (r *recorder) note(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, fmt.Sprintf(format, a...))
}

func (r *recorder) article(block []string, wire []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.articles = append(r.articles, block)
	r.articleRaw = append(r.articleRaw, wire)
}

// The limits of what a raw connection reads (a client or a DEFLATE bomb cannot fill the memory):
// one line (Options.MaxLine, default 64 KiB, the line end counts; <0 no cap: the same as in the
// other modes), one dot block, and everything read on one connection. A connection that goes
// over one is closed and a note says so.
const (
	maxBlock = 16 << 20
	maxConn  = 64 << 20
)

// rawMaxLine is the line cap of raw mode for Options.MaxLine v: 0 means fakeserver.DefaultMaxLine,
// negative none (returned as 0).
func rawMaxLine(v int) int {
	switch {
	case v == 0:
		return fakeserver.DefaultMaxLine
	case v < 0:
		return 0
	}
	return v
}

var errTooLong = errors.New("line longer than the limit")

// rawConn is one connection of mode raw.
type rawConn struct {
	c        *fakeserver.Conn
	rec      *recorder
	deadline time.Duration // Options.Deadline: bounds every wait, read and write
	maxLine  int           // longest line with its line end; 0: no cap
	conn     net.Conn
	r        *bufio.Reader
	total    int // bytes read so far
}

func (rc *rawConn) tcp() *net.TCPConn {
	nc := rc.conn
	if t, ok := nc.(*tls.Conn); ok {
		nc = t.NetConn()
	}
	t, _ := nc.(*net.TCPConn)
	return t
}

// readWire reads one line with its line end. A timed read waits at most the Deadline for it
// (the read of a line the script expects); an untimed one waits for the client (hang, the lines
// after the script) and ends with the connection. A line over the line cap or a connection over
// maxConn ends the read with a note.
func (rc *rawConn) readWire(timed bool) ([]byte, error) {
	if timed {
		_ = rc.conn.SetReadDeadline(time.Now().Add(rc.deadline))
	} else {
		_ = rc.conn.SetReadDeadline(time.Time{})
	}
	var line []byte
	for {
		if rc.r.Buffered() == 0 {
			if _, err := rc.r.Peek(1); err != nil { // waits for data without consuming
				var ce flate.CorruptInputError
				switch {
				case errors.Is(err, os.ErrDeadlineExceeded):
					rc.rec.note("read: nothing from the client within the Deadline (%v)", rc.deadline)
				case errors.As(err, &ce):
					rc.rec.note("read: %v", err)
				}
				return line, err
			}
		}
		// check the caps on what has arrived, without waiting for an LF or a full buffer
		b, _ := rc.r.Peek(rc.r.Buffered())
		n, found := len(b), false
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			n, found = i+1, true
		}
		if rc.maxLine > 0 && (len(line)+n > rc.maxLine || (!found && len(line)+n == rc.maxLine)) {
			rc.rec.note("read: a line over %d bytes, connection closed", rc.maxLine)
			return line, errTooLong
		}
		line = append(line, b[:n]...)
		_, _ = rc.r.Discard(n)
		rc.total += n
		if rc.total > maxConn {
			rc.rec.note("read: over %d bytes on one connection, closed", maxConn)
			return line, errTooLong
		}
		if found {
			return line, nil
		}
	}
}

// readLine is readWire without the line end.
func (rc *rawConn) readLine(timed bool) (string, error) {
	w, err := rc.readWire(timed)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(w), "\r\n"), nil
}

// write writes data within the Deadline.
func (rc *rawConn) write(data []byte) error {
	_ = rc.conn.SetWriteDeadline(time.Now().Add(rc.deadline))
	_, err := rc.conn.Write(data)
	return err
}

// writeChunk writes the chunk after its Gap, in one write or in pieces of Split bytes with
// SplitGap between them. The pauses end when the server closes or at the Deadline.
func (rc *rawConn) writeChunk(ch rawChunk) error {
	data, err := ch.bytes()
	if err != nil {
		return err
	}
	if ch.Gap > 0 && !rc.c.Sleep(ch.Gap) {
		return errors.New("server closed")
	}
	if ch.Split <= 0 {
		return rc.write(data)
	}
	for i := 0; i < len(data); i += ch.Split {
		if i > 0 && ch.SplitGap > 0 && !rc.c.Sleep(ch.SplitGap) {
			return errors.New("server closed")
		}
		if err := rc.write(data[i:min(i+ch.Split, len(data))]); err != nil {
			return err
		}
	}
	return nil
}

// rawHandler runs the steps on every connection, on Conn.Raw(). After the last step it records
// every line and answers it with a 500, so a client that sends more than the script expects
// shows up in mismatches.
func rawHandler(steps []rawStep, rec *recorder, tlsCfg *tls.Config, deadline time.Duration, maxLine int) func(*fakeserver.Conn) {
	res := make([]*regexp.Regexp, len(steps))
	for i, st := range steps {
		if st.Expect != nil {
			res[i] = regexp.MustCompile("^(?:" + *st.Expect + ")$") // checked by parseConfig
		}
	}
	return func(c *fakeserver.Conn) {
		rc := &rawConn{c: c, rec: rec, deadline: deadline, maxLine: maxLine, conn: c.Raw()}
		if t := rc.tcp(); t != nil {
			_ = t.SetNoDelay(true)
		}
		rc.r = bufio.NewReader(rc.conn)
		for i, st := range steps {
			if res[i] != nil {
				for {
					line, err := rc.readLine(true)
					if err != nil {
						return
					}
					rec.line(line)
					if res[i].MatchString(line) {
						break
					}
					rec.mismatch(line)
					_ = rc.write([]byte("500 unexpected " + line + "\r\n"))
				}
			}
			if st.ReadDot {
				var block []string
				var wire []byte
				for {
					w, err := rc.readWire(true)
					if err != nil {
						if !errors.Is(err, errTooLong) && !errors.Is(err, os.ErrDeadlineExceeded) {
							rec.note("dot block read: %v after %d lines", err, len(block))
						}
						return
					}
					line := strings.TrimRight(string(w), "\r\n")
					block = append(block, line)
					if line != "." {
						wire = append(wire, w...)
					}
					if len(wire) > maxBlock {
						rec.note("dot block: over %d bytes, connection closed", maxBlock)
						return
					}
					if line == "." || (st.DotMax > 0 && len(block) >= st.DotMax) {
						break
					}
				}
				rec.article(block, wire)
			}
			for _, ch := range st.Send {
				if err := rc.writeChunk(ch); err != nil {
					rec.note("write: %v", err)
					return
				}
			}
			if st.Sleep > 0 && !c.Sleep(st.Sleep) {
				return
			}
			switch st.Then {
			case "close":
				return
			case "rst":
				if t := rc.tcp(); t != nil {
					_ = t.SetLinger(0)
				}
				return
			case "hang":
				for { // holds the connection: no read deadline
					line, err := rc.readLine(false)
					if err != nil {
						return
					}
					rec.line(line)
				}
			case "starttls":
				if n := rc.r.Buffered(); n > 0 {
					rec.note("starttls: %d bytes of plaintext pipelined after the STARTTLS line", n)
					return
				}
				// The handshake is bounded by the server's Deadline; the server counts it.
				if err := c.StartTLS(tlsCfg); err != nil {
					rec.note("starttls handshake: %v", err)
					return
				}
				rc.conn = c.Raw()
				rc.r = bufio.NewReader(rc.conn)
			case "inflate":
				// the kit's streaming inflater, not compress/flate: that one holds the bytes
				// of a Z_PARTIAL_FLUSH back until more input comes (see internal/inflate)
				pr, pw := io.Pipe()
				go func(src *bufio.Reader) {
					_ = pw.CloseWithError(inflate.Run(src, func(b []byte) error {
						_, err := pw.Write(b)
						return err
					}))
				}(rc.r)
				defer pr.Close() // ends the inflater when the handler returns first
				rc.r = bufio.NewReader(pr)
			}
		}
		for { // the lines after the script: no read deadline either
			line, err := rc.readLine(false)
			if err != nil {
				return
			}
			rec.line(line)
			rec.mismatch(line)
			_ = rc.write([]byte("500 unexpected " + line + "\r\n"))
		}
	}
}
