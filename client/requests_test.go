package client

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/fakeserver"
)

func TestStuffBytes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ".\r\n"},
		{"plain", "a\r\nb\r\n", "a\r\nb\r\n.\r\n"},
		{"no final CRLF", "a\r\nb", "a\r\nb\r\n.\r\n"},
		{"dot lines", ".a\r\n.\r\n..b\r\nc", "..a\r\n..\r\n...b\r\nc\r\n.\r\n"},
		{"dot first line", ".", "..\r\n.\r\n"},
		{"dot after blank", "H: v\r\n\r\n.x\r\n", "H: v\r\n\r\n..x\r\n.\r\n"},
		{"bare CR stays", "a\rb\r\n\rc\r\n", "a\rb\r\n\rc\r\n.\r\n"},
		{"ends in CR", "a\r", "a\r\r\n.\r\n"},
		{"bare LF is no line start", "a\n.b\r\n", "a\n.b\r\n.\r\n"},
		{"NUL and 8-bit", "n\x00\r\n.\xe4\xf6\xff\r\n", "n\x00\r\n..\xe4\xf6\xff\r\n.\r\n"},
		{"only CRLF", "\r\n", "\r\n.\r\n"},
	} {
		if got := StuffBytes([]byte(tc.in)); string(got) != tc.want {
			t.Errorf("%s: StuffBytes(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// The same as Stuff for line-oriented input.
	lines := []string{"From: a", "", ".x", ".", "end"}
	if got, want := string(StuffBytes([]byte(strings.Join(lines, "\r\n")))), Stuff(lines); got != want {
		t.Errorf("StuffBytes = %q, Stuff = %q", got, want)
	}
	// The input is not modified.
	in := []byte(".a\r\nb")
	StuffBytes(in)
	if string(in) != ".a\r\nb" {
		t.Errorf("input changed: %q", in)
	}
}

const art = "Newsgroups: test.group\r\nFrom: a@test.invalid\r\nSubject: s\r\nMessage-ID: <x@t>\r\n\r\nbody\r\n"

func peerServer(t *testing.T, o *fakeserver.PeerOptions) *fakeserver.Server {
	t.Helper()
	return fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(o)})
}

func TestTransferReplies(t *testing.T) {
	var arrived []string
	var mu sync.Mutex
	s := peerServer(t, &fakeserver.PeerOptions{
		Have:         map[string]bool{"<have@t>": true},
		Reject:       map[string]bool{"<rej@t>": true},
		Defer431Once: map[string]bool{"<d431@t>": true},
		Defer436Once: map[string]bool{"<d436@t>": true},
		OnArticle: func(id string, wire []string) {
			mu.Lock()
			arrived = append(arrived, id)
			mu.Unlock()
		},
	})
	c := Dial(t, s.Addr())
	a := []byte(art)
	type result struct {
		code int
		line string
	}
	for _, tc := range []struct {
		name string
		do   func() (int, string, error)
		want int
	}{
		{"ihave new", func() (int, string, error) { return c.TryIhave("<new@t>", a) }, 235},
		{"ihave have", func() (int, string, error) { return c.TryIhave("<have@t>", a) }, 435},
		{"ihave again (now held)", func() (int, string, error) { return c.TryIhave("<new@t>", a) }, 435},
		{"ihave reject", func() (int, string, error) { return c.TryIhave("<rej@t>", a) }, 437},
		{"ihave defer", func() (int, string, error) { return c.TryIhave("<d436@t>", a) }, 436},
		{"ihave defer then ok", func() (int, string, error) { return c.TryIhave("<d436@t>", a) }, 235},
		{"ihave bad id", func() (int, string, error) { return c.TryIhave("nonsense", a) }, 435},
		{"check wanted", func() (int, string, error) { return c.TryCheck("<c@t>") }, 238},
		{"check have", func() (int, string, error) { return c.TryCheck("<have@t>") }, 438},
		{"check defer", func() (int, string, error) { return c.TryCheck("<d431@t>") }, 431},
		{"takethis ok", func() (int, string, error) { return c.TryTakethis("<tk@t>", a) }, 239},
		{"takethis dup", func() (int, string, error) { return c.TryTakethis("<tk@t>", a) }, 439},
		{"takethis reject", func() (int, string, error) { return c.TryTakethis("<rej@t>", a) }, 439},
		{"mode stream", func() (int, string, error) { return c.TryModeStream() }, 203},
		{"post unknown", func() (int, string, error) { return c.TryPost(a) }, 500},
	} {
		code, line, err := tc.do()
		if err != nil || code != tc.want {
			t.Errorf("%s: %d %q err %v, want %d", tc.name, code, line, err, tc.want)
		}
	}
	// the connection is still in sync after all of that
	if code, _ := c.Cmd("MODE STREAM"); code != 203 {
		t.Errorf("connection out of sync: %d", code)
	}
	mu.Lock()
	want := []string{"<new@t>", "<d436@t>", "<tk@t>"}
	if !slices.Equal(arrived, want) {
		t.Errorf("articles that arrived: %q, want %q", arrived, want)
	}
	mu.Unlock()
}

