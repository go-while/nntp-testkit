package fakeserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/client"
)

// recordTB records Fatalf instead of failing the test and ends the calling goroutine.
type recordTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	msg    string
}

func (r *recordTB) Helper() {}

func (r *recordTB) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

// run calls fn with r on its own goroutine (Goexit must not end the test goroutine) and
// returns whether fn failed r, and the message.
func (r *recordTB) run(fn func(tb testing.TB)) (bool, string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed, r.msg
}

// rawDial opens a plain TCP connection to s with the kit's hang guard (no greeting read).
func rawDial(t *testing.T, s *Server) net.Conn {
	t.Helper()
	nc, err := net.DialTimeout("tcp", s.Addr(), testkit.Deadline())
	if err != nil {
		t.Fatalf("dial fakeserver: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return nc
}

// wantCode runs cmd on c and checks the reply code; it returns the status line.
func wantCode(t *testing.T, c *client.Conn, cmd string, want int) string {
	t.Helper()
	code, line := c.Cmd("%s", cmd)
	if code != want {
		t.Fatalf("%q: got %q, want %d", cmd, line, want)
	}
	return line
}

// wantStatus reads the next status line from c and checks its code.
func wantStatus(t *testing.T, c *client.Conn, want int) string {
	t.Helper()
	code, line, err := c.ReadStatus()
	if err != nil || code != want {
		t.Fatalf("status: got %q (%v), want %d", line, err, want)
	}
	return line
}

// wantEOF checks that the server closed c: the next read ends without a line.
func wantEOF(t *testing.T, c *client.Conn) {
	t.Helper()
	if l, err := c.ReadLine(); err == nil {
		t.Fatalf("read %q, want EOF", l)
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isReset(err) {
		t.Fatalf("read error %v, want EOF", err)
	}
}

// isReset reports a connection reset (a close with unread data on either side).
func isReset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection reset")
}

func TestScript(t *testing.T) {
	s := New(t, Options{Script: []Step{
		{Expect: "HELLO .*", Reply: []string{"280 hi"}},
		{Expect: "POST", ReadDot: true, Delay: 20 * time.Millisecond, Reply: []string{"240 ok"}},
	}})
	c := client.Dial(t, s.Addr())
	if c.Greeting != DefaultGreeting {
		t.Fatalf("greeting %q", c.Greeting)
	}
	wantCode(t, c, "NOPE", 500)
	wantCode(t, c, "HELLO world", 280)
	if err := c.Send("POST\r\na\r\n..b\r\n\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 240)
	// after the last step: recorded, never answered
	if err := c.Send("AFTER\r\n"); err != nil {
		t.Fatal(err)
	}
	s.WaitLines(t, []string{"NOPE", "HELLO world", "POST", "AFTER"})
	if got := s.Mismatches(); !reflect.DeepEqual(got, []string{"NOPE"}) {
		t.Errorf("Mismatches() = %q", got)
	}
	arts := s.Articles()
	if len(arts) != 1 || !reflect.DeepEqual(arts[0], []string{"a", "..b", ""}) {
		t.Fatalf("Articles() = %q", arts)
	}
	if got := client.Unstuff(arts[0]); !reflect.DeepEqual(got, []string{"a", ".b", ""}) {
		t.Errorf("Unstuff = %q", got)
	}

	// CloseAfter: the server closes after the reply
	cs := New(t, Options{Greeting: "201 no posting", Script: []Step{
		{Expect: "QUIT", Reply: []string{"205 bye"}, CloseAfter: true},
	}})
	c2 := client.Dial(t, cs.Addr())
	if !strings.HasPrefix(c2.Greeting, "201 ") {
		t.Fatalf("greeting %q", c2.Greeting)
	}
	wantCode(t, c2, "QUIT", 205)
	wantEOF(t, c2)
	cs.WaitOpen(t, 0, 2*time.Second)

	// Reply then Hang: the reply arrives, nothing follows, lines are still recorded
	hs := New(t, Options{Script: []Step{
		{Expect: "ARTICLE .*", Reply: []string{"220 0 <a@b>", "Path: x"}, Hang: true},
	}})
	ch := client.Dial(t, hs.Addr())
	if line := wantCode(t, ch, "ARTICLE <a@b>", 220); line != "220 0 <a@b>" {
		t.Errorf("220 line %q", line)
	}
	if l, err := ch.ReadLine(); err != nil || l != "Path: x" {
		t.Fatalf("second reply line %q %v", l, err)
	}
	if err := ch.Send("STAT <a@b>\r\n"); err != nil {
		t.Fatal(err)
	}
	hs.WaitLines(t, []string{"ARTICLE <a@b>", "STAT <a@b>"})
	// deliberate short negative window: nothing may arrive within 150 ms
	if err := ch.ExpectNoPending(150 * time.Millisecond); err != nil {
		t.Errorf("Hang step wrote after its reply: %v", err)
	}
	hs.WaitOpen(t, 1, 2*time.Second)

	// NoGreeting + Handler: the Handler writes its own greeting
	ng := New(t, Options{NoGreeting: true, Handler: func(c *Conn) {
		if !c.sleep(20 * time.Millisecond) {
			return
		}
		if err := c.WriteLines("201 own greeting"); err != nil {
			return
		}
		c.drain()
	}})
	if g := client.Dial(t, ng.Addr()).Greeting; g != "201 own greeting" {
		t.Errorf("handler greeting %q", g)
	}
}

func TestOpenCount(t *testing.T) {
	// client close observed by the server
	s := New(t, Options{})
	c := client.Dial(t, s.Addr())
	s.WaitOpen(t, 1, 2*time.Second)
	c.Close()
	s.WaitOpen(t, 0, 2*time.Second)
	if n := s.Accepted(); n != 1 {
		t.Errorf("Accepted() = %d, want 1", n)
	}

	// RawStall: no greeting, client close still observed
	r := New(t, Options{RawStall: true})
	nc := rawDial(t, r)
	rc := client.New(t, nc)
	if err := rc.Send("hello\r\n"); err != nil {
		t.Fatal(err)
	}
	// deliberate short negative window: RawStall must not write within 100 ms
	if err := rc.ExpectNoPending(100 * time.Millisecond); err != nil {
		t.Errorf("RawStall wrote: %v", err)
	}
	r.WaitOpen(t, 1, 2*time.Second)
	rc.Close()
	r.WaitOpen(t, 0, 2*time.Second)
	if got := r.Lines(); len(got) != 0 {
		t.Errorf("RawStall recorded lines %q", got)
	}

	// NoGreeting records lines; Hang holds the conn until Close
	g := New(t, Options{NoGreeting: true})
	gc := client.New(t, rawDial(t, g))
	if err := gc.Send("CAPABILITIES\r\n"); err != nil {
		t.Fatal(err)
	}
	g.WaitLines(t, []string{"CAPABILITIES"})
	g.WaitOpen(t, 1, 2*time.Second)

	h := New(t, Options{Script: []Step{{Expect: "ARTICLE .*", Hang: true}}})
	hc := client.Dial(t, h.Addr())
	if err := hc.Send("ARTICLE <a@b>\r\nSTAT <a@b>\r\n"); err != nil {
		t.Fatal(err)
	}
	h.WaitLines(t, []string{"ARTICLE <a@b>", "STAT <a@b>"})
	h.WaitOpen(t, 1, 2*time.Second)
	h.Close()
	if n := h.Open(); n != 0 {
		t.Errorf("Open() after Close = %d", n)
	}
	wantEOF(t, hc)
}

func TestPeerStream(t *testing.T) {
	var mu sync.Mutex
	var got []string
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		User:         "u",
		Pass:         "p",
		Defer431Once: map[string]bool{"<d@test.invalid>": true},
		OnArticle: func(id string, wire []string) {
			mu.Lock()
			got = append(got, id)
			mu.Unlock()
		},
	})})
	c := client.Dial(t, s.Addr())
	if !strings.HasPrefix(c.Greeting, "200 ") {
		t.Fatalf("greeting %q", c.Greeting)
	}
	wantCode(t, c, "CHECK <x@test.invalid>", 480)
	wantCode(t, c, "CAPABILITIES", 101)
	if caps := c.ReadDotLines(); !reflect.DeepEqual(caps, []string{"VERSION 2", "READER", "STREAMING"}) {
		t.Fatalf("capabilities %q", caps)
	}
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	wantCode(t, c, "MODE STREAM", 203)
	if line := wantCode(t, c, "CHECK <x@test.invalid>", 238); line != "238 <x@test.invalid>" {
		t.Errorf("238 line %q", line)
	}
	wantCode(t, c, "CHECK <d@test.invalid>", 431)
	wantCode(t, c, "CHECK <d@test.invalid>", 238)
	payload := []string{"Message-ID: <x@test.invalid>", "", "..dot", "body", ""}
	if err := c.Send("TAKETHIS <x@test.invalid>\r\n" + strings.Join(payload, "\r\n") + "\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 239)
	wantCode(t, c, "CHECK <x@test.invalid>", 438)
	if err := c.Send("TAKETHIS <x@test.invalid>\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 439)
	wantCode(t, c, "XYZZY", 500)
	wantCode(t, c, "QUIT", 205)
	s.WaitOpen(t, 0, 2*time.Second)

	arts := s.Articles()
	if len(arts) != 2 || !reflect.DeepEqual(arts[0], payload) || len(arts[1]) != 0 {
		t.Fatalf("Articles() = %q", arts)
	}
	if body := client.Unstuff(arts[0]); body[2] != ".dot" {
		t.Errorf("unstuffed %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(got, []string{"<x@test.invalid>"}) {
		t.Errorf("OnArticle ids %q", got)
	}
	if n := len(s.Mismatches()); n != 0 {
		t.Errorf("Mismatches() = %d", n)
	}
}

func TestStartWithoutTB(t *testing.T) {
	s, err := Start(Options{Script: []Step{{Expect: "PING", Reply: []string{"200 pong"}}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Host() != "127.0.0.1" || s.Port() <= 0 || s.Addr() != fmt.Sprintf("127.0.0.1:%d", s.Port()) {
		t.Errorf("Host %q Port %d Addr %q", s.Host(), s.Port(), s.Addr())
	}
	c, err := client.TryDial(s.Addr(), 0)
	if err != nil {
		t.Fatalf("TryDial: %v", err)
	}
	defer c.Close()
	if code, line, err := c.TryCmd("PING"); err != nil || code != 200 {
		t.Fatalf("PING: %d %q %v", code, line, err)
	}
	s.Close()
	s.Close() // idempotent
	if n := s.Open(); n != 0 {
		t.Errorf("Open() after Close = %d", n)
	}
	if _, err := c.ReadLine(); err == nil {
		t.Error("connection still open after Close")
	}
	if c2, err := client.TryDial(s.Addr(), time.Second); err == nil {
		c2.Close()
		t.Error("listener still accepting after Close")
	}

	if _, err := Start(Options{Script: []Step{{Expect: "("}}}); err == nil {
		t.Error("Start with a bad Expect: no error")
	}
	if _, err := Start(Options{Addr: "127.0.0.1:-1"}); err == nil {
		t.Error("Start with a bad Addr: no error")
	}
	rt := &recordTB{}
	if failed, msg := rt.run(func(tb testing.TB) { New(tb, Options{Script: []Step{{Expect: "("}}}) }); !failed ||
		!strings.Contains(msg, "bad Expect") {
		t.Errorf("New with a bad Expect: failed %v %q", failed, msg)
	}
}

func TestCounts(t *testing.T) {
	// Script mode: mismatches, matched steps and lines after the last step are all counted
	s := New(t, Options{Script: []Step{{Expect: "(?i)GROUP .*", Reply: []string{"211 0 1 0 a"}}}})
	c := client.Dial(t, s.Addr())
	wantCode(t, c, "article 1", 500)
	wantCode(t, c, "group a", 211)
	if err := c.Send("ARTICLE 2\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	s.WaitLines(t, []string{"article 1", "group a", "ARTICLE 2", ""})
	if n := s.Count("Article"); n != 2 {
		t.Errorf("Count(Article) = %d, want 2", n)
	}
	if want := map[string]int{"ARTICLE": 2, "GROUP": 1, "": 1}; !reflect.DeepEqual(s.Counts(), want) {
		t.Errorf("Counts() = %v, want %v", s.Counts(), want)
	}

	// record-only (NoGreeting) and Handler mode count as well
	g := New(t, Options{NoGreeting: true})
	gc := client.New(t, rawDial(t, g))
	if err := gc.Send("CAPABILITIES\r\nmode reader\r\n"); err != nil {
		t.Fatal(err)
	}
	g.WaitLines(t, []string{"CAPABILITIES", "mode reader"})
	if want := map[string]int{"CAPABILITIES": 1, "MODE": 1}; !reflect.DeepEqual(g.Counts(), want) {
		t.Errorf("record-only Counts() = %v, want %v", g.Counts(), want)
	}

	p := New(t, Options{Handler: PeerHandler(&PeerOptions{})})
	pc := client.Dial(t, p.Addr())
	wantCode(t, pc, "MODE STREAM", 203)
	wantCode(t, pc, "CHECK <a@b>", 238)
	if err := pc.Send("TAKETHIS <a@b>\r\nX: y\r\n\r\nbody\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, pc, 239)
	// the payload lines of TAKETHIS are neither lines nor commands
	if want := map[string]int{"MODE": 1, "CHECK": 1, "TAKETHIS": 1}; !reflect.DeepEqual(p.Counts(), want) {
		t.Errorf("handler Counts() = %v, want %v", p.Counts(), want)
	}
	if n := p.Count("QUIT"); n != 0 {
		t.Errorf("Count(QUIT) = %d", n)
	}
}

func TestWaitLines(t *testing.T) {
	s := New(t, Options{Deadline: 50 * time.Millisecond})
	s.WaitLines(t, nil) // nil and empty are equal; returns at once
	s.WaitLines(t, []string{})
	rt := &recordTB{}
	failed, msg := rt.run(func(tb testing.TB) { s.WaitLines(tb, []string{"X"}) })
	if !failed || !strings.Contains(msg, "Lines()") {
		t.Errorf("WaitLines on a silent server: failed %v %q", failed, msg)
	}
}

func TestFaultsScript(t *testing.T) {
	steps := func() []Step {
		return []Step{
			{Expect: "A", Reply: []string{"200 a"}},
			{Expect: "B", Reply: []string{"200 b"}},
			{Expect: "ARTICLE .*", Reply: []string{"220 1 <a@b>", "Path: x", "Subject: s", "", "body", "."}},
		}
	}

	t.Run("Delay", func(t *testing.T) {
		s := New(t, Options{Script: steps(), Faults: Faults{Delay: map[string]time.Duration{"B": 30 * time.Millisecond}}})
		c := client.Dial(t, s.Addr())
		wantCode(t, c, "A", 200)
		if line := wantCode(t, c, "B", 200); line != "200 b" {
			t.Errorf("delayed reply %q", line)
		}
	})

	t.Run("Override", func(t *testing.T) {
		s := New(t, Options{Script: steps(), Faults: Faults{Override: map[string][]string{
			"A":       {"480 nope"},
			"ARTICLE": {"222 1 <a@b>", "body only", "."},
		}}})
		c := client.Dial(t, s.Addr())
		if line := wantCode(t, c, "A", 480); line != "480 nope" {
			t.Errorf("override %q", line)
		}
		wantCode(t, c, "B", 200)
		wantCode(t, c, "ARTICLE <a@b>", 222)
		if body := c.ReadDotLines(); !reflect.DeepEqual(body, []string{"body only"}) {
			t.Errorf("override body %q", body)
		}
	})

	t.Run("CloseAfter", func(t *testing.T) {
		s := New(t, Options{Script: steps(), Faults: Faults{CloseAfter: 3}})
		c := client.Dial(t, s.Addr())
		wantCode(t, c, "X", 500) // a mismatch is a command too
		wantCode(t, c, "A", 200)
		wantCode(t, c, "B", 200)
		wantEOF(t, c)
		s.WaitOpen(t, 0, 2*time.Second)
		if got := s.Lines(); !reflect.DeepEqual(got, []string{"X", "A", "B"}) {
			t.Errorf("Lines() = %q", got)
		}
	})

	t.Run("CloseAfterTrailing", func(t *testing.T) {
		// after the last step the trailing reads count too
		s := New(t, Options{Script: []Step{{Expect: "A", Reply: []string{"200 a"}}}, Faults: Faults{CloseAfter: 2}})
		c := client.Dial(t, s.Addr())
		wantCode(t, c, "A", 200)
		if err := c.Send("LATE\r\n"); err != nil {
			t.Fatal(err)
		}
		wantEOF(t, c)
		s.WaitOpen(t, 0, 2*time.Second)
	})

	t.Run("DropInMultiline", func(t *testing.T) {
		s := New(t, Options{Script: steps(), Faults: Faults{DropInMultiline: map[string]int{"ARTICLE": 2, "A": 5}}})
		c := client.Dial(t, s.Addr())
		wantCode(t, c, "A", 200) // shorter than 5 lines: not cut
		wantCode(t, c, "B", 200)
		wantCode(t, c, "ARTICLE <a@b>", 220)
		lines, err := c.TryReadDotLines()
		var se *client.StrictError
		if !errors.As(err, &se) || se.Kind != client.NoTerminator {
			t.Fatalf("cut reply: err %v, want NoTerminator", err)
		}
		if !reflect.DeepEqual(lines, []string{"Path: x"}) {
			t.Errorf("lines before the drop %q", lines)
		}
		s.WaitOpen(t, 0, 2*time.Second)
	})

	t.Run("DropBeforeStatus", func(t *testing.T) {
		s := New(t, Options{Script: steps(), Faults: Faults{DropInMultiline: map[string]int{"A": 0}}})
		c := client.Dial(t, s.Addr())
		if code, line, err := c.TryCmd("A"); err == nil {
			t.Fatalf("A: got %d %q, want EOF", code, line)
		}
		s.WaitOpen(t, 0, 2*time.Second)
	})
}

func TestTLS(t *testing.T) {
	srvCfg, roots := TLSConfig(t)
	s := New(t, Options{TLS: srvCfg, Script: []Step{{Expect: "PING", Reply: []string{"200 pong"}}}})
	nc := rawDial(t, s)
	tc := tls.Client(nc, &tls.Config{RootCAs: roots, ServerName: s.Host(), MinVersion: tls.VersionTLS12})
	c := client.New(t, tc)
	if g, err := c.ReadGreeting(); err != nil || g != DefaultGreeting {
		t.Fatalf("TLS greeting %q %v", g, err)
	}
	if line := wantCode(t, c, "PING", 200); line != "200 pong" {
		t.Errorf("PING over TLS %q", line)
	}
	if !tc.ConnectionState().HandshakeComplete {
		t.Error("handshake not complete")
	}
	s.WaitLines(t, []string{"PING"})
}

func TestStorePrecedence(t *testing.T) {
	old := storeHandler
	t.Cleanup(func() { storeHandler = old })
	st := NewStore()

	// no store handler linked in: Store mode records only (after the greeting)
	storeHandler = nil
	s := New(t, Options{Store: st, Script: []Step{{Expect: "", Reply: []string{"299 script"}}}})
	c := client.Dial(t, s.Addr())
	if err := c.Send("LIST\r\n"); err != nil {
		t.Fatal(err)
	}
	s.WaitLines(t, []string{"LIST"})
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Errorf("record-only Store mode answered: %v", err)
	}

	// with a store handler: Store wins over Script, Handler wins over Store
	var built int
	storeHandler = func(got *Store, o StoreOptions, f Faults) func(*Conn) {
		built++
		if got != st || o.User != "u" || f.CloseAfter != 7 {
			t.Errorf("store handler built with %p %+v %+v", got, o, f)
		}
		return func(c *Conn) {
			if _, err := c.ReadLine(); err != nil {
				return
			}
			_ = c.WriteLines("298 store")
			c.drain()
		}
	}
	so := Options{Store: st, StoreOpts: StoreOptions{User: "u"}, Faults: Faults{CloseAfter: 7},
		Script: []Step{{Expect: "", Reply: []string{"299 script"}}}}
	ss := New(t, so)
	wantCode(t, client.Dial(t, ss.Addr()), "LIST", 298)
	wantCode(t, client.Dial(t, ss.Addr()), "LIST", 298)
	if built != 1 {
		t.Errorf("store handler built %d times, want once per server", built)
	}

	so.Handler = func(c *Conn) {
		if _, err := c.ReadLine(); err != nil {
			return
		}
		_ = c.WriteLines("297 handler")
		c.drain()
	}
	wantCode(t, client.Dial(t, New(t, so).Addr()), "LIST", 297)

	// NoGreeting without Handler: neither Store nor Script runs
	so.Handler = nil
	so.NoGreeting = true
	ns := New(t, so)
	nc := client.New(t, rawDial(t, ns))
	if err := nc.Send("LIST\r\n"); err != nil {
		t.Fatal(err)
	}
	ns.WaitLines(t, []string{"LIST"})
	if err := nc.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Errorf("NoGreeting Store mode wrote: %v", err)
	}
}
