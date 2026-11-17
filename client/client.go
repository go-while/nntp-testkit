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
	"github.com/go-while/nntp-testkit/internal/dotstuff"
)

// readerSize is the buffer of Conn.R: large enough for any status line and most article lines.
const readerSize = 1 << 20

// DefaultMaxLine is the cap of one line a Conn reads (the line end included) when Conn.MaxLine
// is 0: 64 KiB. DefaultMaxBlock is the cap of one dot block (un-stuffed lines and 2 bytes per
// line end) when Conn.MaxBlock is 0: 64 MiB, and of a TryReadUntilQuiet with limit <= 0. They only
// stop a server that never ends a line, a block or a raw stream from growing the test process
// without bound; they are not RFC limits (RFC 3977 3.1: 512 bytes for a status line; RFC 5322
// 2.1.1: 998 characters per article line; OVER and HDR
// rows have no fixed limit).
const (
	DefaultMaxLine  = 64 << 10
	DefaultMaxBlock = 64 << 20
)

// blockFactor is how many hang guards a dot block may last in all when Conn.BlockTime is 0.
const blockFactor = 10

// ErrLineTooLong is returned (wrapped: use errors.Is) by ReadLine, ReadStatus, TryCmd,
// TryReadDotLines, TryRawUntilDot, TryPipeline and the other readers when a line is longer
// than Conn.MaxLine. It is raised while the line is read, as soon as it is over the cap, so a
// server that sends an endless line cannot grow the process; the rest of that line stays
// unread, so the Conn is no longer in step with the server: close it.
var ErrLineTooLong = errors.New("line too long")

// ErrBlockTooLong is the error inside the *StrictError (Kind NoTerminator) that a dot block
// reader returns when a block goes over Conn.MaxBlock bytes or lasts longer than Conn.BlockTime
// (the "." terminator never came); errors.Is finds it. TryReadUntilQuiet with limit <= 0 wraps it
// directly (no *StrictError) when the server sends more than MaxBlock bytes.
var ErrBlockTooLong = errors.New("dot block too long")