func TestLogin(t *testing.T) {
	s := peerServer(t, &fakeserver.PeerOptions{User: "u", Pass: "p"})
	c := Dial(t, s.Addr())
	if code, line, err := c.TryIhave("<a@t>", []byte(art)); err != nil || code != 480 {
		t.Errorf("IHAVE before login: %d %q %v, want 480", code, line, err)
	}
	if code, line, err := c.TryTakethis("<a@t>", []byte(art)); err != nil || code != 480 {
		t.Errorf("TAKETHIS before login: %d %q %v, want 480", code, line, err)
	}
	if code, _, err := c.TryLogin("u", "wrong"); err != nil || code != 481 {
		t.Errorf("bad password: %d %v, want 481", code, err)
	}
	if code, _, err := c.TryLogin("nobody", "p"); err != nil || code != 481 {
		t.Errorf("bad user: %d %v, want 481", code, err)
	}
	if code, line := c.Login("u", "p"); code != 281 {
		t.Errorf("login: %d %q, want 281", code, line)
	}
	if code, _, err := c.TryLogin("u", "p"); err != nil || code != 502 {
		t.Errorf("second login: %d %v, want 502", code, err)
	}
	if code, _ := c.Ihave("<a@t>", []byte(art)); code != 235 {
		t.Errorf("IHAVE after login: %d, want 235", code)
	}
}

func TestPostStore(t *testing.T) {
	st := fakeserver.NewStore()
	st.AddGroup("test.group", "t")
	s := fakeserver.New(t, fakeserver.Options{Store: st})
	c := Dial(t, s.Addr())
	code, line := c.Post([]byte(art + ".dotted\r\n"))
	if code != 240 {
		t.Fatalf("POST: %d %q", code, line)
	}
	// a refusal (441: no Newsgroups) is a result, not a failure
	code, line, err := c.TryPost([]byte("Subject: s\r\n\r\nx\r\n"))
	if err != nil || code != 441 {
		t.Errorf("POST without Newsgroups: %d %q %v, want 441", code, line, err)
	}
	c.ExpectCmd(222, "BODY <x@t>")
	if body := c.ReadDotLines(); !slices.Equal(body, []string{"body", ".dotted"}) {
		t.Errorf("body %q", body)
	}

	// posting not allowed: the first reply is the result
	ro := fakeserver.New(t, fakeserver.Options{Store: st, StoreOpts: fakeserver.StoreOptions{ReaderOnly: true}})
	c2 := Dial(t, ro.Addr())
	if code, line := c2.Post([]byte(art)); code != 440 {
		t.Errorf("POST on a read-only server: %d %q, want 440", code, line)
	}
	if code, line := c2.Cmd("DATE"); code != 111 {
		t.Errorf("not in sync after 440: %d %q", code, line)
	}
}

