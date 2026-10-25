package conformance

import (
	"crypto/tls"
	"fmt"
	"strings"
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

	TLS      *tls.Config   // nil: plain TCP; else a TLS client from the first byte (no STARTTLS)
	Deadline time.Duration // hang guard per read/write; 0: testkit.Deadline()

	// Skip lists check names (Result.Name) to leave out; they are reported as Skip.
	Skip []string

	// ReportOnly makes Run return the report without failing tb on a Fail.
	ReportOnly bool
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
