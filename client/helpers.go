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