func TestTransferLineHandlerOdd(t *testing.T) {
	// 480 to the first POST; the second gets 340 and an odd final reply (436 to a POST)
	posts := 0
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		if line == "POST" {
			posts++
		}
		switch {
		case line == "POST" && posts == 1:
			return []string{"480 authentication required"}, false
		case line == "POST":
			return []string{"340 go"}, false
		case line == ".":
			return []string{"436 try later"}, false
		}
		return nil, false // article lines
	})})
	c := Dial(t, s.Addr())
	if code, _, err := c.TryPost([]byte(art)); err != nil || code != 480 {
		t.Errorf("POST: %d %v, want 480", code, err)
	}
	if code, line, err := c.TryPost([]byte(art)); err != nil || code != 436 {
		t.Errorf("POST: %d %q %v, want 436", code, line, err)
	}
}

// rawServer records the bytes the client sends after POST, IHAVE and TAKETHIS (up to and
// including the line ".\r\n"), exactly as read.
type rawServer struct {
	*peer
	mu   sync.Mutex
	blob [][]byte
}

func newRawServer(t *testing.T) *rawServer {
	rs := &rawServer{}
	rs.peer = newPeer(t, func(p *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 raw\r\n")
		for {
			line, ok := p.readLine(r)
			if !ok {
				return
			}
			switch {
			case line == "POST":
				write(c, "340 go\r\n")
			case strings.HasPrefix(line, "IHAVE "):
				write(c, "335 go\r\n")
			case !strings.HasPrefix(line, "TAKETHIS "):
				write(c, "500 ?\r\n")
				continue
			}
			var b []byte
			for {
				l, err := r.ReadBytes('\n')
				b = append(b, l...)
				if err != nil {
					return
				}
				if string(l) == ".\r\n" {
					break
				}
			}
			rs.mu.Lock()
			rs.blob = append(rs.blob, b)
			rs.mu.Unlock()
			switch {
			case line == "POST":
				write(c, "240 ok\r\n")
			case strings.HasPrefix(line, "IHAVE "):
				write(c, "235 ok\r\n")
			default:
				write(c, "239 "+line[9:]+"\r\n")
			}
		}
	})
	return rs
}

func TestArticleBytesSurvive(t *testing.T) {
	article := "Subject: x\r\n\r\n.dot\r\n..two\r\nbare\rcr\r\nnul\x00\r\n8bit \xe4\xf6\xff\r\n.\r\n\r\nlast"
	wire := "Subject: x\r\n\r\n..dot\r\n...two\r\nbare\rcr\r\nnul\x00\r\n8bit \xe4\xf6\xff\r\n..\r\n\r\nlast\r\n.\r\n"
	rs := newRawServer(t)
	c := Dial(t, rs.addr())
	a := []byte(article)
	if code, _, err := c.TryPost(a); err != nil || code != 240 {
		t.Fatalf("POST: %d %v", code, err)
	}
	if code, _, err := c.TryIhave("<i@t>", a); err != nil || code != 235 {
		t.Fatalf("IHAVE: %d %v", code, err)
	}
	if code, line, err := c.TryTakethis("<t@t>", a); err != nil || code != 239 || line != "239 <t@t>" {
		t.Fatalf("TAKETHIS: %d %q %v", code, line, err)
	}
	if string(a) != article {
		t.Errorf("the caller's article was changed")
	}
	c.Close()
	rs.lines() // wait for the handler
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.blob) != 3 {
		t.Fatalf("%d blobs, want 3", len(rs.blob))
	}
	for i, b := range rs.blob {
		if string(b) != wire {
			t.Errorf("blob %d = %q, want %q", i, b, wire)
		}
	}
}

func TestTakethisSendsWithoutWaiting(t *testing.T) {
	// the server answers only after it has read command and article: the client must not wait
	// for a reply between them (a 335-like step does not exist).
	rs := newRawServer(t)
	c := Dial(t, rs.addr())
	code, line := c.Takethis("<w@t>", []byte("a\r\n"))
	if code != 239 || line != "239 <w@t>" {
		t.Errorf("%d %q", code, line)
	}
	if got := c.R.Buffered(); got != 0 {
		t.Errorf("%d bytes left", got)
	}
}

