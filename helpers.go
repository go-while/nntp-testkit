package testkit

import (
	"net"
	"testing"
	"time"
)

// waitPoll is how often WaitFor evaluates its condition.
const waitPoll = 50 * time.Millisecond

// WaitFor polls cond every 50 ms until it returns true; when d has passed without that it
// fails tb with tb.Fatalf, naming what ("timed out after 10s waiting for <what>"). d <= 0 is
// Deadline(). cond runs at once and on the test's goroutine, so it may use tb, send commands
// on a client connection or read files; it runs once more after d before the failure.
func WaitFor(tb testing.TB, d time.Duration, what string, cond func() bool) {
	tb.Helper()
	if d <= 0 {
		d = Deadline()
	}
	limit := time.Now().Add(d)
	for !cond() {
		if time.Now().After(limit) {
			tb.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(waitPoll)
	}
}

// FreePort returns a TCP port of 127.0.0.1 that was free a moment ago: it listens on port 0
// and closes the listener. Another process may take the port before the test uses it, so
// prefer handing a server a listener (or ":0" and reading its address back) when it can take
// one; FreePort is for a server that must be told its port in advance. A listen error fails tb.
func FreePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
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
