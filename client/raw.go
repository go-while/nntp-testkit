package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// timedOut reports a deadline error.
func timedOut(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// TryReadUntilQuiet reads raw bytes (the bytes buffered in R first) until the server has been
// silent for quiet or limit bytes have been read (limit <= 0: no limit), and returns them with
// a nil error. A closed connection gives the bytes read so far and io.EOF itself, so
// `err == io.EOF` tells "the server closed or crashed" from "the server went quiet". A read
// error other than a timeout is returned as it is. Whatever the server sends, the call ends
// after one hang guard (the Conn's Deadline) with an error. Nothing is interpreted: no strict
// checks, no line ends.
func (c *Conn) TryReadUntilQuiet(quiet time.Duration, limit int) ([]byte, error) {
	var out []byte
	start := time.Now()
	defer c.bump()
	chunk := make([]byte, 32<<10)
	for limit <= 0 || len(out) < limit {
		if time.Since(start) > c.deadline() {
			return out, fmt.Errorf("server still sending after %v (%d bytes read)", c.deadline(), len(out))
		}
		p := chunk
		if limit > 0 && limit-len(out) < len(p) {
			p = p[:limit-len(out)]
		}
		// each read waits for quiet, or for what is left of the hang guard when that is less
		guard := false
		if c.R.Buffered() == 0 {
			wait := quiet
			if left := c.deadline() - time.Since(start); left < wait {
				wait, guard = max(left, 0), true
			}
			_ = c.Conn.SetReadDeadline(time.Now().Add(wait))
		}
		n, err := c.R.Read(p)
		out = append(out, p[:n]...)
		if err != nil {
			if timedOut(err) {
				if guard {
					return out, fmt.Errorf("server still sending after %v (%d bytes read): %w", c.deadline(), len(out), err)
				}
				return out, nil
			}
			return out, err
		}
	}
	return out, nil
}

// ReadUntilQuiet is TryReadUntilQuiet that fails tb on an error other than EOF; eof tells that
// the server closed the connection.
func (c *Conn) ReadUntilQuiet(quiet time.Duration, limit int) (data []byte, eof bool) {
	if c.tb != nil {
		c.tb.Helper()
	}
	data, err := c.TryReadUntilQuiet(quiet, limit)
	if err == io.EOF {
		return data, true
	}
	if err != nil {
		c.fatalf("read until quiet: %v (%d bytes read: %q)", err, len(data), data)
	}
	return data, false
}

// closedMaxLines and closedMaxBytes bound what ExpectClosed accepts before the close.
const (
	closedMaxLines = 16
	closedMaxBytes = 64 << 10
)

// ExpectClosed waits up to d (0 = the Conn's hang guard) for the peer to close the connection
// and returns the lines it sent before that, CRLF stripped (QUIT's 205, "400 shutting down",
// "503 timeout"; a last partial line is included). An error is returned when the peer is
// still connected after d, sends more than 16 lines or 64 KiB (a line without LF counts its bytes: at most 64 KiB are held), or the read fails other than
// by a close: EOF and a connection reset (a peer that closes with our data unread) both count
// as closed. The lines read are returned with the error too. Nothing is checked about the
// bytes (no strict mode: a crashing peer may send anything).
func (c *Conn) ExpectClosed(d time.Duration) ([]string, error) {
	if d <= 0 {
		d = c.deadline()
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(d))
	defer c.bump()
	var lines []string
	var cur []byte // the line being read; at most closedMaxBytes+1 bytes are ever held
	total := 0
	for {
		_, err := c.R.Peek(1) // waits for data without consuming
		if err != nil {
			if len(cur) > 0 {
				lines = append(lines, strings.TrimRight(string(cur), "\r\n"))
			}
			if err == io.EOF || errors.Is(err, syscall.ECONNRESET) {
				return lines, nil
			}
			if timedOut(err) {
				return lines, fmt.Errorf("peer did not close within %v (lines before: %q)", d, lines)
			}
			return lines, err
		}
		b, _ := c.R.Peek(c.R.Buffered())
		n := len(b)
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			n = i + 1
		}
		n = min(n, closedMaxBytes+1-total)
		cur = append(cur, b[:n]...)
		_, _ = c.R.Discard(n)
		total += n
		if len(cur) > 0 && cur[len(cur)-1] == '\n' {
			lines = append(lines, strings.TrimRight(string(cur), "\r\n"))
			cur = cur[:0]
		}
		if len(lines) > closedMaxLines || total > closedMaxBytes {
			return lines, fmt.Errorf("peer sent %d lines (%d bytes) and has not closed: %q", len(lines), total, lines)
		}
	}
}

// MustClosed is ExpectClosed that fails tb on an error; it returns the lines sent before the
// close.
func (c *Conn) MustClosed(d time.Duration) []string {
	if c.tb != nil {
		c.tb.Helper()
	}
	lines, err := c.ExpectClosed(d)
	if err != nil {
		c.fatalf("expect closed: %v", err)
	}
	return lines
}

// SendChunks writes raw in pieces of n bytes (n < 1 is 1), one socket write each (Go sets
// TCP_NODELAY, so each piece is its own segment unless the kernel merges a backlog), pausing
// for pause between two writes, so a test can split a CRLF, a command or the ".\r\n"
// terminator across writes. Every write is bounded by the Conn's hang guard; the pauses are
// the caller's to size (a few milliseconds to let the server read each piece).
func (c *Conn) SendChunks(raw []byte, n int, pause time.Duration) error {
	n = max(n, 1)
	for len(raw) > 0 {
		k := min(n, len(raw))
		if err := c.SendBytes(raw[:k]); err != nil {
			return err
		}
		raw = raw[k:]
		if len(raw) > 0 && pause > 0 {
			time.Sleep(pause)
		}
	}
	return nil
}

// SendDrip is SendChunks with one byte per write.
func (c *Conn) SendDrip(raw []byte, pause time.Duration) error {
	return c.SendChunks(raw, 1, pause)
}
