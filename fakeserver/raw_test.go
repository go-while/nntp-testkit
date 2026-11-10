package fakeserver

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/article"
	"github.com/go-while/nntp-testkit/client"
)

// rawArt has what a byte-exact feed check cares about: dot lines, a bare CR, a NUL, 8-bit bytes.
const rawArt = "Message-ID: <raw@x>\r\nSubject: s\r\n\r\n.dot\r\n..two\r\nbare\rcr\r\nnul\x00\r\n8bit \xe4\xff\r\n.\r\nlast\r\n"

// stuffedNoTerminator is rawArt as it goes on the wire, without the ".\r\n" terminator.
func stuffedNoTerminator(art string) string {
	return strings.TrimSuffix(string(client.StuffBytes([]byte(art))), ".\r\n")
}

func TestArticlesRawPeer(t *testing.T) {
	var mu sync.Mutex
	var rawSeen [][]byte
	s := NewPeer(t, Options{}, &PeerOptions{
		Reject:       map[string]bool{"<rej@x>": true},
		OnArticleRaw: func(id string, raw []byte) { mu.Lock(); rawSeen = append(rawSeen, raw); mu.Unlock() },
	})
	c := client.Dial(t, s.Addr())
	a := []byte(rawArt)
	c.ExpectIhave(235, "<i@x>", a)
	c.ExpectTakethis(239, "<t@x>", a)
	c.ExpectTakethis(439, "<t@x>", a) // already held: refused, but the article was read
	c.ExpectIhave(437, "<rej@x>", a)

	want := stuffedNoTerminator(rawArt)
	raws := s.ArticlesRaw()
	if len(raws) != 4 || len(s.Articles()) != 4 {
		t.Fatalf("ArticlesRaw %d, Articles %d, want 4", len(raws), len(s.Articles()))
	}
	for i, r := range raws {
		if string(r) != want {
			t.Errorf("ArticlesRaw()[%d] = %q, want %q", i, r, want)
		}
		// the lines of Articles() are the same bytes without line ends
		if got := strings.Join(s.Articles()[i], "\r\n") + "\r\n"; got != want {
			t.Errorf("Articles()[%d] joined = %q, want %q", i, got, want)
		}
	}
	// a byte-exact feed check: ParseDotBlock gives the article back
	if back := article.ParseDotBlock(raws[0]).Bytes(); !bytes.Equal(back, a) {
		t.Errorf("ParseDotBlock(ArticlesRaw()[0]).Bytes() = %q, want %q", back, a)
	}
	mu.Lock()
	if len(rawSeen) != 2 { // the held and the rejected one do not reach OnArticle(Raw)
		t.Errorf("OnArticleRaw ran %d times, want 2", len(rawSeen))
	}
	for i, r := range rawSeen {
		if string(r) != want {
			t.Errorf("OnArticleRaw %d = %q", i, r)
		}
	}
	mu.Unlock()
	// the accessor returns copies
	raws[0][0] = 'X'
	if s.ArticlesRaw()[0][0] == 'X' {
		t.Errorf("ArticlesRaw shares its bytes with the server")
	}
}

func TestArticlesRawStorePost(t *testing.T) {
	st := NewStore()
	st.AddGroup("misc.new", "t")
	s := New(t, Options{Store: st})
	c := client.Dial(t, s.Addr())
	post := "Newsgroups: misc.new\r\nSubject: s\r\nFrom: f@x\r\n\r\nfirst\r\n.leading dot\r\n"
	c.ExpectPost(240, []byte(post))
	raws := s.ArticlesRaw()
	if len(raws) != 1 || string(raws[0]) != stuffedNoTerminator(post) {
		t.Fatalf("ArticlesRaw() = %q", raws)
	}
	if got := article.ParseDotBlock(raws[0]).Bytes(); string(got) != post {
		t.Errorf("ParseDotBlock = %q, want %q", got, post)
	}
}

// A script step and a bare LF from the client: the raw bytes keep it, the lines do not show it.
func TestArticlesRawScriptBareLF(t *testing.T) {
	s := New(t, Options{Script: []Step{{Expect: "POST", ReadDot: true, Reply: []string{"240 ok"}}}})
	c := client.Dial(t, s.Addr())
	c.MustSend("POST\r\n")
	c.MustSend("a\nb\r\n..c\r\n.\r\n")
	c.ExpectStatus(240, "POST")
	raws := s.ArticlesRaw()
	if len(raws) != 1 || string(raws[0]) != "a\nb\r\n..c\r\n" {
		t.Fatalf("ArticlesRaw() = %q", raws)
	}
	if arts := s.Articles(); len(arts) != 1 || strings.Join(arts[0], "|") != "a|b|..c" {
		t.Errorf("Articles() = %q", arts)
	}
	// an empty block is empty, not nil
	e := New(t, Options{Script: []Step{{Expect: "POST", ReadDot: true, Reply: []string{"240 ok"}}}})
	ec := client.Dial(t, e.Addr())
	ec.MustSend("POST\r\n.\r\n")
	ec.ExpectStatus(240, "empty POST")
	if r := e.ArticlesRaw(); len(r) != 1 || r[0] == nil || len(r[0]) != 0 {
		t.Errorf("empty block: %#v", r)
	}
}

func TestNewPeerHoldGreeting(t *testing.T) {
	gate := make(chan struct{})
	s := NewPeer(t, Options{Greeting: "200 held"}, &PeerOptions{HoldGreeting: gate})
	if !s.o.NoGreeting {
		t.Fatalf("NewPeer did not set NoGreeting for HoldGreeting")
	}
	done := make(chan *client.Conn, 1)
	go func() {
		c, err := client.TryDial(s.Addr(), 10*time.Second)
		if err != nil {
			done <- nil
			return
		}
		done <- c
	}()
	s.WaitOpen(t, 1, 5*time.Second)
	select {
	case <-done:
		t.Fatalf("greeting arrived before the gate was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	c := <-done
	if c == nil {
		t.Fatalf("dial failed after the gate was released")
	}
	defer c.Close()
	if c.Greeting != "200 held" {
		t.Errorf("greeting %q", c.Greeting)
	}
	// no tb error was reported (the recorded test would fail)

	d := NewPeer(t, Options{}, &PeerOptions{GreetingDelay: 10 * time.Millisecond})
	if !d.o.NoGreeting {
		t.Errorf("NewPeer did not set NoGreeting for GreetingDelay")
	}
	client.Dial(t, d.Addr()) // greeting after the delay

	// without the knobs the server greets by itself, and po nil is fine
	p := NewPeer(t, Options{}, nil)
	if p.o.NoGreeting {
		t.Errorf("NoGreeting set without HoldGreeting/GreetingDelay")
	}
	if c := client.Dial(t, p.Addr()); c.Greeting != DefaultGreeting {
		t.Errorf("greeting %q", c.Greeting)
	}
}

func TestStartPeer(t *testing.T) {
	s, err := StartPeer(Options{}, &PeerOptions{GreetingDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.o.NoGreeting {
		t.Errorf("StartPeer did not set NoGreeting")
	}
	c := client.Dial(t, s.Addr())
	c.ExpectCmd(205, "QUIT")

	// the peer is the handler: another one is a mistake
	if _, err := StartPeer(Options{Handler: func(*Conn) {}}, nil); err == nil || !strings.Contains(err.Error(), "Handler") {
		t.Errorf("StartPeer with a Handler: %v", err)
	}
}
