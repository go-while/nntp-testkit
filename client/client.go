package client

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// readerSize is the buffer of Conn.R: large enough for any status line and most article lines.
const readerSize = 1 << 20

// Conn is a raw NNTP test client with a hang guard per blocking step. The fields are exported
// so a test can reach the socket or the reader directly; mixing direct reads with the methods
// is fine as long as both go through R.
type Conn struct {
	Conn     net.Conn
	R        *bufio.Reader
	Greeting string        // the greeting line read by Dial/ReadGreeting (CRLF stripped)
	Deadline time.Duration // hang guard per blocking step; 0 = testkit.Deadline()
	Strict   bool          // report wire mistakes (see the package doc); Dial sets it

	tb testing.TB // nil for TryDial: the tb-failing methods then panic
}

// Dial is DialWith(tb, addr, 0): the hang guard is testkit.Deadline().
func Dial(tb testing.TB, addr string) *Conn {
	tb.Helper()
	return DialWith(tb, addr, 0)
}

// DialWith connects to addr and reads the greeting into c.Greeting within deadline (0 =
// testkit.Deadline()), sets c.Deadline to it and registers tb.Cleanup(Close). Strict mode is
// on. A dial or greeting error fails tb.
func DialWith(tb testing.TB, addr string, deadline time.Duration) *Conn {
	tb.Helper()
	c, err := TryDial(addr, deadline)
	if err != nil {
		tb.Fatalf("dial %s: %v", addr, err)
	}
	return c.WithTB(tb)
}

// WithTB attaches tb to c (for a Conn from TryDial or a struct literal) and registers
// tb.Cleanup(Close): Cmd, ReadDotLines and RawUntilDot then fail tb instead of panicking.
func (c *Conn) WithTB(tb testing.TB) *Conn {
	tb.Helper()
	c.tb = tb
	tb.Cleanup(c.Close)
	return c
}

// TryDial connects to addr and reads the greeting, without a testing dependency. deadline is
// the hang guard for the dial and every later step (0 = testkit.Deadline()). Strict mode is
// on. The tb-failing methods (Cmd, ReadDotLines, RawUntilDot) panic on a Conn from TryDial
// unless WithTB attached a tb; use the Try forms there. The caller closes the Conn.
func TryDial(addr string, deadline time.Duration) (*Conn, error) {
	c := &Conn{Deadline: deadline, Strict: true}
	nc, err := net.DialTimeout("tcp", addr, c.deadline())
	if err != nil {
		return nil, err
	}
	c.Conn = nc
	c.R = bufio.NewReaderSize(nc, readerSize)
	if _, err := c.ReadGreeting(); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	return c, nil
}

// New wraps an open connection (for example one end of net.Pipe or a TLS client). It does
// not read a greeting (call ReadGreeting) and registers tb.Cleanup(Close). Strict mode is on.
func New(tb testing.TB, nc net.Conn) *Conn {
	tb.Helper()
	c := &Conn{Conn: nc, R: bufio.NewReaderSize(nc, readerSize), Strict: true, tb: tb}
	tb.Cleanup(c.Close)
	return c
}

// deadline is the effective hang guard: c.Deadline, or testkit.Deadline() when it is 0.
func (c *Conn) deadline() time.Duration {
	if c.Deadline > 0 {
		return c.Deadline
	}
	return testkit.Deadline()
}

// bump moves the read and write deadline of the socket to now + the hang guard.
func (c *Conn) bump() {
	_ = c.Conn.SetDeadline(time.Now().Add(c.deadline()))
}

// fatalf fails c.tb, or panics when the Conn has no tb (TryDial).
func (c *Conn) fatalf(format string, args ...any) {
	if c.tb == nil {
		panic("client: " + fmt.Sprintf(format, args...))
	}
	c.tb.Helper()
	c.tb.Fatalf(format, args...)
}

// ReadGreeting reads one line into c.Greeting and returns it.
func (c *Conn) ReadGreeting() (string, error) {
	line, err := c.ReadLine()
	if err != nil {
		return line, err
	}
	c.Greeting = line
	return line, nil
}

// Send writes raw bytes with nothing appended: use it for pipelining or malformed input.
func (c *Conn) Send(raw string) error {
	c.bump()
	_, err := io.WriteString(c.Conn, raw)
	return err
}