func TestPlainVariantsFailOnIOError(t *testing.T) {
	s := peerServer(t, &fakeserver.PeerOptions{})
	c := Dial(t, s.Addr())
	c.Close()
	for name, fn := range map[string]func(c *Conn){
		"Post":       func(c *Conn) { c.Post([]byte(art)) },
		"Ihave":      func(c *Conn) { c.Ihave("<a@t>", []byte(art)) },
		"Check":      func(c *Conn) { c.Check("<a@t>") },
		"Takethis":   func(c *Conn) { c.Takethis("<a@t>", []byte(art)) },
		"ModeStream": func(c *Conn) { c.ModeStream() },
		"Login":      func(c *Conn) { c.Login("u", "p") },
	} {
		rec := &recordTB{TB: t}
		rec.run(func(tb testing.TB) { fn(c.For(tb)) })
		if !rec.failed {
			t.Errorf("%s on a closed conn did not fail tb", name)
		}
	}
	// a protocol error (a reply that is no status line) is an error too
	bad := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		return []string{"what is this"}, false
	})})
	c2 := Dial(t, bad.Addr())
	if _, _, err := c2.TryPost([]byte(art)); err == nil {
		t.Errorf("TryPost: no error for a garbage reply")
	}
	rec := &recordTB{TB: t}
	rec.run(func(tb testing.TB) { c2.For(tb).Check("<a@t>") })
	if !rec.failed || !strings.Contains(rec.msg, "CHECK <a@t>") {
		t.Errorf("Check with a garbage reply: failed %v, %q", rec.failed, rec.msg)
	}
}

func TestPipelineBasic(t *testing.T) {
	s := storeServer(t)
	c := Dial(t, s.Addr())
	replies := c.Pipeline([]string{
		"DATE", "GROUP test.group", "LISTGROUP test.group", "STAT <nope@t>", "BODY <1@test.invalid>",
		"HEAD <1@test.invalid>", "DATE",
	}, nil)
	if len(replies) != 7 {
		t.Fatalf("%d replies", len(replies))
	}
	for i, want := range []int{111, 211, 211, 430, 222, 221, 111} {
		if replies[i].Code != want {
			t.Errorf("reply %d (%s) = %d %q, want %d", i, replies[i].Cmd, replies[i].Code, replies[i].Line, want)
		}
	}
	if replies[0].Lines != nil || replies[1].Lines != nil || replies[3].Lines != nil {
		t.Errorf("single-line replies have blocks: %+v", replies)
	}
	if !slices.Equal(replies[2].Lines, []string{"1"}) || !slices.Equal(replies[4].Lines, []string{"body"}) {
		t.Errorf("blocks: %q %q", replies[2].Lines, replies[4].Lines)
	}
	if replies[5].Lines[0] != "From: a@test.invalid" {
		t.Errorf("head: %q", replies[5].Lines)
	}
	// a custom multi function; the connection stays in sync
	r2, err := c.TryPipeline([]string{"CAPABILITIES", "DATE"}, func(cmd string, code int) bool { return code == 101 })
	if err != nil || len(r2) != 2 || r2[0].Lines[0] != "VERSION 2" || r2[1].Code != 111 {
		t.Errorf("custom multi: %+v %v", r2, err)
	}
	if r, err := c.TryPipeline(nil, nil); err != nil || len(r) != 0 {
		t.Errorf("empty pipeline: %v %v", r, err)
	}
	// an entry that ends in CRLF is sent as it is: TAKETHIS with its article
	ps := peerServer(t, &fakeserver.PeerOptions{})
	pc := Dial(t, ps.Addr())
	r3 := pc.Pipeline([]string{"MODE STREAM", "CHECK <a@t>", "TAKETHIS <a@t>\r\n" + string(StuffBytes([]byte(art))), "CHECK <a@t>"}, nil)
	var codes []int
	for _, r := range r3 {
		codes = append(codes, r.Code)
	}
	if !slices.Equal(codes, []int{203, 238, 239, 438}) {
		t.Errorf("codes %v", codes)
	}
}

