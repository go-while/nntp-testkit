package conformance

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/go-while/nntp-testkit/client"
)

// defaultWorkers is the number of checks that run at the same time when Options.Workers is 0.
const defaultWorkers = 4

// workers is the number of checks that run at the same time, each on its own connection.
func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return defaultWorkers
}

// allChecks is every check in report order: reader, auth, stream.
func allChecks() []check {
	return slices.Concat(readerChecks(), authChecks(), streamChecks())
}

// Names returns the check names (Result.Name) in report order: the values Options.Skip takes.
func Names() []string {
	var out []string
	for _, c := range allChecks() {
		out = append(out, c.Name)
	}
	return out
}

// Run runs every check against o.Addr and returns the report. Unless o.ReportOnly, each Fail is
// reported with tb.Errorf and a summary line follows; a server that cannot be reached fails tb
// too (with ReportOnly it is only logged).
func Run(tb testing.TB, o Options) Report {
	tb.Helper()
	r, err := Check(o)
	if err != nil {
		if o.ReportOnly {
			tb.Logf("conformance %s: %v", o.Addr, err)
		} else {
			tb.Errorf("conformance %s: %v", o.Addr, err)
		}
		return r
	}
	failed := r.Failed()
	if o.ReportOnly || len(failed) == 0 {
		return r
	}
	for _, f := range failed {
		tb.Errorf("conformance %s: FAIL %s (%s): %s", o.Addr, f.Name, f.RFC, f.Detail)
	}
	pass, fail, skip := r.Counts()
	tb.Errorf("conformance %s: %d of %d checks failed (%d pass, %d skip)", o.Addr, fail, len(r.Results), pass, skip)
	return r
}

// Check runs every check against o.Addr without testing. The error is non-nil only when the
// first connection (the one that reads the capability list for Report.Capabilities) cannot be
// opened: no listener, a TLS failure, a dial or greeting timeout. A greeting with a wire
// mistake (bare LF, ...) is not an error: the greeting check reports it. Neither is a server
// that refuses service (400 or 502 greeting): the greeting check fails with the line, every
// other check is skipped.
func Check(o Options) (Report, error) {
	if o.Addr == "" {
		return Report{}, errors.New("no Options.Addr")
	}
	caps, refused, err := preRead(o)
	if err != nil {
		return Report{}, err
	}
	if refused != "" {
		return Report{Results: unavailable(o, allChecks(), refused)}, nil
	}
	return Report{Results: runChecks(o, allChecks()), Capabilities: caps}, nil
}

// preRead opens one session and returns the capability list read right after the greeting
// (nil when CAPABILITIES failed or the greeting is broken), or the greeting line when the
// server refused service (400 or 502).
func preRead(o Options) (caps []string, refused string, err error) {
	s, err := dial(o)
	if err != nil {
		var se *client.StrictError
		if errors.As(err, &se) {
			return nil, "", nil
		}
		return nil, "", err
	}
	defer s.close()
	if s.code == 400 || s.code == 502 {
		return nil, s.greeting, nil
	}
	caps, _ = s.capabilities()
	return caps, "", nil
}

// unavailable is the report of a server that refused service with greeting line: the
// greeting check fails (whatever Options.Skip says), every other check is skipped.
func unavailable(o Options, checks []check, line string) []Result {
	out := make([]Result, len(checks))
	for i, c := range checks {
		switch {
		case c.Name == "greeting":
			out[i] = c.fail("service unavailable: " + line)
		case slices.Contains(o.Skip, c.Name):
			out[i] = c.skip("skipped by option")
		default:
			out[i] = c.skip("no usable session, the server refused service (see check greeting)")
		}
	}
	return out
}

// runChecks runs checks with at most o.workers() at a time and returns the results in check
// order. It runs in two phases: first every check that does not post, then the ones that do
// (Posts), so no article lands in Options.PostGroup while a reader check looks at its numbers
// (next-last, listgroup, over).
func runChecks(o Options, checks []check) []Result {
	out := make([]Result, len(checks))
	var reads, posts []int
	for i, c := range checks {
		if c.Posts {
			posts = append(posts, i)
		} else {
			reads = append(reads, i)
		}
	}
	runPhase(o, checks, reads, out)
	runPhase(o, checks, posts, out)
	return out
}

// runPhase runs the checks at the indexes idx, at most o.workers() at a time, into out.
func runPhase(o Options, checks []check, idx []int, out []Result) {
	if len(idx) == 0 {
		return
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(o.workers(), len(idx)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				out[i] = runOne(o, checks[i])
			}
		}()
	}
	for _, i := range idx {
		next <- i
	}
	close(next)
	wg.Wait()
}

// runOne runs check c on a fresh session. The skips that need no connection come first
// (Options.Skip, Auth without Options.User, Posts without Options.PostGroup); then the session
// is dialed (a dial error is turned into a Result by sessionFailed), CAPABILITIES is read, and
// c is skipped when the server does not advertise c.Needs, unless it advertises MODE-READER
// (the check's ready switches the mode and tests Needs again). A panic in the check is a Fail.
func runOne(o Options, c check) (r Result) {
	switch {
	case slices.Contains(o.Skip, c.Name):
		return c.skip("skipped by option")
	case c.Auth && o.User == "":
		return c.skip("no Options.User")
	case c.Posts && o.PostGroup == "":
		return c.skip("no Options.PostGroup")
	}
	var s *session
	defer func() {
		if p := recover(); p != nil {
			r = c.fail(fmt.Sprintf("panic: %v", p))
		}
		s.close()
	}()
	s, err := dial(o)
	if err != nil {
		return sessionFailed(c, err)
	}
	_, _ = s.capabilities() // a failure stays in s.capsErr; the capabilities check reports it
	if c.Needs != "" && !s.has(c.Needs) && !s.has("MODE-READER") {
		return c.skip("server does not advertise " + c.Needs)
	}
	return c.Run(s, c)
}
