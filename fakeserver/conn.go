package fakeserver

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strings"
	"time"
)

// errDropInUpload is what ReadDotBlockRaw returns when Faults.DropInUpload cut the block.
var errDropInUpload = errors.New("fakeserver: Faults.DropInUpload")

// readerSize is the read buffer of a server-side connection (bufio grows lines beyond it).
const readerSize = 64 << 10

// Conn is one server-side connection of a Server. Its methods are meant for the connection's
// own goroutine (the Handler); they are not safe for concurrent use.
type Conn struct {
	s    *Server
	raw  net.Conn  // the accepted TCP conn
	conn net.Conn  // raw, or the TLS server conn on top of it
	tls  *tls.Conn // the TLS conn once TLS is on (Options.TLS or StartTLS), else nil
	r    *bufio.Reader
	gone bool           // guarded by s.mu: counted out of Open()
	cmds int            // client lines read with ReadLine on this conn (Faults.CloseAfter)
	id   int            // accept order, from 1 (Conn.ID)
	seen map[string]int // commands read on this conn (Faults.OverrideFrom)
	used map[string]int // overrides applied on this conn (Faults.OverrideTimes)
	last string         // status line of the last reply written by writeReply (after the faults; "" when it wrote none)

	cmd     string // command of the last client line read with ReadLine (the key of the byte-level faults)
	faulted bool   // a fault closes the connection now (Faults.Abort decides how)

	shape     CompressShape // the shape of the stream compressAfter starts (the handler's CompressShape)
	shapeStep bool          // Script: the step being served starts compression, whatever its command
	enc       *encoder      // made when the 206 carried pipelined bytes; startCompress takes it over
}

// ID returns the connection's number in accept order, starting at 1 (connections rejected by
// Options.MaxConns count too). StoreOptions.GroupNumbers gets it.
func (c *Conn) ID() int { return c.id }

func newReader(c net.Conn) *bufio.Reader { return bufio.NewReaderSize(c, readerSize) }

// command returns the upper-cased first word of a client line ("" for a blank line): the key
// of Count, Counts and Faults.
func command(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return strings.ToUpper(f[0])
}

// lineKey is the key of Faults.OverrideLine for a client line: the command upper-cased and the
// arguments as sent, joined by single spaces.
func lineKey(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	f[0] = strings.ToUpper(f[0])
	return strings.Join(f, " ")
}

// ReadLine reads one client line (CRLF or LF stripped), records it in Lines() and counts its
// command (Count, Counts, Faults.CloseAfter).
func (c *Conn) ReadLine() (string, error) {
	line, err := c.readRaw()
	if err != nil {
		return "", err
	}
	c.cmds++
	c.cmd = command(line)
	if c.seen == nil {
		c.seen = make(map[string]int)
	}
	c.seen[command(line)]++
	c.s.mu.Lock()
	c.s.lines = append(c.s.lines, line)
	c.s.counts[command(line)]++
	c.s.mu.Unlock()
	return line, nil
}

// WriteLines writes each line followed by CRLF in one Write.
func (c *Conn) WriteLines(lines ...string) error {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	_, err := io.WriteString(c.conn, b.String())
	return err
}

// ReadDotBlock reads lines until "." and records them (still dot-stuffed, without the
// terminator) in Articles(), and the exact bytes in ArticlesRaw(). The lines are neither
// recorded in Lines() nor counted. A cut-off block is not recorded.
func (c *Conn) ReadDotBlock() ([]string, error) {
	block, _, err := c.ReadDotBlockRaw()
	return block, err
}

