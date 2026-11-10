package conformance

import (
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
)

// runReader runs checks against o the way the runner does (run.go comes in a later wave):
// one fresh session per check, CAPABILITIES read before the check, Needs skipped when not
// advertised, a dial error turned into a Result by sessionFailed. The checks run in parallel,
// each on its own connection, the ones that post (post) after all the others, as in the
// runner; the results come back in check order.
func runReader(t *testing.T, o Options, checks []check) []Result {
	t.Helper()
	out := make([]Result, len(checks))
	for _, posts := range []bool{false, true} {
		runReaderPhase(o, checks, posts, out)
	}
	return out
}

// runReaderPhase runs the checks whose Posts is posts into out, in parallel.
func runReaderPhase(o Options, checks []check, posts bool, out []Result) {
	var wg sync.WaitGroup
	for i, c := range checks {
		if c.Posts != posts {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := dial(o)
			if err != nil {
				out[i] = sessionFailed(c, err)
				return
			}
			defer s.close()
			_, _ = s.capabilities() // a failure stays in s.capsErr, as in the runner
			if c.Needs != "" && !s.has(c.Needs) && !s.has("MODE-READER") {
				out[i] = c.notAdvertised(c.Needs)
				return
			}
			out[i] = c.Run(s, c)
		}()
	}
	wg.Wait()
}

// postSkip is the Skip Detail of the post check in a runReader run: on a reader-only server
// POST is not advertised (runReader tests Needs first), else without Options.PostGroup the
// check skips itself (the runner would skip it before the dial).
func postSkip(o Options, so fakeserver.StoreOptions) string {
	switch {
	case so.ReaderOnly:
		return "POST not advertised"
	case o.PostGroup == "":
		return "no Options.PostGroup"
	}
	return ""
}

// testStore holds three groups: conf.test.a (three articles), conf.test.b (two) and the empty
// conf.test.empty. The first article of both a and b is one crosspost whose body has lines
// that start with a dot (dot-stuffed on the wire) and a line that is only a dot.
func testStore() *fakeserver.Store {
	st := fakeserver.NewStore()
	st.AddGroup("conf.test.a", "first conformance test group")
	st.AddGroup("conf.test.b", "second conformance test group")
	st.AddGroup("conf.test.empty", "a group without articles")
	cross := fakeserver.Article{
		Headers: []string{
			"From: tester@conformance.invalid",
			"Subject: crosspost with dot-leading body lines",
			"Newsgroups: conf.test.a,conf.test.b",
			"Message-ID: <cross-1@conformance.invalid>",
		},
		Body: []string{".a line that starts with a dot", "..two dots", ".", "the end"},
	}
	st.AddArticle("conf.test.a", cross)
	st.AddArticle("conf.test.b", cross)
	for _, id := range []string{"<a-2@conformance.invalid>", "<a-3@conformance.invalid>"} {
		st.AddArticle("conf.test.a", fakeserver.Article{
			Headers: []string{"From: tester@conformance.invalid", "Subject: plain " + id,
				"Newsgroups: conf.test.a", "Message-ID: " + id},
			Body: []string{"body of " + id},
		})
	}
	st.AddArticle("conf.test.b", fakeserver.Article{
		Headers: []string{"From: tester@conformance.invalid", "Subject: only in b",
			"Newsgroups: conf.test.b", "Message-ID: <b-2@conformance.invalid>",
			"References: <cross-1@conformance.invalid>"},
		Body: []string{"a follow-up"},
	})
	return st
}

// report formats results for a failure message.
func report(res []Result) string { return Report{Results: res}.String() }

