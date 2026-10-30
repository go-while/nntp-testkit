package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/fakeserver"
)

func store() *fakeserver.Store {
	st := fakeserver.NewStore()
	st.AddGroup("cli.test", "a group for the CLI test")
	for _, id := range []string{"<1@cli.invalid>", "<2@cli.invalid>"} {
		st.AddArticle("cli.test", fakeserver.Article{
			Headers: []string{"From: t@cli.invalid", "Subject: " + id, "Newsgroups: cli.test", "Message-ID: " + id},
			Body:    []string{"body", ".dotted"},
		})
	}
	return st
}

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

// TestRunExitCodes: 0 when no check fails, 1 on a Fail, 2 on a usage or connection error.
func TestRunExitCodes(t *testing.T) {
	good := fakeserver.New(t, fakeserver.Options{Store: store()})
	h := fakeserver.StoreHandler(store(), fakeserver.StoreOptions{})
	bareLF := fakeserver.New(t, fakeserver.Options{NoGreeting: true, Handler: func(c *fakeserver.Conn) {
		if _, err := io.WriteString(c.Raw(), "200 fakeserver ready\n"); err != nil {
			return
		}
		h(c)
	}})
	busy := fakeserver.New(t, fakeserver.Options{Store: store(), Greeting: "502 Too many connections"})
	cases := []struct {
		name   string
		args   []string
		code   int
		stdout string // substring of stdout
		stderr string // substring of stderr
	}{
		{name: "pass", args: []string{"-addr", good.Addr(), "-deadline", "20s"}, code: 0, stdout: "0 fail"},
		{name: "skip", args: []string{"-addr", good.Addr(), "-skip", "help, date"}, code: 0, stdout: "skipped by option"},
		{name: "bare-lf", args: []string{"-addr", bareLF.Addr()}, code: 1, stdout: "BareLF"},
		{name: "no-addr", args: nil, code: 2, stderr: "-addr is required"},
		{name: "bad-addr", args: []string{"-addr", "localhost"}, code: 2, stderr: "missing port"},
		{name: "bad-flag", args: []string{"-nope"}, code: 2, stderr: "-nope"},
		{name: "extra-arg", args: []string{"-addr", good.Addr(), "x"}, code: 2, stderr: "unexpected arguments"},
		{name: "unknown-skip", args: []string{"-addr", good.Addr(), "-skip", "nope"}, code: 2, stderr: `unknown check "nope"`},
		{name: "closed-port", args: []string{"-addr", closedAddr(t)}, code: 2, stderr: "refused"},
		{name: "help", args: []string{"-h"}, code: 0, stderr: "quit-eof"},
		{name: "workers", args: []string{"-addr", good.Addr(), "-workers", "1"}, code: 0, stdout: "0 fail"},
		{name: "bad-workers", args: []string{"-addr", good.Addr(), "-workers", "0"}, code: 2, stderr: "-workers must be positive"},
		{name: "tls-no-host", args: []string{"-addr", ":119", "-tls"}, code: 2, stderr: "-tls needs a host"},
		{name: "unavailable", args: []string{"-addr", busy.Addr()}, code: 1, stdout: "service unavailable: 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(tc.args, &out, &errb); code != tc.code {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.code, out.String(), errb.String())
			}
			if !strings.Contains(out.String(), tc.stdout) {
				t.Errorf("stdout lacks %q:\n%s", tc.stdout, out.String())
			}
			if !strings.Contains(errb.String(), tc.stderr) {
				t.Errorf("stderr lacks %q:\n%s", tc.stderr, errb.String())
			}
		})
	}
}

func TestRunStrictOverviewFmt(t *testing.T) {
	for _, tc := range []struct {
		name           string
		legacy, strict bool
		want           int
	}{
		{"preferred-strict", false, true, 0},
		{"legacy-default", true, false, 0},
		{"legacy-strict", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			so := fakeserver.StoreOptions{}
			if tc.legacy {
				so.OverviewFmt = []string{"Subject:", "From:", "Date:", "Message-ID:", "References:", "Bytes:", "Lines:", "Xref:full"}
			}
			srv := fakeserver.New(t, fakeserver.Options{Store: store(), StoreOpts: so})
			args := []string{"-addr", srv.Addr()}
			if tc.strict {
				args = append(args, "-strict-overview-fmt")
			}
			var out, errb bytes.Buffer
			if code := run(args, &out, &errb); code != tc.want {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.want, out.String(), errb.String())
			}
			if tc.want == 1 && !strings.Contains(out.String(), "LIST OVERVIEW.FMT line 6") {
				t.Errorf("strict mode failed for another reason:\n%s", out.String())
			}
		})
	}
}

// TestRunJSON: -json prints the results and the capability list as JSON.
func TestRunJSON(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: store()})
	var out, errb bytes.Buffer
	if code := run([]string{"-addr", srv.Addr(), "-json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	var r jsonReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out.String())
	}
	if r.Fail != 0 || len(r.Results) != r.Pass+r.Skip || len(r.Results) == 0 {
		t.Errorf("counts pass %d fail %d skip %d for %d results", r.Pass, r.Fail, r.Skip, len(r.Results))
	}
	if len(r.Results) == 0 || r.Results[0].Name != "greeting" || r.Results[0].Status != "PASS" {
		t.Errorf("first result %+v, want greeting PASS", r.Results[0])
	}
	if len(r.Capabilities) == 0 || r.Capabilities[0] != "VERSION 2" {
		t.Errorf("capabilities %q", r.Capabilities)
	}
}

// TestRunTLS: -tls-insecure talks TLS from the first byte to a self-signed server; plain -tls
// verifies the certificate and fails to connect (exit 2).
func TestRunTLS(t *testing.T) {
	cfg, _ := fakeserver.TLSConfig(t)
	srv := fakeserver.New(t, fakeserver.Options{Store: store(), TLS: cfg})
	var out, errb bytes.Buffer
	if code := run([]string{"-addr", srv.Addr(), "-tls-insecure"}, &out, &errb); code != 0 {
		t.Errorf("-tls-insecure: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"-addr", srv.Addr(), "-tls"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "tls") {
		t.Errorf("-tls with an untrusted cert: exit %d, stderr %q; want 2 naming tls", code, errb.String())
	}
}
