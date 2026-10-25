package fakeserver

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// IHAVE, Reject and NoStream of the transit peer.

// peerLog records the OnArticle calls of a PeerHandler.
type peerLog struct {
	mu   sync.Mutex
	ids  []string
	wire [][]string
}

func (l *peerLog) onArticle(id string, wire []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, id)
	l.wire = append(l.wire, wire)
}

func (l *peerLog) got() ([]string, [][]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ids...), append([][]string(nil), l.wire...)
}

// sendArticle writes wire lines (already dot-stuffed) and the terminator.
func sendArticle(t *testing.T, c *client.Conn, wire []string) {
	t.Helper()
	if err := c.Send(strings.Join(wire, "\r\n") + "\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
}

func TestPeerIHAVE(t *testing.T) {
	var log peerLog
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		User:         "u",
		Pass:         "p",
		Have:         map[string]bool{"<had@x>": true},
		Defer436Once: map[string]bool{"<d@x>": true},
		Reject:       map[string]bool{"<r@x>": true},
		OnArticle:    log.onArticle,
	})})
	c := client.Dial(t, s.Addr())
	// before auth: 480 at once, no 335, so the client sends no article
	wantCode(t, c, "IHAVE <n@x>", 480)
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)

	for _, tc := range []struct {
		cmd  string
		want int
	}{
		{"IHAVE", 435}, // innd answers 435, not 501, to a bad message-id
		{"IHAVE n@x", 435},
		{"IHAVE <a b>", 435},
		{"IHAVE <had@x>", 435},
		{"ihave <had@x>", 435},
		{"IHAVE <d@x>", 436}, // deferred once, before the transfer
	} {
		wantCode(t, c, tc.cmd, tc.want)
	}
	if n := len(s.Articles()); n != 0 {
		t.Fatalf("articles read without a 335: %d", n)
	}

	// accepted: 335, the dot-stuffed article, 235
	stuffed := []string{"Message-ID: <n@x>", "Subject: s", "", "..leading dot", "...", "body"}
	wantCode(t, c, "IHAVE <n@x>", 335)
	sendArticle(t, c, stuffed)
	if line := wantStatus(t, c, 235); line != "235 Article transferred OK" {
		t.Errorf("235 line %q", line)
	}
	wantCode(t, c, "IHAVE <n@x>", 435) // now held
	wantCode(t, c, "CHECK <n@x>", 438)

	// the deferred id is taken on the second offer
	wantCode(t, c, "IHAVE <d@x>", 335)
	sendArticle(t, c, []string{"Message-ID: <d@x>", "", "d"})
	wantStatus(t, c, 235)

	// rejected after the transfer, every time; CHECK still wants it, TAKETHIS gets 439
	for i := 0; i < 2; i++ {
		wantCode(t, c, "IHAVE <r@x>", 335)
		sendArticle(t, c, []string{"Message-ID: <r@x>", "", "spam"})
		if line := wantStatus(t, c, 437); !strings.Contains(line, "<r@x>") {
			t.Errorf("437 line %q", line)
		}
	}
	wantCode(t, c, "CHECK <r@x>", 238)
	if err := c.Send("TAKETHIS <r@x>\r\nMessage-ID: <r@x>\r\n\r\nspam\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 439)
	wantCode(t, c, "QUIT", 205)
	s.WaitOpen(t, 0, 2*time.Second)

	arts := s.Articles()
	if len(arts) != 5 || !reflect.DeepEqual(arts[0], stuffed) {
		t.Fatalf("Articles() = %q", arts)
	}
	if body := client.Unstuff(arts[0]); body[3] != ".leading dot" || body[4] != ".." {
		t.Errorf("unstuffed %q", body)
	}
	ids, wire := log.got()
	wantEqual(t, "OnArticle ids", ids, []string{"<n@x>", "<d@x>"})
	if !reflect.DeepEqual(wire[0], stuffed) {
		t.Errorf("OnArticle wire %q", wire[0])
	}
	if want := map[string]int{"IHAVE": 12, "AUTHINFO": 2, "CHECK": 2, "TAKETHIS": 1, "QUIT": 1}; !reflect.DeepEqual(s.Counts(), want) {
		t.Errorf("Counts() = %v, want %v (article lines are not commands)", s.Counts(), want)
	}
}

func TestPeerIHAVEPipelined(t *testing.T) {
	var log peerLog
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{
		Have:         map[string]bool{"<had@x>": true},
		Defer436Once: map[string]bool{"<d@x>": true},
		OnArticle:    log.onArticle,
	})})
	c := client.Dial(t, s.Addr())
	// commands without a payload go out in one write; the replies come in order
	if err := c.Send("MODE STREAM\r\nIHAVE <had@x>\r\nIHAVE <d@x>\r\nCHECK <a@x>\r\nIHAVE <a@x>\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{203, 435, 436, 238, 335} {
		wantStatus(t, c, want)
	}
	// the article and the next commands in one write after the 335
	if err := c.Send("Message-ID: <a@x>\r\n\r\n..a\r\n.\r\nIHAVE <a@x>\r\nCHECK <a@x>\r\nIHAVE <d@x>\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{235, 435, 438, 335} {
		wantStatus(t, c, want)
	}
	if err := c.Send("Message-ID: <d@x>\r\n\r\nd\r\n.\r\nQUIT\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 235)
	wantStatus(t, c, 205)
	s.WaitOpen(t, 0, 2*time.Second)
	ids, wire := log.got()
	wantEqual(t, "OnArticle ids", ids, []string{"<a@x>", "<d@x>"})
	wantEqual(t, "first article", wire[0], []string{"Message-ID: <a@x>", "", "..a"})
}

func TestPeerNoStream(t *testing.T) {
	var log peerLog
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{NoStream: true, OnArticle: log.onArticle})})
	c := client.Dial(t, s.Addr())
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	wantEqual(t, "caps", caps, []string{"VERSION 2", "IHAVE"})
	if line := wantCode(t, c, "MODE STREAM", 501); line != "501 Unknown MODE variant" {
		t.Errorf("MODE STREAM %q", line)
	}
	wantCode(t, c, "CHECK <a@x>", 500)
	// TAKETHIS is unknown: its article lines are read as commands, each answered 500
	if err := c.Send("TAKETHIS <a@x>\r\nSubject: s\r\n\r\nbody\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		wantStatus(t, c, 500)
	}
	// the fallback works
	wantCode(t, c, "IHAVE <a@x>", 335)
	sendArticle(t, c, []string{"Message-ID: <a@x>", "", "body"})
	wantStatus(t, c, 235)
	wantCode(t, c, "IHAVE <a@x>", 435)
	if err := c.ExpectNoPending(20 * time.Millisecond); err != nil {
		t.Errorf("extra bytes: %v", err)
	}
	wantCode(t, c, "QUIT", 205)
	s.WaitOpen(t, 0, 2*time.Second)
	if want := map[string]int{"CAPABILITIES": 1, "MODE": 1, "CHECK": 1, "TAKETHIS": 1, "SUBJECT:": 1, "": 1, "BODY": 1, ".": 1, "IHAVE": 2, "QUIT": 1}; !reflect.DeepEqual(s.Counts(), want) {
		t.Errorf("Counts() = %v, want %v", s.Counts(), want)
	}
	if arts := s.Articles(); len(arts) != 1 {
		t.Errorf("Articles() = %q, want only the IHAVE one", arts)
	}
	ids, _ := log.got()
	wantEqual(t, "OnArticle ids", ids, []string{"<a@x>"})
}
