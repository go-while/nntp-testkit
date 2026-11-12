// Command nntp-fakeserver runs the kit's fake NNTP server (package fakeserver) as a process that
// a JSON file configures, so test suites in other languages (Python, shell, C) can use it.
//
//	nntp-fakeserver [-listen host:port] config.json
//
// Protocol with the caller: once the server listens, one line of JSON goes to stdout,
//
//	{"addr":"127.0.0.1:41233","host":"127.0.0.1","port":41233,"ca":"/tmp/nntp-fakeserver-1/ca.pem","ca_pem":"-----BEGIN CERTIFICATE-----\n..."}
//
// ca is a PEM file that holds the certificate of the server (it is self-signed, valid for
// localhost, 127.0.0.1 and ::1), ca_pem its text: a client verifies implicit TLS and STARTTLS
// against it. The
// server then runs until stdin reaches end of file (or the process gets SIGINT or SIGTERM). It
// prints one more line of JSON, the summary, removes its temporary directory and exits 0:
//
//	{"lines":[...],"mismatches":[...],"articles":[[...]],"notes":[...],"accepted":1,
//	 "counts":{"GROUP":1},"tls_handshakes":0,"tls_failed":0,"server_names":[],
//	 "lines_b64":[...],"mismatches_b64":[...],"articles_raw":[...],"max_open":1,"closed":1}
//
// lines are the client lines of all connections, mismatches those that did not fit (raw mode,
// script mode), articles the dot blocks received (POST, IHAVE, TAKETHIS, raw ReadDot), notes
// what the server itself ran into (a failed handshake, a cut-off block), counts the number of
// lines per upper-cased command, tls_handshakes and tls_failed the TLS handshakes (implicit and
// STARTTLS) and server_names the SNI of every ClientHello ("" when none). Exit status 2 and a
// message on stderr for a bad config or flag, 1 for another failure.
//
// A JSON string is UTF-8, so lines, mismatches and articles show any other byte (8-bit text,
// yEnc) as U+FFFD. lines_b64 and mismatches_b64 hold the same lines, articles_raw the blocks
// (the exact bytes up to the "." line, still dot-stuffed, one entry per block), all in base64
// and exact. max_open is the most connections open at once, closed the number that ended.
//
// The config is one JSON object. Its keys are the Go field names of the kit types, matched
// exactly, and a key that does not exist is an error (a typo fails at once). Durations are
// numbers of milliseconds (fractions allowed). Mode says what runs:
//
//	store   Store + Options.StoreOpts + Options.Faults: the RFC 3977 reader of the kit
//	peer    PeerOptions: a transit peer (IHAVE, CHECK, TAKETHIS), Options.Faults apply to it
//	script  Options.Script: lines of text, as fakeserver.Step
//	raw     Raw: a byte-exact script, run by this program
//
// Common to all modes is Options (fakeserver.Options): Addr, Greeting, NoGreeting, RawStall,
// TLS (true: implicit TLS from the first byte, with the certificate behind ca), Deadline (ms,
// default 10000: bounds every server-side wait and each TLS handshake), MaxConns and
// MaxConnsGreeting. A StartTLS that is {} (or has only the knobs Refuse, HideCapability,
// AuthNeedsTLS, CloseAfterHandshake) offers STARTTLS with the same certificate. Functions,
// channels and the Store itself cannot be set from JSON. A part that the mode does not use is
// an error, unless it is empty ({} or []). "StartTLS": {"Config": false} switches STARTTLS off
// again. The command names that key the maps of Faults are upper case (a lower case key is an
// error: it would never fire), except OverrideLine, which takes whole client lines.
//
// Store mode, with a fault and STARTTLS:
//
//	{"Mode": "store",
//	 "Options": {
//	   "Greeting": "200 hello",
//	   "Faults": {"Delay": {"BODY": 2500}, "DropInMultiline": {"ARTICLE": 2}, "LongLine": {"BODY": 65535}},
//	   "StoreOpts": {"User": "u", "Pass": "p", "Compress": true, "StartTLS": {"HideCapability": true}}},
//	 "Store": {
//	   "Groups": [{"Name": "alt.test", "Description": "test group"}],
//	   "Articles": [{"Group": "alt.test", "Number": 1, "MessageID": "<1@test>",
//	                 "Headers": ["Subject: one", "From: a@b.example"], "Body": ["text", ".dot"]}]}}
//
// Groups take Name, Description, Created (RFC 3339, default 2020-01-01) and NoPosting;
// Articles take Group, Number, MessageID, Headers, Body (lines, no CRLF, not dot-stuffed), Date
// and Arrived (RFC 3339, default 2020-01-02T03:04:05Z). HeadersB64 and BodyB64 are Headers and
// Body with every line in base64, for bytes that are not UTF-8 (use one form of each). The
// replies of Faults (Override and so on) are UTF-8 only; raw mode with B64 chunks writes any
// bytes.
//
// Peer mode:
//
//	{"Mode": "peer", "PeerOptions": {"Have": {"<have@x>": true}, "Reject": {"<rej@x>": true},
//	 "Defer436Once": {"<def@x>": true}, "User": "u", "Pass": "p", "Compress": true}}
//
// Script mode:
//
//	{"Mode": "script", "Options": {"Script": [{"Expect": "CAPABILITIES", "Reply": ["101 caps", "VERSION 2", "."]}]}}
//
// Raw mode: Raw is a list of steps. Each step reads one client line that matches Expect (an
// anchored regexp; null or absent: read nothing), then a dot block when ReadDot (DotMax > 0:
// only that many lines), writes its Send chunks, waits Sleep ms and then does Then: "" (go on),
// "close", "rst" (reset the TCP connection), "hang" (keep reading, never answer), "starttls"
// (handshake on the open connection, then read through TLS) or "inflate" (read the client
// through raw DEFLATE from now on; the compressed replies are in the script). A chunk is Text
// (UTF-8) or B64 (base64), written after Gap ms, in one write or in pieces of Split bytes with
// SplitGap ms between them. A line that does not match gets "500 unexpected ..." and is a
// mismatch, as is every line after the last step. Pauses, reads of an expected line and
// writes are bounded by Options.Deadline and end when the server closes (hang and the lines
// after the script wait for the client without a limit): a client that stays silent longer than
// the Deadline before an expected line is cut off, and a Sleep, Gap or SplitGap longer than the
// Deadline is a config error. A line over 1 MiB, a dot block over 16 MiB or more than 64 MiB on
// one connection close the connection and add a note. The script writes the greeting itself (a
// first step with Expect null).
//
//	{"Mode": "raw", "Raw": [
//	  {"Send": [{"Text": "200 raw ready\r\n"}]},
//	  {"Expect": "GROUP .*", "Send": [{"Text": "211 1 1 1 alt.a\r\n", "Split": 3, "SplitGap": 1}]},
//	  {"Expect": "STARTTLS", "Send": [{"Text": "382 go\r\n"}], "Then": "starttls"},
//	  {"Expect": "QUIT", "Send": [{"B64": "MjA1IGJ5ZQ0K"}], "Then": "close"}]}
//
// The certificate (tlscert.GenerateSelfSigned) and the CA file are made on every start and live
// in a temporary directory that is removed on exit.
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
	"github.com/go-while/nntp-testkit/tlscert"
)