func TestReaderChecksPassAgainstStore(t *testing.T) {
	cases := []struct {
		name   string
		so     fakeserver.StoreOptions
		o      Options
		prep   func(st *fakeserver.Store)
		detail map[string]string // check → substring of its Pass Detail
	}{
		{name: "default"},
		{name: "post-group", o: Options{PostGroup: "conf.test.b"}},
		{name: "reader-only", so: fakeserver.StoreOptions{ReaderOnly: true}},
		{name: "auth", so: fakeserver.StoreOptions{User: "u", Pass: "p"}, o: Options{User: "u", Pass: "p"}},
		// credentials given, but the server has no login (AUTHINFO answers 502): the checks go
		// on unauthenticated
		{name: "auth-not-offered", o: Options{User: "u", Pass: "p"}},
		// the high water mark article of conf.test.a is gone (expired): next-last cannot check
		// the 421 at the end and says so
		{
			name:   "expired-high",
			prep:   func(st *fakeserver.Store) { st.RemoveArticle("conf.test.a", 3) },
			detail: map[string]string{"next-last": "not checked"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := testStore()
			if tc.prep != nil {
				tc.prep(st)
			}
			srv := fakeserver.New(t, fakeserver.Options{Store: st, StoreOpts: tc.so})
			o := tc.o
			o.Addr = srv.Addr()
			res := runReader(t, o, readerChecks())
			for _, r := range res {
				if want := postSkip(o, tc.so); r.Name == "post" && want != "" {
					if r.Status != Skip || r.Detail != want {
						t.Errorf("%s: %s %q, want SKIP %q", r.Name, r.Status, r.Detail, want)
					}
					continue
				}
				if r.Status != Pass {
					t.Errorf("%s: %s %s", r.Name, r.Status, r.Detail)
				}
				if want, ok := tc.detail[r.Name]; ok && !strings.Contains(r.Detail, want) {
					t.Errorf("%s: Detail %q, want it to say %q", r.Name, r.Detail, want)
				}
			}
			if t.Failed() {
				t.Logf("report:\n%s", report(res))
			}
		})
	}
}

func TestOverviewFmtStrictMode(t *testing.T) {
	var checks []check
	for _, c := range readerChecks() {
		if c.Name == "list-overview-fmt" || c.Name == "over" {
			checks = append(checks, c)
		}
	}
	for _, tc := range []struct {
		name, bytes, lines string
		legacy             bool
	}{
		{"preferred", ":bytes", ":lines", false},
		{"case-insensitive", ":BYTES", ":LiNeS", false},
		{"legacy", "Bytes:", "Lines:", true},
		{"legacy-bytes", "Bytes:", ":lines", true},
		{"legacy-lines", ":bytes", "Lines:", true},
	} {
		for _, mode := range []struct {
			name   string
			strict bool
		}{{"default", false}, {"strict", true}} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				fields := []string{"Subject:", "From:", "Date:", "Message-ID:", "References:", tc.bytes, tc.lines, "Xref:full", ":extra"}
				srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{OverviewFmt: fields}})
				res := runReader(t, Options{Addr: srv.Addr(), StrictOverviewFmt: mode.strict}, checks)
				want := Pass
				if mode.strict && tc.legacy {
					want = Fail
				}
				if len(res) != 2 {
					t.Fatalf("got %d checks, want both overview consumers", len(res))
				}
				for _, r := range res {
					if r.Status != want {
						t.Errorf("%s: %s %s, want %s", r.Name, r.Status, r.Detail, want)
					}
					if r.Status == Fail && !strings.Contains(r.Detail, "LIST OVERVIEW.FMT line") {
						t.Errorf("%s: failed for another reason: %s", r.Name, r.Detail)
					}
				}
			})
		}
	}
}

// TestReaderChecksSkipWithoutCapability: a server without OVER skips the OVER checks (and
// only them); a store without any group skips the checks that need one, with the reason.
func TestReaderChecksSkipWithoutCapability(t *testing.T) {
	t.Parallel()
	t.Run("no-over", func(t *testing.T) {
		t.Parallel()
		srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{NoOver: true}})
		res := runReader(t, Options{Addr: srv.Addr()}, readerChecks())
		for _, r := range res {
			want := Pass
			if r.Name == "over" || r.Name == "over-errors" || r.Name == "post" {
				want = Skip
			}
			if r.Status != want {
				t.Errorf("%s: %s %s, want %s", r.Name, r.Status, r.Detail, want)
			}
		}
	})
	t.Run("no-groups", func(t *testing.T) {
		t.Parallel()
		srv := fakeserver.New(t, fakeserver.Options{Store: fakeserver.NewStore()})
		res := runReader(t, Options{Addr: srv.Addr()}, readerChecks())
		skipped := map[string]bool{"group": true, "listgroup": true, "next-last": true, "article": true,
			"head": true, "body": true, "stat": true, "no-current-article": true,
			"article-out-of-range": true, "over": true, "over-errors": true,
			"listgroup-range": true, "hdr": true, "newnews": true, "post": true}
		for _, r := range res {
			want := Pass
			if skipped[r.Name] {
				want = Skip
			}
			if r.Status != want {
				t.Errorf("%s: %s %s, want %s", r.Name, r.Status, r.Detail, want)
			}
			if r.Status == Skip && r.Detail == "" {
				t.Errorf("%s: Skip without a reason", r.Name)
			}
		}
	})
}

