package conformance

import (
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// names returns the names of list.
func names(list []check) []string {
	var out []string
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

// byName returns the results of r by check name.
func byName(r Report) map[string]Result {
	out := make(map[string]Result, len(r.Results))
	for _, res := range r.Results {
		out[res.Name] = res
	}
	return out
}

// mustCheck runs Check and fails t on an error; the report is logged.
func mustCheck(t *testing.T, o Options) Report {
	t.Helper()
	r, err := Check(o)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	t.Logf("report:\n%s", r)
	return r
}

// bareLFServer greets with a bare LF, then serves the store.
func bareLFServer(t *testing.T) *fakeserver.Server {
	h := fakeserver.StoreHandler(testStore(), fakeserver.StoreOptions{})
	return fakeserver.New(t, fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
		if _, err := io.WriteString(c.Raw(), "200 fakeserver ready\n"); err != nil {
			return
		}
		h(c)
	}})
}

// closedAddr returns an address nothing listens on.
func closedAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestNames: the runner's checks are reader, auth, stream in that order, with unique names.
func TestNames(t *testing.T) {
	want := slices.Concat(names(readerChecks()), names(authChecks()), names(streamChecks()))
	got := Names()
	if !slices.Equal(got, want) {
		t.Fatalf("Names() = %q, want %q", got, want)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Errorf("duplicate check name %q", n)
		}
		seen[n] = true
	}
}

// TestCheckStorePasses: the kit's own Store server passes every check it can be given. What
// is skipped, and why: the auth checks without Options.User; the stream and IHAVE checks
// always, since the Store server is a reader and advertises neither STREAMING nor IHAVE
// (mode-stream-capability runs and passes: MODE STREAM answers 501 and STREAMING is not
// listed).
func TestCheckStorePasses(t *testing.T) {
	streamSkip := map[string]string{
		"mode-stream": "STREAMING not advertised", "check-new": "STREAMING not advertised",
		"check-pipelined": "STREAMING not advertised", "takethis": "STREAMING not advertised",
		"check-have": "STREAMING not advertised", "takethis-duplicate": "STREAMING not advertised",
		"ihave": "IHAVE not advertised", "ihave-have": "IHAVE not advertised",
		"ihave-malformed": "IHAVE not advertised",
	}
	cases := []struct {
		name string
		so   fakeserver.StoreOptions
		o    Options
		skip map[string]string // check → substring of its Skip Detail (every other check passes)
	}{
		{name: "anonymous", o: Options{PostGroup: "conf.test.b"}, skip: maps(streamSkip, authSkip())},
		{
			name: "auth",
			so:   fakeserver.StoreOptions{User: "u", Pass: "p"},
			o:    Options{User: "u", Pass: "p", PostGroup: "conf.test.b"},
			skip: streamSkip,
		},
		{
			// without PostGroup the posting stream checks would skip for that reason, but the
			// runner skips them on Posts first
			name: "no-post-group",
			so:   fakeserver.StoreOptions{User: "u", Pass: "p", ReaderOnly: true},
			o:    Options{User: "u", Pass: "p"},
			skip: maps(streamSkip, map[string]string{
				"takethis": "no Options.PostGroup", "check-have": "no Options.PostGroup",
				"takethis-duplicate": "no Options.PostGroup", "ihave": "no Options.PostGroup",
				"ihave-have": "no Options.PostGroup",
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: tc.so})
			o := tc.o
			o.Addr = srv.Addr()
			r := Run(t, o) // fails t on any Fail
			if len(r.Results) != len(Names()) {
				t.Fatalf("%d results, want %d", len(r.Results), len(Names()))
			}
			for i, res := range r.Results {
				if res.Name != Names()[i] {
					t.Errorf("result %d is %q, want %q (check order)", i, res.Name, Names()[i])
				}
				want, skipped := tc.skip[res.Name]
				switch {
				case skipped && (res.Status != Skip || !strings.Contains(res.Detail, want)):
					t.Errorf("%s: %s %q, want SKIP %q", res.Name, res.Status, res.Detail, want)
				case !skipped && res.Status != Pass:
					t.Errorf("%s: %s %q, want PASS", res.Name, res.Status, res.Detail)
				}
			}
			if !slices.Contains(r.Capabilities, "VERSION 2") || !slices.Contains(r.Capabilities, "READER") {
				t.Errorf("Capabilities %q, want VERSION 2 and READER", r.Capabilities)
			}
			if tc.so.User != "" && !slices.Contains(r.Capabilities, "AUTHINFO USER") {
				t.Errorf("Capabilities %q, want AUTHINFO USER before the login", r.Capabilities)
			}
			if t.Failed() {
				t.Logf("report:\n%s", r)
			}
		})
	}
}