// ReadDotBlockRaw is ReadDotBlock that also returns the block as the client wrote it: the
// exact bytes after the command up to, not including, the "." terminator line, still
// dot-stuffed, every line end (CRLF, or a bare LF) and every other byte as sent; empty (not
// nil) for an empty block. article.ParseDotBlock takes it. The same bytes are recorded in
// ArticlesRaw().
func (c *Conn) ReadDotBlockRaw() ([]string, []byte, error) {
	block := []string{}
	raw := []byte{}
	limit, cut := c.s.o.Faults.DropInUpload[c.cmd]
	for n := 0; ; n++ {
		if cut && n >= limit {
			c.faulted = true
			return block, raw, errDropInUpload
		}
		line, wire, err := c.readRawLine()
		if err != nil {
			return block, raw, err
		}
		if line == "." {
			break
		}
		block = append(block, line)
		raw = append(raw, wire...)
	}
	c.s.mu.Lock()
	c.s.articles = append(c.s.articles, append([]string{}, block...))
	c.s.articlesRaw = append(c.s.articlesRaw, append([]byte{}, raw...))
	c.s.mu.Unlock()
	return block, raw, nil
}

// Raw returns the connection the server reads and writes (the TLS conn when TLS is set;
// after StartCompress or StartCompressOutputOnly the compressing conn on top of it).
func (c *Conn) Raw() net.Conn { return c.conn }

// Server returns the server this connection belongs to.
func (c *Conn) Server() *Server { return c.s }

// readRaw reads one line without recording it; a read error counts the conn out of Open().
// There is no read deadline: Close unblocks it (a TLS handshake is bounded by the server's Deadline).
func (c *Conn) readRaw() (string, error) {
	line, _, err := c.readRawLine()
	return line, err
}

// readRawLine is readRaw that also returns the line with its line end as it arrived.
func (c *Conn) readRawLine() (line, wire string, err error) {
	wire, err = c.r.ReadString('\n')
	if err != nil {
		c.markGone()
		return "", "", err
	}
	line = strings.TrimSuffix(wire, "\n")
	return strings.TrimSuffix(line, "\r"), wire, nil
}

// drain reads and records lines until EOF/error.
func (c *Conn) drain() {
	for {
		if _, err := c.ReadLine(); err != nil {
			return
		}
	}
}

// sleep waits d (at most the server's Deadline) or until the server is closed; false when closed.
func (c *Conn) sleep(d time.Duration) bool {
	tm := time.NewTimer(min(d, c.s.deadline))
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-c.s.done:
		return false
	}
}

func (c *Conn) markGone() {
	c.s.mu.Lock()
	if !c.gone {
		c.gone = true
		c.s.open--
	}
	c.s.mu.Unlock()
}

func (c *Conn) finish() {
	c.markGone()
	if c.faulted && c.s.o.Faults.Abort[c.cmd] {
		// Faults.Abort: plain TCP is reset; a TLS conn is cut under the TLS layer, which would
		// send close_notify on Close (so no linger there: the end is a FIN after the last record)
		if tc, ok := c.raw.(*net.TCPConn); ok && c.tls == nil {
			_ = tc.SetLinger(0)
		}
		if d, ok := c.conn.(*deflateConn); ok && d.pr != nil {
			_ = d.pr.Close() // ends the inflater goroutine, which may block in a write to the pipe
		}
		_ = c.raw.Close()
	} else {
		_ = c.conn.Close()
		if c.conn != c.raw {
			_ = c.raw.Close()
		}
	}
	c.s.mu.Lock()
	delete(c.s.conns, c.raw)
	c.s.mu.Unlock()
}

// faultDelay sleeps Faults.Delay[cmd] when set (bounded like sleep); false when the server was
// closed meanwhile.
func (c *Conn) faultDelay(cmd string) bool {
	d := c.s.o.Faults.Delay[cmd]
	if d <= 0 {
		return true
	}
	return c.sleep(d)
}

// override returns the Faults.OverrideLine reply for the client line, else the Faults.Override
// reply for its command; ok is false when neither is set.
func (c *Conn) override(line string) (lines []string, ok bool) {
	f := c.s.o.Faults
	cmd := command(line)
	if from, set := f.OverrideFrom[cmd]; set && c.seen[cmd] < from {
		return nil, false
	}
	if n, set := f.OverrideTimes[cmd]; set && c.used[cmd] >= n {
		return nil, false
	}
	o, ok := f.OverrideLine[lineKey(line)]
	if !ok {
		o, ok = f.Override[cmd]
	}
	if ok && f.OverrideTimes != nil {
		if c.used == nil {
			c.used = make(map[string]int)
		}
		c.used[cmd]++
	}
	return o, ok
}