// TestReaderChecksCatchMistakes: a fake with one deliberate mistake fails exactly the check
// that is about it, naming the mistake; every other check passes (or, for the broken
// greeting, is skipped because no session can be opened).
func TestReaderChecksCatchMistakes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		srv      func(st *fakeserver.Store) fakeserver.Options
		fail     string // the check that must fail
		detail   string // a substring of its Detail
		skipRest bool   // every other check is skipped (no usable session)
	}{
		{
			// Handler mode: the greeting ends in a bare LF, then the store handler serves
			name: "bare-lf-greeting",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				h := fakeserver.StoreHandler(st, fakeserver.StoreOptions{})
				return fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
					if _, err := io.WriteString(c.Raw(), "200 fakeserver ready\n"); err != nil {
						return
					}
					h(c)
				}}
			},
			fail: "greeting", detail: "BareLF", skipRest: true,
		},
		{
			// OVER lines with 7 fields: :lines and Xref dropped
			name: "over-seven-fields",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, StoreOpts: fakeserver.StoreOptions{
					OverviewFields: func(_ int, _ string, f []string) []string { return f[:7] },
				}}
			},
			fail: "over", detail: "has 7 fields",
		},
		{
			// every ARTICLE block is cut after 4 lines and the connection closed: no terminator
			name: "article-no-terminator",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, Faults: fakeserver.Faults{
					DropInMultiline: map[string]int{"ARTICLE": 4},
				}}
			},
			fail: "article", detail: "NoTerminator",
		},
		{
			// a NUL line after the 411 for the unknown group
			name: "nul-after-411",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, Faults: fakeserver.Faults{
					OverrideLine: map[string][]string{"GROUP " + unknownGroup: {"411 No such newsgroup", "\x00"}},
				}}
			},
			fail: "group", detail: "Pending",
		},
		{
			// LISTGROUP lists the whole group whatever the range (servers before RFC 3977)
			name: "listgroup-ignores-range",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, StoreOpts: fakeserver.StoreOptions{ListgroupRange: "ignore"}}
			},
			fail: "listgroup-range", detail: "outside the range",
		},
		{
			// LIST ACTIVE.TIMES with a time that is not a number
			name: "active-times-bad-time",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, Faults: fakeserver.Faults{
					OverrideLine: map[string][]string{"LIST ACTIVE.TIMES": {"215 Information follows", "conf.test.a yesterday news@x", "."}},
				}}
			},
			fail: "list-active-times", detail: "is not a number",
		},
		{
			// LIST HEADERS names a header with its colon
			name: "list-headers-colon",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, Faults: fakeserver.Faults{
					OverrideLine: map[string][]string{"LIST HEADERS": {"215 Fields follow", "Subject:", ":bytes", "."}},
				}}
			},
			fail: "list-headers", detail: "without its colon",
		},
		{
			// HDR values out of step with the articles
			name: "hdr-wrong-value",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, StoreOpts: fakeserver.StoreOptions{
					HdrLines: func(_ int, _, _ string, lines []string) []string {
						for i, l := range lines {
							n, _, _ := strings.Cut(l, " ")
							lines[i] = n + " not the subject"
						}
						return lines
					},
				}}
			},
			fail: "hdr", detail: "want the Subject",
		},
		{
			// NEWGROUPS lists a group whatever the date
			name: "newgroups-ignores-date",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, Faults: fakeserver.Faults{
					Override: map[string][]string{"NEWGROUPS": {"231 New newsgroups follow", "conf.test.a 3 1 y", "."}},
				}}
			},
			fail: "newgroups", detail: "two days ahead",
		},
		{
			// NEWNEWS lists an article whatever the date
			name: "newnews-ignores-date",
			srv: func(st *fakeserver.Store) fakeserver.Options {
				return fakeserver.Options{Store: st, Faults: fakeserver.Faults{
					Override: map[string][]string{"NEWNEWS": {"230 New articles follow", "<cross-1@conformance.invalid>", "."}},
				}}
			},
			fail: "newnews", detail: "two days ahead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := fakeserver.New(t, tc.srv(testStore()))
			res := runReader(t, Options{Addr: srv.Addr()}, readerChecks())
			for _, r := range res {
				switch {
				case r.Name == tc.fail:
					if r.Status != Fail || !strings.Contains(r.Detail, tc.detail) {
						t.Errorf("%s: %s %q, want FAIL naming %q", r.Name, r.Status, r.Detail, tc.detail)
					}
				case tc.skipRest:
					if r.Status != Skip {
						t.Errorf("%s: %s %s, want SKIP", r.Name, r.Status, r.Detail)
					}
				case r.Name == "post": // no Options.PostGroup
					if r.Status != Skip {
						t.Errorf("%s: %s %s, want SKIP", r.Name, r.Status, r.Detail)
					}
				case r.Status != Pass:
					t.Errorf("%s: %s %s, want PASS", r.Name, r.Status, r.Detail)
				}
			}
			if t.Failed() {
				t.Logf("report:\n%s", report(res))
			}
		})
	}
}

