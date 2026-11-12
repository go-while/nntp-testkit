package testkit

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// waitPoll is how often WaitFor evaluates its condition.
const waitPoll = 50 * time.Millisecond

// WaitFor polls cond every 50 ms until it returns true; when d has passed without that it
// fails tb with tb.Fatalf, naming what ("timed out after 10s waiting for <what>"). d <= 0 is
// Deadline(). cond runs at once and on the test's goroutine, so it may use tb, send commands
// on a client connection or read files; it runs once more after d before the failure. Use
// WaitForErr when cond has a reason to give, Poll where tb.Fatalf is wrong (t.Cleanup).
func WaitFor(tb testing.TB, d time.Duration, what string, cond func() bool) {
	tb.Helper()
	if !Poll(d, cond) {
		tb.Fatalf("timed out after %v waiting for %s", pollLimit(d), what)
	}
}

// WaitForErr is WaitFor for a condition that says why it does not hold yet: it polls check
// every 50 ms until it returns nil; when d has passed without that it fails tb with the last
// error ("timed out after 10s waiting for <what>: <last error>"). d <= 0 is Deadline().
func WaitForErr(tb testing.TB, d time.Duration, what string, check func() error) {
	tb.Helper()
	if err := PollErr(d, check); err != nil {
		tb.Fatalf("timed out after %v waiting for %s: %v", pollLimit(d), what, errors.Unwrap(err))
	}
}

// Poll is WaitFor without a tb: it evaluates cond at once and then every 50 ms, and reports
// whether cond returned true within d (d <= 0 is Deadline()); it runs cond once more after d
// before it gives up. Nothing fails, so it works where t.Fatal must not be called, such as a
// t.Cleanup function (which may not stop the test but can report with t.Errorf), and in
// goroutines other than the test's.
func Poll(d time.Duration, cond func() bool) bool {
	return PollErr(d, func() error {
		if cond() {
			return nil
		}
		return errNotYet
	}) == nil
}

// PollErr is Poll for a condition that gives a reason: it evaluates check at once and every
// 50 ms until it returns nil (then PollErr returns nil), or until d has passed (d <= 0 is
// Deadline()) and check, run once more, still fails. The error then is "timed out after <d>"
// wrapping the last error of check, so errors.Is and errors.As see it.
func PollErr(d time.Duration, check func() error) error {
	d = pollLimit(d)
	limit := time.Now().Add(d)
	for {
		err := check()
		if err == nil {
			return nil
		}
		if time.Now().After(limit) {
			return &pollTimeout{d: d, err: err}
		}
		time.Sleep(waitPoll)
	}
}

// errNotYet is the error Poll gives PollErr for a false condition.
var errNotYet = errors.New("condition not met")

// pollLimit is d, or Deadline() when d <= 0.
func pollLimit(d time.Duration) time.Duration {
	if d <= 0 {
		return Deadline()
	}
	return d
}

// pollTimeout is the error of PollErr: the last error of the check, and how long it was tried.
type pollTimeout struct {
	d   time.Duration
	err error
}

func (e *pollTimeout) Error() string { return fmt.Sprintf("timed out after %v: %v", e.d, e.err) }
func (e *pollTimeout) Unwrap() error { return e.err }

// FreePort returns a TCP port of 127.0.0.1 that was free a moment ago: it listens on port 0
// and closes the listener. Another process may take the port before the test uses it, so
// prefer handing a server a listener (or ":0" and reading its address back) when it can take
// one; FreePort is for a server that must be told its port in advance. A port that something
// else already uses on 0.0.0.0 or [::] is skipped (see wildcardFree). A listen error fails tb.
func FreePort(tb testing.TB) int {
	tb.Helper()
	for range freePortTries {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			tb.Fatalf("free port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		if wildcardFree(port) {
			return port
		}
	}
	tb.Fatalf("free port: %d ports of 127.0.0.1 were all in use on 0.0.0.0 or [::]", freePortTries)
	return 0
}

// freePortTries is how many ports FreePort tries before it gives up.
const freePortTries = 100

// wildcardFree reports whether port is free on 0.0.0.0 and on [::]. On illumos, macOS and
// the BSDs a 127.0.0.1 listener can share a port with another program's wildcard listener
// (Go sets SO_REUSEADDR), and the test's first dial can then reach that program instead of
// the test server: on illumos, inetd's RPC listeners took a server's port this way. Only
// "address in use" counts as taken; a host without IPv6 passes.
func wildcardFree(port int) bool {
	for _, a := range [...]struct{ network, host string }{{"tcp4", "0.0.0.0"}, {"tcp6", "::"}} {
		l, err := net.Listen(a.network, net.JoinHostPort(a.host, strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			return false
		}
		if err == nil {
			_ = l.Close()
		}
	}
	return true
}

// listenProbe is the longest Listening waits for its dial.
const listenProbe = time.Second

// Listening reports whether a TCP dial to addr ("host:port") succeeds within a second (or
// Deadline() when that is shorter); the connection is closed at once, without reading or
// writing, so the server sees a client that goes away before its first command. Use it to
// check that a server is up, or with WaitFor that it has stopped: a refused dial returns false
// at once.
func Listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, min(listenProbe, Deadline()))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
