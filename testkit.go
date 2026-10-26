// Package testkit is the root of nntp-testkit: shared NNTP test tools (a scripted or stateful
// fake server, a strict test client, an external-process runner) for projects that implement or
// consume NNTP. Only the standard library is used.
//
// The sub-packages are client, fakeserver, proc, conformance, yenc, canlock and mailrec. This
// package holds what they share: the hang guard, plus two small helpers that most suites
// otherwise write themselves (Eventually, FreePort).
package testkit

import (
	"net"
	"os"
	"testing"
	"time"
)

// DefaultDeadline is the hang guard when NNTP_TEST_DEADLINE is not set.
const DefaultDeadline = 60 * time.Second

// EnvDeadline is the environment variable that overrides the hang guard: a Go duration such as
// "120s".
const EnvDeadline = "NNTP_TEST_DEADLINE"

// Deadline returns the hang guard of the kit: the outer bound of every blocking step (dial,
// read, write, waits). It only stops a hung test; a passing test never waits on it. It is
// NNTP_TEST_DEADLINE when that is a positive duration, else DefaultDeadline. Each package also
// accepts an explicit deadline per server or connection, which wins over this default.
func Deadline() time.Duration {
	return deadlineFromEnv(os.Getenv(EnvDeadline), DefaultDeadline)
}

// deadlineFromEnv parses v; an empty, invalid or non-positive value gives def.
func deadlineFromEnv(v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// PollInterval is how often Eventually checks its condition.
const PollInterval = 50 * time.Millisecond

// Eventually polls cond every PollInterval until it returns true, and fails tb with "timed out
// after <d> waiting for <what>" once d has passed. d 0 means Deadline(). Use it for state that a
// server under test reaches on its own time (a file written, an article visible in a group, a
// counter reached); cond runs on the test goroutine and must not call tb.Fatal.
func Eventually(tb testing.TB, d time.Duration, what string, cond func() bool) {
	tb.Helper()
	if d <= 0 {
		d = Deadline()
	}
	limit := time.Now().Add(d)
	for !cond() {
		if time.Now().After(limit) {
			tb.Fatalf("timed out after %v waiting for %s", d, what)
			return
		}
		time.Sleep(PollInterval)
	}
}

// FreePort returns a TCP port on 127.0.0.1 that was free a moment ago: for a server under test
// that must be told its port before it starts. Another process can take the port before the
// server binds it, so a server that can listen on port 0 and report its address is the better
// choice where it exists.
func FreePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("FreePort: %v", err)
		return 0
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		tb.Fatalf("FreePort: %v", err)
	}
	return port
}