// TestPostCatchesLostArticle: a server that answers 240 to POST but never files the article
// fails post once the hang guard runs out. Not parallel: it replaces asNewID.
// TestPostLostArticleNoted: a 240 whose message-id STAT never finds passes, with a Detail
// saying so: a server may assign its own message-id or queue the article for a moderator.
func TestPostLostArticleNoted(t *testing.T) {
	old := asNewID
	asNewID = func(name string) string { return "<" + name + "@conformance.invalid>" }
	t.Cleanup(func() { asNewID = old })
	id := asNewID("post")
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), Faults: fakeserver.Faults{
		OverrideLine: map[string][]string{"STAT " + id: {"430 No article with that message-id"}},
	}})
	var post []check
	for _, c := range readerChecks() {
		if c.Name == "post" {
			post = append(post, c)
		}
	}
	r := runReader(t, Options{Addr: srv.Addr(), PostGroup: "conf.test.b", Deadline: 300 * time.Millisecond}, post)[0]
	if r.Status != Pass || !strings.Contains(r.Detail, "may have assigned its own message-id") {
		t.Errorf("post: %s %q, want PASS with a Detail that the message-id was not found", r.Status, r.Detail)
	}
	// the article did arrive: the store has it, only STAT was told to deny it
	if n := len(srv.Articles()); n != 1 {
		t.Errorf("%d articles posted on the wire, want 1", n)
	}
}

// TestModeReaderAuthFirst: a server that answers MODE READER with 480 until the client logs in
// (it lists AUTHINFO). Without Options.User mode-reader passes and says so; with it the check
// logs in and MODE READER must then work; a 480 from a server that does not list AUTHINFO (yet)
// passes with a Detail saying so.
func TestModeReaderAuthFirst(t *testing.T) {
	t.Parallel()
	caps := func(extra ...string) []string {
		return slices.Concat([]string{"101 Capability list:", "VERSION 2", "READER"}, extra, []string{"."})
	}
	authFirst := func(retry bool) []fakeserver.Step {
		steps := []fakeserver.Step{
			{Expect: "CAPABILITIES", Reply: caps("AUTHINFO USER")},
			{Expect: "MODE READER", Reply: []string{"480 Authentication required"}},
		}
		if retry {
			steps = append(steps,
				fakeserver.Step{Expect: "AUTHINFO USER u", Reply: []string{"381 Password required"}},
				fakeserver.Step{Expect: "AUTHINFO PASS p", Reply: []string{"281 Authentication accepted"}},
				fakeserver.Step{Expect: "CAPABILITIES", Reply: caps()},
				fakeserver.Step{Expect: "MODE READER", Reply: []string{"200 Posting allowed"}},
				fakeserver.Step{Expect: "CAPABILITIES", Reply: caps()},
			)
		}
		return steps
	}
	var modeReader []check
	for _, c := range readerChecks() {
		if c.Name == "mode-reader" {
			modeReader = append(modeReader, c)
		}
	}
	cases := []struct {
		name   string
		steps  []fakeserver.Step
		o      Options
		status Status
		detail string
	}{
		{name: "no-user", steps: authFirst(false), status: Pass, detail: "needs authentication first"},
		{name: "login-retry", steps: authFirst(true), o: Options{User: "u", Pass: "p"}, status: Pass, detail: "checked again after the login"},
		{
			name: "no-authinfo",
			steps: []fakeserver.Step{
				{Expect: "CAPABILITIES", Reply: caps()},
				{Expect: "MODE READER", Reply: []string{"480 Authentication required"}},
			},
			status: Pass, detail: "does not list AUTHINFO yet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := fakeserver.New(t, fakeserver.Options{Script: tc.steps})
			o := tc.o
			o.Addr = srv.Addr()
			r := runReader(t, o, modeReader)[0]
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("mode-reader: %s %q, want %s naming %q", r.Status, r.Detail, tc.status, tc.detail)
			}
			if m := srv.Mismatches(); len(m) != 0 {
				t.Errorf("lines the script did not expect: %q", m)
			}
		})
	}
}