// faultReply applies Faults.OverrideLine or Override, LongLine and DropInMultiline for the
// client line to lines: it returns the lines to write and drop >= 0 when the connection must be
// closed after writing them (the reply was cut), else drop == -1.
func (c *Conn) faultReply(line string, lines []string, useOverride bool) (out []string, drop int) {
	f := c.s.o.Faults
	cmd := command(line)
	if !useOverride {
		// lines are already the override (OverrideFinal)
	} else if o, ok := c.override(line); ok {
		lines = o
	}
	if n, ok := f.LongLine[cmd]; ok && n > 0 && len(lines) > 0 {
		lines = append([]string{lines[0], strings.Repeat("x", n)}, lines[1:]...)
	}
	if n, ok := f.DropInMultiline[cmd]; ok && n >= 0 && len(lines) > n {
		return lines[:n], n
	}
	return lines, -1
}

// writeReply writes the reply to the client line with the Faults applied (see Faults for the
// order). An empty reply writes nothing. It returns false when the caller must stop serving
// the connection: the server closed during the delay, a write error, a reply cut by
// DropInMultiline or DropAfterBytes, a Drip or Split that ran past the Deadline, or an Endless
// reply.
func (c *Conn) writeReply(line string, lines ...string) bool {
	return c.writeKind(line, replyFirst, lines)
}

// writeFinal writes the final reply of POST or IHAVE (after the article): with the Faults of
// the command that are meant for it (see Faults), not the ones of its first reply.
func (c *Conn) writeFinal(line string, lines ...string) bool {
	return c.writeKind(line, replyFinal, lines)
}

// replyKind selects the faults writeKind applies.
type replyKind int

const (
	replyFirst replyKind = iota // every fault but OverrideFinal and DelayFinal
	replyFinal                  // OverrideFinal, DelayFinal and the byte-level faults only
	replySole                   // the only reply of a command (TAKETHIS): all faults
)

func (c *Conn) writeKind(line string, kind replyKind, lines []string) bool {
	cmd := command(line)
	f := c.s.o.Faults
	normal, final := kind != replyFinal, kind != replyFirst
	if normal && !c.faultDelay(cmd) {
		return false
	}
	if final {
		if d := f.DelayFinal[cmd]; d > 0 && !c.sleep(d) {
			return false
		}
	}
	drop := -1
	of, haveOF := f.OverrideFinal[cmd]
	if !final {
		haveOF = false
	}
	if haveOF {
		lines = of
	}
	if normal {
		lines, drop = c.faultReply(line, lines, !haveOF)
	}
	c.last = ""
	if len(lines) > 0 {
		c.last = lines[0]
	}
	tail := c.pipeline206(cmd == "COMPRESS" || c.shapeStep, c.last) // empty flush etc. in the 206's write
	endless := normal && f.Endless[cmd]
	if endless && len(lines) > 0 && lines[len(lines)-1] == "." {
		lines = lines[:len(lines)-1]
	}
	var drip, pause time.Duration
	if normal {
		drip, pause = f.Drip[cmd], f.LineDelay[cmd]
	}
	sp := f.Split[cmd]
	limit := -1
	if n, ok := f.DropAfterBytes[cmd]; ok && n >= 0 {
		limit = n
	}
	if drip > 0 || endless || pause > 0 || sp.Gap > 0 {
		// bounded like every server-side wait; cleared again after this reply
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.s.deadline))
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	w := c.newReplyWriter(drip, sp, limit)
	if pause > 0 {
		end := time.Now().Add(c.s.deadline)
		for i, l := range lines {
			if i > 0 && (time.Now().After(end) || !c.sleep(pause)) {
				return c.cutIf(w)
			}
			b := joinCRLF([]string{l})
			if i == len(lines)-1 {
				b = append(b, tail...)
			}
			if !w.write(b) {
				return c.cutIf(w)
			}
		}
	} else if len(lines) > 0 && !w.write(append(joinCRLF(lines), tail...)) {
		return c.cutIf(w)
	}
	if endless {
		for n := 1; ; n++ {
			if !w.write(joinCRLF([]string{fmt.Sprintf("%d endless", n)})) {
				return c.cutIf(w)
			}
		}
	}
	if drop >= 0 {
		c.faulted = true
		return false
	}
	if tr, ok := f.Trailing[cmd]; ok && len(tr.Lines) > 0 {
		if tr.Gap > 0 && !c.sleep(tr.Gap) {
			return false
		}
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.s.deadline))
		_, err := c.conn.Write(joinCRLF(tr.Lines))
		_ = c.conn.SetWriteDeadline(time.Time{})
		return err == nil
	}
	return true
}

