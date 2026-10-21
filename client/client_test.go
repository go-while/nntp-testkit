package client

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// peer is a local one-connection fixture: the accept goroutine runs handler on the first
// connection and closes it afterwards. Lines the client sent (read by peer.readLine) are
// recorded. Cleanup closes the listener and the connection and waits for the goroutine.
type peer struct {
	ln   net.Listener
	mu   sync.Mutex
	conn net.Conn
	got  []string
	done chan struct{}
}

// newPeer starts the fixture; handler gets the server side and the peer for readLine.
func newPeer(tb testing.TB, handler func(p *peer, c net.Conn, r *bufio.Reader)) *peer {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	p := &peer{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetDeadline(time.Now().Add(testkit.Deadline()))
		p.mu.Lock()
		p.conn = c
		p.mu.Unlock()
		defer func() { _ = c.Close() }()
		handler(p, c, bufio.NewReader(c))
	}()
	tb.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		if p.conn != nil {
			_ = p.conn.Close()
		}
		p.mu.Unlock()
		<-p.done
	})
	return p
}

func (p *peer) addr() string { return p.ln.Addr().String() }

// readLine reads one client line (CRLF stripped) and records it; ok is false on EOF/error.
func (p *peer) readLine(r *bufio.Reader) (string, bool) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", false
	}
	line = strings.TrimRight(line, "\r\n")
	p.mu.Lock()
	p.got = append(p.got, line)
	p.mu.Unlock()
	return line, true
}

// lines returns the recorded client lines after the handler finished.
func (p *peer) lines() []string {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

// write sends raw bytes to the client.
func write(c net.Conn, raw string) {
	_, _ = io.WriteString(c, raw)
}

// drain blocks until the client closes the connection (or the hang guard fires).
func drain(r *bufio.Reader) {
	_, _ = io.Copy(io.Discard, r)
}

// scripted returns a handler that writes raw and then waits for the client to close.
func scripted(raw string) func(*peer, net.Conn, *bufio.Reader) {
	return func(_ *peer, c net.Conn, r *bufio.Reader) {
		write(c, raw)
		drain(r)
	}
}

// scriptedClose returns a handler that writes raw and closes the connection at once.
func scriptedClose(raw string) func(*peer, net.Conn, *bufio.Reader) {
	return func(_ *peer, c net.Conn, _ *bufio.Reader) {
		write(c, raw)
	}
}

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
	r.msg = strings.TrimSpace(fmt.Sprintf(format, args...))
	r.mu.Unlock()
	runtime.Goexit()
}

// run calls fn with r on its own goroutine (Goexit must not end the test goroutine).
func (r *recordTB) run(fn func(tb testing.TB)) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
}

func strictKind(tb testing.TB, err error, want Kind) *StrictError {
	tb.Helper()
	var se *StrictError
	if !errors.As(err, &se) {
		tb.Fatalf("err = %v (%T), want *StrictError %v", err, err, want)
	}
	if se.Kind != want {
		tb.Fatalf("kind = %v, want %v (err %v)", se.Kind, want, err)
	}
	if se.Error() == "" || !strings.HasPrefix(se.Error(), "strict: ") {
		tb.Fatalf("Error() = %q", se.Error())
	}
	return se
}

func TestDialGreeting(t *testing.T) {
	p := newPeer(t, func(p *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 hello posting ok\r\n")
		if line, ok := p.readLine(r); ok && line == "QUIT" {
			write(c, "205 bye\r\n")
		}
	})
	c := Dial(t, p.addr())
	if c.Greeting != "200 hello posting ok" {
		t.Fatalf("greeting = %q", c.Greeting)
	}
	if !c.Strict {
		t.Fatal("Dial must set Strict")
	}
	code, line := c.Cmd("QUIT")
	if code != 205 || line != "205 bye" {
		t.Fatalf("QUIT = %d %q", code, line)
	}
	c.Close()
	c.Close() // idempotent
	if err := ExpectLines(p.lines(), []string{"QUIT"}); err != nil {
		t.Fatal(err)
	}
}

