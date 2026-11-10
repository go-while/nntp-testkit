package conformance

import (
	"crypto/tls"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Options names the server under test and what the checks may do to it.
type Options struct {
	Addr string // host:port of the server under test
	User string // AUTHINFO USER; empty: the RFC 4643 checks are skipped
	Pass string // AUTHINFO PASS

	// PostGroup is a group the checks may post into (POST, IHAVE, TAKETHIS). Empty: every check
	// that would modify the server is skipped. The articles carry a Message-ID under
	// @conformance.invalid and say what they are in Subject.
	PostGroup string

	TLS *tls.Config // nil: plain TCP; else a TLS client from the first byte (no STARTTLS)

	// StartTLS is the client configuration of the STARTTLS checks (starttls, starttls-twice,
	// starttls-pipelined): RootCAs, ServerName (default: the host of Addr), MinVersion and so
	// on. nil: the handshake is made without verifying the certificate (a test server has a
	// self-signed one) and the Pass Detail of check starttls says so; &tls.Config{} verifies
	// against the system roots with the host of Addr as the name. Not used with TLS (the
	// connection is TLS from the first byte then and the STARTTLS checks are skipped).
	StartTLS *tls.Config

	Deadline time.Duration // hang guard per read/write; 0: testkit.Deadline()

	// Skip lists check names (Result.Name) to leave out; they are reported as Skip.
	Skip []string

	// ReportOnly makes Run return the report without failing tb on a Fail.
	ReportOnly bool

	// Workers is how many checks run at the same time, each on its own connection; 0: 4. Public
	// servers often allow only 2 to 4 connections per account: a refused dial is a Fail.
	Workers int

	// StrictOverviewFmt requires the RFC 3977 section 8.4.2 preferred metadata
	// names :bytes and :lines in LIST OVERVIEW.FMT. The zero value also accepts
	// the legacy Bytes:/Lines: aliases permitted by that RFC. This is an opt-in
	// naming policy, not a claim that a legacy-compatible server violates it.
	StrictOverviewFmt bool
}

// Status is the outcome of one check.
type Status int

const (
	Pass Status = iota
	Fail
	Skip
)

// String returns "PASS", "FAIL" or "SKIP".
func (s Status) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Fail:
		return "FAIL"
	case Skip:
		return "SKIP"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// Result is the outcome of one check.
type Result struct {
	Name   string // check name, stable (used by Options.Skip)
	Status Status
	Detail string // why it failed or was skipped; on Pass a note worth reading (e.g. "not restricted"), usually empty
	RFC    string // the section the check is about, e.g. "RFC 3977 §5.1.1"
	// Missing names the capability whose absence skipped the check ("NEWNEWS", "STREAMING",
	// "AUTHINFO", "LIST ACTIVE.TIMES" for a LIST keyword); empty for any other result. Detail
	// then reads "<Missing> not advertised", plus what the server answered where it was asked.
	// Match skips on Missing rather than on Detail.
	Missing string
}

// Report is the outcome of a Run or Check.
type Report struct {
	Results      []Result
	Capabilities []string // the CAPABILITIES lines the server advertised (without the 101 line and the terminator)
}

// Failed returns the results with Status Fail.
func (r Report) Failed() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status == Fail {
			out = append(out, res)
		}
	}
	return out
}

// ByName returns the results by check name (Result.Name, the values of Names()).
func (r Report) ByName() map[string]Result {
	out := make(map[string]Result, len(r.Results))
	for _, res := range r.Results {
		out[res.Name] = res
	}
	return out
}

// Require fails tb, once per check named, for every one that did not Pass: a Fail, a Skip (a
// check the server's capabilities or the Options left out is not a pass) and a name that is
// not one of Names(). Each failure line has the check's status, its RFC and its Detail (and
// the capability a Skip is missing). Use it to pin what a server must do, e.g.
// r.Require(t, "compress", "starttls"), after Run with ReportOnly or RunVerbose. A name
// listed twice is reported once.
func (r Report) Require(tb testing.TB, names ...string) {
	tb.Helper()
	by := r.ByName()
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		res, ok := by[n]
		switch {
		case !ok:
			tb.Errorf("conformance: required check %q is not in the report (a check name from Names(), and Check or Run must have returned a report)", n)
		case res.Status != Pass:
			msg := fmt.Sprintf("conformance: required check %s (%s) is %s", n, res.RFC, res.Status)
			if res.Detail != "" {
				msg += ": " + res.Detail
			}
			tb.Errorf("%s", msg)
		}
	}
}

// RequireNoFail fails tb once per check that failed; Pass and Skip are fine. It is what Run
// does without ReportOnly, for a report taken with ReportOnly or Check. An empty report (the
// server was not reached) is a failure too.
func (r Report) RequireNoFail(tb testing.TB) {
	tb.Helper()
	if len(r.Results) == 0 {
		tb.Errorf("conformance: the report has no results (the server was not reached)")
		return
	}
	for _, f := range r.Failed() {
		tb.Errorf("conformance: FAIL %s (%s): %s", f.Name, f.RFC, f.Detail)
	}
}

// Counts returns the number of Pass, Fail and Skip results.
func (r Report) Counts() (pass, fail, skip int) {
	for _, res := range r.Results {
		switch res.Status {
		case Pass:
			pass++
		case Fail:
			fail++
		case Skip:
			skip++
		}
	}
	return
}

// String formats the report, one line per result, then a totals line.
func (r Report) String() string {
	var b strings.Builder
	for _, res := range r.Results {
		fmt.Fprintf(&b, "%-4s %-32s %s", res.Status, res.Name, res.RFC)
		if res.Detail != "" {
			fmt.Fprintf(&b, ": %s", res.Detail)
		}
		b.WriteByte('\n')
	}
	pass, fail, skip := r.Counts()
	fmt.Fprintf(&b, "%d checks: %d pass, %d fail, %d skip\n", len(r.Results), pass, fail, skip)
	return b.String()
}

// pass, fail and skip build a Result for check c.
func (c check) pass() Result { return Result{Name: c.Name, Status: Pass, RFC: c.RFC} }
func (c check) skip(why string) Result {
	return Result{Name: c.Name, Status: Skip, Detail: why, RFC: c.RFC}
}

// notAdvertised skips c because the server does not advertise capability what.
func (c check) notAdvertised(what string) Result {
	r := c.skip(what + " not advertised")
	r.Missing = what
	return r
}
func (c check) fail(why string) Result {
	return Result{Name: c.Name, Status: Fail, Detail: why, RFC: c.RFC}
}
func (c check) failf(f string, a ...any) Result { return c.fail(fmt.Sprintf(f, a...)) }

// check is one conformance check. Run opens a session for it (the connection, the greeting and
// the capability list) and calls Run; Needs names a capability keyword (e.g. "READER", "OVER",
// "STREAMING") the server must advertise, else the check is skipped; Posts marks checks that
// modify the server and need Options.PostGroup; Auth marks checks that need Options.User.
//
// Each slice contributes its checks through a function returning []check (readerChecks,
// authChecks, streamChecks); run.go concatenates them in that order.
type check struct {
	Name  string
	RFC   string
	Needs string
	Posts bool
	Auth  bool
	Run   func(*session, check) Result
}
