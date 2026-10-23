package fakeserver

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// readerSize is the read buffer of a server-side connection (bufio grows lines beyond it).
const readerSize = 64 << 10

// Conn is one server-side connection of a Server. Its methods are meant for the connection's
// own goroutine (the Handler); they are not safe for concurrent use.
type Conn struct {
	s    *Server
	raw  net.Conn // the accepted TCP conn
	conn net.Conn // raw, or the TLS server conn on top of it
	r    *bufio.Reader
	gone bool           // guarded by s.mu: counted out of Open()
	cmds int            // client lines read with ReadLine on this conn (Faults.CloseAfter)
	id   int            // accept order, from 1 (Conn.ID)
	seen map[string]int // commands read on this conn (Faults.OverrideFrom)
	used map[string]int // overrides applied on this conn (Faults.OverrideTimes)
	last string         // status line of the last reply written by writeReply (after the faults; "" when it wrote none)
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
// terminator) in Articles(). The lines are neither recorded in Lines() nor counted. A cut-off
// block is not recorded.
func (c *Conn) ReadDotBlock() ([]string, error) {
	block := []string{}
	for {
		line, err := c.readRaw()
		if err != nil {
			return block, err
		}
		if line == "." {
			break
		}
		block = append(block, line)
	}
	c.s.mu.Lock()
	c.s.articles = append(c.s.articles, append([]string{}, block...))
	c.s.mu.Unlock()
	return block, nil
}

// Raw returns the connection the server reads and writes (the TLS conn when TLS is set;
// after StartCompress or StartCompressOutputOnly the compressing conn on top of it).
func (c *Conn) Raw() net.Conn { return c.conn }

// Server returns the server this connection belongs to.
func (c *Conn) Server() *Server { return c.s }

// readRaw reads one line without recording it; a read error counts the conn out of Open().
// There is no read deadline: Close unblocks it.
func (c *Conn) readRaw() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.markGone()
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r"), nil
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
	_ = c.conn.Close()
	if c.conn != c.raw {
		_ = c.raw.Close()
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
func (c *Conn) faultReply(line string, lines []string) (out []string, drop int) {
	f := c.s.o.Faults
	cmd := command(line)
	if o, ok := c.override(line); ok {
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
// DropInMultiline, a Drip that ran past the Deadline, or an Endless reply.
func (c *Conn) writeReply(line string, lines ...string) bool {
	cmd := command(line)
	if !c.faultDelay(cmd) {
		return false
	}
	lines, drop := c.faultReply(line, lines)
	c.last = ""
	if len(lines) > 0 {
		c.last = lines[0]
	}
	f := c.s.o.Faults
	endless := f.Endless[cmd]
	if endless && len(lines) > 0 && lines[len(lines)-1] == "." {
		lines = lines[:len(lines)-1]
	}
	drip := f.Drip[cmd]
	pause := f.LineDelay[cmd]
	if drip > 0 || endless || pause > 0 {
		// bounded like every server-side wait; cleared again after this reply
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.s.deadline))
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	w := c.writer(drip)
	if pause > 0 {
		end := time.Now().Add(c.s.deadline)
		for i, l := range lines {
			if i > 0 && (time.Now().After(end) || !c.sleep(pause)) {
				return false
			}
			if !w(joinCRLF([]string{l})) {
				return false
			}
		}
	} else if len(lines) > 0 && !w(joinCRLF(lines)) {
		return false
	}
	if endless {
		for n := 1; ; n++ {
			if !w(joinCRLF([]string{fmt.Sprintf("%d endless", n)})) {
				return false
			}
		}
	}
	return drop < 0
}

func joinCRLF(lines []string) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// writer returns a function that writes p to the client and reports whether it may go on: at
// once, or with Faults.Drip, one byte at a time with drip between them, for at most the
// server's Deadline from now. writeReply sets a write deadline for Drip and Endless, so a
// client that stops reading cannot block them for longer; a closed server stops them too.
func (c *Conn) writer(drip time.Duration) func(p []byte) bool {
	end := time.Now().Add(c.s.deadline)
	return func(p []byte) bool {
		if drip <= 0 {
			_, err := c.conn.Write(p)
			return err == nil
		}
		for _, b := range p {
			if _, err := c.conn.Write([]byte{b}); err != nil {
				return false
			}
			if time.Now().After(end) || !c.sleep(drip) {
				return false
			}
		}
		return true
	}
}

// closeDue reports whether Faults.CloseAfter is reached: this connection has read that many
// client lines with ReadLine. Call it after the reply to a command.
func (c *Conn) closeDue() bool {
	n := c.s.o.Faults.CloseAfter
	return n > 0 && c.cmds >= n
}