func TestTryDialAndStatus(t *testing.T) {
	p := newPeer(t, func(p *peer, c net.Conn, r *bufio.Reader) {
		write(c, "201 no posting\r\n")
		for {
			line, ok := p.readLine(r)
			if !ok {
				return
			}
			switch line {
			case "GROUP misc.test":
				write(c, "211 3 1 3 misc.test\r\n")
			case "GARBAGE":
				write(c, "xyz\r\n")
			case "NOSPACE":
				write(c, "200x\r\n")
			}
		}
	})
	c, err := TryDial(p.addr(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Greeting != "201 no posting" || !c.Strict {
		t.Fatalf("greeting %q strict %v", c.Greeting, c.Strict)
	}
	code, line, err := c.TryCmd("GROUP %s", "misc.test")
	if err != nil || code != 211 || line != "211 3 1 3 misc.test" {
		t.Fatalf("GROUP = %d %q %v", code, line, err)
	}
	for _, cmd := range []string{"GARBAGE", "NOSPACE"} {
		code, line, err = c.TryCmd("%s", cmd)
		if err == nil || code != 0 || line == "" {
			t.Fatalf("%s = %d %q %v, want a parse error with the line", cmd, code, line, err)
		}
	}
	c.Close()
	if err := ExpectLines(p.lines(), []string{"GROUP misc.test", "GARBAGE", "NOSPACE"}); err != nil {
		t.Fatal(err)
	}
}

func TestSendPipelining(t *testing.T) {
	p := newPeer(t, func(p *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 hi\r\n")
		for i := 0; i < 2; i++ {
			if _, ok := p.readLine(r); !ok {
				return
			}
			write(c, "223 ok\r\n")
		}
		drain(r)
	})
	c := Dial(t, p.addr())
	if err := c.Send("STAT 1\r\nSTAT 2\r\n"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		code, _, err := c.ReadStatus()
		if err != nil || code != 223 {
			t.Fatalf("status %d: %d %v", i, code, err)
		}
	}
	c.Close()
	if err := ExpectLines(p.lines(), []string{"STAT 1", "STAT 2"}); err != nil {
		t.Fatal(err)
	}
}

func TestReadDotLines(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n220 1 <a@b> article\r\nHeader: x\r\n\r\n..dot line\r\n...two\r\nplain\r\n.\r\n"+
		"100 help\r\n.\r\n"))
	c := Dial(t, p.addr())
	code, _, err := c.ReadStatus()
	if err != nil || code != 220 {
		t.Fatalf("status %d %v", code, err)
	}
	got := c.ReadDotLines()
	if err := ExpectLines(got, []string{"Header: x", "", ".dot line", "..two", "plain"}); err != nil {
		t.Fatal(err)
	}
	if code, _, err = c.ReadStatus(); err != nil || code != 100 {
		t.Fatalf("status %d %v", code, err)
	}
	if got := c.ReadDotLines(); len(got) != 0 {
		t.Fatalf("empty block = %q", got)
	}
}

func TestRawUntilDot(t *testing.T) {
	block := "Subject: s\r\n\r\n..stuffed\r\nbody\r\n.\r\n"
	p := newPeer(t, scripted("200 hi\r\n"+block+".\r\n"))
	c := Dial(t, p.addr())
	if got := c.RawUntilDot(); string(got) != block {
		t.Fatalf("raw = %q, want %q", got, block)
	}
	if got := c.RawUntilDot(); string(got) != ".\r\n" {
		t.Fatalf("empty raw = %q", got)
	}
}

func TestNew(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = b.Close() }()
		_ = b.SetDeadline(time.Now().Add(testkit.Deadline()))
		write(b, "200 pipe\r\n")
		r := bufio.NewReader(b)
		if line, err := r.ReadString('\n'); err == nil && line == "DATE\r\n" {
			write(b, "111 20261017000000\r\n")
		}
	}()
	t.Cleanup(func() { <-done })
	c := New(t, a)
	if c.Greeting != "" || !c.Strict {
		t.Fatalf("New: greeting %q strict %v", c.Greeting, c.Strict)
	}
	g, err := c.ReadGreeting()
	if err != nil || g != "200 pipe" || c.Greeting != g {
		t.Fatalf("ReadGreeting = %q %v (field %q)", g, err, c.Greeting)
	}
	if code, _ := c.Cmd("DATE"); code != 111 {
		t.Fatalf("DATE = %d", code)
	}
}

func TestParseCode(t *testing.T) {
	cases := []struct {
		line string
		code int
		ok   bool
	}{
		{"", 0, false},
		{"20", 0, false},
		{"200", 200, true},
		{"200 ok", 200, true},
		{"100 help", 100, true},
		{"599 x", 599, true},
		{"099 x", 0, false},
		{"600 x", 0, false},
		{"abc def", 0, false},
		{"200x", 0, false},
		{"200\tx", 0, false},
		{"2000 x", 0, false},
		{"-20 x", 0, false},
	}
	for _, tc := range cases {
		code, err := ParseCode(tc.line)
		if (err == nil) != tc.ok || code != tc.code {
			t.Errorf("ParseCode(%q) = %d, %v; want %d ok=%v", tc.line, code, err, tc.code, tc.ok)
		}
	}
}

func TestExpectCode(t *testing.T) {
	if err := ExpectCode(211, "211 x", 211); err != nil {
		t.Fatal(err)
	}
	if err := ExpectCode(411, "411 x", 211, 411); err != nil {
		t.Fatal(err)
	}
	err := ExpectCode(500, "500 what", 211, 411)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "500 what") {
		t.Fatalf("err = %v", err)
	}
	if err := ExpectCode(200, "200"); err == nil {
		t.Fatal("no want must fail")
	}
}

func TestExpectLines(t *testing.T) {
	if err := ExpectLines(nil, []string{}); err != nil {
		t.Fatal(err)
	}
	if err := ExpectLines([]string{"a", "b"}, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := ExpectLines([]string{"a"}, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "got 1 lines, want 2") {
		t.Fatalf("length: %v", err)
	}
	if err := ExpectLines([]string{"a", "x"}, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("diff: %v", err)
	}
}

func TestUnstuff(t *testing.T) {
	wire := []string{"..", "...x", ".y", "z", ""}
	got := Unstuff(wire)
	if err := ExpectLines(got, []string{".", "..x", ".y", "z", ""}); err != nil {
		t.Fatal(err)
	}
	if wire[0] != ".." {
		t.Fatal("Unstuff modified its input")
	}
	if got := Unstuff(nil); len(got) != 0 {
		t.Fatalf("nil = %q", got)
	}
}

func TestStrictErrors(t *testing.T) {
	cases := []struct {
		name   string
		script string
		close  bool // the server closes after the script
		op     string
		want   Kind
		line   string // wanted StrictError.Line, when set
	}{
		{"bare LF", "200 hi\r\n211 x\n", false, "line", BareLF, "211 x\n"},
		{"empty line bare LF", "200 hi\r\n\n", false, "line", BareLF, "\n"},
		{"bare CR", "200 hi\r\n211 a\rb\r\n", false, "line", BareCR, "211 a\rb\r\n"},
		{"double CR", "200 hi\r\n211 a\r\r\n", false, "line", BareCR, "211 a\r\r\n"},
		{"no CRLF at EOF", "200 hi\r\n211 partial", true, "line", NoCRLF, "211 partial"},
		{"bad stuffing", "200 hi\r\nx\r\n.bad\r\n.\r\n", false, "dot", BadDotStuffing, ".bad\r\n"},
		{"bare LF in block", "200 hi\r\nx\r\ny\n.\r\n", false, "dot", BareLF, "y\n"},
		{"LF terminator", "200 hi\r\nx\r\n.\n", false, "dot", BareLF, ".\n"},
		{"missing terminator", "200 hi\r\na\r\nb\r\n", true, "dot", NoTerminator, ""},
		{"partial terminator", "200 hi\r\na\r\n.", true, "dot", NoTerminator, "."},
		{"raw bare LF", "200 hi\r\na\r\nb\n.\r\n", false, "raw", BareLF, "b\n"},
		{"raw missing terminator", "200 hi\r\na\r\n", true, "raw", NoTerminator, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := scripted(tc.script)
			if tc.close {
				h = scriptedClose(tc.script)
			}
			p := newPeer(t, h)
			c := Dial(t, p.addr())
			var err error
			switch tc.op {
			case "line":
				_, err = c.ReadLine()
			case "dot":
				_, err = c.TryReadDotLines()
			case "raw":
				_, err = c.tryRawUntilDot()
			}
			se := strictKind(t, err, tc.want)
			if tc.line != "" && se.Line != tc.line {
				t.Fatalf("Line = %q, want %q", se.Line, tc.line)
			}
			if tc.close && !errors.Is(err, io.EOF) {
				t.Fatalf("err %v must wrap io.EOF", err)
			}
		})
	}
}

func TestStrictPending(t *testing.T) {
	p := newPeer(t, scripted("400 too many users\r\n\x00"))
	c, err := TryDial(p.addr(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Greeting != "400 too many users" {
		t.Fatalf("greeting = %q", c.Greeting)
	}
	// The NUL arrives (or is buffered already): ExpectNoPending returns at once with it.
	se := strictKind(t, c.ExpectNoPending(testkit.Deadline()), Pending)
	if !bytes.Equal(se.Bytes, []byte{0}) {
		t.Fatalf("Bytes = %q", se.Bytes)
	}
	if !strings.Contains(se.Error(), `\x00`) {
		t.Fatalf("Error() = %q", se.Error())
	}
}

// TestPendingArrivesDuringWait: nothing is buffered when ExpectNoPending starts; the bytes
// arrive while it waits, are reported and stay readable.
func TestPendingArrivesDuringWait(t *testing.T) {
	gate := make(chan struct{})
	p := newPeer(t, func(_ *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 hi\r\n")
		<-gate
		write(c, "PENDING\r\n")
		drain(r)
	})
	c := Dial(t, p.addr())
	if n := c.R.Buffered(); n != 0 {
		t.Fatalf("buffered before the wait: %d", n)
	}
	close(gate)
	se := strictKind(t, c.ExpectNoPending(testkit.Deadline()), Pending)
	if len(se.Bytes) == 0 || !strings.HasPrefix("PENDING\r\n", string(se.Bytes)) {
		t.Fatalf("Bytes = %q", se.Bytes)
	}
	if line, err := c.ReadLine(); err != nil || line != "PENDING" {
		t.Fatalf("after Pending: %q %v (bytes consumed?)", line, err)
	}
}

// TestPendingAlreadyBuffered: the bytes came in with the greeting (one pipe write); they are
// reported without a wait and stay readable.
func TestPendingAlreadyBuffered(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = b.Close() }()
		_ = b.SetDeadline(time.Now().Add(testkit.Deadline()))
		write(b, "200 hi\r\nPENDING\r\n")
		drain(bufio.NewReader(b))
	}()
	t.Cleanup(func() { <-done }) // registered first: runs after New's Close (LIFO)
	c := New(t, a)
	if _, err := c.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	if n := c.R.Buffered(); n != len("PENDING\r\n") {
		t.Fatalf("buffered = %d, want the pending line", n)
	}
	se := strictKind(t, c.ExpectNoPending(testkit.Deadline()), Pending)
	if string(se.Bytes) != "PENDING\r\n" {
		t.Fatalf("Bytes = %q", se.Bytes)
	}
	if line, err := c.ReadLine(); err != nil || line != "PENDING" {
		t.Fatalf("after Pending: %q %v (bytes consumed?)", line, err)
	}
}

// TestExpectNoPendingRestoresDeadline: after a quiet probe a direct read on R still works.
func TestExpectNoPendingRestoresDeadline(t *testing.T) {
	gate := make(chan struct{})
	p := newPeer(t, func(_ *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 hi\r\n")
		<-gate
		write(c, "211 later\r\n")
		drain(r)
	})
	c := Dial(t, p.addr())
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("quiet: %v", err)
	}
	close(gate)
	line, err := c.R.ReadBytes('\n')
	if err != nil || string(line) != "211 later\r\n" {
		t.Fatalf("direct read after the probe: %q %v", line, err)
	}
}

