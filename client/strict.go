package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// Kind classifies a wire mistake found in strict mode.
type Kind int

// The kinds of *StrictError.
const (
	BareLF         Kind = iota + 1 // a line ends in LF without a preceding CR
	BareCR                         // a CR inside a line that is not part of the CRLF line end
	NoCRLF                         // EOF or deadline after partial data, without any line end
	BadDotStuffing                 // a dot-block line starts with "." but is neither ".." nor the terminator
	NoTerminator                   // a dot block ends (EOF or deadline) without ".\r\n"
	Pending                        // bytes arrived where the server should have stayed quiet
)

// String returns the name of the kind, as in the constant.
func (k Kind) String() string {
	switch k {
	case BareLF:
		return "BareLF"
	case BareCR:
		return "BareCR"
	case NoCRLF:
		return "NoCRLF"
	case BadDotStuffing:
		return "BadDotStuffing"
	case NoTerminator:
		return "NoTerminator"
	case Pending:
		return "Pending"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// StrictError is a wire mistake of the server seen in strict mode (or by ExpectNoPending).
type StrictError struct {
	Kind  Kind
	Line  string // the offending line, raw (line end included when there was one)
	Bytes []byte // the unexpected bytes (Pending)
	Err   error  // the underlying read error (NoCRLF, NoTerminator), or nil
}

// Error describes the mistake, quotes the offending bytes and appends the read error, if any.
func (e *StrictError) Error() string {
	var s string
	switch e.Kind {
	case BareLF:
		s = fmt.Sprintf("strict: line ends in a bare LF (no CR before it): %q", e.Line)
	case BareCR:
		s = fmt.Sprintf("strict: bare CR inside the line: %q", e.Line)
	case NoCRLF:
		s = fmt.Sprintf("strict: data without a CRLF line end before the stream stopped: %q", e.Line)
	case BadDotStuffing:
		s = fmt.Sprintf("strict: dot-block line starts with \".\" but is not dot-stuffed: %q", e.Line)
	case NoTerminator:
		s = "strict: dot block ended without the \".\\r\\n\" terminator"
		if e.Line != "" {
			s += fmt.Sprintf(" (partial line %q)", e.Line)
		}
	case Pending:
		s = fmt.Sprintf("strict: %d unexpected pending bytes: %q", len(e.Bytes), e.Bytes)
	default:
		s = fmt.Sprintf("strict: %v: %q", e.Kind, e.Line)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the underlying read error, so errors.Is(err, io.EOF) and timeout checks work.
func (e *StrictError) Unwrap() error { return e.Err }

// readStrictLine reads one raw line and checks its line end: the strict form of ReadLine. On a
// StrictError the raw line is returned with it.
func (c *Conn) readStrictLine() (string, error) {
	c.bump()
	raw, err := c.R.ReadBytes('\n')
	if err != nil {
		if len(raw) > 0 {
			return string(raw), &StrictError{Kind: NoCRLF, Line: string(raw), Err: err}
		}
		return "", err
	}
	if serr := checkLine(raw); serr != nil {
		return string(raw), serr
	}
	return string(raw[:len(raw)-2]), nil
}

// checkLine checks one raw line that ends in "\n": it must end in CRLF and hold no other CR.
func checkLine(raw []byte) *StrictError {
	if len(raw) < 2 || raw[len(raw)-2] != '\r' {
		return &StrictError{Kind: BareLF, Line: string(raw)}
	}
	if bytes.IndexByte(raw[:len(raw)-2], '\r') >= 0 {
		return &StrictError{Kind: BareCR, Line: string(raw)}
	}
	return nil
}

// ExpectNoPending reads for up to d and returns a *StrictError of Kind Pending holding the
// bytes when any arrive (or are already buffered in R), for example a NUL after
// "400 too many users\r\n". A timeout gives nil, and so does EOF (a closed connection is not
// pending data); any other read error is returned. It works in both modes and returns as soon
// as the first bytes arrive.
func (c *Conn) ExpectNoPending(d time.Duration) error {
	if n := c.R.Buffered(); n > 0 {
		b, _ := c.R.Peek(n)
		return &StrictError{Kind: Pending, Bytes: append([]byte(nil), b...)}
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 4096)
	n, err := c.R.Read(buf)
	if n > 0 {
		return &StrictError{Kind: Pending, Bytes: buf[:n]}
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil
	}
	return err
}