// TestSessionFailed: a dial error that is not a greeting wire mistake fails the check itself.
func TestSessionFailed(t *testing.T) {
	c := check{Name: "group", RFC: "RFC 3977 §6.1.1"}
	if r := sessionFailed(c, &net.OpError{Op: "dial", Err: errors.New("connection refused")}); r.Status != Fail {
		t.Errorf("refused dial: %s %q, want FAIL", r.Status, r.Detail)
	}
	strict := &client.StrictError{Kind: client.BareCR, Line: "200 a\rb\r\n"}
	if r := sessionFailed(c, strict); r.Status != Skip {
		t.Errorf("broken greeting, other check: %s %q, want SKIP", r.Status, r.Detail)
	}
	g := check{Name: "greeting"}
	if r := sessionFailed(g, strict); r.Status != Fail || !strings.Contains(r.Detail, "BareCR") {
		t.Errorf("broken greeting, greeting check: %s %q, want FAIL naming BareCR", r.Status, r.Detail)
	}
}

// TestReaderCheckNames: names are unique kebab-case, every check names its RFC section.
func TestReaderCheckNames(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range readerChecks() {
		if seen[c.Name] {
			t.Errorf("duplicate check name %q", c.Name)
		}
		seen[c.Name] = true
		if c.Name == "" || strings.ToLower(c.Name) != c.Name || strings.ContainsAny(c.Name, " _") {
			t.Errorf("check name %q is not kebab-case", c.Name)
		}
		if !strings.HasPrefix(c.RFC, "RFC 3977 §") {
			t.Errorf("check %q: RFC %q", c.Name, c.RFC)
		}
		if c.Run == nil || c.Auth || c.Posts != (c.Name == "post") {
			t.Errorf("check %q: Run nil, flagged Auth, or Posts on a check that does not post", c.Name)
		}
	}
}

// TestParseHelpers: LIST ACTIVE takes any status token (§7.6.3.2), numbers have 1 to 16 digits.
func TestParseHelpers(t *testing.T) {
	for _, l := range []string{"a.b 10 1 y", "a.b 0000000010 0000000001 m", "a.b 2 1 =c.d", "a.b 2 1 private"} {
		if _, err := parseActive(l); err != nil {
			t.Errorf("parseActive(%q): %v", l, err)
		}
	}
	for _, l := range []string{"a.b 10 1", "a.b x 1 y", "a.b 10 1 y extra"} {
		if _, err := parseActive(l); err == nil {
			t.Errorf("parseActive(%q): no error", l)
		}
	}
	if _, err := parseNum("1234567890123456"); err != nil {
		t.Errorf("16 digits: %v", err)
	}
	if _, err := parseNum("12345678901234567"); err == nil {
		t.Error("17 digits: no error")
	}
	if n := farNumber(maxArticle - 5); n != maxArticle {
		t.Errorf("farNumber near the 32-bit limit: %d", n)
	}
}