// authSkip is the skip of every auth check without Options.User.
func authSkip() map[string]string {
	out := map[string]string{}
	for _, c := range authChecks() {
		out[c.Name] = "no Options.User"
	}
	return out
}

// maps merges ms into a new map (later maps win).
func maps(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// TestCheckBareLFGreeting: a greeting that ends in a bare LF is one Fail (greeting, naming
// BareLF); no other check gets a usable session, so all of them are skipped, and Check itself
// does not report the server as unreachable.
func TestCheckBareLFGreeting(t *testing.T) {
	srv := bareLFServer(t)
	r := mustCheck(t, Options{Addr: srv.Addr(), User: "u", Pass: "p", PostGroup: "conf.test.b"})
	fails := r.Failed()
	if len(fails) != 1 || fails[0].Name != "greeting" || !strings.Contains(fails[0].Detail, "BareLF") {
		t.Fatalf("failed %+v, want exactly greeting naming BareLF", fails)
	}
	for _, res := range r.Results {
		if res.Name != "greeting" && res.Status != Skip {
			t.Errorf("%s: %s %q, want SKIP", res.Name, res.Status, res.Detail)
		}
	}
	if r.Capabilities != nil {
		t.Errorf("Capabilities %q, want nil (no usable session)", r.Capabilities)
	}
}

// TestCheckArticleNoTerminator: a Store server whose ARTICLE replies are cut before the
// terminator fails exactly the article check.
func TestCheckArticleNoTerminator(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), Faults: fakeserver.Faults{
		DropInMultiline: map[string]int{"ARTICLE": 4},
	}})
	r := mustCheck(t, Options{Addr: srv.Addr()})
	fails := r.Failed()
	if len(fails) != 1 || fails[0].Name != "article" || !strings.Contains(fails[0].Detail, "NoTerminator") {
		t.Fatalf("failed %+v, want exactly article naming NoTerminator", fails)
	}
}

// TestCheckPeer: the streaming peer passes every stream check. It lists READER but answers
// no reader command (GROUP, LIST, ... are 500), and its AUTHINFO differs from RFC 4643 in
// places (481 for PASS without USER, 381 after a login), so the reader and auth checks are
// left out by Options.Skip, which reports them as skipped.
func TestCheckPeer(t *testing.T) {
	po := &fakeserver.PeerOptions{User: "u", Pass: "p"}
	srv := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(po)})
	skip := slices.Concat(names(readerChecks()), names(authChecks()))
	r := mustCheck(t, Options{Addr: srv.Addr(), User: "u", Pass: "p", PostGroup: "alt.test", Skip: skip})
	for _, res := range r.Results {
		want := Pass
		if slices.Contains(skip, res.Name) {
			want = Skip
			if res.Detail != "skipped by option" {
				t.Errorf("%s: Detail %q, want \"skipped by option\"", res.Name, res.Detail)
			}
		}
		if res.Status != want {
			t.Errorf("%s: %s %q, want %s", res.Name, res.Status, res.Detail, want)
		}
	}
	if !slices.Contains(r.Capabilities, "STREAMING") {
		t.Errorf("Capabilities %q, want STREAMING", r.Capabilities)
	}
	if n := srv.Count("GROUP") + srv.Count("LIST") + srv.Count("HELP"); n != 0 {
		t.Errorf("%d GROUP/LIST/HELP sent although the reader checks are skipped", n)
	}
}

// TestCheckSkipOption: a check named in Options.Skip is reported as Skip and never sent.
func TestCheckSkipOption(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore()})
	r := mustCheck(t, Options{Addr: srv.Addr(), Skip: []string{"date", "help"}})
	res := byName(r)
	for _, n := range []string{"date", "help"} {
		if res[n].Status != Skip || res[n].Detail != "skipped by option" {
			t.Errorf("%s: %s %q, want SKIP \"skipped by option\"", n, res[n].Status, res[n].Detail)
		}
	}
	if res["over"].Status != Pass {
		t.Errorf("over: %s %q, want PASS", res["over"].Status, res["over"].Detail)
	}
	if n := srv.Count("DATE") + srv.Count("HELP"); n != 0 {
		t.Errorf("DATE/HELP sent %d times, want 0", n)
	}
}

