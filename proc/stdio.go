package proc

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// StartStdio starts the process with its stdin and stdout connected to pipes (inetd style);
// Conn returns the client side. Reads and writes on Conn while no deadline is set are bounded
// by Options.Deadline (the hang guard).
func StartStdio(tb testing.TB, o Options) *Process {
	tb.Helper()
	p := newProcess(tb, o)
	inR, inW, err := os.Pipe()
	if err != nil {
		tb.Fatalf("proc: stdin pipe: %v", err)
		return p
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		tb.Fatalf("proc: stdout pipe: %v", err)
		return p
	}
	p.cmd.Stdin = inR
	p.cmd.Stdout = outW
	p.conn = &pipeConn{r: outR, w: inW, guard: p.deadline}
	err = p.start(tb)
	// The child holds its own copies of these ends now (or never started).
	_ = inR.Close()
	_ = outW.Close()
	if err != nil {
		_ = p.conn.Close()
		tb.Fatalf("proc: start %s: %v", o.Path, err)
		return p
	}
	return p
}

// pipeConn is a net.Conn over the child's stdout (Read) and stdin (Write). os.Pipe files are
// pollable, so deadlines map onto the files directly without extra goroutines.
type pipeConn struct {
	r, w  *os.File
	guard time.Duration // per-operation bound while no deadline is set

	mu     sync.Mutex // orders deadline changes with the per-operation guard
	rd, wd time.Time  // deadlines set by the user; zero = guard

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (c *pipeConn) effective(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().Add(c.guard)
	}
	return t
}

// Read reads from the child's stdout.
func (c *pipeConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	err := c.r.SetReadDeadline(c.effective(c.rd))
	c.mu.Unlock()
	if err != nil {
		return 0, c.opError("read", err)
	}
	n, err := c.r.Read(b)
	return n, c.opError("read", err)
}

// Write writes to the child's stdin.
func (c *pipeConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	err := c.w.SetWriteDeadline(c.effective(c.wd))
	c.mu.Unlock()
	if err != nil {
		return 0, c.opError("write", err)
	}
	n, err := c.w.Write(b)
	return n, c.opError("write", err)
}

// Close closes both pipe ends; the child sees EOF on stdin.
func (c *pipeConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.closeErr = errors.Join(c.w.Close(), c.r.Close())
	})
	return c.closeErr
}

// LocalAddr returns the pipeAddr of the client side.
func (c *pipeConn) LocalAddr() net.Addr { return pipeAddr("client") }

// RemoteAddr returns the pipeAddr of the process side.
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr("stdio") }

// SetDeadline sets the read and write deadlines; the zero time restores the hang guard.
func (c *pipeConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

// SetReadDeadline sets the read deadline, also for a Read in progress; the zero time
// restores the hang guard.
func (c *pipeConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rd = t
	return c.opError("set read deadline", c.r.SetReadDeadline(c.effective(t)))
}

// SetWriteDeadline sets the write deadline, also for a Write in progress; the zero time
// restores the hang guard.
func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wd = t
	return c.opError("set write deadline", c.w.SetWriteDeadline(c.effective(t)))
}

// opError wraps file errors in *net.OpError so callers can use net.Error (Timeout) as with a
// socket. io.EOF stays unwrapped; any error after Close maps to net.ErrClosed (os reports a
// closed file differently for deadlines and I/O).
func (c *pipeConn) opError(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	if c.closed.Load() || errors.Is(err, os.ErrClosed) {
		err = net.ErrClosed
	}
	return &net.OpError{Op: op, Net: "pipe", Addr: pipeAddr("stdio"), Err: err}
}

// pipeAddr is the net.Addr of the pipe ends; Network is "pipe".
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }
