package fakeserver

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/tlscert"
)

// Byte-exact tests of Faults.Split, DropAfterBytes, Abort, OverrideFinal, DelayFinal,
// DropInUpload and Trailing, with a raw net.Conn client: the pieces of a reply are separate
// reads (the gap between them is far above any scheduling jitter on loopback).

const (
	dateReply = "111 12345678901234\r\n" // the Override of DATE in these tests: 20 bytes
	testGap   = 300 * time.Millisecond
)

func dateServer(t *testing.T, f Faults) *Server {
	t.Helper()
	if f.Override == nil {
		f.Override = map[string][]string{}
	}
	f.Override["DATE"] = []string{"111 12345678901234"}
	return New(t, Options{Store: testStore(), Faults: f, Deadline: 10 * time.Second})
}

// rawDial connects and reads the greeting byte by byte (so that no later byte is consumed).
func bytesDial(t *testing.T, s *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	readGreeting(t, c)
	return c
}

func readGreeting(t *testing.T, c io.Reader) {
	t.Helper()
	var line []byte
	b := make([]byte, 1)
	for !strings.HasSuffix(string(line), "\r\n") {
		if _, err := io.ReadFull(c, b); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		line = append(line, b[0])
	}
}

func rawSend(t *testing.T, c net.Conn, s string) {
	t.Helper()
	if _, err := io.WriteString(c, s); err != nil {
		t.Fatal(err)
	}
}

// rawRead is one Read of at most 4096 bytes.
func rawRead(t *testing.T, c net.Conn) string {
	t.Helper()
	b := make([]byte, 4096)
	n, err := c.Read(b)
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, b[:n])
	}
	return string(b[:n])
}

// readQuiet reports whether nothing arrives within d (a read timeout).
func readQuiet(t *testing.T, c net.Conn, d time.Duration) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Now().Add(20 * time.Second))
	n, err := c.Read(make([]byte, 1))
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if n > 0 {
		t.Fatalf("unexpected data after %v", d)
	}
	t.Fatalf("read: %v", err)
	return false
}

// readAll reads until an error and returns the bytes and that error.
func readAll(c net.Conn) (string, error) {
	var sb strings.Builder
	b := make([]byte, 4096)
	for {
		n, err := c.Read(b)
		sb.Write(b[:n])
		if err != nil {
			return sb.String(), err
		}
	}
}

func TestFaultSplitAt(t *testing.T) {
	// cut inside the status line, and between CR and LF
	s := dateServer(t, Faults{Split: map[string]Split{"DATE": {At: []int{3, 99, 19, 3, -1, 0}, Gap: testGap}}})
	c := bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	start := time.Now()
	if got := rawRead(t, c); got != "111" {
		t.Fatalf("first piece %q", got)
	}
	if !readQuiet(t, c, testGap/3) {
		t.Fatal("not quiet")
	}
	if got := rawRead(t, c); got != " 12345678901234\r" {
		t.Fatalf("second piece %q", got)
	}
	if got := rawRead(t, c); got != "\n" {
		t.Fatalf("third piece %q", got)
	}
	if took := time.Since(start); took < 2*testGap-testGap/4 {
		t.Errorf("pieces arrived in %v, want two gaps", took)
	}
	// the next reply is normal
	rawSend(t, c, "MODE READER\r\n")
	if got := rawRead(t, c); !strings.HasPrefix(got, "200 ") {
		t.Errorf("MODE READER %q", got)
	}
}

func TestFaultSplitSize(t *testing.T) {
	s := dateServer(t, Faults{Split: map[string]Split{"DATE": {Size: 7, Gap: 100 * time.Millisecond}}})
	c := bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	for i, want := range []string{dateReply[:7], dateReply[7:14], dateReply[14:]} {
		if got := rawRead(t, c); got != want {
			t.Fatalf("piece %d = %q, want %q", i, got, want)
		}
	}
}

// Split offsets count over the whole reply, across the lines of a LineDelay write.
func TestFaultSplitLineDelay(t *testing.T) {
	s := New(t, Options{Store: testStore(), Deadline: 10 * time.Second, Faults: Faults{
		Override:  map[string][]string{"HELP": {"100 ab", "cd", "."}}, // 8 + 4 + 3 bytes
		LineDelay: map[string]time.Duration{"HELP": 50 * time.Millisecond},
		Split:     map[string]Split{"HELP": {At: []int{10}, Gap: 100 * time.Millisecond}},
	}})
	c := bytesDial(t, s)
	rawSend(t, c, "HELP\r\n")
	want := []string{"100 ab\r\n", "cd", "\r\n", ".\r\n"}
	for i, w := range want {
		if got := rawRead(t, c); got != w {
			t.Fatalf("piece %d = %q, want %q", i, got, w)
		}
	}
}

func TestFaultDropAfterBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, ""}, {1, "1"}, {5, "111 1"}, {19, dateReply[:19]}} { // 19: before the final LF
		s := dateServer(t, Faults{DropAfterBytes: map[string]int{"DATE": tc.n}})
		c := bytesDial(t, s)
		rawSend(t, c, "DATE\r\n")
		got, err := readAll(c)
		if got != tc.want || !errors.Is(err, io.EOF) {
			t.Errorf("DropAfterBytes %d: got %q, %v; want %q, EOF", tc.n, got, err, tc.want)
		}
		s.WaitOpen(t, 0, 5*time.Second)
	}
	// a reply of that length or shorter is written whole, and the connection goes on
	s := dateServer(t, Faults{DropAfterBytes: map[string]int{"DATE": 20}})
	c := bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	if got := rawRead(t, c); got != dateReply {
		t.Errorf("whole reply %q", got)
	}
	rawSend(t, c, "MODE READER\r\n")
	if got := rawRead(t, c); !strings.HasPrefix(got, "200 ") {
		t.Errorf("after the whole reply: %q", got)
	}
}

func TestFaultDropAfterBytesMultiline(t *testing.T) {
	s := New(t, Options{Store: testStore(), Deadline: 10 * time.Second, Faults: Faults{
		Override:       map[string][]string{"HELP": {"100 help", "line one", "line two", "."}},
		DropAfterBytes: map[string]int{"HELP": 17},
	}})
	c := bytesDial(t, s)
	rawSend(t, c, "HELP\r\n")
	got, err := readAll(c)
	if want := "100 help\r\nline on"; got != want || !errors.Is(err, io.EOF) {
		t.Errorf("got %q, %v; want %q, EOF", got, err, want)
	}
}

// Abort: a close by a fault of that command is a reset on plain TCP.
func TestFaultAbortPlain(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Faults
		cmd  string
		pre  string // what may arrive before the reset
	}{
		{"DropAfterBytes", Faults{DropAfterBytes: map[string]int{"DATE": 5}, Abort: map[string]bool{"DATE": true}}, "DATE", "111 1"},
		{"DropInMultiline", Faults{Override: map[string][]string{"HELP": {"100 help", "line", "."}}, DropInMultiline: map[string]int{"HELP": 1}, Abort: map[string]bool{"HELP": true}}, "HELP", "100 help\r\n"},
		{"CloseAfter", Faults{CloseAfter: 1, Abort: map[string]bool{"DATE": true}}, "DATE", dateReply},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dateServer(t, tc.f)
			c := bytesDial(t, s)
			rawSend(t, c, tc.cmd+"\r\n")
			got, err := readAll(c)
			if errors.Is(err, io.EOF) {
				t.Fatalf("clean EOF after %q, want a reset", got)
			}
			if !strings.HasPrefix(tc.pre, got) {
				t.Errorf("got %q, want a prefix of %q", got, tc.pre)
			}
			if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
				if !errors.Is(err, syscall.ECONNRESET) {
					t.Errorf("err %v, want ECONNRESET", err)
				}
			}
			s.WaitOpen(t, 0, 5*time.Second)
		})
	}
	// Abort of another command does not change a close by this one
	s := dateServer(t, Faults{DropAfterBytes: map[string]int{"DATE": 5}, Abort: map[string]bool{"HELP": true}})
	c := bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	if got, err := readAll(c); got != "111 1" || !errors.Is(err, io.EOF) {
		t.Errorf("without Abort: %q, %v", got, err)
	}
	// and a clean end of the connection of an aborted command is not a reset
	s = dateServer(t, Faults{DropAfterBytes: map[string]int{"DATE": 5}, Abort: map[string]bool{"DATE": true}})
	c = bytesDial(t, s)
	rawSend(t, c, "MODE READER\r\n")
	rawRead(t, c)
	rawSend(t, c, "QUIT\r\n")
	if _, err := readAll(c); !errors.Is(err, io.EOF) {
		t.Errorf("QUIT: %v, want EOF", err)
	}
}

type countConn struct {
	net.Conn
	n atomic.Int64
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// tlsDrop runs DATE against an implicit-TLS server that cuts the reply after 5 bytes and
// returns what the client read, its error and the TLS bytes that came after the handshake.
func tlsDrop(t *testing.T, abort bool) (string, error, int64) {
	t.Helper()
	ca := tlscert.NewCA(t)
	f := Faults{DropAfterBytes: map[string]int{"DATE": 5}}
	if abort {
		f.Abort = map[string]bool{"DATE": true}
	}
	f.Override = map[string][]string{"DATE": {"111 12345678901234"}}
	s := New(t, Options{Store: testStore(), Faults: f, Deadline: 10 * time.Second,
		TLS: ca.Issue(t, tlscert.Options{}).ServerConfig()})
	raw, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	cc := &countConn{Conn: raw}
	c := tls.Client(cc, ca.ClientConfig("127.0.0.1"))
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	mark := cc.n.Load()
	readGreeting(t, c)
	rawSend(t, c, "DATE\r\n")
	var sb strings.Builder
	b := make([]byte, 100)
	for {
		n, err := c.Read(b)
		sb.Write(b[:n])
		if err != nil {
			return sb.String(), err, cc.n.Load() - mark
		}
	}
}

// Abort on TLS closes the TCP connection under the TLS layer: the server sends no
// close_notify. A Go client reads io.EOF at a record boundary either way, so the test compares
// the bytes that arrive: a normal close sends the alert record on top.
func TestFaultAbortTLS(t *testing.T) {
	got, err, normal := tlsDrop(t, false)
	if got != "111 1" || !errors.Is(err, io.EOF) {
		t.Fatalf("normal drop: %q, %v", got, err)
	}
	got, err, aborted := tlsDrop(t, true)
	if got != "111 1" || err == nil {
		t.Fatalf("aborted: %q, %v", got, err)
	}
	if normal-aborted < 20 { // the close_notify record: 23 bytes in TLS 1.3, more in 1.2
		t.Errorf("bytes after the handshake: normal %d, aborted %d, want the alert (>= 20) on top for the normal close", normal, aborted)
	}
}

func postArticle() string {
	return "Newsgroups: alt.test\r\nSubject: s\r\nFrom: f@x\r\n\r\nbody\r\n.\r\n"
}

func TestFaultFinalPost(t *testing.T) {
	st := testStore()
	s := New(t, Options{Store: st, Deadline: 10 * time.Second, Faults: Faults{
		OverrideFinal: map[string][]string{"POST": {"441 not today"}},
		DelayFinal:    map[string]time.Duration{"POST": testGap},
	}})
	c := bytesDial(t, s)
	rawSend(t, c, "POST\r\n")
	if got := rawRead(t, c); got != "340 Send article to be posted\r\n" { // the first reply is untouched, at once
		t.Fatalf("POST: %q", got)
	}
	rawSend(t, c, postArticle())
	start := time.Now()
	if got := rawRead(t, c); got != "441 not today\r\n" {
		t.Fatalf("final reply %q", got)
	}
	if took := time.Since(start); took < testGap-testGap/4 {
		t.Errorf("final reply after %v, want the delay", took)
	}
	if len(st.Posted()) != 1 || len(s.Articles()) != 1 { // the article is handled as usual
		t.Errorf("posted %d, articles %d", len(st.Posted()), len(s.Articles()))
	}
	// the connection goes on
	rawSend(t, c, "DATE\r\n")
	if got := rawRead(t, c); !strings.HasPrefix(got, "111 ") {
		t.Errorf("DATE %q", got)
	}
}

// OverrideFinal with no lines sends no final reply; Split, DropAfterBytes and Abort apply to it.
func TestFaultFinalBytes(t *testing.T) {
	long := "240 " + strings.Repeat("x", 56) // 60 bytes + CRLF
	s := New(t, Options{Store: testStore(), Deadline: 10 * time.Second, Faults: Faults{
		OverrideFinal:  map[string][]string{"POST": {long}},
		DropAfterBytes: map[string]int{"POST": 40}, // the 340 is 31 bytes: whole
		Abort:          map[string]bool{"POST": true},
	}})
	c := bytesDial(t, s)
	rawSend(t, c, "POST\r\n")
	if got := rawRead(t, c); !strings.HasPrefix(got, "340 ") {
		t.Fatalf("POST: %q", got)
	}
	rawSend(t, c, postArticle())
	got, err := readAll(c)
	if got != long[:40] && !strings.HasPrefix(long[:40], got) {
		t.Errorf("cut final reply %q", got)
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("clean EOF after %q, want a reset", got)
	}

	s = New(t, Options{Store: testStore(), Deadline: 10 * time.Second, Faults: Faults{
		OverrideFinal: map[string][]string{"POST": {}},
	}})
	c = bytesDial(t, s)
	rawSend(t, c, "POST\r\n")
	rawRead(t, c)
	rawSend(t, c, postArticle())
	rawSend(t, c, "DATE\r\n")
	if got := rawRead(t, c); !strings.HasPrefix(got, "111 ") { // no 240 in between
		t.Errorf("after an empty final reply: %q", got)
	}

	// Split["POST"] cuts the 340 and the final reply alike
	s = New(t, Options{Store: testStore(), Deadline: 10 * time.Second, Faults: Faults{
		OverrideFinal: map[string][]string{"POST": {"240 12345678"}},
		Split:         map[string]Split{"POST": {At: []int{7}, Gap: 100 * time.Millisecond}},
	}})
	c = bytesDial(t, s)
	rawSend(t, c, "POST\r\n")
	for _, want := range []string{"340 Sen", "d article to be posted\r\n"} {
		if got := rawRead(t, c); got != want {
			t.Fatalf("340 piece %q, want %q", got, want)
		}
	}
	rawSend(t, c, postArticle())
	for _, want := range []string{"240 123", "45678\r\n"} {
		if got := rawRead(t, c); got != want {
			t.Fatalf("final piece %q, want %q", got, want)
		}
	}
}

func TestFaultFinalPeer(t *testing.T) {
	s := NewPeer(t, Options{Deadline: 10 * time.Second, Faults: Faults{
		OverrideFinal: map[string][]string{"IHAVE": {"436 later"}, "TAKETHIS": {"439 <t@x>"}},
		DelayFinal:    map[string]time.Duration{"IHAVE": testGap},
	}}, &PeerOptions{})
	c := bytesDial(t, s)
	rawSend(t, c, "IHAVE <i@x>\r\n")
	if got := rawRead(t, c); got != "335 Send it\r\n" {
		t.Fatalf("IHAVE: %q", got)
	}
	rawSend(t, c, "Subject: s\r\n\r\nbody\r\n.\r\n")
	start := time.Now()
	if got := rawRead(t, c); got != "436 later\r\n" {
		t.Fatalf("IHAVE final %q", got)
	}
	if took := time.Since(start); took < testGap-testGap/4 {
		t.Errorf("final reply after %v, want the delay", took)
	}
	rawSend(t, c, "TAKETHIS <t@x>\r\nSubject: s\r\n\r\nbody\r\n.\r\n")
	if got := rawRead(t, c); got != "439 <t@x>\r\n" {
		t.Errorf("TAKETHIS %q", got)
	}
	if len(s.Articles()) != 2 {
		t.Errorf("articles read: %d, want 2", len(s.Articles()))
	}

	// byte-level faults apply to the 235 as to the first reply
	s = NewPeer(t, Options{Deadline: 10 * time.Second, Faults: Faults{
		DropAfterBytes: map[string]int{"IHAVE": 10}, // the 335 is 14 bytes: cut, never reaches the 235
	}}, &PeerOptions{})
	c = bytesDial(t, s)
	rawSend(t, c, "IHAVE <i@x>\r\n")
	if got, err := readAll(c); got != "335 Send i" || !errors.Is(err, io.EOF) {
		t.Errorf("IHAVE cut: %q, %v", got, err)
	}
	s = NewPeer(t, Options{Deadline: 10 * time.Second, Faults: Faults{
		DropAfterBytes: map[string]int{"IHAVE": 20}, // the 335 is whole; the 235 (28 bytes) is cut
	}}, &PeerOptions{})
	c = bytesDial(t, s)
	rawSend(t, c, "IHAVE <i@x>\r\n")
	if got := rawRead(t, c); got != "335 Send it\r\n" {
		t.Fatalf("IHAVE: %q", got)
	}
	rawSend(t, c, "Subject: s\r\n\r\nbody\r\n.\r\n")
	if got, err := readAll(c); got != "235 Article transfer" || !errors.Is(err, io.EOF) {
		t.Errorf("235 cut: %q, %v", got, err)
	}
}

func TestFaultDropInUpload(t *testing.T) {
	for _, abort := range []bool{false, true} {
		f := Faults{DropInUpload: map[string]int{"POST": 3}}
		if abort {
			f.Abort = map[string]bool{"POST": true}
		}
		s := New(t, Options{Store: testStore(), Deadline: 10 * time.Second, Faults: f})
		c := bytesDial(t, s)
		rawSend(t, c, "POST\r\n")
		rawRead(t, c)
		rawSend(t, c, "Newsgroups: alt.test\r\nSubject: s\r\n")
		if !readQuiet(t, c, 200*time.Millisecond) { // two lines read: the server waits for more
			t.Fatal("closed early")
		}
		rawSend(t, c, "From: f@x\r\n") // the third line
		got, err := readAll(c)
		switch {
		case got != "":
			t.Errorf("abort %v: reply %q after the cut", abort, got)
		case abort && errors.Is(err, io.EOF):
			t.Errorf("abort: clean EOF, want a reset")
		case !abort && !errors.Is(err, io.EOF):
			t.Errorf("no abort: %v, want EOF", err)
		}
		if len(s.Articles()) != 0 {
			t.Errorf("cut block recorded: %q", s.Articles())
		}
	}

	// before the first line, and TAKETHIS / IHAVE on the peer
	s := NewPeer(t, Options{Deadline: 10 * time.Second, Faults: Faults{
		DropInUpload: map[string]int{"TAKETHIS": 1, "IHAVE": 0},
	}}, &PeerOptions{})
	c := bytesDial(t, s)
	rawSend(t, c, "TAKETHIS <t@x>\r\nSubject: s\r\n")
	if got, err := readAll(c); got != "" || !errors.Is(err, io.EOF) {
		t.Errorf("TAKETHIS cut: %q, %v", got, err)
	}
	c = bytesDial(t, s)
	rawSend(t, c, "IHAVE <i@x>\r\n")
	if got := rawRead(t, c); !strings.HasPrefix(got, "335 ") {
		t.Fatalf("IHAVE %q", got)
	}
	if got, err := readAll(c); got != "" || !errors.Is(err, io.EOF) {
		t.Errorf("IHAVE cut: %q, %v", got, err)
	}
}

func TestFaultTrailing(t *testing.T) {
	s := dateServer(t, Faults{Trailing: map[string]Trailing{"DATE": {Lines: []string{"200 extra", "more"}, Gap: testGap}}})
	c := bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	start := time.Now()
	if got := rawRead(t, c); got != dateReply {
		t.Fatalf("reply %q", got)
	}
	if !readQuiet(t, c, testGap/3) {
		t.Fatal("not quiet")
	}
	if got := rawRead(t, c); got != "200 extra\r\nmore\r\n" {
		t.Fatalf("trailing %q", got)
	}
	if took := time.Since(start); took < testGap-testGap/4 {
		t.Errorf("trailing data after %v, want the gap", took)
	}
	// a cut reply gets none
	s = dateServer(t, Faults{DropAfterBytes: map[string]int{"DATE": 5}, Trailing: map[string]Trailing{"DATE": {Lines: []string{"extra"}}}})
	c = bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	if got, _ := readAll(c); got != "111 1" {
		t.Errorf("cut reply with trailing data: %q", got)
	}
}

// Data behind the STARTTLS 382, before the handshake, in its own packet.
func TestFaultTrailingStartTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := New(t, Options{Store: testStore(), Deadline: 10 * time.Second,
		StoreOpts: StoreOptions{StartTLS: &StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}},
		Faults:    Faults{Trailing: map[string]Trailing{"STARTTLS": {Lines: []string{"junk"}, Gap: 100 * time.Millisecond}}}})
	c := bytesDial(t, s)
	rawSend(t, c, "STARTTLS\r\n")
	if got := rawRead(t, c); got != "382 Continue with TLS negotiation\r\n" {
		t.Fatalf("STARTTLS %q", got)
	}
	if got := rawRead(t, c); got != "junk\r\n" {
		t.Fatalf("trailing %q", got)
	}
}

// New copies the new maps too.
func TestFaultBytesCopied(t *testing.T) {
	at := []int{3}
	f := Faults{Split: map[string]Split{"DATE": {At: at, Gap: time.Millisecond}}, DropAfterBytes: map[string]int{"DATE": 25}}
	s := dateServer(t, f)
	at[0] = 9
	f.DropAfterBytes["DATE"] = 0
	c := bytesDial(t, s)
	rawSend(t, c, "DATE\r\n")
	if got := rawRead(t, c); got != "111" {
		t.Errorf("first piece %q, want the offset as it was at New", got)
	}
}
