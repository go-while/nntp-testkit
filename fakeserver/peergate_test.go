package fakeserver

import (
	"net"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// The test knobs of PeerHandler: HoldGreeting, GreetingDelay, BeforeReply, CapabilitiesReply,
// ModeStreamReply, DropAfterChecks, SilentAfterModeStream and Faults.

// dialRaw connects without reading the greeting; the client's hang guard is d.
func dialRaw(t *testing.T, addr string, d time.Duration) *client.Conn {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(t, nc)
	c.Deadline = d
	return c
}

// noLeak fails t when goroutines started after the call are still there once Close returned.
func noLeak(t *testing.T) func() {
	t.Helper()
	before := runtime.NumGoroutine()
	return func() {
		t.Helper()
		end := time.Now().Add(3 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(end) {
				t.Errorf("goroutines: %d before, %d after Close", before, runtime.NumGoroutine())
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestPeerHoldGreeting(t *testing.T) {
	gate := make(chan struct{})
	have := map[string]bool{"<h@x>": true}
	s := New(t, Options{NoGreeting: true, Handler: PeerHandler(&PeerOptions{HoldGreeting: gate, Have: have})})
	const n = 4
	var conns []*client.Conn
	for i := 0; i < n; i++ {
		conns = append(conns, dialRaw(t, s.Addr(), 150*time.Millisecond))
	}
	// all four are open on the server while held, none has a greeting
	s.WaitOpen(t, n, 2*time.Second)
	for i, c := range conns {
		if line, err := c.ReadGreeting(); err == nil || !isTimeout(err) {
			t.Fatalf("conn %d: greeting %q (%v) while held", i, line, err)
		}
	}
	if got, open, max := s.Accepted(), s.Open(), s.MaxOpen(); got != n || open != n || max != n {
		t.Fatalf("held: Accepted %d Open %d MaxOpen %d, want %d each", got, open, max, n)
	}
	close(gate)
	for i, c := range conns {
		c.Deadline = 2 * time.Second
		if line, err := c.ReadGreeting(); err != nil || line != DefaultGreeting {
			t.Fatalf("conn %d: greeting %q (%v) after release", i, line, err)
		}
		if code, _ := c.Cmd("CHECK <h@x>"); code != 438 {
			t.Fatalf("conn %d: CHECK %d, Have must still work", i, code)
		}
	}
	if got := s.Count("CHECK"); got != n {
		t.Errorf("Count(CHECK) = %d, want %d", got, n)
	}
	// a later connection is not held: the gate is closed
	late := dialRaw(t, s.Addr(), 2*time.Second)
	if _, err := late.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
}

// HoldGreeting as a bound for a client with a limit K: the client opens connections while
// the earlier ones have no greeting yet; the test releases one at a time (a value on the
// channel releases one connection) and sees that the server never held more than K.
func TestPeerHoldGreetingMaxOpen(t *testing.T) {
	gate := make(chan struct{})
	s := New(t, Options{NoGreeting: true, Handler: PeerHandler(&PeerOptions{HoldGreeting: gate})})
	const k, total = 2, 5
	sem := make(chan struct{}, k)
	done := make(chan error, total)
	for i := 0; i < total; i++ {
		go func() {
			sem <- struct{}{} // the client's own limit of K connections
			defer func() { <-sem }()
			c, err := client.TryDial(s.Addr(), 5*time.Second)
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			if _, _, err = c.TryCmd("QUIT"); err == nil {
				_, err = c.ReadLine() // EOF: the server has counted the connection out
				if err != nil && strings.Contains(err.Error(), "EOF") {
					err = nil
				}
			}
			done <- err
		}()
	}
	s.WaitOpen(t, k, 2*time.Second) // two connections held, the other workers wait for a slot
	for i := 0; i < total; i++ {
		gate <- struct{}{} // releases exactly one held connection
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := s.MaxOpen(); got > k {
		t.Errorf("MaxOpen = %d, want <= %d", got, k)
	}
	if got := s.Accepted(); got != total {
		t.Errorf("Accepted = %d, want %d", got, total)
	}
}

func TestPeerHoldGreetingClose(t *testing.T) {
	check := noLeak(t)
	s, err := Start(Options{NoGreeting: true, Deadline: time.Minute, Handler: PeerHandler(&PeerOptions{HoldGreeting: make(chan struct{})})})
	if err != nil {
		t.Fatal(err)
	}
	var conns []*client.Conn
	for i := 0; i < 3; i++ {
		conns = append(conns, dialRaw(t, s.Addr(), 5*time.Second))
	}
	waitOpenStart(t, s, 3)
	start := time.Now()
	s.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v with held connections, Deadline is a minute", d)
	}
	for _, c := range conns {
		if line, err := c.ReadGreeting(); err == nil {
			t.Errorf("greeting %q after Close", line)
		}
	}
	if s.Open() != 0 {
		t.Errorf("Open = %d after Close", s.Open())
	}
	check()
}

// waitOpenStart polls Open for a server made with Start (no tb).
func waitOpenStart(t *testing.T, s *Server, n int) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for s.Open() != n {
		if time.Now().After(end) {
			t.Fatalf("Open = %d, want %d", s.Open(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPeerHoldGreetingDeadline(t *testing.T) {
	s := New(t, Options{NoGreeting: true, Deadline: 200 * time.Millisecond, Handler: PeerHandler(&PeerOptions{HoldGreeting: make(chan struct{})})})
	c := dialRaw(t, s.Addr(), 5*time.Second)
	start := time.Now()
	if line, err := c.ReadGreeting(); err == nil {
		t.Fatalf("greeting %q after the Deadline of a hold", line)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("held for %v, Deadline is 200ms", d)
	}
	s.WaitOpen(t, 0, 2*time.Second)
}

func TestPeerGreetingDelay(t *testing.T) {
	check := noLeak(t)
	s := New(t, Options{NoGreeting: true, Greeting: "200 hello", Handler: PeerHandler(&PeerOptions{GreetingDelay: 300 * time.Millisecond})})
	start := time.Now()
	c := dialRaw(t, s.Addr(), 5*time.Second)
	line, err := c.ReadGreeting()
	if err != nil || line != "200 hello" {
		t.Fatalf("greeting %q (%v)", line, err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("greeting after %v, want >= 300ms", d)
	}
	if code, _ := c.Cmd("MODE STREAM"); code != 203 {
		t.Errorf("MODE STREAM %d", code)
	}
	// capped at the Deadline: an hour of delay ends at 150ms
	s2 := New(t, Options{NoGreeting: true, Deadline: 150 * time.Millisecond, Handler: PeerHandler(&PeerOptions{GreetingDelay: time.Hour})})
	start = time.Now()
	c2 := dialRaw(t, s2.Addr(), 5*time.Second)
	if line, err := c2.ReadGreeting(); err != nil || line != DefaultGreeting {
		t.Fatalf("capped delay: greeting %q (%v)", line, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("delay not capped: %v", d)
	}
	s.Close()
	s2.Close()
	check()
}

func TestPeerBeforeReply(t *testing.T) {
	check := noLeak(t)
	release := make(chan struct{})
	type call struct{ cmd, line, status string }
	calls := make(chan call, 16)
	entered := make(chan struct{}, 4)
	var log peerLog
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		OnArticle: log.onArticle,
		BeforeReply: func(c *Conn, cmd, line, status string) {
			calls <- call{cmd, line, status}
			if cmd == "CHECK" && strings.Contains(line, "<held@x>") {
				entered <- struct{}{}
				c.Wait(release)
			}
		},
	})})
	c := dialRaw(t, s.Addr(), 150*time.Millisecond)
	if _, err := c.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	if code, _ := c.Cmd("CHECK <free@x>"); code != 238 {
		t.Fatalf("CHECK %d", code)
	}
	if err := c.Send("CHECK <held@x>\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, _, err := c.ReadStatus(); err == nil || !isTimeout(err) {
		t.Fatalf("reply while held: %v", err)
	}
	if got := s.Count("CHECK"); got != 2 {
		t.Errorf("Count(CHECK) = %d while the reply is held, want 2", got)
	}
	close(release)
	c.Deadline = 2 * time.Second
	if code, line, err := c.ReadStatus(); err != nil || code != 238 || !strings.Contains(line, "<held@x>") {
		t.Fatalf("after release: %q (%v)", line, err)
	}
	// IHAVE: the hook sees the 335 and the final 235; TAKETHIS: OnArticle ran before its hook
	wantCode(t, c, "IHAVE <i@x>", 335)
	sendArticle(t, c, []string{"Message-ID: <i@x>", "", "b"})
	wantStatus(t, c, 235)
	wantCode(t, c, "QUIT", 205)
	var got []call
	for len(calls) > 0 {
		got = append(got, <-calls)
	}
	want := []call{
		{"CHECK", "CHECK <free@x>", "238 <free@x>"},
		{"CHECK", "CHECK <held@x>", "238 <held@x>"},
		{"IHAVE", "IHAVE <i@x>", "335 Send it"},
		{"IHAVE", "IHAVE <i@x>", "235 Article transferred OK"},
		{"QUIT", "QUIT", "205 bye"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hook calls %q, want %q", got, want)
	}
	if ids, _ := log.got(); !reflect.DeepEqual(ids, []string{"<i@x>"}) {
		t.Errorf("OnArticle ids %q", ids)
	}
	s.Close()
	check()
}

// A reply held by BeforeReply is released by Server.Close, not by the Deadline.
func TestPeerBeforeReplyClose(t *testing.T) {
	check := noLeak(t)
	entered := make(chan struct{}, 3)
	s, err := Start(Options{Deadline: time.Minute, Handler: PeerHandler(&PeerOptions{
		BeforeReply: func(c *Conn, cmd, line, status string) {
			entered <- struct{}{}
			c.Wait(nil) // never released by the test
		},
	})})
	if err != nil {
		t.Fatal(err)
	}
	var conns []*client.Conn
	for i := 0; i < 3; i++ {
		c := dialRaw(t, s.Addr(), 5*time.Second)
		if _, err := c.ReadGreeting(); err != nil {
			t.Fatal(err)
		}
		if err := c.Send("CHECK <x@x>\r\n"); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		<-entered
	}
	start := time.Now()
	s.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v with held replies", d)
	}
	for _, c := range conns {
		if line, err := c.ReadLine(); err == nil {
			t.Errorf("reply %q after Close", line)
		}
	}
	check()
}

func TestPeerDropAfterChecks(t *testing.T) {
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{DropAfterChecks: 2})})
	for round := 0; round < 2; round++ { // the count is per connection
		c := client.Dial(t, s.Addr())
		wantCode(t, c, "CHECK <a@x>", 238)
		wantCode(t, c, "CHECK <b@x>", 238)
		line := wantCode(t, c, "CHECK <c@x>", 400)
		if line != "400 Service temporarily unavailable" {
			t.Errorf("drop reply %q", line)
		}
		wantEOF(t, c)
	}
	if got := s.Count("CHECK"); got != 6 {
		t.Errorf("Count(CHECK) = %d, want 6", got)
	}
	s.WaitOpen(t, 0, 2*time.Second)

	// custom line; CHECKs before authentication do not count
	s2 := New(t, Options{Handler: PeerHandler(&PeerOptions{User: "u", Pass: "p", DropAfterChecks: 1, DropReply: "400 Closing down"})})
	c := client.Dial(t, s2.Addr())
	wantCode(t, c, "CHECK <a@x>", 480)
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	wantCode(t, c, "CHECK <a@x>", 238)
	if line := wantCode(t, c, "CHECK <b@x>", 400); line != "400 Closing down" {
		t.Errorf("drop reply %q", line)
	}
	wantEOF(t, c)
}

func TestPeerSilentAfterModeStream(t *testing.T) {
	check := noLeak(t)
	// the server Deadline leaves room for two 200ms client timeouts on a loaded host
	s := New(t, Options{Deadline: 2 * time.Second, Handler: PeerHandler(&PeerOptions{SilentAfterModeStream: true})})
	c := dialRaw(t, s.Addr(), 200*time.Millisecond)
	if _, err := c.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c, "MODE STREAM", 203)
	for _, cmd := range []string{"CHECK <a@x>", "CAPABILITIES"} {
		if code, line, err := c.TryCmd("%s", cmd); err == nil || !isTimeout(err) {
			t.Fatalf("%s after MODE STREAM: %d %q (%v), want a client timeout", cmd, code, line, err)
		}
	}
	// the lines are read and counted; the server's Deadline then closes the connection
	if want := map[string]int{"MODE": 1, "CHECK": 1, "CAPABILITIES": 1}; !reflect.DeepEqual(s.Counts(), want) {
		t.Errorf("Counts() = %v, want %v", s.Counts(), want)
	}
	s.WaitOpen(t, 0, 5*time.Second)
	c.Deadline = 5 * time.Second
	wantEOF(t, c)
	s.Close()
	check()

	// released by Close well before the Deadline
	s2, err := Start(Options{Deadline: time.Minute, Handler: PeerHandler(&PeerOptions{SilentAfterModeStream: true})})
	if err != nil {
		t.Fatal(err)
	}
	c2 := dialRaw(t, s2.Addr(), 5*time.Second)
	if _, err := c2.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c2, "MODE STREAM", 203)
	start := time.Now()
	s2.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v with a silent connection", d)
	}
}

// Diablo's feeder: CAPABILITIES is unknown (500), MODE STREAM answers 203 StreamOK.
func TestPeerDiabloFeeder(t *testing.T) {
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		CapabilitiesReply: []string{"500 What?"},
		ModeStreamReply:   []string{"203 StreamOK."},
		Have:              map[string]bool{"<h@x>": true},
	})})
	c := client.Dial(t, s.Addr())
	if line := wantCode(t, c, "CAPABILITIES", 500); line != "500 What?" {
		t.Errorf("CAPABILITIES reply %q", line)
	}
	if line := wantCode(t, c, "MODE STREAM", 203); line != "203 StreamOK." {
		t.Errorf("MODE STREAM reply %q", line)
	}
	wantCode(t, c, "CHECK <h@x>", 438)
	wantCode(t, c, "CHECK <n@x>", 238)
	wantCode(t, c, "QUIT", 205)
	if want := map[string]int{"CAPABILITIES": 1, "MODE": 1, "CHECK": 2, "QUIT": 1}; !reflect.DeepEqual(s.Counts(), want) {
		t.Errorf("Counts() = %v, want %v", s.Counts(), want)
	}
}

