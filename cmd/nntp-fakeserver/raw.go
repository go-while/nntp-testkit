package main

import (
	"bufio"
	"compress/flate"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// recorder holds what raw mode saw (the fakeserver records the other modes; a raw step reads
// the socket itself, so the fakeserver sees no lines).
type recorder struct {
	mu         sync.Mutex
	lines      []string
	mismatches []string
	articles   [][]string
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

func (r *recorder) article(block []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.articles = append(r.articles, block)
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// writeChunk writes data after the chunk's Gap, in one write or in pieces of Split bytes with
// SplitGap between them.
func writeChunk(w io.Writer, ch rawChunk) error {
	data, err := ch.bytes()
	if err != nil {
		return err
	}
	time.Sleep(ch.Gap)
	if ch.Split <= 0 {
		_, err = w.Write(data)
		return err
	}
	for i := 0; i < len(data); i += ch.Split {
		if i > 0 {
			time.Sleep(ch.SplitGap)
		}
		if _, err = w.Write(data[i:min(i+ch.Split, len(data))]); err != nil {
			return err
		}
	}
	return nil
}

// rawHandler runs the steps on every connection, on Conn.Raw(). After the last step it records
// every line and answers it with a 500, so a client that sends more than the script expects
// shows up in mismatches.
func rawHandler(steps []rawStep, rec *recorder, tlsCfg *tls.Config) func(*fakeserver.Conn) {
	res := make([]*regexp.Regexp, len(steps))
	for i, st := range steps {
		if st.Expect != nil {
			res[i] = regexp.MustCompile("^(?:" + *st.Expect + ")$") // checked by parseConfig
		}
	}
	return func(c *fakeserver.Conn) {
		conn := c.Raw()
		tcp := func() *net.TCPConn {
			nc := conn
			if t, ok := nc.(*tls.Conn); ok {
				nc = t.NetConn()
			}
			t, _ := nc.(*net.TCPConn)
			return t
		}
		if t := tcp(); t != nil {
			_ = t.SetNoDelay(true)
		}
		r := bufio.NewReader(conn)
		var w io.Writer = conn
		for i, st := range steps {
			if res[i] != nil {
				for {
					line, err := readLine(r)
					if err != nil {
						return
					}
					rec.line(line)
					if res[i].MatchString(line) {
						break
					}
					rec.mismatch(line)
					fmt.Fprintf(w, "500 unexpected %s\r\n", line)
				}
			}
			if st.ReadDot {
				var block []string
				for {
					line, err := readLine(r)
					if err != nil {
						rec.note("dot block read: %v after %d lines", err, len(block))
						return
					}
					block = append(block, line)
					if line == "." || (st.DotMax > 0 && len(block) >= st.DotMax) {
						break
					}
				}
				rec.article(block)
			}
			for _, ch := range st.Send {
				if err := writeChunk(w, ch); err != nil {
					rec.note("write: %v", err)
					return
				}
			}
			time.Sleep(st.Sleep)
			switch st.Then {
			case "close":
				return
			case "rst":
				if t := tcp(); t != nil {
					_ = t.SetLinger(0)
				}
				return
			case "hang":
				for {
					line, err := readLine(r)
					if err != nil {
						return
					}
					rec.line(line)
				}
			case "starttls":
				if n := r.Buffered(); n > 0 {
					rec.note("starttls: %d bytes of plaintext pipelined after the STARTTLS line", n)
					return
				}
				// The handshake is bounded by the server's Deadline; the server counts it.
				if err := c.StartTLS(tlsCfg); err != nil {
					rec.note("starttls handshake: %v", err)
					return
				}
				conn = c.Raw()
				r = bufio.NewReader(conn)
				w = conn
			case "inflate":
				r = bufio.NewReader(flate.NewReader(r))
			}
		}
		for {
			line, err := readLine(r)
			if err != nil {
				return
			}
			rec.line(line)
			rec.mismatch(line)
			fmt.Fprintf(w, "500 unexpected %s\r\n", line)
		}
	}
}
