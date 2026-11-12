package testkit

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fatalTB records Fatalf instead of failing the test and ends the calling goroutine.
type fatalTB struct {
	testing.TB
	msg string
}

func (f *fatalTB) Helper() {}
func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// run calls fn with f on its own goroutine (Goexit must not end the test goroutine) and
// returns the Fatalf message, "" when fn did not fail.
func (f *fatalTB) run(fn func(tb testing.TB)) string {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(f)
	}()
	<-done
	return f.msg
}

func TestWaitFor(t *testing.T) {
	n := 0
	WaitFor(t, time.Second, "the third call", func() bool { n++; return n == 3 })
	if n != 3 {
		t.Errorf("cond called %d times, want 3", n)
	}
	n = 0
	WaitFor(t, 0, "at once", func() bool { n++; return true })
	if n != 1 {
		t.Errorf("a true cond called %d times, want 1", n)
	}

	start := time.Now()
	msg := (&fatalTB{TB: t}).run(func(tb testing.TB) {
		WaitFor(tb, 120*time.Millisecond, "never", func() bool { return false })
	})
	if !strings.Contains(msg, "timed out after 120ms waiting for never") {
		t.Errorf("Fatalf %q", msg)
	}
	if el := time.Since(start); el < 120*time.Millisecond {
		t.Errorf("failed after %v, before d", el)
	}

	// d <= 0 is Deadline().
	t.Setenv(EnvDeadline, "100ms")
	msg = (&fatalTB{TB: t}).run(func(tb testing.TB) {
		WaitFor(tb, 0, "the env deadline", func() bool { return false })
	})
	if !strings.Contains(msg, "timed out after 100ms") {
		t.Errorf("d=0 with %s=100ms: Fatalf %q", EnvDeadline, msg)
	}
}

func TestWaitForErr(t *testing.T) {
	n := 0
	WaitForErr(t, time.Second, "the third call", func() error {
		n++
		if n < 3 {
			return fmt.Errorf("call %d", n)
		}
		return nil
	})
	if n != 3 {
		t.Errorf("check called %d times, want 3", n)
	}

	// the last error is named
	n = 0
	msg := (&fatalTB{TB: t}).run(func(tb testing.TB) {
		WaitForErr(tb, 120*time.Millisecond, "never", func() error { n++; return fmt.Errorf("attempt %d", n) })
	})
	want := fmt.Sprintf("timed out after 120ms waiting for never: attempt %d", n)
	if msg != want || n < 2 {
		t.Errorf("Fatalf %q (%d calls), want %q", msg, n, want)
	}

	// d <= 0 is Deadline()
	t.Setenv(EnvDeadline, "100ms")
	msg = (&fatalTB{TB: t}).run(func(tb testing.TB) {
		WaitForErr(tb, 0, "the env deadline", func() error { return errors.New("still no") })
	})
	if msg != "timed out after 100ms waiting for the env deadline: still no" {
		t.Errorf("d=0: Fatalf %q", msg)
	}
}

func TestPoll(t *testing.T) {
	n := 0
	if !Poll(time.Second, func() bool { n++; return n == 3 }) || n != 3 {
		t.Errorf("Poll: n = %d, want true after 3 calls", n)
	}
	start := time.Now()
	if Poll(120*time.Millisecond, func() bool { return false }) {
		t.Error("Poll of a false cond returned true")
	}
	if el := time.Since(start); el < 120*time.Millisecond || el > 5*time.Second {
		t.Errorf("Poll gave up after %v, want about 120ms", el)
	}
	t.Setenv(EnvDeadline, "100ms")
	start = time.Now()
	if Poll(0, func() bool { return false }) || time.Since(start) < 100*time.Millisecond {
		t.Error("Poll(0) did not wait for the env deadline")
	}
	// usable inside t.Cleanup: fails nothing, reports with Errorf
	ok := false
	t.Run("cleanup", func(t *testing.T) {
		t.Cleanup(func() { ok = Poll(time.Second, func() bool { return true }) })
	})
	if !ok {
		t.Error("Poll in a Cleanup returned false")
	}
}

func TestPollErr(t *testing.T) {
	n := 0
	if err := PollErr(time.Second, func() error {
		n++
		if n < 3 {
			return errors.New("not yet")
		}
		return nil
	}); err != nil || n != 3 {
		t.Errorf("PollErr = %v after %d calls", err, n)
	}

	sentinel := errors.New("sentinel")
	err := PollErr(120*time.Millisecond, func() error { return fmt.Errorf("wrapped: %w", sentinel) })
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "timed out after 120ms: wrapped: sentinel") {
		t.Errorf("PollErr timeout error = %v", err)
	}
	var pe *pollTimeout
	if !errors.As(err, &pe) {
		t.Errorf("error %T is not a *pollTimeout", err)
	}
}

func TestFreePortAndListening(t *testing.T) {
	port := FreePort(t)
	if port <= 0 {
		t.Fatalf("FreePort = %d", port)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if Listening(addr) {
		t.Fatalf("Listening(%s) before anything listens", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on the free port: %v", err)
	}
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	if !Listening(addr) {
		t.Errorf("Listening(%s) with a listener", addr)
	}
	<-accepted
	_ = ln.Close()
	if Listening(addr) {
		t.Errorf("Listening(%s) after Close", addr)
	}
	if Listening("127.0.0.1:not-a-port") {
		t.Error("Listening on a bad address")
	}
}

// TestFreePortSkipsWildcard holds a port on a wildcard address the way another program's
// listener would and checks that FreePort's check treats it as taken, while 127.0.0.1 may
// still share it on illumos, macOS and the BSDs (logged, not required).
func TestFreePortSkipsWildcard(t *testing.T) {
	for _, a := range []struct{ network, host string }{{"tcp4", "0.0.0.0"}, {"tcp6", "::"}} {
		t.Run(a.network, func(t *testing.T) {
			w, err := net.Listen(a.network, net.JoinHostPort(a.host, "0"))
			if err != nil {
				t.Skipf("no %s wildcard listener here: %v", a.network, err)
			}
			defer func() { _ = w.Close() }()
			port := w.Addr().(*net.TCPAddr).Port
			if wildcardFree(port) {
				t.Errorf("wildcardFree(%d) with a %s listener on it", port, a.network)
			}
			if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
				t.Logf("%s: 127.0.0.1 shares port %d with the wildcard listener", runtime.GOOS, port)
				_ = l.Close()
			}
		})
	}
	if port := FreePort(t); !wildcardFree(port) {
		t.Errorf("FreePort returned %d, which is in use on a wildcard address", port)
	}
}