// ReadLine reads one line and strips the line end. In strict mode the line must end in CRLF
// and must not hold a bare CR (else a *StrictError with the raw line returned too); in
// non-strict mode a bare LF is a line end as well and the partial line is returned with the
// read error.
func (c *Conn) ReadLine() (string, error) {
	if c.Strict {
		return c.readStrictLine()
	}
	c.bump()
	line, err := c.R.ReadString('\n')
	if err != nil {
		return line, err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ReadStatus reads the next status line and returns its code and the full line.
func (c *Conn) ReadStatus() (int, string, error) {
	line, err := c.ReadLine()
	if err != nil {
		return 0, line, err
	}
	code, err := ParseCode(line)
	if err != nil {
		return 0, line, err
	}
	return code, line, nil
}

// TryCmd sends one command line (format and args, CRLF appended) and reads the status line.
func (c *Conn) TryCmd(format string, args ...any) (int, string, error) {
	if err := c.Send(fmt.Sprintf(format, args...) + "\r\n"); err != nil {
		return 0, "", err
	}
	return c.ReadStatus()
}

// Cmd is TryCmd that fails tb on an I/O, parse or strict error.
func (c *Conn) Cmd(format string, args ...any) (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryCmd(format, args...)
	if err != nil {
		c.fatalf("cmd %q: %v (line %q)", fmt.Sprintf(format, args...), err, line)
	}
	return code, line
}

// TryReadDotLines reads a dot-terminated block, un-stuffs leading dots and strips the line
// ends. On error it returns the lines read so far. In strict mode a line that starts with "."
// but is neither ".." stuffed nor the terminator is a BadDotStuffing error (the rest of the
// block stays unread), a BareLF or BareCR line inside the block is returned as ReadLine
// reports it, and a block that ends (EOF or deadline, with or without a partial last line)
// before ".\r\n" is a NoTerminator error wrapping the read error.
func (c *Conn) TryReadDotLines() ([]string, error) {
	var out []string
	for {
		line, err := c.ReadLine()
		if err != nil {
			var se *StrictError
			if errors.As(err, &se) {
				if se.Kind == NoCRLF {
					// a partial last line at EOF/deadline: the block has no terminator
					return out, &StrictError{Kind: NoTerminator, Line: se.Line,
						Err: fmt.Errorf("after %d lines: %w", len(out), se.Err)}
				}
				return out, err
			}
			err = fmt.Errorf("after %d lines: %w", len(out), err)
			if c.Strict {
				return out, &StrictError{Kind: NoTerminator, Line: line, Err: err}
			}
			return out, err
		}
		if line == "." {
			return out, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		} else if c.Strict && strings.HasPrefix(line, ".") {
			return out, &StrictError{Kind: BadDotStuffing, Line: line + "\r\n"}
		}
		out = append(out, line)
	}
}

// ReadDotLines is TryReadDotLines that fails tb on error.
func (c *Conn) ReadDotLines() []string {
	if c.tb != nil {
		c.tb.Helper()
	}
	out, err := c.TryReadDotLines()
	if err != nil {
		c.fatalf("readDotLines: %v", err)
	}
	return out
}

// RawUntilDot returns the exact bytes up to and including the "\r\n.\r\n" terminator (or
// ".\r\n" when the block is empty), without un-stuffing. It fails tb on error. In strict mode
// every line must end in CRLF without a bare CR and only ".\r\n" terminates; in non-strict mode
// ".\n" terminates too.
func (c *Conn) RawUntilDot() []byte {
	if c.tb != nil {
		c.tb.Helper()
	}
	b, err := c.tryRawUntilDot()
	if err != nil {
		c.fatalf("rawUntilDot: %v", err)
	}
	return b
}

// tryRawUntilDot is RawUntilDot without the tb: it returns the bytes read so far and the error.
func (c *Conn) tryRawUntilDot() ([]byte, error) {
	var buf bytes.Buffer
	for {
		c.bump()
		line, err := c.R.ReadBytes('\n')
		buf.Write(line)
		if err != nil {
			err = fmt.Errorf("after %d bytes: %w", buf.Len(), err)
			if c.Strict {
				return buf.Bytes(), &StrictError{Kind: NoTerminator, Line: string(line), Err: err}
			}
			return buf.Bytes(), err
		}
		if c.Strict {
			if serr := checkLine(line); serr != nil {
				return buf.Bytes(), serr
			}
			if bytes.Equal(line, []byte(".\r\n")) {
				return buf.Bytes(), nil
			}
			continue
		}
		if bytes.Equal(line, []byte(".\r\n")) || bytes.Equal(line, []byte(".\n")) {
			return buf.Bytes(), nil
		}
	}
}

// Close closes the connection; it is safe to call more than once and on a nil Conn.
func (c *Conn) Close() {
	if c == nil || c.Conn == nil {
		return
	}
	_ = c.Conn.Close()
}

// ParseCode extracts the 3-digit status code (100..599) at the start of line; the code must be
// followed by a space or the end of the line.
func ParseCode(line string) (int, error) {
	if len(line) < 3 {
		return 0, fmt.Errorf("short status line %q", line)
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil || code < 100 || code > 599 {
		return 0, fmt.Errorf("bad status line %q", line)
	}
	if len(line) > 3 && line[3] != ' ' {
		return 0, fmt.Errorf("bad status line %q", line)
	}
	return code, nil
}

// ExpectCode returns an error unless code is one of want; line is quoted in the error.
func ExpectCode(code int, line string, want ...int) error {
	for _, w := range want {
		if code == w {
			return nil
		}
	}
	return fmt.Errorf("got %d (%q), want %v", code, line, want)
}

// ExpectLines returns an error describing the first difference between got and want.
func ExpectLines(got, want []string) error {
	if len(got) != len(want) {
		return fmt.Errorf("got %d lines, want %d: got=%q want=%q", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
	return nil
}

// Unstuff removes the dot-stuffing of raw wire lines (".." prefix → "."); it returns a new slice.
func Unstuff(wire []string) []string {
	out := make([]string, len(wire))
	for i, l := range wire {
		if strings.HasPrefix(l, "..") {
			l = l[1:]
		}
		out[i] = l
	}
	return out
}
