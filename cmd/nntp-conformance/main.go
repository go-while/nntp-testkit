// Command nntp-conformance runs the conformance checks (package conformance) against an NNTP
// server and prints the report.
//
//	nntp-conformance -addr host:port [-user u -pass p] [-group g] [-tls [-tls-insecure]]
//	                 [-deadline 10s] [-workers 4] [-skip name,name] [-json]
//
// Exit status: 0 when no check failed, 1 on any Fail, 2 on a usage error or when the server
// cannot be reached.
package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-while/nntp-testkit/conformance"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// jsonResult is one Result in the -json output (Status as "PASS", "FAIL" or "SKIP").
type jsonResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	RFC    string `json:"rfc"`
	Detail string `json:"detail,omitempty"`
}

// jsonReport is the -json output.
type jsonReport struct {
	Addr         string       `json:"addr"`
	Results      []jsonResult `json:"results"`
	Capabilities []string     `json:"capabilities"`
	Pass         int          `json:"pass"`
	Fail         int          `json:"fail"`
	Skip         int          `json:"skip"`
}

// run is main without os.Exit: it parses args, runs the checks, writes the report to stdout
// and errors to stderr, and returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nntp-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "host:port of the server under test (required)")
	user := fs.String("user", "", "AUTHINFO USER; empty: the RFC 4643 checks are skipped")
	pass := fs.String("pass", "", "AUTHINFO PASS")
	group := fs.String("group", "", "a group the checks may post into (IHAVE, TAKETHIS); empty: no check modifies the server")
	useTLS := fs.Bool("tls", false, "TLS from the first byte (no STARTTLS), ServerName = the host of -addr")
	insecure := fs.Bool("tls-insecure", false, "TLS without certificate verification (implies -tls)")
	deadline := fs.Duration("deadline", 10*time.Second, "hang guard per read and write")
	workers := fs.Int("workers", 4, "checks that run at the same time, each on its own connection (servers that allow fewer connections: lower it)")
	skip := fs.String("skip", "", "comma-separated check names to leave out")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: nntp-conformance -addr host:port [flags]\n\nflags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nchecks: %s\n", strings.Join(conformance.Names(), " "))
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(f string, a ...any) int {
		fmt.Fprintf(stderr, "nntp-conformance: "+f+"\n", a...)
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		return usage("unexpected arguments %q", fs.Args())
	}
	if *addr == "" {
		return usage("-addr is required")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return usage("-addr %q: %v", *addr, err)
	}
	if *deadline <= 0 {
		return usage("-deadline must be positive")
	}
	if *workers <= 0 {
		return usage("-workers must be positive")
	}
	if (*useTLS || *insecure) && host == "" {
		return usage("-tls needs a host in -addr (the TLS ServerName)")
	}
	o := conformance.Options{Addr: *addr, User: *user, Pass: *pass, PostGroup: *group, Deadline: *deadline, Workers: *workers}
	names := conformance.Names()
	for _, n := range strings.Split(*skip, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if !slices.Contains(names, n) {
			return usage("-skip: unknown check %q", n)
		}
		o.Skip = append(o.Skip, n)
	}
	if *useTLS || *insecure {
		// -tls-insecure is the operator's explicit choice (self-signed test servers)
		o.TLS = &tls.Config{ServerName: host, InsecureSkipVerify: *insecure} //nolint:gosec
	}

	r, err := conformance.Check(o)
	if err != nil {
		fmt.Fprintf(stderr, "nntp-conformance: %s: %v\n", *addr, err)
		return 2
	}
	if *asJSON {
		p, f, s := r.Counts()
		out := jsonReport{Addr: *addr, Results: []jsonResult{}, Capabilities: r.Capabilities, Pass: p, Fail: f, Skip: s}
		if out.Capabilities == nil {
			out.Capabilities = []string{}
		}
		for _, res := range r.Results {
			out.Results = append(out.Results, jsonResult{Name: res.Name, Status: res.Status.String(), RFC: res.RFC, Detail: res.Detail})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		// a report that cannot be written is not a verdict about the server: 2, as a usage error
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "nntp-conformance: %v\n", err)
			return 2
		}
	} else if _, err := io.WriteString(stdout, r.String()); err != nil { // as above: 2
		fmt.Fprintf(stderr, "nntp-conformance: %v\n", err)
		return 2
	}
	if len(r.Failed()) > 0 {
		return 1
	}
	return 0
}