func TestPipelineSlowServerNoDeadlock(t *testing.T) {
	// 8000 commands of 2 KB, each echoed with a 2 KB reply: 16 MB each way. A server that
	// reads slowly (and stops reading while its replies cannot be written) would deadlock a
	// client that writes everything before it reads.
	const n = 8000
	pad := strings.Repeat("x", 2000)
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		for i := 0; ; i++ {
			line, err := c.ReadLine()
			if err != nil {
				return
			}
			if i%1000 == 999 {
				time.Sleep(5 * time.Millisecond)
			}
			if c.WriteLines("223 0 "+line[5:]) != nil {
				return
			}
		}
	}})
	c := Dial(t, s.Addr())
	cmds := make([]string, n)
	for i := range cmds {
		cmds[i] = fmt.Sprintf("STAT %d%s", i, pad)
	}
	replies := c.Pipeline(cmds, nil)
	if len(replies) != n {
		t.Fatalf("%d replies, want %d", len(replies), n)
	}
	for i, r := range replies {
		if r.Code != 223 || !strings.HasPrefix(r.Line, fmt.Sprintf("223 0 %d", i)) {
			t.Fatalf("reply %d: %q", i, r.Line[:min(len(r.Line), 30)])
		}
	}
}

func TestPipelineErrors(t *testing.T) {
	// the server closes after two replies: the two are returned with the error
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		for range 2 {
			if _, err := c.ReadLine(); err != nil {
				return
			}
			_ = c.WriteLines("111 20240101000000")
		}
	}})
	c := Dial(t, s.Addr())
	r, err := c.TryPipeline([]string{"DATE", "DATE", "DATE", "DATE"}, nil)
	if err == nil || len(r) != 2 || !errors.Is(err, io.EOF) {
		t.Errorf("TryPipeline: %d replies, err %v", len(r), err)
	}
	rec := &recordTB{TB: t}
	c2 := Dial(t, s.Addr())
	rec.run(func(tb testing.TB) { c2.For(tb).Pipeline([]string{"DATE", "DATE", "DATE"}, nil) })
	if !rec.failed || !strings.Contains(rec.msg, "2 of 3 replies") {
		t.Errorf("Pipeline: failed %v, %q", rec.failed, rec.msg)
	}

	// a stalled server fails after the Conn's deadline
	stall := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		_, _ = c.ReadLine()
		_, _ = c.ReadLine() // until the client closes
	}})
	c3 := Dial(t, stall.Addr())
	c3.Deadline = 200 * time.Millisecond
	start := time.Now()
	if _, err := c3.TryPipeline([]string{"DATE"}, nil); err == nil || !timedOut(err) {
		t.Errorf("stalled server: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func canBind(ip string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

func TestDialFrom(t *testing.T) {
	if !canBind("127.0.0.2") {
		t.Skip("this OS cannot bind 127.0.0.2 (macOS needs: ifconfig lo0 alias 127.0.0.2)")
	}
	s := fakeserver.New(t, fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
		host, _, _ := net.SplitHostPort(c.Raw().RemoteAddr().String())
		_ = c.WriteLines("200 you are " + host)
		_, _ = c.ReadLine()
	}})
	c := DialFrom(t, s.Addr(), "127.0.0.2")
	if c.Greeting != "200 you are 127.0.0.2" {
		t.Errorf("greeting %q", c.Greeting)
	}
	c2, err := TryDialFrom(s.Addr(), "127.0.0.1", 0)
	if err != nil || c2.Greeting != "200 you are 127.0.0.1" {
		t.Errorf("from 127.0.0.1: %v %v", c2, err)
	}
	c2.Close()
	if _, err := TryDialFrom(s.Addr(), "not-an-ip", 0); err == nil {
		t.Errorf("no error for a bad local address")
	}
	if _, err := TryDialFrom(s.Addr(), "192.0.2.77", time.Second); err == nil {
		t.Errorf("no error for an address this host does not own")
	}
}

