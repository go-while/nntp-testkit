package fakeserver

import (
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// TestLineHandler: f answers each line in any order, can read an article as a block, close
// the connection, and the Faults apply to its replies.
func TestLineHandler(t *testing.T) {
	s := New(t, Options{
		Faults: Faults{Override: map[string][]string{"DATE": {"111 20200101000000"}}, CloseAfter: 6},
		Handler: LineHandler(func(c *Conn, line string) ([]string, bool) {
			f := strings.Fields(line)
			switch strings.ToUpper(f[0]) {
			case "MODE":
				return []string{"203 stream ok"}, false
			case "CHECK":
				return []string{"238 " + f[1]}, false
			case "TAKETHIS":
				if _, err := c.ReadDotBlock(); err != nil {
					return nil, true
				}
				return []string{"239 " + f[1]}, false
			case "SILENT":
				return nil, false
			case "QUIT":
				return []string{"205 bye"}, true
			}
			return []string{"500 what?"}, false
		}),
	})
	c := client.Dial(t, s.Addr())
	c.ExpectCmd(238, "CHECK <a@b>") // before MODE STREAM: the order is the client's
	c.ExpectCmd(203, "MODE STREAM")
	c.MustSend("TAKETHIS <a@b>\r\n" + client.Stuff([]string{"Message-ID: <a@b>", "", ".x"}))
	c.ExpectStatus(239, "TAKETHIS")
	c.MustSend("SILENT\r\n")
	c.ExpectCmd(111, "DATE") // Faults.Override; DATE's own reply would be 500
	c.ExpectCmd(500, "HELP") // the 6th line: Faults.CloseAfter closes after the reply
	wantEOF(t, c)
	if got, want := s.Lines(), []string{"CHECK <a@b>", "MODE STREAM", "TAKETHIS <a@b>", "SILENT", "DATE", "HELP"}; !slices.Equal(got, want) {
		t.Errorf("Lines = %q, want %q", got, want)
	}
	if arts := s.Articles(); len(arts) != 1 || !slices.Equal(arts[0], []string{"Message-ID: <a@b>", "", "..x"}) {
		t.Errorf("Articles = %q", arts)
	}

	c = client.Dial(t, s.Addr())
	c.ExpectCmd(205, "QUIT")
	wantEOF(t, c)
}

// TestMaxOpen: MaxOpen is the peak of Open; a connection over Options.MaxConns is not counted.
func TestMaxOpen(t *testing.T) {
	s := New(t, Options{NoGreeting: true, MaxConns: 3})
	var conns []net.Conn
	for range 3 {
		conns = append(conns, rawDial(t, s))
	}
	s.WaitOpen(t, 3, time.Second)
	b, _ := io.ReadAll(rawDial(t, s)) // the 4th: rejected
	if !strings.HasPrefix(string(b), "502 ") {
		t.Errorf("4th connection got %q, want the MaxConns 502", b)
	}
	_ = conns[0].Close()
	_ = conns[1].Close()
	s.WaitOpen(t, 1, time.Second)
	rawDial(t, s)
	s.WaitOpen(t, 2, time.Second)
	if got := s.MaxOpen(); got != 3 {
		t.Errorf("MaxOpen = %d, want 3", got)
	}
	if got := s.Accepted(); got != 5 {
		t.Errorf("Accepted = %d, want 5", got)
	}
}

// The tests below each build, from the kit, a fake peer that a test suite (Diablo's e2e
// tests) wrote by hand, and check it behaves the same; the doc comment names the helper it
// replaces and shows the replacement.

// TestReplaceLinePeer replaces a hand-written peer that greets "200 fake" and gives each
// lower-cased, trimmed line to h, writing h's raw reply text (with its own CRLF, "" for none)
// and closing when h says so; conn is the accept order from 1 (Diablo: dnlPeer, dnlPeerN):
//
//	func dnlPeerN(t *testing.T, h func(conn int, line string) (string, bool)) int {
//		s := fakeserver.New(t, fakeserver.Options{Greeting: "200 fake",
//			Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
//				rep, done := h(c.ID(), strings.ToLower(strings.TrimSpace(line)))
//				if rep != "" {
//					if _, err := io.WriteString(c.Raw(), rep); err != nil {
//						return nil, true
//					}
//				}
//				return nil, done
//			})})
//		return s.Port()
//	}
func TestReplaceLinePeer(t *testing.T) {
	dnlPeerN := func(t *testing.T, h func(conn int, line string) (string, bool)) int {
		s := New(t, Options{Greeting: "200 fake",
			Handler: LineHandler(func(c *Conn, line string) ([]string, bool) {
				rep, done := h(c.ID(), strings.ToLower(strings.TrimSpace(line)))
				if rep != "" {
					if _, err := io.WriteString(c.Raw(), rep); err != nil {
						return nil, true
					}
				}
				return nil, done
			})})
		return s.Port()
	}
	var mu sync.Mutex
	var seen []string
	port := dnlPeerN(t, func(conn int, line string) (string, bool) {
		mu.Lock()
		seen = append(seen, strconv.Itoa(conn)+" "+line)
		mu.Unlock()
		switch {
		case strings.HasPrefix(line, "mode stream"):
			return "203 stream ok\r\n", false
		case strings.HasPrefix(line, "check <a1@"):
			return "431 " + strings.Fields(line)[1] + " try later\r\n", false
		case strings.HasPrefix(line, "check <a3@"):
			return "400 going away\r\n", true
		}
		return "", false
	})
	addr := "127.0.0.1:" + strconv.Itoa(port)
	c := client.Dial(t, addr)
	if c.Greeting != "200 fake" {
		t.Errorf("greeting %q", c.Greeting)
	}
	c.ExpectCmd(203, "MODE STREAM")
	c.ExpectCmd(431, "CHECK <A1@e2e.test>")
	c.MustSend("NOREPLY\r\n")
	c.ExpectCmd(400, "CHECK <a3@e2e.test>")
	wantEOF(t, c)
	c2 := client.Dial(t, addr)
	c2.ExpectCmd(203, "MODE STREAM")
	mu.Lock()
	defer mu.Unlock()
	want := []string{"1 mode stream", "1 check <a1@e2e.test>", "1 noreply", "1 check <a3@e2e.test>", "2 mode stream"}
	if !slices.Equal(seen, want) {
		t.Errorf("h saw %q, want %q", seen, want)
	}
}

// TestReplaceOneLineServer replaces a server whose commands all have one-line replies: 200
// greeting, "111 20261026120000" for DATE, "211 1 1 1 e2e.test" for GROUP, 205 for QUIT (then
// close), 500 for anything else (Diablo: oneLineServer). The Store server answers QUIT and
// unknown commands so; Faults.Override fixes the DATE and GROUP replies:
//
//	s := fakeserver.New(t, fakeserver.Options{Store: fakeserver.NewStore(),
//		Faults: fakeserver.Faults{Override: map[string][]string{
//			"DATE": {"111 20261026120000"}, "GROUP": {"211 1 1 1 e2e.test"}}}})
//	port := s.Port()
//
// Unlike the hand-written one, commands such as LIST, HELP or CAPABILITIES get the Store's
// real (multi-line) answers; a LineHandler with a switch is the exact copy.
func TestReplaceOneLineServer(t *testing.T) {
	s := New(t, Options{Store: NewStore(),
		Faults: Faults{Override: map[string][]string{
			"DATE": {"111 20261026120000"}, "GROUP": {"211 1 1 1 e2e.test"}}}})
	c := client.Dial(t, s.Addr())
	if !strings.HasPrefix(c.Greeting, "200 ") {
		t.Errorf("greeting %q", c.Greeting)
	}
	if line := c.ExpectCmd(111, "DATE"); line != "111 20261026120000" {
		t.Errorf("DATE: %q", line)
	}
	if line := c.ExpectCmd(211, "group e2e.test"); line != "211 1 1 1 e2e.test" {
		t.Errorf("GROUP: %q", line)
	}
	c.ExpectCmd(500, "FOO")
	c.ExpectCmd(205, "QUIT")
	wantEOF(t, c)
}

// TestReplaceRefilePeer replaces a peer that greets "200 fake", answers MODE HEADFEED with
// 250 and MODE STREAM with 203, and goes away at the first CHECK without answering it
// (Diablo: the peer of TestDnewslinkRefileKeepsFlags):
//
//	s := fakeserver.New(t, fakeserver.Options{Greeting: "200 fake",
//		Handler: fakeserver.LineHandler(func(_ *fakeserver.Conn, line string) ([]string, bool) {
//			switch line = strings.ToLower(line); {
//			case strings.HasPrefix(line, "mode headfeed"):
//				return []string{"250 ok"}, false
//			case strings.HasPrefix(line, "mode stream"):
//				return []string{"203 stream ok"}, false
//			case strings.HasPrefix(line, "check"):
//				return nil, true // go away with the CHECKs unanswered
//			}
//			return nil, false
//		})})
//	port := s.Port()
//
// The hand-written peer accepted one connection; this one serves every connection alike.
func TestReplaceRefilePeer(t *testing.T) {
	s := New(t, Options{Greeting: "200 fake",
		Handler: LineHandler(func(_ *Conn, line string) ([]string, bool) {
			switch line = strings.ToLower(line); {
			case strings.HasPrefix(line, "mode headfeed"):
				return []string{"250 ok"}, false
			case strings.HasPrefix(line, "mode stream"):
				return []string{"203 stream ok"}, false
			case strings.HasPrefix(line, "check"):
				return nil, true
			}
			return nil, false
		})})
	c := client.Dial(t, s.Addr())
	c.ExpectCmd(250, "MODE HEADFEED")
	c.ExpectCmd(203, "MODE STREAM")
	c.MustSend("CHECK <a1@e2e.test>\r\nCHECK <a2@e2e.test>\r\n")
	wantEOF(t, c)
	if got := s.Count("CHECK"); got != 1 {
		t.Errorf("CHECK read %d times, want 1 (the peer goes away at the first)", got)
	}
}

// TestReplaceHoldPeer replaces a peer that accepts connections and never answers, counting
// the connections open now, the most open at once and all accepted (Diablo: holdPeer):
//
//	peer := fakeserver.New(t, fakeserver.Options{NoGreeting: true})
//	port := peer.Port()
//	open, most, accepted := peer.Open(), peer.MaxOpen(), peer.Accepted()
//
// A connection counts as open until the client closes it (the server reads EOF).
func TestReplaceHoldPeer(t *testing.T) {
	peer := New(t, Options{NoGreeting: true})
	var conns []net.Conn
	for range 3 {
		nc := rawDial(t, peer)
		conns = append(conns, nc)
		if _, err := io.WriteString(nc, "MODE STREAM\r\n"); err != nil {
			t.Fatal(err)
		}
	}
	peer.WaitOpen(t, 3, time.Second)
	peer.WaitLines(t, []string{"MODE STREAM", "MODE STREAM", "MODE STREAM"})
	_ = conns[0].Close()
	_ = conns[1].Close()
	peer.WaitOpen(t, 1, time.Second)
	if open, most, accepted := peer.Open(), peer.MaxOpen(), peer.Accepted(); open != 1 || most != 3 || accepted != 3 {
		t.Errorf("open %d, most %d, accepted %d; want 1, 3, 3", open, most, accepted)
	}
	// Never answers: nothing arrives on the open connection.
	_ = conns[2].SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := conns[2].Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("read %d bytes (%v) from a peer that never answers", n, err)
	}
}
