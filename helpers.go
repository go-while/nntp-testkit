package testkit

import (
	"errors"
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