func TestDialUntil(t *testing.T) {
	// the first three connections get 400, then 200
	s := fakeserver.New(t, fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
		if c.ID() <= 3 {
			_ = c.WriteLines("400 starting up")
			return
		}
		_ = c.WriteLines("200 ready")
		_, _ = c.ReadLine()
	}})
	c := DialUntil(t, s.Addr(), "200", 0)
	if c.Greeting != "200 ready" || s.Accepted() != 4 {
		t.Errorf("greeting %q after %d connections", c.Greeting, s.Accepted())
	}
	if c.Deadline != 0 {
		t.Errorf("deadline %v, want the default", c.Deadline)
	}

	// never matching: an error naming the last greeting, within d
	nope := fakeserver.New(t, fakeserver.Options{Greeting: "400 not yet"})
	start := time.Now()
	_, err := TryDialUntil(nope.Addr(), "200", 400*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "400 not yet") {
		t.Errorf("TryDialUntil: %v", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond || el > 3*time.Second {
		t.Errorf("took %v", el)
	}
	rec := &recordTB{TB: t}
	rec.run(func(tb testing.TB) { DialUntil(tb, nope.Addr(), "201", 100*time.Millisecond) })
	if !rec.failed {
		t.Errorf("DialUntil did not fail tb")
	}
}

func TestDialUntilServerNotYetListening(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(testkit.FreePort(t)))
	done := make(chan *fakeserver.Server, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		s, err := fakeserver.Start(fakeserver.Options{Addr: addr})
		if err != nil {
			done <- nil
			return
		}
		done <- s
	}()
	c, err := TryDialUntil(addr, "200", 5*time.Second)
	s := <-done
	if s == nil {
		t.Skip("the port was taken")
	}
	defer s.Close()
	if err != nil {
		t.Fatalf("TryDialUntil: %v", err)
	}
	c.Close()
}

func TestReadUntilQuiet(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
		for {
			line, err := c.ReadLine()
			if err != nil {
				return
			}
			switch line {
			case "HELLO":
				_, _ = c.Raw().Write([]byte("hel"))
				time.Sleep(20 * time.Millisecond)
				_, _ = c.Raw().Write([]byte("lo\x00\r"))
			case "BIG":
				_, _ = c.Raw().Write(bytes.Repeat([]byte("z"), 1000))
			case "BYE":
				_, _ = c.Raw().Write([]byte("bye"))
				return
			}
		}
	}})
	nc, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	c := New(t, nc)

	c.MustSend("HELLO\r\n")
	got, err := c.TryReadUntilQuiet(150*time.Millisecond, 0)
	if err != nil || string(got) != "hello\x00\r" {
		t.Errorf("HELLO: %q %v", got, err)
	}
	// limit: stops at the limit, the rest stays readable
	c.MustSend("BIG\r\n")
	got, err = c.TryReadUntilQuiet(time.Second, 100)
	if err != nil || len(got) != 100 {
		t.Errorf("BIG limit 100: %d bytes %v", len(got), err)
	}
	got, err = c.TryReadUntilQuiet(100*time.Millisecond, 0)
	if err != nil || len(got) != 900 {
		t.Errorf("rest: %d bytes %v", len(got), err)
	}
	// quiet at once: nothing, no error
	if got, err := c.TryReadUntilQuiet(30*time.Millisecond, 0); err != nil || len(got) != 0 {
		t.Errorf("quiet: %q %v", got, err)
	}
	// the deadline was restored: a normal command works
	c.MustSend("BIG\r\n")
	if got, eof := c.ReadUntilQuiet(100*time.Millisecond, 0); eof || len(got) != 1000 {
		t.Errorf("ReadUntilQuiet: %d bytes eof %v", len(got), eof)
	}
	// close: bytes and io.EOF
	c.MustSend("BYE\r\n")
	got, err = c.TryReadUntilQuiet(time.Second, 0)
	if err != io.EOF || string(got) != "bye" {
		t.Errorf("BYE: %q %v, want bye and io.EOF", got, err)
	}
	if got, eof := c.For(t).ReadUntilQuiet(time.Second, 0); !eof || len(got) != 0 {
		t.Errorf("after close: %q %v", got, eof)
	}
}

func TestReadUntilQuietBounded(t *testing.T) {
	// a server that never goes quiet: the hang guard ends the call
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		for {
			if _, err := c.Raw().Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}})
	c := Dial(t, s.Addr())
	c.Deadline = 300 * time.Millisecond
	start := time.Now()
	got, err := c.TryReadUntilQuiet(100*time.Millisecond, 0)
	if err == nil || len(got) == 0 || time.Since(start) > 3*time.Second {
		t.Errorf("%d bytes, %v, after %v", len(got), err, time.Since(start))
	}
}