func TestDialWith(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n"))
	c := DialWith(t, p.addr(), 200*time.Millisecond)
	if c.Deadline != 200*time.Millisecond || !c.Strict || c.Greeting != "200 hi" {
		t.Fatalf("DialWith: deadline %v strict %v greeting %q", c.Deadline, c.Strict, c.Greeting)
	}
	if _, _, err := c.TryCmd("DATE"); !isTimeout(err) {
		t.Fatalf("err = %v, want a timeout from the DialWith deadline", err)
	}

	p = newPeer(t, func(_ *peer, _ net.Conn, r *bufio.Reader) { drain(r) })
	r := &recordTB{TB: t}
	r.run(func(tb testing.TB) { DialWith(tb, p.addr(), 200*time.Millisecond) })
	if !r.failed || !strings.Contains(r.msg, "greeting") {
		t.Fatalf("silent peer: failed=%v msg=%q", r.failed, r.msg)
	}
}

func TestWithTB(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n211 x\n"))
	c, err := TryDial(p.addr(), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := &recordTB{TB: t}
	if got := c.WithTB(r); got != c {
		t.Fatal("WithTB must return c")
	}
	r.run(func(testing.TB) { c.Cmd("GROUP x") })
	if !r.failed || !strings.Contains(r.msg, "bare LF") {
		t.Fatalf("Cmd: failed=%v msg=%q", r.failed, r.msg)
	}
}

// TestFor: a view bound to another tb fails that tb, shares the stream with c and registers no
// cleanup (c is still open after the view's tb finished).
func TestFor(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n211 x\n205 bye\r\n"))
	c := Dial(t, p.addr())
	r := &recordTB{TB: t}
	r.run(func(testing.TB) { c.For(r).Cmd("GROUP x") })
	if !r.failed || !strings.Contains(r.msg, "bare LF") {
		t.Fatalf("view Cmd: failed=%v msg=%q", r.failed, r.msg)
	}
	// The stream position is shared: the next line on c is the one after the bad one.
	if code, line := c.Cmd("QUIT"); code != 205 || line != "205 bye" {
		t.Fatalf("after the view: %d %q", code, line)
	}
}

// TestTryRawUntilDot: the error-returning form of RawUntilDot.
func TestTryRawUntilDot(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\nline\r\n..x\r\n.\r\n"))
	c := Dial(t, p.addr())
	b, err := c.TryRawUntilDot()
	if err != nil || string(b) != "line\r\n..x\r\n.\r\n" {
		t.Fatalf("TryRawUntilDot = %q, %v", b, err)
	}
}

// TestLongLines: lines beyond the 1 MiB reader buffer in strict mode.
func TestLongLines(t *testing.T) {
	long := strings.Repeat("x", 2<<20)
	p := newPeer(t, scripted("200 hi\r\n"+long+"\r\n"+long+"\n"+"211 after\r\n"))
	c := Dial(t, p.addr())
	if line, err := c.ReadLine(); err != nil || line != long {
		t.Fatalf("CRLF line: len %d err %v", len(line), err)
	}
	_, err := c.ReadLine()
	se := strictKind(t, err, BareLF)
	if se.Line != long+"\n" {
		t.Fatalf("BareLF Line len %d, want %d", len(se.Line), len(long)+1)
	}
	if code, _, err := c.ReadStatus(); err != nil || code != 211 {
		t.Fatalf("after the long lines: %d %v", code, err)
	}
}

func TestNonStrictAcceptsLF(t *testing.T) {
	p := newPeer(t, scripted("200 hi\n"))
	c, err := TryDial(p.addr(), 0)
	if err == nil {
		c.Close()
		t.Fatal("strict TryDial must reject a bare LF greeting")
	}
	strictKind(t, err, BareLF)

	p = newPeer(t, scripted("200 hi\n211 ok\r\na\n..b\n.c\n.\n"+"x\r\n.\n"))
	nc, err := net.Dial("tcp", p.addr())
	if err != nil {
		t.Fatal(err)
	}
	c = New(t, nc)
	c.Strict = false
	if g, err := c.ReadGreeting(); err != nil || g != "200 hi" {
		t.Fatalf("greeting %q %v", g, err)
	}
	if code, _, err := c.ReadStatus(); err != nil || code != 211 {
		t.Fatalf("status %d %v", code, err)
	}
	got, err := c.TryReadDotLines()
	if err != nil {
		t.Fatal(err)
	}
	if err := ExpectLines(got, []string{"a", ".b", ".c"}); err != nil {
		t.Fatal(err)
	}
	if raw := c.RawUntilDot(); string(raw) != "x\r\n.\n" {
		t.Fatalf("raw = %q", raw)
	}
}

func TestNonStrictMissingTerminator(t *testing.T) {
	p := newPeer(t, scriptedClose("200 hi\r\na\r\n"))
	c := Dial(t, p.addr())
	c.Strict = false
	got, err := c.TryReadDotLines()
	var se *StrictError
	if err == nil || errors.As(err, &se) || !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want a plain EOF error", err)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("partial = %q", got)
	}
}

