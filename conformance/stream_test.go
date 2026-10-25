package conformance

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
)

func TestStreamChecksPeer(t *testing.T) {
	po := &fakeserver.PeerOptions{User: "u", Pass: "p"}
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(po)})
	res := asRunAll(t, Options{Addr: s.Addr(), User: "u", Pass: "p", PostGroup: "alt.test"}, streamChecks())
	for name, r := range res {
		if r.Status != Pass {
			t.Errorf("%s: %v %s, want PASS", name, r.Status, r.Detail)
		}
	}
	if d := res["ihave-malformed"].Detail; !strings.Contains(d, "435") {
		t.Errorf("ihave-malformed Detail %q, want the 435 recorded", d)
	}
	arts := s.Articles()
	s.Close() // Have may be read once the server is closed

	// takethis, check-have, takethis-duplicate, ihave and ihave-have each delivered one article
	var held []string
	for id := range po.Have {
		if !strings.HasSuffix(id, "@conformance.invalid>") {
			t.Errorf("Have holds %q, want only conformance ids", id)
		}
		held = append(held, id)
	}
	if len(held) != 5 {
		t.Errorf("Have holds %d ids %q, want 5", len(held), held)
	}
	// six articles on the wire (takethis-duplicate sends its article twice), each dot-stuffed
	if len(arts) != 6 {
		t.Fatalf("%d articles on the wire, want 6", len(arts))
	}
	for i, wire := range arts {
		if !slices.Contains(wire, "Newsgroups: alt.test") || !slices.Contains(wire, "Path: conformance.invalid!not-for-mail") {
			t.Errorf("article %d: headers %q", i, wire)
		}
		if !slices.ContainsFunc(wire, func(l string) bool { return strings.HasPrefix(l, "..dotted line") }) {
			t.Errorf("article %d: no dot-stuffed line in %q", i, wire)
		}
		if !slices.ContainsFunc(client.Unstuff(wire), func(l string) bool { return strings.HasPrefix(l, ".dotted line") }) {
			t.Errorf("article %d: un-stuffed body lacks the .dotted line: %q", i, wire)
		}
	}
}

func TestStreamChecksNoStream(t *testing.T) {
	po := &fakeserver.PeerOptions{NoStream: true}
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(po)})
	res := asRunAll(t, Options{Addr: s.Addr(), PostGroup: "alt.test"}, streamChecks())
	for _, name := range []string{"mode-stream", "check-new", "check-pipelined", "takethis", "check-have", "takethis-duplicate"} {
		if r := res[name]; r.Status != Skip {
			t.Errorf("%s: %v %s, want SKIP (no STREAMING)", name, r.Status, r.Detail)
		}
	}
	if r := res["mode-stream-capability"]; r.Status != Pass || !strings.Contains(r.Detail, "no streaming") {
		t.Errorf("mode-stream-capability: %v %q, want PASS no streaming", r.Status, r.Detail)
	}
	for _, name := range []string{"ihave", "ihave-have", "ihave-malformed"} {
		if r := res[name]; r.Status != Pass {
			t.Errorf("%s: %v %s, want PASS", name, r.Status, r.Detail)
		}
	}
	if n := s.Count("CHECK") + s.Count("TAKETHIS"); n != 0 {
		t.Errorf("%d CHECK/TAKETHIS sent to a peer without STREAMING", n)
	}
}

func TestStreamChecksNoPostGroup(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(&fakeserver.PeerOptions{})})
	res := asRunAll(t, Options{Addr: s.Addr()}, streamChecks())
	for _, c := range streamChecks() {
		want := Pass
		if c.Posts {
			want = Skip
		}
		if r := res[c.Name]; r.Status != want {
			t.Errorf("%s: %v %s, want %v", c.Name, r.Status, r.Detail, want)
		}
	}
	if n := len(s.Articles()); n != 0 {
		t.Errorf("%d articles sent without a PostGroup", n)
	}
}

func TestModeStreamWithoutCapability(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: []string{"101 caps", "VERSION 2", "IHAVE", "."}},
		{Expect: "MODE STREAM", Reply: []string{"203 streaming anyway"}},
	}})
	r := asRunCheck(t, Options{Addr: s.Addr()}, asFind(t, streamChecks(), "mode-stream-capability"))
	if r.Status != Fail || !strings.Contains(r.Detail, "does not list STREAMING") {
		t.Fatalf("got %v %q, want FAIL 203 without STREAMING", r.Status, r.Detail)
	}
}

// TestCheckPipelinedOrder: a server that answers pipelined CHECKs out of order fails.
func TestCheckPipelinedOrder(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Handler: func(c *fakeserver.Conn) {
		var ids []string
		for {
			line, err := c.ReadLine()
			if err != nil {
				return
			}
			var reply []string
			switch f := strings.Fields(line); strings.ToUpper(f[0]) {
			case "CAPABILITIES":
				reply = []string{"101 caps", "VERSION 2", "STREAMING", "."}
			case "MODE":
				reply = []string{"203 streaming"}
			case "CHECK":
				if ids = append(ids, f[1]); len(ids) < 3 {
					continue
				}
				for i := len(ids) - 1; i >= 0; i-- { // reversed
					reply = append(reply, "238 "+ids[i])
				}
				ids = nil
			default:
				reply = []string{"500 what"}
			}
			if err := c.WriteLines(reply...); err != nil {
				return
			}
		}
	}})
	r := asRunCheck(t, Options{Addr: s.Addr()}, asFind(t, streamChecks(), "check-pipelined"))
	if r.Status != Fail || !strings.Contains(r.Detail, "does not name") {
		t.Fatalf("got %v %q, want FAIL naming the wrong id", r.Status, r.Detail)
	}
}

func TestAsArticleStuffing(t *testing.T) {
	wire := asArticle("alt.test", "x", "<x@conformance.invalid>")
	if !strings.HasSuffix(wire, "\r\n.\r\n") || strings.Contains(strings.ReplaceAll(wire, "\r\n", ""), "\n") {
		t.Fatalf("article not CRLF with terminator: %q", wire)
	}
	lines := strings.Split(strings.TrimSuffix(wire, "\r\n.\r\n"), "\r\n")
	for _, l := range lines {
		if strings.HasPrefix(l, ".") && !strings.HasPrefix(l, "..") {
			t.Errorf("unstuffed line %q", l)
		}
	}
	if !slices.Contains(lines, "") || !slices.Contains(lines, "Message-ID: <x@conformance.invalid>") {
		t.Errorf("article lines %q", lines)
	}
}