// Conn is a raw NNTP test client with a hang guard per blocking step. The fields are exported
// so a test can reach the socket or the reader directly; mixing direct reads with the methods
// is fine as long as both go through R.
type Conn struct {
	Conn     net.Conn
	R        *bufio.Reader
	Greeting string        // the greeting line read by Dial/ReadGreeting (CRLF stripped)
	Deadline time.Duration // hang guard per blocking step; 0 = testkit.Deadline()
	Strict   bool          // report wire mistakes (see the package doc); Dial sets it

	// MaxLine caps one line read by ReadLine, the dot-block readers and Pipeline, the line end
	// included: 0 = DefaultMaxLine (64 KiB), negative = no cap. A longer line fails the read
	// with ErrLineTooLong as soon as it is over the cap.
	MaxLine int
	// MaxBlock caps a dot block read by TryReadDotLines, TryRawUntilDot and TryPipeline, and a
	// TryReadUntilQuiet with limit <= 0, in bytes: 0 = DefaultMaxBlock (64 MiB), negative = no cap.
	// Over it a block read fails with a *StrictError (NoTerminator) wrapping ErrBlockTooLong,
	// TryReadUntilQuiet with an error wrapping ErrBlockTooLong.
	MaxBlock int
	// BlockTime is the longest a whole dot block may take, however often lines arrive (the
	// hang guard Deadline only limits the wait for the next line): 0 = 10 times the hang guard,
	// negative = no limit. Over it the read fails as for MaxBlock; the read of the line under
	// way is cut at that moment too.
	BlockTime time.Duration

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

// For returns a view of c whose tb-failing methods (Cmd, ReadDotLines, RawUntilDot) fail tb
// instead of the tb c was dialed with, sharing the socket, the reader and the settings; no
// cleanup is registered, the view is as short-lived as its caller. Use it when one connection
// dialed in a parent test is used inside t.Run subtests: a failure then fails the subtest, not
// the parent. Changes to the exported fields of the view are not seen by c.
func (c *Conn) For(tb testing.TB) *Conn {
	v := *c
	v.tb = tb
	return &v
}

// TryRawUntilDot is RawUntilDot that returns the error instead of failing tb.
func (c *Conn) TryRawUntilDot() ([]byte, error) { return c.tryRawUntilDot() }

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
// not read a greeting (call ReadGreeting; a connection whose greeting was read already is used
// as it is) and registers tb.Cleanup(Close). Strict mode is on. NewConn is the form without tb.
func New(tb testing.TB, nc net.Conn) *Conn {
	tb.Helper()
	c := &Conn{Conn: nc, R: bufio.NewReaderSize(nc, readerSize), Strict: true, tb: tb}
	tb.Cleanup(c.Close)
	return c
}

// NewConn wraps an open connection without a testing dependency and without reading anything:
// for a connection whose greeting (and more) the caller has read already, or that has none,
// such as a TLS client after the handshake, one end of net.Pipe or a conn from a custom dialer.
// The Conn is strict, has the default hang guard and an empty Greeting (set c.Greeting
// yourself if the caller read it); the caller closes it, nothing is registered. Bytes the
// caller buffered while reading before (a bufio.Reader of its own) are not carried over: read
// the rest through the Conn from then on.
//
// Like TryDial's it has no tb: Cmd, ReadDotLines, RawUntilDot and the other plain forms panic
// with a "client: ..." message on a failure, so use the Try* forms, or call WithTB (which also
// registers the Close) or For to give it one.
func NewConn(nc net.Conn) *Conn {
	return &Conn{Conn: nc, R: bufio.NewReaderSize(nc, readerSize), Strict: true}
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
// read error. A line longer than Conn.MaxLine (the line end counts) fails with ErrLineTooLong,
// the part read so far returned with it.
func (c *Conn) ReadLine() (string, error) {
	return c.readLine(time.Time{}, c.maxLine())
}

// readLine is ReadLine whose read deadline is also clamped to until (zero: no clamp) and whose
// line cap is limit (<= 0: none).
func (c *Conn) readLine(until time.Time, limit int) (string, error) {
	if c.Strict {
		return c.readStrictLine(until, limit)
	}
	line, err := c.readLineBytes(until, limit)
	if err != nil {
		return string(line), err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// maxLine is the effective line cap; 0 means none.
func (c *Conn) maxLine() int { return capOf(c.MaxLine, DefaultMaxLine) }

// maxBlock is the effective block cap in bytes; 0 means none.
func (c *Conn) maxBlock() int { return capOf(c.MaxBlock, DefaultMaxBlock) }

// lineLimit is the cap of the next line of a dot block that holds size bytes so far: MaxLine,
// or what is left of MaxBlock (plus the "." line) when that is less, so a line without an LF
// cannot outgrow the block cap either, also with MaxLine < 0. fromBlock: going over it is the
// block's error (ErrBlockTooLong), not the line's.
func (c *Conn) lineLimit(size, maxBlock int) (limit int, fromBlock bool) {
	limit = c.maxLine()
	if maxBlock > 0 {
		if left := maxBlock - size + len(".\r\n"); limit <= 0 || left < limit {
			return left, true
		}
	}
	return limit, false
}

func capOf(v, def int) int {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	}
	return v
}

// blockUntil is the absolute end of a dot block that starts now; the zero time: no limit.
func (c *Conn) blockUntil() time.Time {
	switch {
	case c.BlockTime < 0:
		return time.Time{}
	case c.BlockTime > 0:
		return time.Now().Add(c.BlockTime)
	}
	return time.Now().Add(blockFactor * c.deadline())
}

// blockBound is the time a block may take, for messages.
func (c *Conn) blockBound() time.Duration {
	if c.BlockTime > 0 {
		return c.BlockTime
	}
	return blockFactor * c.deadline()
}

// blockErr is the error of a block that went over MaxBlock or BlockTime.
func (c *Conn) blockErr(lines, size int) error {
	return &StrictError{Kind: NoTerminator, Err: fmt.Errorf(
		"%w: still going after %d lines, %d bytes (bounds: %d bytes, %v)",
		ErrBlockTooLong, lines, size, c.maxBlock(), c.blockBound())}
}

// readLineBytes reads one line with its line end, giving up as soon as it is longer than limit
// (<= 0: no cap): it never holds more than the cap. It moves the hang guard on first (with until set,
// the read deadline never past it; the write deadline slides as always). On a read error the
// partial line comes back with it.
func (c *Conn) readLineBytes(until time.Time, limit int) ([]byte, error) {
	if until.IsZero() {
		c.bump()
	} else {
		now := time.Now()
		_ = c.Conn.SetReadDeadline(minTime(now.Add(c.deadline()), until))
		// the write side keeps its sliding hang guard: TryPipeline's writer may still be
		// sending while a block is read
		_ = c.Conn.SetWriteDeadline(now.Add(c.deadline()))
	}
	var line []byte
	for {
		if c.R.Buffered() == 0 {
			if _, err := c.R.Peek(1); err != nil { // waits for data without consuming
				return line, err
			}
		}
		b, _ := c.R.Peek(c.R.Buffered())
		n, found := len(b), false
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			n, found = i+1, true
		}
		// over: longer than limit with its LF, or limit bytes without an LF yet
		if limit > 0 && (len(line)+n > limit || (!found && len(line)+n == limit)) {
			take := min(n, limit-len(line))
			line = append(line, b[:take]...)
			_, _ = c.R.Discard(take)
			return line, fmt.Errorf("%w: longer than %d bytes with its line end", ErrLineTooLong, limit)
		}
		line = append(line, b[:n]...)
		_, _ = c.R.Discard(n)
		if found {
			return line, nil
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
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
// before ".\r\n" is a NoTerminator error wrapping the read error. The block is bounded: a line
// over Conn.MaxLine is ErrLineTooLong, a block over Conn.MaxBlock bytes or Conn.BlockTime is a
// NoTerminator error wrapping ErrBlockTooLong (so a server that never ends the block cannot
// keep the test alive by sending lines).
func (c *Conn) TryReadDotLines() ([]string, error) { return c.readBlockBounded() }

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
// It is bounded like TryReadDotLines (Conn.MaxLine, MaxBlock, BlockTime).
func (c *Conn) tryRawUntilDot() ([]byte, error) {
	var buf bytes.Buffer
	until, maxBlock := c.blockUntil(), c.maxBlock()
	for {
		if (!until.IsZero() && time.Now().After(until)) || (maxBlock > 0 && buf.Len() > maxBlock) {
			return buf.Bytes(), c.blockErr(bytes.Count(buf.Bytes(), []byte("\n")), buf.Len())
		}
		limit, fromBlock := c.lineLimit(buf.Len(), maxBlock)
		line, err := c.readLineBytes(until, limit)
		buf.Write(line)
		if err != nil {
			if errors.Is(err, ErrLineTooLong) {
				if fromBlock {
					return buf.Bytes(), c.blockErr(bytes.Count(buf.Bytes(), []byte("\n")), buf.Len())
				}
				return buf.Bytes(), err
			}
			if timedOut(err) && !until.IsZero() && !time.Now().Before(until) {
				return buf.Bytes(), c.blockErr(bytes.Count(buf.Bytes(), []byte("\n")), buf.Len())
			}
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

// Unstuff removes the dot-stuffing of raw wire lines as RFC 3977 section 3.1.1 has the receiver
// do it: the first "." of every line that starts with one goes ("..x" becomes ".x", ".x"
// becomes "x"). The terminator is not part of the lines; a lone "." would become "". It
// returns a new slice.
func Unstuff(wire []string) []string {
	out := make([]string, len(wire))
	for i, l := range wire {
		l = strings.TrimPrefix(l, ".")
		out[i] = l
	}
	return out
}

// Stuff returns lines as a multi-line block on the wire, as a client sends an article after
// POST, IHAVE or TAKETHIS: each line dot-stuffed (a leading "." doubled), followed by CRLF, and
// the ".\r\n" terminator; no lines give ".\r\n" alone. The lines must not hold line ends.
// Unstuff undoes the stuffing of the lines (without the terminator); Send writes the block.
func Stuff(lines []string) string {
	var b strings.Builder
	for _, l := range dotstuff.Append(make([]string, 0, len(lines)), lines) {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	return b.String()
}
