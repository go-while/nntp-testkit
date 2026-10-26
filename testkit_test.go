package testkit

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeadlineFromEnv(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultDeadline},
		{"junk", DefaultDeadline},
		{"-1s", DefaultDeadline},
		{"0", DefaultDeadline},
		{"120s", 120 * time.Second},
		{"1m30s", 90 * time.Second},
	} {
		if got := deadlineFromEnv(tc.in, DefaultDeadline); got != tc.want {
			t.Errorf("deadlineFromEnv(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	t.Setenv(EnvDeadline, "7s")
	if got := Deadline(); got != 7*time.Second {
		t.Errorf("Deadline() with %s=7s = %v", EnvDeadline, got)
	}
}

func TestEventually(t *testing.T) {
	n := 0
	Eventually(t, 5*time.Second, "the third poll", func() bool { n++; return n >= 3 })
	if n != 3 {
		t.Errorf("cond ran %d times, want 3", n)
	}

	ft := &fatalTB{TB: t}
	start := time.Now()
	Eventually(ft, 120*time.Millisecond, "never", func() bool { return false })
	if !strings.Contains(ft.msg, "timed out after 120ms waiting for never") {
		t.Errorf("failure message %q", ft.msg)
	}
	if el := time.Since(start); el < 120*time.Millisecond || el > 5*time.Second {
		t.Errorf("gave up after %v, want just over 120ms", el)
	}
}

func TestFreePort(t *testing.T) {
	p := FreePort(t)
	if p <= 0 || p > 65535 {
		t.Fatalf("FreePort() = %d", p)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatalf("listen on the free port %d: %v", p, err)
	}
	_ = l.Close()
}

// fatalTB records Fatalf instead of stopping the test.
type fatalTB struct {
	testing.TB
	msg string
}

func (f *fatalTB) Helper() {}
func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
}