// defaultDeadline is Options.Deadline when the config sets none.
const defaultDeadline = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() { // after the first signal the next one kills the process, as it should while shutting down
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// ready is the line on stdout when the server listens.
type ready struct {
	Addr string `json:"addr"`
	Host string `json:"host"`
	Port int    `json:"port"`
	CA   string `json:"ca"`
	// CAPEM is the text of the CA file, for a caller that wants the certificate in memory.
	CAPEM string `json:"ca_pem"`
}

// summary is the line on stdout when the server has stopped.
type summary struct {
	Lines         []string       `json:"lines"`
	Mismatches    []string       `json:"mismatches"`
	Articles      [][]string     `json:"articles"`
	Notes         []string       `json:"notes"`
	Accepted      int            `json:"accepted"`
	Counts        map[string]int `json:"counts"`
	TLSHandshakes int            `json:"tls_handshakes"`
	TLSFailed     int            `json:"tls_failed"`
	ServerNames   []string       `json:"server_names"`

	// The lossless forms: JSON strings are UTF-8, so lines, mismatches and articles hold U+FFFD
	// for any other byte. These hold the bytes as sent, in base64.
	LinesB64      []string `json:"lines_b64"`      // lines, one base64 string per line
	MismatchesB64 []string `json:"mismatches_b64"` // mismatches
	ArticlesRaw   []string `json:"articles_raw"`   // per block: the exact bytes up to the "." line, still dot-stuffed

	MaxOpen int `json:"max_open"` // the most connections open at once
	Closed  int `json:"closed"`   // connections that have ended
}

// run is main without os.Exit: it serves until stdin reaches end of file or ctx is done, and
// returns the exit status.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nntp-fakeserver", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "listen address `host:port` (default Options.Addr of the config, else 127.0.0.1:0)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: nntp-fakeserver [flags] config.json\n\nflags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nprints {addr, host, port, ca, ca_pem} when listening,\n"+
			"serves until stdin reaches end of file,\n"+
			"then prints {lines, lines_b64, mismatches, mismatches_b64, articles, articles_raw, notes,\n"+
			"accepted, counts, tls_handshakes, tls_failed, server_names, max_open, closed} and exits;\n"+
			"see the package documentation for the config\n")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(code int, f string, a ...any) int {
		fmt.Fprintf(stderr, "nntp-fakeserver: "+f+"\n", a...)
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "nntp-fakeserver: want exactly one argument, the config file")
		fs.Usage()
		return 2
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return fail(2, "%v", err)
	}

	dir, err := os.MkdirTemp("", "nntp-fakeserver-")
	if err != nil {
		return fail(1, "%v", err)
	}
	defer os.RemoveAll(dir)
	leaf, err := tlscert.GenerateSelfSigned(tlscert.Options{CommonName: "nntp-fakeserver"})
	if err != nil {
		return fail(1, "%v", err)
	}
	files, err := leaf.WriteFiles(dir, 0o600)
	if err != nil {
		return fail(1, "%v", err)
	}
	tlsCfg := leaf.ServerConfig()

	cfg, err := parseConfig(data, tlsCfg)
	if err != nil {
		return fail(2, "%s: %v", fs.Arg(0), err)
	}
	var store *fakeserver.Store
	if cfg.Mode == "store" {
		if store, err = buildStore(cfg.Store); err != nil {
			return fail(2, "%s: %v", fs.Arg(0), err)
		}
	}
	srv, rec, err := start(cfg, store, *listen, tlsCfg)
	if err != nil {
		return fail(1, "%v", err)
	}
	defer srv.Close()

	out := json.NewEncoder(stdout)
	out.SetEscapeHTML(false)
	if err := out.Encode(ready{Addr: srv.Addr(), Host: srv.Host(), Port: srv.Port(), CA: files.CA, CAPEM: string(leaf.CAPEM)}); err != nil {
		return fail(1, "stdout: %v", err)
	}

	eof := make(chan struct{})
	go func() { // the caller closes stdin when its client is done
		_, _ = io.Copy(io.Discard, stdin)
		close(eof)
	}()
	select {
	case <-eof:
	case <-ctx.Done():
	}
	srv.Close() // waits for the connection goroutines: what they recorded is complete

	rec.mu.Lock()
	defer rec.mu.Unlock()
	s := summary{
		Lines: srv.Lines(), Mismatches: srv.Mismatches(), Articles: srv.Articles(), Notes: rec.notes,
		Accepted: srv.Accepted(), Counts: srv.Counts(),
		TLSHandshakes: srv.TLSHandshakes(), TLSFailed: srv.TLSFailed(), ServerNames: srv.TLSServerNames(),
		MaxOpen: srv.MaxOpen(), Closed: srv.Closed(),
	}
	raws := srv.ArticlesRaw()
	if cfg.Mode == "raw" {
		s.Lines, s.Mismatches, s.Articles, s.Counts = rec.lines, rec.mismatches, rec.articles, rec.counts
		raws = rec.articleRaw
	}
	s.LinesB64, s.MismatchesB64, s.ArticlesRaw = b64s(s.Lines), b64s(s.Mismatches), []string{}
	for _, b := range raws {
		s.ArticlesRaw = append(s.ArticlesRaw, base64.StdEncoding.EncodeToString(b))
	}
	s.Lines, s.Mismatches, s.Notes, s.ServerNames = nz(s.Lines), nz(s.Mismatches), nz(s.Notes), nz(s.ServerNames)
	if s.Articles == nil {
		s.Articles = [][]string{}
	}
	if s.Counts == nil {
		s.Counts = map[string]int{}
	}
	if err := out.Encode(s); err != nil {
		return fail(1, "stdout: %v", err)
	}
	return 0
}

// start starts the server of cfg (store: the Store of mode store); listen, when not empty,
// overrides Options.Addr.
func start(cfg *config, store *fakeserver.Store, listen string, tlsCfg *tls.Config) (*fakeserver.Server, *recorder, error) {
	o := cfg.Options
	if listen != "" {
		o.Addr = listen
	}
	if o.Deadline <= 0 {
		o.Deadline = defaultDeadline
	}
	rec := &recorder{}
	var srv *fakeserver.Server
	var err error
	switch cfg.Mode {
	case "store":
		o.Store = store
		srv, err = fakeserver.Start(o)
	case "peer":
		po := cfg.PeerOptions
		srv, err = fakeserver.StartPeer(o, &po)
	case "script":
		srv, err = fakeserver.Start(o)
	case "raw":
		o.NoGreeting = true // the script writes the greeting
		o.Handler = rawHandler(cfg.Raw, rec, tlsCfg, o.Deadline)
		srv, err = fakeserver.Start(o)
	}
	return srv, rec, err
}

// b64s returns the strings in base64 (never nil).
func b64s(s []string) []string {
	out := make([]string, len(s))
	for i, l := range s {
		out[i] = base64.StdEncoding.EncodeToString([]byte(l))
	}
	return out
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