func TestExpectClosed(t *testing.T) {
	s := peerServer(t, &fakeserver.PeerOptions{})
	c := Dial(t, s.Addr())
	c.MustSend("QUIT\r\n")
	lines, err := c.ExpectClosed(time.Second)
	if err != nil || !slices.Equal(lines, []string{"205 bye"}) {
		t.Errorf("after QUIT: %q %v", lines, err)
	}
	if got := c.For(t).MustClosed(time.Second); len(got) != 0 {
		t.Errorf("closed again: %q", got)
	}

	// closes without a word, with a partial line, after a delay
	srv := fakeserver.New(t, fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
		line, _ := c.ReadLine()
		switch line {
		case "SILENT":
		case "PARTIAL":
			_, _ = c.Raw().Write([]byte("400 going\r\nbye"))
		case "LATE":
			time.Sleep(100 * time.Millisecond)
		case "HANG":
			_, _ = c.ReadLine() // until the client closes
		case "CHATTY":
			for range 50 {
				_ = c.WriteLines("200 more")
			}
			_, _ = c.ReadLine()
		}
	}})
	dial := func() *Conn {
		nc, err := net.Dial("tcp", srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		return New(t, nc)
	}
	for _, tc := range []struct {
		cmd   string
		lines []string
		ok    bool
	}{
		{"SILENT", nil, true},
		{"PARTIAL", []string{"400 going", "bye"}, true},
		{"LATE", nil, true},
		{"HANG", nil, false},
		{"CHATTY", nil, false},
	} {
		c := dial()
		c.MustSend(tc.cmd + "\r\n")
		start := time.Now()
		lines, err := c.ExpectClosed(500 * time.Millisecond)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v, want ok=%v", tc.cmd, err, tc.ok)
		}
		if tc.ok && !slices.Equal(lines, tc.lines) {
			t.Errorf("%s: lines %q, want %q", tc.cmd, lines, tc.lines)
		}
		if tc.cmd == "HANG" && (!strings.Contains(err.Error(), "did not close") || time.Since(start) > 1500*time.Millisecond) {
			t.Errorf("HANG: %v after %v", err, time.Since(start))
		}
		if tc.cmd == "CHATTY" && len(lines) == 0 {
			t.Errorf("CHATTY: no lines returned with the error")
		}
	}
	// the tb form fails
	c3 := dial()
	c3.MustSend("HANG\r\n")
	rec := &recordTB{TB: t}
	rec.run(func(tb testing.TB) { c3.For(tb).MustClosed(100 * time.Millisecond) })
	if !rec.failed {
		t.Errorf("MustClosed did not fail")
	}
}