// cutIf marks the connection as closed by a fault when the writer stopped at DropAfterBytes;
// it returns false: the reply did not complete.
func (c *Conn) cutIf(w *replyWriter) bool {
	if w.dropped {
		c.faulted = true
	}
	return false
}

func joinCRLF(lines []string) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// replyWriter writes one reply to the client, possibly in several writes, and reports whether
// it may go on: at once, or cut at Faults.Split's offsets with its Gap between the pieces, or
// with Faults.Drip one byte at a time with drip between them, and stopped by
// Faults.DropAfterBytes, for at most the server's Deadline from its creation. The offsets count
// over all write calls of the reply. writeKind sets a write deadline for Drip, Endless, LineDelay
// and Split, so a client that stops reading cannot block them for longer; a closed server stops
// them too.
type replyWriter struct {
	c       *Conn
	end     time.Time
	drip    time.Duration
	at      []int // Split.At: sorted, positive, unique
	size    int   // Split.Size when At is empty
	gap     time.Duration
	limit   int // DropAfterBytes; -1: none
	pos     int // bytes written
	dropped bool
}

func (c *Conn) newReplyWriter(drip time.Duration, sp Split, limit int) *replyWriter {
	w := &replyWriter{c: c, end: time.Now().Add(c.s.deadline), drip: drip, gap: sp.Gap, limit: limit}
	for _, a := range sp.At {
		if a > 0 {
			w.at = append(w.at, a)
		}
	}
	sort.Ints(w.at)
	w.at = slices.Compact(w.at)
	if len(w.at) == 0 && sp.Size > 0 {
		w.size = sp.Size
	}
	return w
}

// nextCut returns the next offset after pos where a piece ends; 0: none.
func (w *replyWriter) nextCut() int {
	if w.size > 0 {
		return (w.pos/w.size + 1) * w.size
	}
	for _, a := range w.at {
		if a > w.pos {
			return a
		}
	}
	return 0
}

func (w *replyWriter) write(p []byte) bool {
	if w.dropped {
		return false
	}
	if w.limit >= 0 && w.pos+len(p) > w.limit {
		p = p[:max(0, w.limit-w.pos)]
		w.dropped = true
	}
	for len(p) > 0 {
		n, cut := len(p), false
		if w.drip > 0 {
			n = 1
		} else if k := w.nextCut(); k > 0 && k-w.pos < n {
			n, cut = k-w.pos, true
		}
		if _, err := w.c.conn.Write(p[:n]); err != nil {
			return false
		}
		w.pos += n
		p = p[n:]
		switch {
		case w.drip > 0:
			if time.Now().After(w.end) || !w.c.sleep(w.drip) {
				return false
			}
		case cut && w.gap > 0:
			if time.Now().After(w.end) || !w.c.sleep(w.gap) {
				return false
			}
		}
	}
	return !w.dropped
}

// closeDue reports whether Faults.CloseAfter is reached: this connection has read that many
// client lines with ReadLine. Call it after the reply to a command.
func (c *Conn) closeDue() bool {
	n := c.s.o.Faults.CloseAfter
	if n > 0 && c.cmds >= n {
		c.faulted = true
		return true
	}
	return false
}
