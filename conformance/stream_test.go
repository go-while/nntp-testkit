package conformance

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func init() {
	// short back-off in the tests; the production values are 100ms doubling up to 1s
	asRetryStart, asRetryMax = 5*time.Millisecond, 20*time.Millisecond
}

// commitFake is a peer that takes an article (TAKETHIS or IHAVE) and then answers the
// re-offer of its id as a server that commits asynchronously does: Defers answers of the
// deferral code (431 to CHECK, 436 to IHAVE), then Final (438 / 238 to CHECK, 435 / 335 to
// IHAVE). Offers counts the CHECK or IHAVE commands for the id taken before.
type commitFake struct {
	Defers int
	Final  int
	offers atomic.Int32
}

func (f *commitFake) handler(c *fakeserver.Conn) {
	taken := map[string]bool{}
	article := func() { // the lines of an article up to the terminator
		for {
			l, err := c.ReadLine()
			if err != nil || l == "." {
				return
			}
		}
	}
	for {
		line, err := c.ReadLine()
		if err != nil {
			return
		}
		fs := strings.Fields(line)
		if len(fs) == 0 {
			continue
		}
		var reply []string
		switch strings.ToUpper(fs[0]) {
		case "CAPABILITIES":
			reply = []string{"101 caps", "VERSION 2", "STREAMING", "IHAVE", "."}
		case "MODE":
			reply = []string{"203 streaming"}
		case "TAKETHIS":
			article()
			taken[fs[1]] = true
			reply = []string{"239 " + fs[1]}
		case "CHECK":
			switch n := int(f.offers.Add(1)); {
			case !taken[fs[1]]:
				reply = []string{"238 " + fs[1]}
			case n <= f.Defers:
				reply = []string{"431 " + fs[1]}
			default:
				reply = []string{fmt.Sprintf("%d %s", f.Final, fs[1])}
			}
		case "IHAVE":
			switch seen := taken[fs[1]]; {
			case !seen:
				if err := c.WriteLines("335 send it"); err != nil {
					return
				}
				article()
				taken[fs[1]] = true
				reply = []string{"235 ok"}
			case int(f.offers.Add(1)) <= f.Defers:
				reply = []string{"436 try later"}
			case f.Final == 335:
				if err := c.WriteLines("335 send it again"); err != nil {
					return
				}
				article()
				reply = []string{"437 no thanks"}
			default:
				reply = []string{"435 have it"}
			}
		default:
			reply = []string{"500 what"}
		}
		if err := c.WriteLines(reply...); err != nil {
			return
		}
	}
}

func commitCheck(t *testing.T, f *commitFake, name string, wait time.Duration) (Result, time.Duration) {
	t.Helper()
	s := fakeserver.New(t, fakeserver.Options{Handler: f.handler})
	start := time.Now()
	r := asRunCheck(t, Options{Addr: s.Addr(), PostGroup: "alt.test", CommitWait: wait}, asFind(t, streamChecks(), name))
	took := time.Since(start)
	t.Logf("%s %s (%v)", r.Status, r.Detail, took)
	return r, took
}

// TestCheckHaveRetry: a 431 to the re-offer while the article is committed is retried; the
// Pass names the retries.
func TestCheckHaveRetry(t *testing.T) {
	f := &commitFake{Defers: 2, Final: 438}
	r, _ := commitCheck(t, f, "check-have", 5*time.Second)
	if r.Status != Pass || !strings.Contains(r.Detail, "re-offered 2 times") {
		t.Errorf("got %v %q, want PASS naming 2 re-offers", r.Status, r.Detail)
	}
	if n := f.offers.Load(); n != 3 {
		t.Errorf("%d CHECKs for the taken id, want 3", n)
	}
	// no deferral: Pass without a note
	if r, _ := commitCheck(t, &commitFake{Final: 438}, "check-have", 0); r.Status != Pass || r.Detail != "" {
		t.Errorf("no deferral: %v %q, want a plain PASS", r.Status, r.Detail)
	}
}

// TestCheckHaveDeferredForever: a server that always defers is a Skip when the window ends,
// after more than one offer; CommitWait < 0 asks once.
func TestCheckHaveDeferredForever(t *testing.T) {
	f := &commitFake{Defers: 1 << 20, Final: 438}
	r, took := commitCheck(t, f, "check-have", 200*time.Millisecond)
	if r.Status != Skip || !strings.HasPrefix(r.Detail, "server deferred: 431 <") || !strings.Contains(r.Detail, "still deferred after") {
		t.Errorf("got %v %q, want SKIP still deferred", r.Status, r.Detail)
	}
	if took < 200*time.Millisecond || took > 3*time.Second {
		t.Errorf("took %v, want about the 200ms window", took)
	}
	if n := f.offers.Load(); n < 3 {
		t.Errorf("%d offers within the window, want several", n)
	}

	f = &commitFake{Defers: 1 << 20, Final: 438}
	r, _ = commitCheck(t, f, "check-have", -1)
	if r.Status != Skip || strings.Contains(r.Detail, "still deferred") || !strings.HasPrefix(r.Detail, "server deferred: 431 <") {
		t.Errorf("CommitWait -1: got %v %q, want the plain SKIP", r.Status, r.Detail)
	}
	if n := f.offers.Load(); n != 1 {
		t.Errorf("CommitWait -1: %d offers, want 1", n)
	}
}

// TestCheckHaveWants: a 238 for an article the server took (before or after deferrals) fails.
func TestCheckHaveWants(t *testing.T) {
	for _, defers := range []int{0, 2} {
		r, _ := commitCheck(t, &commitFake{Defers: defers, Final: 238}, "check-have", 5*time.Second)
		if r.Status != Fail || !strings.Contains(r.Detail, `got "238 <`) || !strings.Contains(r.Detail, "want 438") {
			t.Errorf("%d deferrals: got %v %q, want FAIL 238", defers, r.Status, r.Detail)
		}
		if defers > 0 && !strings.Contains(r.Detail, "(after 2 re-offers)") {
			t.Errorf("Detail %q lacks the re-offers", r.Detail)
		}
	}
}

