// Command nntp-conformance runs the conformance checks (package conformance) against an NNTP
// server and prints the report.
//
//	nntp-conformance -addr host:port [-user u -pass p] [-group g] [-tls [-tls-insecure]]
//	                 [-ca ca.pem] [-servername name] [-starttls-verify] [-allow name=substring]...
//	                 [-allow-reply name:CMD=code]... [-commit-wait 5s]
//	                 [-deadline 10s] [-workers 4] [-skip name,name] [-json] [-strict-overview-fmt]
//
// The STARTTLS checks (RFC 4642) handshake without verifying the certificate unless
// -starttls-verify or -ca is given (then against -ca, else the system roots, with -servername or
// the host of -addr as the name); the report says "certificate not verified" in that case.
//
// Exit status: 0 when no check failed, 1 on any Fail, 2 on a usage error or when the server
// cannot be reached.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
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
	// Missing is the capability whose absence skipped the check ("COMPRESS", "STARTTLS", ...)
	Missing string `json:"missing,omitempty"`
	// Cmd and Code: the command and the reply code of a Fail that is a wrong reply (the values
	// -allow-reply matches; kept on an allowed Fail)
	Cmd  string `json:"cmd,omitempty"`
	Code int    `json:"code,omitempty"`
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

// allowFlag is the repeatable -allow name=substring flag (conformance.Options.Allow): one
// substring per check, a second -allow for the same check is an error (it would silently
// replace the first).
type allowFlag map[string]string

func (a allowFlag) String() string { return fmt.Sprint(map[string]string(a)) }

// Set parses "name=substring": the substring may itself contain "=" and must not be empty.
func (a allowFlag) Set(v string) error {
	name, sub, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(name) == "" || sub == "" {
		return fmt.Errorf("%q: want name=substring with a non-empty substring", v)
	}
	name = strings.TrimSpace(name)
	if _, dup := a[name]; dup {
		return fmt.Errorf("%q: check %q already has an -allow substring (one per check)", v, name)
	}
	a[name] = sub
	return nil
}

// allowReplyFlag is the repeatable -allow-reply name:CMD=code flag (conformance.Options.AllowReply).
type allowReplyFlag []conformance.AllowReply

func (a *allowReplyFlag) String() string { return fmt.Sprint([]conformance.AllowReply(*a)) }

// Set parses "name:CMD=code": CMD (the command's keywords, may hold spaces) and "=code" are each
// optional but not both ("help:HELP", "ihave-have:IHAVE=335", "mode-stream:=502").
func (a *allowReplyFlag) Set(v string) error {
	name, rest, ok := strings.Cut(v, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return fmt.Errorf("%q: want name:CMD=code (CMD or =code may be left out, not both)", v)
	}
	cmd, codeText, hasCode := strings.Cut(rest, "=")
	e := conformance.AllowReply{Check: name, Cmd: strings.TrimSpace(cmd)}
	if hasCode {
		code, err := strconv.Atoi(strings.TrimSpace(codeText))
		if err != nil || code < 100 || code > 599 {
			return fmt.Errorf("%q: the code must be an NNTP reply code (100-599)", v)
		}
		e.Code = code
	}
	if e.Cmd == "" && e.Code == 0 {
		return fmt.Errorf("%q: give a command, a code or both (an entry with neither allows nothing)", v)
	}
	*a = append(*a, e)
	return nil
}