func TestPeerCapabilitiesAndModeStreamReply(t *testing.T) {
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		CapabilitiesReply: []string{"101 caps", "VERSION 2", "IMPLEMENTATION x", "."},
		ModeStreamReply:   []string{"501 refused"},
	})})
	c := client.Dial(t, s.Addr())
	wantCode(t, c, "CAPABILITIES", 101)
	lines, err := c.TryReadDotLines()
	if err != nil || !reflect.DeepEqual(lines, []string{"VERSION 2", "IMPLEMENTATION x"}) {
		t.Fatalf("capabilities %q (%v)", lines, err)
	}
	// refused MODE STREAM, yet CHECK works (it is not NoStream)
	if line := wantCode(t, c, "MODE STREAM", 501); line != "501 refused" {
		t.Errorf("MODE STREAM reply %q", line)
	}
	wantCode(t, c, "CHECK <n@x>", 238)

	// under NoStream the override still wins; CHECK is unknown there
	s2 := New(t, Options{Handler: PeerHandler(&PeerOptions{NoStream: true, ModeStreamReply: []string{"500 no"}})})
	c2 := client.Dial(t, s2.Addr())
	wantCode(t, c2, "MODE STREAM", 500)
	wantCode(t, c2, "CHECK <n@x>", 500)
	// without the knobs the replies are the defaults
	s3 := New(t, Options{Handler: PeerHandler(&PeerOptions{})})
	c3 := client.Dial(t, s3.Addr())
	if line := wantCode(t, c3, "MODE STREAM", 203); line != "203 streaming ok" {
		t.Errorf("default reply %q", line)
	}
}

