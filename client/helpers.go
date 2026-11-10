package client

import (
	"fmt"
	"strings"
)

// ExpectCmd sends one command line (format and args, CRLF appended) and fails tb unless the
// reply code is want; it returns the status line. An I/O, parse or strict error fails tb as in
// Cmd. Use c.For(t).ExpectCmd in a subtest that shares a parent's connection.
func (c *Conn) ExpectCmd(want int, format string, args ...any) string {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line := c.Cmd(format, args...)
	if code != want {
		c.fatalf("%s: got %q, want %d", fmt.Sprintf(format, args...), line, want)
	}
	return line
}

// ExpectStatus reads the next status line and fails tb unless its code is want; what names
// the step in the failure (e.g. "POST article"). It returns the line. Use it for the second
// status of a two-step command (POST, IHAVE) or for a pipelined reply.
func (c *Conn) ExpectStatus(want int, what string) string {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.ReadStatus()
	if err != nil {
		c.fatalf("%s: %v (line %q)", what, err, line)
	}
	if code != want {
		c.fatalf("%s: got %q, want %d", what, line, want)
	}
	return line
}

// MustSend is Send that fails tb on a write error: raw bytes, nothing appended (e.g. a
// Stuff block after a 340 or 335).
func (c *Conn) MustSend(raw string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	if err := c.Send(raw); err != nil {
		c.fatalf("send %d bytes: %v", len(raw), err)
	}
}

// MustSendBytes is SendBytes that fails tb on a write error: raw bytes, nothing appended (e.g.
// StuffBytes(article) after a 340 or 335).
func (c *Conn) MustSendBytes(raw []byte) {
	if c.tb != nil {
		c.tb.Helper()
	}
	if err := c.SendBytes(raw); err != nil {
		c.fatalf("send %d bytes: %v", len(raw), err)
	}
}

// expectTransfer is the end of ExpectPost, ExpectIhave and ExpectTakethis: fail tb unless the
// article was sent and the final reply is want.
func (c *Conn) expectTransfer(cmd string, want int, code int, line string, sent bool, err error) string {
	if c.tb != nil {
		c.tb.Helper()
	}
	if err != nil {
		c.fatalf("%s: %v (last reply %d %q)", cmd, err, code, line)
	}
	if !sent {
		c.fatalf("%s: refused before the article was sent: %q, want %d after the transfer", cmd, line, want)
	}
	if code != want {
		c.fatalf("%s: got %q after the transfer, want %d", cmd, line, want)
	}
	return line
}

// ExpectPost is TryPost that fails tb unless the server took the article (340) and the final
// reply is want (240, 441 ...); it returns that final line. A server that refuses POST itself
// (440, 480 ...) fails the test, also when want would match that code (436): use ExpectCmd
// for "POST" to expect a refusal before the transfer. An I/O, parse or strict error fails tb.
func (c *Conn) ExpectPost(want int, article []byte) string {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, sent, err := c.TryPost(article)
	return c.expectTransfer("POST", want, code, line, sent, err)
}

// ExpectIhave is TryIhave that fails tb unless the server took the article (335) and the final
// reply is want (235, 436, 437); it returns that final line. A refusal before the transfer
// (435, 436, 480 ...) fails the test, so a 436 that comes after the article is told from one
// that comes before it. Use ExpectCmd for "IHAVE <id>" to expect the early refusal.
func (c *Conn) ExpectIhave(want int, id string, article []byte) string {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, sent, err := c.TryIhave(id, article)
	return c.expectTransfer("IHAVE "+id, want, code, line, sent, err)
}

// ExpectTakethis is TryTakethis that fails tb unless the reply is want (239, 439 ...); it
// returns the line. The article is always sent; the form exists so that the three transfer
// commands have the same Expect shape.
func (c *Conn) ExpectTakethis(want int, id string, article []byte) string {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, sent, err := c.TryTakethis(id, article)
	return c.expectTransfer("TAKETHIS "+id, want, code, line, sent, err)
}

// Capabilities sends CAPABILITIES, fails tb unless the reply is 101 with a dot block that
// ParseCapabilities accepts, and returns the capabilities by keyword and the lines in order.
func (c *Conn) Capabilities() (caps map[string][]string, lines []string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	c.ExpectCmd(101, "CAPABILITIES")
	lines = c.ReadDotLines()
	caps, err := ParseCapabilities(lines)
	if err != nil {
		c.fatalf("CAPABILITIES: %v (lines %q)", err, lines)
	}
	return caps, lines
}

// ParseCapabilities parses the lines of a CAPABILITIES reply (RFC 3977 section 5.2, without
// the 101 line and the terminator, as ReadDotLines returns them) into keyword → arguments.
// Capability labels are case-insensitive (RFC 3977 section 3.3.1), so the keys are
// upper-cased: look up "READER", not "reader". The arguments are the tokens after the label
// (separated by spaces or tabs), kept as sent; they are case-insensitive too unless the
// capability says otherwise, so compare them with strings.EqualFold. A label without arguments
// maps to an empty, non-nil slice, so `_, ok := caps["READER"]` tells whether it is listed.
//
// A blank line and a label listed twice are errors; the map holds every other line all the
// same (for a label listed twice, its first line), so a lenient caller may ignore the error.
// Neither the order (VERSION first) nor the label syntax is checked: the conformance package's
// capabilities check does that.
func ParseCapabilities(lines []string) (map[string][]string, error) {
	caps := make(map[string][]string, len(lines))
	var err error
	for i, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			if err == nil {
				err = fmt.Errorf("capability line %d is blank: %q", i+1, l)
			}
			continue
		}
		kw := strings.ToUpper(f[0])
		if _, dup := caps[kw]; dup {
			if err == nil {
				err = fmt.Errorf("capability %s is listed twice (line %d: %q)", kw, i+1, l)
			}
			continue
		}
		caps[kw] = f[1:]
	}
	return caps, err
}