func TestExpectNoPendingQuiet(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n"))
	c := Dial(t, p.addr())
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("quiet: %v", err)
	}
	// The connection is still usable after the probe.
	if err := c.Send("QUIT\r\n"); err != nil {
		t.Fatal(err)
	}
}

func TestExpectNoPendingClosed(t *testing.T) {
	p := newPeer(t, scriptedClose("400 bye\r\n"))
	c := Dial(t, p.addr())
	if err := c.ExpectNoPending(testkit.Deadline()); err != nil {
		t.Fatalf("closed: %v", err)
	}
}

// isTimeout reports whether err is a net timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestDeadlineSilentPeer(t *testing.T) {
	p := newPeer(t, func(_ *peer, _ net.Conn, r *bufio.Reader) { drain(r) })
	c, err := TryDial(p.addr(), 200*time.Millisecond)
	if err == nil {
		c.Close()
		t.Fatal("TryDial on a silent peer must fail")
	}
	if !isTimeout(err) || !strings.Contains(err.Error(), "greeting") {
		t.Fatalf("err = %v, want a greeting timeout", err)
	}
}

func TestDeadlineNoTerminator(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\na\r\n"))
	c := Dial(t, p.addr())
	c.Deadline = 200 * time.Millisecond
	got, err := c.TryReadDotLines()
	strictKind(t, err, NoTerminator)
	if !isTimeout(err) {
		t.Fatalf("err = %v, want it to wrap a timeout", err)
	}
	if len(got) != 1 {
		t.Fatalf("partial = %q", got)
	}
}

func TestDeadlineCmd(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n"))
	c := Dial(t, p.addr())
	c.Deadline = 200 * time.Millisecond
	_, _, err := c.TryCmd("DATE")
	if !isTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

func TestFailingMethodsFailTB(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n211 x\n.bad\r\n.\r\n"))
	c := Dial(t, p.addr())
	r := &recordTB{TB: t}
	c.tb = r
	r.run(func(testing.TB) { c.Cmd("GROUP x") })
	if !r.failed || !strings.Contains(r.msg, "bare LF") {
		t.Fatalf("Cmd: failed=%v msg=%q", r.failed, r.msg)
	}
	r.failed, r.msg = false, ""
	r.run(func(testing.TB) { c.ReadDotLines() })
	if !r.failed || !strings.Contains(r.msg, "not dot-stuffed") {
		t.Fatalf("ReadDotLines: failed=%v msg=%q", r.failed, r.msg)
	}
}

func TestDialFailsTB(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens: the dial is refused at once
	r := &recordTB{TB: t}
	r.run(func(tb testing.TB) { Dial(tb, addr) })
	if !r.failed || !strings.Contains(r.msg, "dial "+addr) {
		t.Fatalf("failed=%v msg=%q", r.failed, r.msg)
	}
}

func TestTryDialConnPanicsOnFailingMethod(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n211 x\n"))
	c, err := TryDial(p.addr(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer func() {
		if v := recover(); v == nil || !strings.Contains(fmt.Sprintf("%v", v), "bare LF") {
			t.Fatalf("recover = %v", v)
		}
	}()
	c.Cmd("GROUP x")
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{BareLF: "BareLF", BareCR: "BareCR", NoCRLF: "NoCRLF",
		BadDotStuffing: "BadDotStuffing", NoTerminator: "NoTerminator", Pending: "Pending", Kind(99): "Kind(99)"} {
		if k.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(k), k.String(), want)
		}
	}
}