func TestSendDrip(t *testing.T) {
	// a raw server counts its reads: with a pause the bytes arrive piecemeal
	var mu sync.Mutex
	var reads []int
	var got bytes.Buffer
	p := newPeer(t, func(p *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 raw\r\n")
		buf := make([]byte, 4096)
		for got.Len() < 8 {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			reads = append(reads, n)
			got.Write(buf[:n])
			mu.Unlock()
		}
		write(c, "111 ok\r\n")
	})
	c := Dial(t, p.addr())
	if err := c.SendDrip([]byte("DATE\r\nxx"), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	c.ExpectStatus(111, "reply")
	mu.Lock()
	defer mu.Unlock()
	if got.String() != "DATE\r\nxx" {
		t.Errorf("got %q", got.String())
	}
	if len(reads) < 6 {
		t.Errorf("only %d reads for 8 dripped bytes: %v", len(reads), reads)
	}
}

func TestSendChunksSplitsTerminator(t *testing.T) {
	st := fakeserver.NewStore()
	st.AddGroup("test.group", "t")
	s := fakeserver.New(t, fakeserver.Options{Store: st})
	c := Dial(t, s.Addr())
	c.ExpectCmd(340, "POST")
	// 3 bytes at a time: CRLF and ".\r\n" fall across writes
	if err := c.SendChunks(StuffBytes([]byte(art+".dot\r\n")), 3, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	c.ExpectStatus(240, "dripped article")
	c.ExpectCmd(222, "BODY <x@t>")
	if body := c.ReadDotLines(); !slices.Equal(body, []string{"body", ".dot"}) {
		t.Errorf("body %q", body)
	}
	// a one-byte drip of a command, n < 1 is 1, and an empty write is fine
	if err := c.SendChunks([]byte("DATE\r\n"), 0, 0); err != nil {
		t.Fatal(err)
	}
	c.ExpectStatus(111, "DATE")
	if err := c.SendDrip(nil, time.Second); err != nil {
		t.Errorf("empty drip: %v", err)
	}
	c.Close()
	if err := c.SendDrip([]byte("x"), 0); err == nil {
		t.Errorf("no error writing to a closed conn")
	}
}

func TestMultiLine(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		code int
		want bool
	}{
		{"ARTICLE 1", 220, true}, {"ARTICLE 1", 430, false}, {"STAT 1", 223, false},
		{"LIST", 215, true}, {"CAPABILITIES", 101, true}, {"GROUP a", 211, false},
		{"LISTGROUP a", 211, true}, {"DATE", 111, false}, {"NEWNEWS * 0 0", 230, true},
		{"CHECK <a>", 238, false}, {"OVER 1-2", 224, true}, {"HDR Subject 1", 225, true},
	} {
		if got := MultiLine(tc.cmd, tc.code); got != tc.want {
			t.Errorf("MultiLine(%q, %d) = %v", tc.cmd, tc.code, got)
		}
	}
}

func TestPipelineBlockBound(t *testing.T) {
	// a block that trickles lines forever: the sliding guard never fires, the absolute bound does
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		_, _ = c.ReadLine()
		_ = c.WriteLines("215 list follows")
		for {
			if c.WriteLines("line") != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}})
	c := Dial(t, s.Addr())
	c.Deadline = 100 * time.Millisecond // block bound: 1s
	start := time.Now()
	r, err := c.TryPipeline([]string{"LIST"}, nil)
	var se *StrictError
	if !errors.As(err, &se) || se.Kind != NoTerminator || len(r) != 0 {
		t.Errorf("TryPipeline: %v (%d replies)", err, len(r))
	}
	if el := time.Since(start); el < 900*time.Millisecond || el > 5*time.Second {
		t.Errorf("took %v", el)
	}
}

func TestPipelineCmdFirstLine(t *testing.T) {
	ps := peerServer(t, &fakeserver.PeerOptions{})
	pc := Dial(t, ps.Addr())
	var seen []string
	r := pc.Pipeline([]string{"TAKETHIS <a@t>\r\n" + string(StuffBytes([]byte(art)))}, func(cmd string, code int) bool {
		seen = append(seen, cmd)
		return false
	})
	if r[0].Cmd != "TAKETHIS <a@t>" || !slices.Equal(seen, []string{"TAKETHIS <a@t>"}) {
		t.Errorf("Cmd %q, multi saw %q", r[0].Cmd, seen)
	}
	if !MultiLine("ListGroup a", 211) || MultiLine("LISTGROUPS", 211) {
		t.Errorf("MultiLine case handling")
	}
}

func TestExpectClosedEndlessLine(t *testing.T) {
	// bytes without any LF: the call fails at the cap, holding no more than that
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		chunk := bytes.Repeat([]byte("x"), 8192)
		for {
			if _, err := c.Raw().Write(chunk); err != nil {
				return
			}
		}
	}})
	c := Dial(t, s.Addr())
	start := time.Now()
	lines, err := c.ExpectClosed(5 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "has not closed") {
		t.Errorf("err %v, lines %d", err, len(lines))
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestReadUntilQuietLongQuiet(t *testing.T) {
	// quiet longer than the hang guard: a silent server ends the call at the guard, not at quiet
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		_, _ = c.ReadLine()
	}})
	c := Dial(t, s.Addr())
	c.Deadline = 200 * time.Millisecond
	start := time.Now()
	_, err := c.TryReadUntilQuiet(time.Minute, 0)
	if err == nil || !timedOut(err) || time.Since(start) > 3*time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
}
