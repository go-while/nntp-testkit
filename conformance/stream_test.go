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

// asLoginCapsPeer is a transit peer in the style of innd: before AUTHINFO it lists only
// VERSION 2 and AUTHINFO USER and answers 480 to everything but CAPABILITIES, AUTHINFO and
// QUIT (MODE STREAM included); after it lists IHAVE and STREAMING and takes CHECK, TAKETHIS
// and IHAVE.
func asLoginCapsPeer(c *fakeserver.Conn) {
	authed, userOK := false, false
	have := map[string]bool{}
	for {
		line, err := c.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		var reply []string
		switch {
		case verb == "CAPABILITIES" && authed:
			reply = []string{"101 caps", "VERSION 2", "IHAVE", "STREAMING", "."}
		case verb == "CAPABILITIES":
			reply = []string{"101 caps", "VERSION 2", "AUTHINFO USER", "."}
		case verb == "AUTHINFO" && arg == "USER u":
			userOK, reply = true, []string{"381 password"}
		case verb == "AUTHINFO" && arg == "PASS p" && userOK:
			authed, reply = true, []string{"281 ok"}
		case verb == "AUTHINFO":
			reply = []string{"481 no"}
		case verb == "QUIT":
			_ = c.WriteLines("205 bye")
			return
		case verb == "TAKETHIS":
			if _, err := c.ReadDotBlock(); err != nil {
				return
			}
			switch {
			case !authed:
				reply = []string{"480 login first"}
			case have[arg]:
				reply = []string{"439 " + arg}
			default:
				have[arg], reply = true, []string{"239 " + arg}
			}
		case !authed:
			reply = []string{"480 login first"}
		case verb == "MODE" && strings.EqualFold(arg, "STREAM"):
			reply = []string{"203 streaming"}
		case verb == "CHECK" && have[arg]:
			reply = []string{"438 " + arg}
		case verb == "CHECK":
			reply = []string{"238 " + arg}
		case verb == "IHAVE" && (!strings.HasPrefix(arg, "<") || !strings.HasSuffix(arg, ">")):
			reply = []string{"435 bad id"}
		case verb == "IHAVE" && have[arg]:
			reply = []string{"435 duplicate"}
		case verb == "IHAVE":
			if err := c.WriteLines("335 send it"); err != nil {
				return
			}
			if _, err := c.ReadDotBlock(); err != nil {
				return
			}
			have[arg], reply = true, []string{"235 ok"}
		default:
			reply = []string{"500 what"}
		}
		if err := c.WriteLines(reply...); err != nil {
			return
		}
	}
}

// TestStreamChecksCapsAfterLogin: STREAMING and IHAVE listed only after AUTHINFO; every stream
// check logs in, reads the list again and runs.
func TestStreamChecksCapsAfterLogin(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Handler: asLoginCapsPeer})
	for name, r := range asRunAll(t, Options{Addr: s.Addr(), User: "u", Pass: "p", PostGroup: "alt.test"}, streamChecks()) {
		if r.Status != Pass {
			t.Errorf("%s: %v %s, want PASS", name, r.Status, r.Detail)
		}
	}
	if n := len(s.Articles()); n != 6 {
		t.Errorf("%d articles on the wire, want 6", n)
	}
}

// TestStreamChecksSoftReplies: a deferring and filtering peer (431, 436, 439, 437) makes the
// checks skip with the reply in Detail, not fail.
func TestStreamChecksSoftReplies(t *testing.T) {
	old := asNewID
	asNewID = func(name string) string { return "<" + name + "@conformance.invalid>" }
	t.Cleanup(func() { asNewID = old })
	id := func(name string) string { return asNewID(name) }
	po := &fakeserver.PeerOptions{
		Defer431Once: map[string]bool{id("check-new"): true, id("check-pipelined-2"): true},
		Defer436Once: map[string]bool{id("ihave-have"): true},
		Reject: map[string]bool{id("takethis"): true, id("check-have"): true,
			id("takethis-duplicate"): true, id("ihave"): true},
	}
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(po)})
	res := asRunAll(t, Options{Addr: s.Addr(), PostGroup: "alt.test"}, streamChecks())
	want := map[string]struct {
		st     Status
		detail string
	}{
		"mode-stream":            {Pass, ""},
		"mode-stream-capability": {Pass, ""},
		"check-new":              {Skip, "server deferred: 431 " + id("check-new")},
		"check-pipelined":        {Pass, `in order; not 238: "431 ` + id("check-pipelined-2") + `"`},
		"takethis":               {Skip, "server rejected the article: 439 " + id("takethis")},
		"check-have":             {Skip, "server rejected the article: 439 " + id("check-have")},
		"takethis-duplicate":     {Skip, "server rejected the article: 439 " + id("takethis-duplicate")},
		"ihave":                  {Skip, "server rejected the article: 437 Rejected " + id("ihave")},
		"ihave-have":             {Skip, "server deferred: 436 Retry later"},
		"ihave-malformed":        {Pass, `malformed id answered "435 Syntax error in message-ID"`},
	}
	for name, w := range want {
		if r := res[name]; r.Status != w.st || r.Detail != w.detail {
			t.Errorf("%s: got %v %q, want %v %q", name, r.Status, r.Detail, w.st, w.detail)
		}
	}
}

// TestStreamSoftReplyWrongID: a legal negative reply that names another message-id fails.
func TestStreamSoftReplyWrongID(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: []string{"101 caps", "VERSION 2", "STREAMING", "."}},
		{Expect: "MODE STREAM", Reply: []string{"203 streaming"}},
		{Expect: "CHECK .*", Reply: []string{"431 <other@x>"}},
	}})
	r := asRunCheck(t, Options{Addr: s.Addr()}, asFind(t, streamChecks(), "check-new"))
	if r.Status != Fail || !strings.Contains(r.Detail, "does not name") {
		t.Fatalf("got %v %q, want FAIL naming the wrong id", r.Status, r.Detail)
	}
}

// TestIhaveMalformed335: a server that wants an article for a malformed id fails, and the
// transfer is ended with an empty article whose reply is read.
func TestIhaveMalformed335(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: []string{"101 caps", "VERSION 2", "IHAVE", "."}},
		{Expect: "IHAVE conformance-no-message-id", Reply: []string{"335 send it"}},
		{Expect: `\.`, Reply: []string{"437 empty"}},
	}})
	r := asRunCheck(t, Options{Addr: s.Addr()}, asFind(t, streamChecks(), "ihave-malformed"))
	if r.Status != Fail || !strings.Contains(r.Detail, `the empty article got "437 empty"`) {
		t.Fatalf("got %v %q, want FAIL with the aborted transfer", r.Status, r.Detail)
	}
}
