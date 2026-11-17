package conformance

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// hostile serves a greeting and answers every command with reply(c, line); reply writes
// itself.
func hostile(t *testing.T, o fakeserver.Options, reply func(c *fakeserver.Conn, line string) bool) *fakeserver.Server {
	t.Helper()
	o.Handler = func(c *fakeserver.Conn) {
		for {
			line, err := c.ReadLine()
			if err != nil || !reply(c, line) {
				return
			}
		}
	}
	return fakeserver.New(t, o)
}

// onlyChecks returns Options.Skip that leaves just the named checks.
func onlyChecks(keep ...string) []string {
	var skip []string
outer:
	for _, n := range Names() {
		for _, k := range keep {
			if n == k {
				continue outer
			}
		}
		skip = append(skip, n)
	}
	return skip
}

// within fails the test when f takes longer than d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	if took := time.Since(start); took > d {
		t.Errorf("%s took %v, want under %v", what, took, d)
	}
}

// TestEndlessCapabilitiesBlock: a server that streams CAPABILITIES lines without the "."
// used to hold Check before the checks started (and every check after it, the block being
// unbounded in time and size): now the block is cut at MaxBlock, and the check fails.
func TestEndlessCapabilitiesBlock(t *testing.T) {
	s := hostile(t, fakeserver.Options{}, func(c *fakeserver.Conn, line string) bool {
		if !strings.HasPrefix(line, "CAPABILITIES") {
			return c.WriteLines("500 what?") == nil
		}
		chunk := strings.Repeat("VERSION 2\r\n", 500)
		if _, err := io.WriteString(c.Raw(), "101 caps\r\n"); err != nil {
			return false
		}
		for { // until the client closes
			if _, err := io.WriteString(c.Raw(), chunk); err != nil {
				return false
			}
		}
	})
	var r Report
	var err error
	within(t, 30*time.Second, "Check", func() {
		r, err = Check(Options{Addr: s.Addr(), Deadline: 5 * time.Second, MaxBlock: 50 << 10, Skip: onlyChecks("capabilities")})
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.ByName()["capabilities"]
	if res.Status != Fail || !strings.Contains(res.Detail, "dot block too long") {
		t.Errorf("capabilities: %v %q", res.Status, res.Detail)
	}
	if r.Capabilities != nil {
		t.Errorf("Report.Capabilities = %q", r.Capabilities)
	}
}

// TestTrickledCapabilitiesBlock: one short line every few milliseconds, forever, renews the
// hang guard of every line; the block as a whole ends after BlockWindow hang guards.
func TestTrickledCapabilitiesBlock(t *testing.T) {
	s := hostile(t, fakeserver.Options{}, func(c *fakeserver.Conn, line string) bool {
		if !strings.HasPrefix(line, "CAPABILITIES") {
			return c.WriteLines("500 what?") == nil
		}
		if c.WriteLines("101 caps") != nil {
			return false
		}
		for {
			if c.WriteLines("VERSION 2") != nil {
				return false
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	const deadline = 200 * time.Millisecond
	var r Report
	var err error
	within(t, 10*time.Second, "Check", func() {
		r, err = Check(Options{Addr: s.Addr(), Deadline: deadline, Skip: onlyChecks("capabilities")})
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.ByName()["capabilities"]
	if res.Status != Fail || !strings.Contains(res.Detail, "dot block too long") {
		t.Errorf("capabilities: %v %q", res.Status, res.Detail)
	}
}

// TestLongStatusLine: a status line over MaxLine is a Fail of the check that reads it, with a
// short Detail; raising MaxLine lets the same server pass that read.
func TestLongStatusLine(t *testing.T) {
	long := strings.Repeat("y", 100<<10)
	s := hostile(t, fakeserver.Options{}, func(c *fakeserver.Conn, line string) bool {
		if strings.HasPrefix(line, "CAPABILITIES") {
			return c.WriteLines("101 caps", "VERSION 2", "READER", ".") == nil
		}
		return c.WriteLines("200 "+long) == nil
	})
	r := mustCheck(t, Options{Addr: s.Addr(), Deadline: 5 * time.Second, Skip: onlyChecks("capabilities", "date")})
	res := r.ByName()["date"]
	if res.Status != Fail || !strings.Contains(res.Detail, "line too long") || len(res.Detail) > 2000 {
		t.Errorf("date: %v (%d bytes) %.300q", res.Status, len(res.Detail), res.Detail)
	}
	r = mustCheck(t, Options{Addr: s.Addr(), Deadline: 5 * time.Second, MaxLine: -1, Skip: onlyChecks("date")})
	if res := r.ByName()["date"]; strings.Contains(res.Detail, "line too long") {
		t.Errorf("with the cap lifted: %v %q", res.Status, res.Detail)
	}
}

// TestLongGreeting: a greeting over MaxLine is the greeting check's Fail, like a bare LF; the
// other checks are skipped and Check returns no error.
func TestLongGreeting(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Greeting: "200 " + strings.Repeat("g", 70<<10)})
	r := mustCheck(t, Options{Addr: s.Addr(), Deadline: 5 * time.Second})
	res := r.ByName()["greeting"]
	if res.Status != Fail || !strings.Contains(res.Detail, "line too long") || len(res.Detail) > 2000 {
		t.Errorf("greeting: %v (%d bytes) %.300q", res.Status, len(res.Detail), res.Detail)
	}
	if res := r.ByName()["capabilities"]; res.Status != Skip {
		t.Errorf("capabilities: %v %q", res.Status, res.Detail)
	}
}

// TestBlockTimeOption: Options.BlockTime sets how long one multi-line reply may take: 0 keeps
// BlockWindow hang guards, a positive value replaces it, a negative one lifts it.
func TestBlockTimeOption(t *testing.T) {
	for _, tc := range []struct {
		o    Options
		want time.Duration
	}{
		{Options{}, BlockWindow * time.Second},
		{Options{BlockTime: time.Minute}, time.Minute},
		{Options{BlockTime: -1}, -1},
		{Options{BlockTime: time.Millisecond}, time.Millisecond}, // shorter than the deadline
	} {
		if got := tc.o.blockTime(time.Second); got != tc.want {
			t.Errorf("BlockTime %v: %v, want %v", tc.o.BlockTime, got, tc.want)
		}
	}
}

// TestSlowCapabilitiesBlock: a valid CAPABILITIES block of 100 distinct lines, one every 10 ms
// (about 1 s in all, the same list every time), fails as cut by the default window (3 hang
// guards of 200 ms) and passes when BlockTime is raised or lifted.
func TestSlowCapabilitiesBlock(t *testing.T) {
	s := hostile(t, fakeserver.Options{}, func(c *fakeserver.Conn, line string) bool {
		if !strings.HasPrefix(line, "CAPABILITIES") {
			return c.WriteLines("500 what?") == nil
		}
		if c.WriteLines("101 caps", "VERSION 2") != nil {
			return false
		}
		for i := range 100 {
			if c.WriteLines(fmt.Sprintf("X-SLOW-%d", i)) != nil {
				return false
			}
			time.Sleep(10 * time.Millisecond)
		}
		return c.WriteLines("READER", ".") == nil
	})
	const deadline = 200 * time.Millisecond
	for _, tc := range []struct {
		blockTime time.Duration
		fail      bool
	}{{0, true}, {10 * time.Second, false}, {-1, false}} {
		var r Report
		var err error
		within(t, 15*time.Second, "Check", func() {
			r, err = Check(Options{Addr: s.Addr(), Deadline: deadline, BlockTime: tc.blockTime, Skip: onlyChecks("capabilities")})
		})
		if err != nil {
			t.Fatalf("BlockTime %v: %v", tc.blockTime, err)
		}
		res := r.ByName()["capabilities"]
		switch {
		case tc.fail && (res.Status != Fail || !strings.Contains(res.Detail, "dot block too long")):
			t.Errorf("BlockTime %v: %v %q, want a Fail as cut", tc.blockTime, res.Status, res.Detail)
		case !tc.fail && res.Status != Pass:
			t.Errorf("BlockTime %v: %v %q, want Pass", tc.blockTime, res.Status, res.Detail)
		}
	}
}