// TestIhaveHaveRetry: as TestCheckHaveRetry for IHAVE and 436 / 435.
func TestIhaveHaveRetry(t *testing.T) {
	f := &commitFake{Defers: 2, Final: 435}
	r, _ := commitCheck(t, f, "ihave-have", 5*time.Second)
	if r.Status != Pass || !strings.Contains(r.Detail, "re-offered 2 times") {
		t.Errorf("got %v %q, want PASS naming 2 re-offers", r.Status, r.Detail)
	}
	if n := f.offers.Load(); n != 3 {
		t.Errorf("%d IHAVEs for the taken id, want 3", n)
	}
	if r, _ := commitCheck(t, &commitFake{Final: 435}, "ihave-have", 0); r.Status != Pass || r.Detail != "" {
		t.Errorf("no deferral: %v %q, want a plain PASS", r.Status, r.Detail)
	}
}

func TestIhaveHaveDeferredForever(t *testing.T) {
	f := &commitFake{Defers: 1 << 20, Final: 435}
	r, took := commitCheck(t, f, "ihave-have", 200*time.Millisecond)
	if r.Status != Skip || !strings.HasPrefix(r.Detail, "server deferred: 436") || !strings.Contains(r.Detail, "still deferred after") {
		t.Errorf("got %v %q, want SKIP still deferred", r.Status, r.Detail)
	}
	if took < 200*time.Millisecond || took > 3*time.Second {
		t.Errorf("took %v, want about the 200ms window", took)
	}
	f = &commitFake{Defers: 1 << 20, Final: 435}
	r, _ = commitCheck(t, f, "ihave-have", -1)
	if r.Status != Skip || r.Detail != "server deferred: 436 try later" || f.offers.Load() != 1 {
		t.Errorf("CommitWait -1: got %v %q after %d offers, want the plain SKIP after 1", r.Status, r.Detail, f.offers.Load())
	}
}

// TestIhaveHaveWants335: a 335 to the re-offer (before or after deferrals) fails, the article
// the server asks for is ended with an empty one and its reply read.
func TestIhaveHaveWants335(t *testing.T) {
	for _, defers := range []int{0, 2} {
		r, _ := commitCheck(t, &commitFake{Defers: defers, Final: 335}, "ihave-have", 5*time.Second)
		if r.Status != Fail || !strings.Contains(r.Detail, "want 435") || !strings.Contains(r.Detail, `the empty article got "437 no thanks"`) {
			t.Errorf("%d deferrals: got %v %q, want FAIL 335 with the transfer aborted", defers, r.Status, r.Detail)
		}
		if defers > 0 && !strings.Contains(r.Detail, "after 2 re-offers") {
			t.Errorf("Detail %q lacks the re-offers", r.Detail)
		}
	}
}

func TestCommitWaitDefault(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{0: defaultCommitWait, -1: 0, time.Second: time.Second} {
		if got := (Options{CommitWait: in}).commitWait(); got != want {
			t.Errorf("CommitWait %v: window %v, want %v", in, got, want)
		}
	}
}

// TestAllowReplyIhave: the 335 of ihave-have is allowed by check, command and code, even
// though the Fail went through the abort of the transfer.
func TestAllowReplyIhave(t *testing.T) {
	f := &commitFake{Defers: 1, Final: 335}
	s := fakeserver.New(t, fakeserver.Options{Handler: f.handler})
	c := asFind(t, streamChecks(), "ihave-have")
	o := Options{Addr: s.Addr(), PostGroup: "alt.test", CommitWait: 5 * time.Second}
	r := runOne(o, c)
	if r.Status != Fail || r.Cmd != "IHAVE" || r.Code != 335 {
		t.Fatalf("got %+v, want FAIL with Cmd IHAVE, Code 335", r)
	}
	o.AllowReply = []AllowReply{{"ihave-have", "IHAVE", 335}}
	r = runOne(o, c)
	if r.Status != Skip || !strings.HasPrefix(r.Detail, "allowed: IHAVE <") {
		t.Errorf("allowed: got %+v, want SKIP allowed", r)
	}
}

// TestAllowReplyWrongID: a legal negative reply that names another message-id fails without
// a Cmd and Code, so no AllowReply can hide it; a really wrong code still has both.
func TestAllowReplyWrongID(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: []string{"101 caps", "VERSION 2", "STREAMING", "."}},
		{Expect: "MODE STREAM", Reply: []string{"203 streaming"}},
		{Expect: "CHECK .*", Reply: []string{"431 <other@x>"}},
	}})
	c := asFind(t, streamChecks(), "check-new")
	r := runOne(Options{Addr: s.Addr(), AllowReply: []AllowReply{{"check-new", "CHECK", 431}}}, c)
	if r.Status != Fail || r.Cmd != "" || r.Code != 0 || !strings.Contains(r.Detail, "does not name") {
		t.Errorf("wrong id: %+v, want FAIL without Cmd and Code", r)
	}

	s = fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: []string{"101 caps", "VERSION 2", "STREAMING", "."}},
		{Expect: "MODE STREAM", Reply: []string{"203 streaming"}},
		{Expect: "CHECK .*", Reply: []string{"500 what"}},
	}})
	r = runOne(Options{Addr: s.Addr()}, c)
	if r.Status != Fail || r.Cmd != "CHECK" || r.Code != 500 {
		t.Errorf("wrong code: %+v, want FAIL with Cmd CHECK, Code 500", r)
	}
}