// TestCheckUnreachable: no listener (or no address) is Check's error, and Run's Fail.
func TestCheckUnreachable(t *testing.T) {
	addr := closedAddr(t)
	if r, err := Check(Options{Addr: addr}); err == nil {
		t.Errorf("Check(%s): no error, report:\n%s", addr, r)
	}
	if _, err := Check(Options{}); err == nil {
		t.Error("Check without Addr: no error")
	}
	rec := &recordTB{TB: t}
	Run(rec, Options{Addr: addr})
	if !rec.Failed() || len(rec.errors) != 1 {
		t.Errorf("Run on a closed port: failed %v, errors %q; want one error", rec.Failed(), rec.errors)
	}
	rec = &recordTB{TB: t}
	Run(rec, Options{Addr: addr, ReportOnly: true})
	if rec.Failed() || len(rec.logs) != 1 {
		t.Errorf("Run ReportOnly on a closed port: failed %v, logs %q; want only a log line", rec.Failed(), rec.logs)
	}
}

// recordTB records what Run reports instead of failing the test.
type recordTB struct {
	testing.TB
	mu     sync.Mutex
	failed bool
	errors []string
	logs   []string
}

func (r *recordTB) Helper() {}
func (r *recordTB) Errorf(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
	r.errors = append(r.errors, fmt.Sprintf(f, a...))
}
func (r *recordTB) Logf(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(f, a...))
}
func (r *recordTB) Fail() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
}
func (r *recordTB) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// TestRunReportOnly: Run reports each Fail with Errorf and one summary line, unless
// ReportOnly, which returns the same report and leaves tb alone.
func TestRunReportOnly(t *testing.T) {
	srv := bareLFServer(t)
	rec := &recordTB{TB: t}
	r := Run(rec, Options{Addr: srv.Addr(), ReportOnly: true})
	if rec.Failed() || len(rec.errors) != 0 || len(rec.logs) != 0 {
		t.Errorf("ReportOnly: failed %v, errors %q, logs %q", rec.Failed(), rec.errors, rec.logs)
	}
	if len(r.Failed()) != 1 {
		t.Errorf("ReportOnly report: %d Fail, want 1:\n%s", len(r.Failed()), r)
	}
	rec = &recordTB{TB: t}
	Run(rec, Options{Addr: srv.Addr()})
	if !rec.Failed() || len(rec.errors) != 2 {
		t.Fatalf("Run: failed %v, errors %q; want the greeting Fail and a summary", rec.Failed(), rec.errors)
	}
	if !strings.Contains(rec.errors[0], "FAIL greeting") || !strings.Contains(rec.errors[0], "BareLF") {
		t.Errorf("first error %q, want the greeting Fail naming BareLF", rec.errors[0])
	}
	if want := fmt.Sprintf("1 of %d checks failed", len(Names())); !strings.Contains(rec.errors[1], want) {
		t.Errorf("summary %q", rec.errors[1])
	}
}

// TestRunChecksOrderPanicBound: results come back in check order; a panic in a check is that
// check's Fail; no more than Options.Workers checks (0: 4) hold a session at the same time.
func TestRunChecksOrderPanicBound(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore()})
	for _, tc := range []struct{ workers, max int }{{0, defaultWorkers}, {2, 2}, {1, 1}, {100, 12}} {
		t.Run(fmt.Sprint(tc.workers), func(t *testing.T) {
			var cur, peak atomic.Int32
			var list []check
			for i := range 12 {
				c := check{Name: fmt.Sprintf("c%02d", i), RFC: "test", Run: func(s *session, c check) Result {
					n := cur.Add(1)
					defer cur.Add(-1)
					for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
					}
					if _, _, err := s.cmd("DATE"); err != nil { // a round trip while the slot is held
						return c.fail(err.Error())
					}
					if c.Name == "c05" {
						panic("boom")
					}
					return c.pass()
				}}
				list = append(list, c)
			}
			res := runChecks(Options{Addr: srv.Addr(), Workers: tc.workers}, list)
			for i, r := range res {
				if r.Name != list[i].Name {
					t.Errorf("result %d is %q, want %q", i, r.Name, list[i].Name)
				}
				want := Pass
				if r.Name == "c05" {
					want = Fail
					if !strings.Contains(r.Detail, "panic: boom") {
						t.Errorf("c05 Detail %q, want the panic text", r.Detail)
					}
				}
				if r.Status != want {
					t.Errorf("%s: %s %q, want %s", r.Name, r.Status, r.Detail, want)
				}
			}
			if p := peak.Load(); p > int32(tc.max) {
				t.Errorf("%d checks at once, want at most %d", p, tc.max)
			}
		})
	}
}

