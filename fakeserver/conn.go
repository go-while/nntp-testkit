package fakeserver

import (
	"bufio"
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
	gone bool // guarded by s.mu: counted out of Open()
	cmds int  // client lines read with ReadLine on this conn (Faults.CloseAfter)
}

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

// ReadLine reads one client line (CRLF or LF stripped), records it in Lines() and counts its
// command (Count, Counts, Faults.CloseAfter).
func (c *Conn) ReadLine() (string, error) {
	line, err := c.readRaw()
	if err != nil {
		return "", err
	}
	c.cmds++
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

// Raw returns the connection the server reads and writes (the TLS conn when TLS is set).
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

// faultReply applies Faults.Override and Faults.DropInMultiline for cmd to lines: it returns
// the lines to write and drop >= 0 when the connection must be closed after writing them (the
// reply was cut), else drop == -1.
func (c *Conn) faultReply(cmd string, lines []string) (out []string, drop int) {
	f := c.s.o.Faults
	if o, ok := f.Override[cmd]; ok {
		lines = o
	}
	if n, ok := f.DropInMultiline[cmd]; ok && n >= 0 && len(lines) > n {
		return lines[:n], n
	}
	return lines, -1
}

// writeReply writes the reply to cmd with the Faults applied: Delay, then Override, then
// DropInMultiline. An empty reply writes nothing. It returns false when the caller must stop
// serving the connection: the server closed during the delay, a write error, or a reply cut by
// DropInMultiline.
func (c *Conn) writeReply(cmd string, lines ...string) bool {
	if !c.faultDelay(cmd) {
		return false
	}
	lines, drop := c.faultReply(cmd, lines)
	if len(lines) > 0 {
		if err := c.WriteLines(lines...); err != nil {
			return false
		}
	}
	return drop < 0
}

// closeDue reports whether Faults.CloseAfter is reached: this connection has read that many
// client lines with ReadLine. Call it after the reply to a command.
func (c *Conn) closeDue() bool {
	n := c.s.o.Faults.CloseAfter
	return n > 0 && c.cmds >= n
}