// Faults apply to PeerHandler: Delay, Override, OverrideLine, CloseAfter.
func TestPeerFaults(t *testing.T) {
	var log peerLog
	s := New(t, Options{
		Faults: Faults{
			Delay:        map[string]time.Duration{"CHECK": 300 * time.Millisecond},
			Override:     map[string][]string{"TAKETHIS": {"400 try later"}},
			OverrideLine: map[string][]string{"MODE STREAM": {"203 StreamOK."}},
		},
		Handler: PeerHandler(&PeerOptions{OnArticle: log.onArticle}),
	})
	c := client.Dial(t, s.Addr())
	if line := wantCode(t, c, "MODE STREAM", 203); line != "203 StreamOK." {
		t.Errorf("OverrideLine: %q", line)
	}
	start := time.Now()
	wantCode(t, c, "CHECK <a@x>", 238)
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("Faults.Delay: CHECK answered after %v", d)
	}
	// Override replaces the reply, the article was still taken (Have, OnArticle)
	if err := c.Send("TAKETHIS <t@x>\r\nMessage-ID: <t@x>\r\n\r\nb\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 400)
	if ids, _ := log.got(); !reflect.DeepEqual(ids, []string{"<t@x>"}) {
		t.Errorf("OnArticle ids %q", ids)
	}
	// IHAVE: Faults apply to the 335 only; Override that is not a 335 is final, no article follows
	s2 := New(t, Options{
		Faults:  Faults{Override: map[string][]string{"IHAVE": {"436 not now"}}},
		Handler: PeerHandler(&PeerOptions{}),
	})
	c2 := client.Dial(t, s2.Addr())
	wantCode(t, c2, "IHAVE <i@x>", 436)
	wantCode(t, c2, "CHECK <i@x>", 238) // the next line is a command, not an article
	if len(s2.Articles()) != 0 {
		t.Errorf("articles read after an overridden IHAVE: %d", len(s2.Articles()))
	}
	// CloseAfter
	s3 := New(t, Options{Faults: Faults{CloseAfter: 2}, Handler: PeerHandler(&PeerOptions{})})
	c3 := client.Dial(t, s3.Addr())
	wantCode(t, c3, "CHECK <a@x>", 238)
	wantCode(t, c3, "CHECK <b@x>", 238)
	wantEOF(t, c3)
}

// Faults.Override on COMPRESS must not break the compression state.
func TestPeerFaultsCompressOverride(t *testing.T) {
	s := New(t, Options{
		Faults:  Faults{Override: map[string][]string{"COMPRESS": {"403 no"}}},
		Handler: PeerHandler(&PeerOptions{Compress: true}),
	})
	c := client.Dial(t, s.Addr())
	wantCode(t, c, "COMPRESS DEFLATE", 403)
	wantCode(t, c, "CHECK <a@x>", 238) // still uncompressed
}

// TestPeerEmptyReplyWithAuthNeedsTLS: an empty (non-nil) CapabilitiesReply on a peer that
// requires TLS before AUTHINFO writes nothing and does not panic.
func TestPeerEmptyReplyWithAuthNeedsTLS(t *testing.T) {
	cfg, _ := TLSConfig(t)
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		User: "u", Pass: "p", CapabilitiesReply: []string{},
		StartTLS: &StartTLSOptions{Config: cfg, AuthNeedsTLS: true},
	})})
	c := dialRaw(t, s.Addr(), 300*time.Millisecond)
	if _, err := c.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Send("CAPABILITIES\r\n"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c, "MODE STREAM", 203)
}