// run is main without os.Exit: it parses args, runs the checks, writes the report to stdout
// and errors to stderr, and returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nntp-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "host:port of the server under test (required)")
	user := fs.String("user", "", "AUTHINFO USER; empty: the RFC 4643 checks are skipped")
	pass := fs.String("pass", "", "AUTHINFO PASS")
	group := fs.String("group", "", "a group the checks may post into (POST, IHAVE, TAKETHIS); empty: no check modifies the server")
	useTLS := fs.Bool("tls", false, "TLS from the first byte (no STARTTLS), ServerName = the host of -addr")
	insecure := fs.Bool("tls-insecure", false, "TLS without certificate verification (implies -tls)")
	caFile := fs.String("ca", "", "PEM file of the CA certificates that verify the server certificate of -tls and of STARTTLS (default: the system roots); implies -starttls-verify")
	serverName := fs.String("servername", "", "TLS server name (SNI and the name the certificate must carry) for -tls and STARTTLS (default: the host of -addr)")
	starttlsVerify := fs.Bool("starttls-verify", false, "verify the certificate in the STARTTLS checks (against -ca, else the system roots); off: handshake without verification, noted in the report")
	deadline := fs.Duration("deadline", 10*time.Second, "hang guard per read and write")
	workers := fs.Int("workers", 4, "checks that run at the same time, each on its own connection (servers that allow fewer connections: lower it)")
	skip := fs.String("skip", "", "comma-separated check names to leave out")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	allow := allowFlag{}
	fs.Var(allow, "allow", "tolerate a known server bug: a Fail of check `name=substring` whose detail contains the substring becomes a Skip \"allowed: ...\"; repeatable")
	var allowReply allowReplyFlag
	fs.Var(&allowReply, "allow-reply", "tolerate a known server bug by what the server did, not by the detail text: a Fail of check `name:CMD=code` that is a wrong reply with that code to a command starting with CMD (words case-insensitive; CMD or =code may be left out) becomes a Skip \"allowed: ...\"; -json and the report show the cmd and code of a Fail; repeatable")
	commitWait := fs.Duration("commit-wait", 0, "how long check-have and ihave-have re-offer an article the server deferred (431/436) while it commits it asynchronously; 0: 5s, negative: do not retry")
	strictOverview := fs.Bool("strict-overview-fmt", false, "require preferred :bytes/:lines metadata names (legacy Bytes:/Lines: aliases allowed by default)")
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
	o.StrictOverviewFmt = *strictOverview
	names := conformance.Names()
	for n := range allow {
		if !slices.Contains(names, n) {
			return usage("-allow: unknown check %q", n)
		}
	}
	if len(allow) > 0 {
		o.Allow = allow
	}
	for _, e := range allowReply {
		if !slices.Contains(names, e.Check) {
			return usage("-allow-reply: unknown check %q", e.Check)
		}
	}
	o.AllowReply = allowReply
	o.CommitWait = *commitWait
	for _, n := range strings.Split(*skip, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if !slices.Contains(names, n) {
			return usage("-skip: unknown check %q", n)
		}
		o.Skip = append(o.Skip, n)
	}
	var roots *x509.CertPool
	if *caFile != "" {
		if *insecure {
			return usage("-ca and -tls-insecure exclude each other (-ca verifies, -tls-insecure does not)")
		}
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			return usage("-ca: %v", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return usage("-ca %s: no PEM certificate found", *caFile)
		}
	}
	if *useTLS || *insecure {
		// -tls-insecure is the operator's explicit choice (self-signed test servers)
		name := host
		if *serverName != "" {
			name = *serverName
		}
		o.TLS = &tls.Config{ServerName: name, RootCAs: roots, InsecureSkipVerify: *insecure} //nolint:gosec
	}
	switch {
	case *starttlsVerify || roots != nil:
		o.StartTLS = &tls.Config{ServerName: *serverName, RootCAs: roots} // ServerName "": the host of -addr
	case *serverName != "":
		// the name is sent (SNI), the certificate is still not verified
		o.StartTLS = &tls.Config{ServerName: *serverName, MinVersion: tls.VersionTLS10, InsecureSkipVerify: true} //nolint:gosec // as -tls-insecure: the operator did not ask for verification
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
			out.Results = append(out.Results, jsonResult{Name: res.Name, Status: res.Status.String(), RFC: res.RFC, Detail: res.Detail, Missing: res.Missing, Cmd: res.Cmd, Code: res.Code})
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