// TestRunChecksPostsLast: the checks that post start only after every other check finished,
// wherever they stand in the list, and the results keep the list order.
func TestRunChecksPostsLast(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore()})
	var done atomic.Int32
	reader := func(s *session, c check) Result {
		defer done.Add(1)
		if _, _, err := s.cmd("DATE"); err != nil {
			return c.fail(err.Error())
		}
		return c.pass()
	}
	var list []check
	readers := 0
	for i := range 10 {
		c := check{Name: fmt.Sprintf("r%02d", i), RFC: "test", Run: reader}
		if i == 1 || i == 6 {
			c.Name, c.Posts = fmt.Sprintf("p%02d", i), true
			c.Run = func(_ *session, c check) Result {
				if n := done.Load(); n != int32(readers) {
					return c.failf("started after %d of %d non-posting checks", n, readers)
				}
				return c.pass()
			}
		} else {
			readers++
		}
		list = append(list, c)
	}
	res := runChecks(Options{Addr: srv.Addr(), PostGroup: "conf.test.b", Workers: 4}, list)
	for i, r := range res {
		if r.Name != list[i].Name || r.Status != Pass {
			t.Errorf("result %d: %s %s %q, want %s PASS", i, r.Name, r.Status, r.Detail, list[i].Name)
		}
	}
}

// TestCheckServiceUnavailable: a server that greets with 400 or 502 fails the greeting check
// with the line; every other check is skipped, also when greeting is in Options.Skip.
func TestCheckServiceUnavailable(t *testing.T) {
	for _, greet := range []string{"400 Service temporarily unavailable", "502 Too many connections"} {
		t.Run(greet[:3], func(t *testing.T) {
			srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), Greeting: greet})
			r := mustCheck(t, Options{Addr: srv.Addr(), Skip: []string{"greeting", "help"}})
			fails := r.Failed()
			if len(fails) != 1 || fails[0].Name != "greeting" || fails[0].Detail != "service unavailable: "+greet {
				t.Fatalf("failed %+v, want exactly greeting with \"service unavailable: %s\"", fails, greet)
			}
			for _, res := range r.Results {
				if res.Name != "greeting" && res.Status != Skip {
					t.Errorf("%s: %s %q, want SKIP", res.Name, res.Status, res.Detail)
				}
			}
			if d := byName(r)["help"].Detail; d != "skipped by option" {
				t.Errorf("help Detail %q, want \"skipped by option\"", d)
			}
			if n := srv.Count("CAPABILITIES"); n != 0 {
				t.Errorf("CAPABILITIES sent %d times to a server that refused service", n)
			}
		})
	}
}

// TestRunNeedsModeReader: a check whose capability is missing is skipped by the runner, but
// not on a mode-switching server (MODE-READER listed): there the check runs and tests the
// capability after MODE READER itself.
func TestRunNeedsModeReader(t *testing.T) {
	caps := func(extra string) fakeserver.Options {
		lines := []string{"101 caps", "VERSION 2"}
		if extra != "" {
			lines = append(lines, extra)
		}
		return fakeserver.Options{Script: []fakeserver.Step{{Expect: "CAPABILITIES", Reply: append(lines, ".")}}}
	}
	ran := false
	c := check{Name: "needs-x", RFC: "test", Needs: "XFEATURE", Run: func(_ *session, c check) Result {
		ran = true
		return c.pass()
	}}
	srv := fakeserver.New(t, caps(""))
	r := runChecks(Options{Addr: srv.Addr()}, []check{c})[0]
	if r.Status != Skip || r.Detail != "server does not advertise XFEATURE" || ran {
		t.Errorf("without the capability: %s %q (ran %v), want SKIP without running", r.Status, r.Detail, ran)
	}
	srv = fakeserver.New(t, caps("MODE-READER"))
	r = runChecks(Options{Addr: srv.Addr()}, []check{c})[0]
	if r.Status != Pass || !ran {
		t.Errorf("with MODE-READER: %s %q (ran %v), want the check run", r.Status, r.Detail, ran)
	}
}
