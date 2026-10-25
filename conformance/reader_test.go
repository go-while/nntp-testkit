package conformance

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
)

// runReader runs checks against o the way the runner does (run.go comes in a later wave):
// one fresh session per check, CAPABILITIES read before the check, Needs skipped when not
// advertised, a dial error turned into a Result by sessionFailed. The checks run in parallel,
// each on its own connection; the results come back in check order.
func runReader(t *testing.T, o Options, checks []check) []Result {
	t.Helper()
	out := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
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
				out[i] = c.skip("server does not advertise " + c.Needs)
				return
			}
			out[i] = c.Run(s, c)
		}()
	}
	wg.Wait()
	return out
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
		name string
		so   fakeserver.StoreOptions
		o    Options
	}{
		{name: "default"},
		{name: "post-group", o: Options{PostGroup: "conf.test.b"}},
		{name: "reader-only", so: fakeserver.StoreOptions{ReaderOnly: true}},
		{name: "auth", so: fakeserver.StoreOptions{User: "u", Pass: "p"}, o: Options{User: "u", Pass: "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: tc.so})
			o := tc.o
			o.Addr = srv.Addr()
			res := runReader(t, o, readerChecks())
			for _, r := range res {
				if r.Status != Pass {
					t.Errorf("%s: %s %s", r.Name, r.Status, r.Detail)
				}
			}
			if t.Failed() {
				t.Logf("report:\n%s", report(res))
			}
		})
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
			if r.Name == "over" || r.Name == "over-errors" {
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
			"article-out-of-range": true, "over": true, "over-errors": true}
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
		if c.Run == nil || c.Posts || c.Auth {
			t.Errorf("check %q: Run nil or flagged Posts/Auth", c.Name)
		}
	}
}
