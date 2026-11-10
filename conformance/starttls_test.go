package conformance

import (
	"crypto/tls"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
	"github.com/go-while/nntp-testkit/tlscert"
)

var startTLSNames = names(startTLSChecks())

// stServer is a Store server that offers STARTTLS with a certificate of ca (a new CA when nil).
func stServer(t *testing.T, ca *tlscert.CA, mod func(*fakeserver.StartTLSOptions)) (*fakeserver.Server, *tlscert.CA) {
	t.Helper()
	if ca == nil {
		ca = tlscert.NewCA(t)
	}
	st := &fakeserver.StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}
	if mod != nil {
		mod(st)
	}
	return fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{StartTLS: st}}), ca
}

func TestStartTLSChecksStore(t *testing.T) {
	srv, ca := stServer(t, nil, nil)
	// no verification configured: the handshake goes through and the Detail says so
	o := Options{Addr: srv.Addr(), Deadline: 2 * time.Second}
	ctWant(t, ctCheck(t, o, "starttls"), Pass, "certificate not verified")
	for _, n := range startTLSNames[1:] {
		if r := ctCheck(t, o, n); r.Status != Pass {
			t.Errorf("%s: %s %q", n, r.Status, r.Detail)
		}
	}
	// verified against the CA of the certificate: Pass without a note
	o.StartTLS = &tls.Config{RootCAs: ca.Pool}
	for _, n := range startTLSNames {
		// starttls-pipelined says what it saw behind the STARTTLS; the others have nothing to add
		if r := ctCheck(t, o, n); r.Status != Pass || (r.Detail != "" && n != "starttls-pipelined") {
			t.Errorf("verified %s: %s %q, want PASS without a Detail", n, r.Status, r.Detail)
		}
	}
	// a config that skips verification says so too
	o.StartTLS = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	ctWant(t, ctCheck(t, o, "starttls"), Pass, "skips verification")
	if srv.TLSHandshakes() < 5 {
		t.Errorf("%d handshakes, want one per check and run", srv.TLSHandshakes())
	}
}

func TestStartTLSWrongCA(t *testing.T) {
	srv, _ := stServer(t, nil, nil)
	other := tlscert.NewCA(t)
	o := Options{Addr: srv.Addr(), Deadline: 2 * time.Second, StartTLS: &tls.Config{RootCAs: other.Pool}}
	ctWant(t, ctCheck(t, o, "starttls"), Fail, "not accepted by Options.StartTLS")
	// the system roots do not know the test CA either
	o.StartTLS = &tls.Config{}
	ctWant(t, ctCheck(t, o, "starttls-twice"), Fail, "not accepted by Options.StartTLS")
}

func TestStartTLSNotAdvertised(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore()})
	for _, n := range startTLSNames {
		r := ctCheck(t, Options{Addr: srv.Addr()}, n)
		ctWant(t, r, Skip, "STARTTLS not advertised")
		if r.Missing != "STARTTLS" {
			t.Errorf("%s: Missing %q", n, r.Missing)
		}
	}
	if srv.Count("STARTTLS") != 0 {
		t.Error("STARTTLS sent to a server that does not list it")
	}
	// listed STARTTLS hidden from the capability list: skipped, never probed
	hidden, _ := stServer(t, nil, func(o *fakeserver.StartTLSOptions) { o.HideCapability = true })
	ctWant(t, ctCheck(t, Options{Addr: hidden.Addr()}, "starttls"), Skip, "STARTTLS not advertised")
}

func TestStartTLSOverImplicitTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	cfg := ca.Issue(t, tlscert.Options{}).ServerConfig()
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), TLS: cfg,
		StoreOpts: fakeserver.StoreOptions{StartTLS: &fakeserver.StartTLSOptions{Config: cfg}}})
	o := Options{Addr: srv.Addr(), TLS: ca.ClientConfig("localhost")}
	for _, n := range startTLSNames {
		r := ctCheck(t, o, n)
		ctWant(t, r, Skip, "TLS from the first byte")
		if r.Missing != "" {
			t.Errorf("%s: Missing %q, want empty", n, r.Missing)
		}
	}
}

// 580: the server cannot negotiate TLS: starttls passes with the Detail, the others skip; any
// other refusal of a plain, unauthenticated connection fails.
func TestStartTLSRefused(t *testing.T) {
	srv, _ := stServer(t, nil, func(o *fakeserver.StartTLSOptions) { o.Refuse = "580 Can not initiate TLS negotiation" })
	o := Options{Addr: srv.Addr(), Deadline: 2 * time.Second}
	ctWant(t, ctCheck(t, o, "starttls"), Pass, "580")
	ctWant(t, ctCheck(t, o, "starttls-twice"), Skip, "580")
	ctWant(t, ctCheck(t, o, "starttls-pipelined"), Skip, "580")

	for _, refuse := range []string{"502 STARTTLS not available", "483 encryption required"} {
		srv, _ := stServer(t, nil, func(o *fakeserver.StartTLSOptions) { o.Refuse = refuse })
		o := Options{Addr: srv.Addr(), Deadline: 2 * time.Second}
		for _, n := range startTLSNames {
			ctWant(t, ctCheck(t, o, n), Fail, `got "`+refuse+`", want 382`)
		}
	}
}

// stPeer is a hand-written STARTTLS server (LineHandler) with one mistake at a time.
type stPeer struct {
	capsAfter  []string // CAPABILITIES over TLS (default: without STARTTLS)
	inject     bool     // plaintext right behind the 382
	twice382   bool     // a second STARTTLS over TLS gets 382 again
	greetAfter bool     // a new greeting after the handshake
	execPipe   bool     // reads the command behind STARTTLS as a plaintext command (no handshake)
}

func (p stPeer) server(t *testing.T) *fakeserver.Server {
	t.Helper()
	ca := tlscert.NewCA(t)
	cfg := ca.Issue(t, tlscert.Options{}).ServerConfig()
	plain := []string{"101 caps", "VERSION 2", "READER", "STARTTLS", "."}
	over := []string{"101 caps", "VERSION 2", "READER", "."}
	if p.capsAfter != nil {
		over = p.capsAfter
	}
	return fakeserver.New(t, fakeserver.Options{Deadline: 3 * time.Second, Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		switch strings.ToUpper(strings.Fields(line + " x")[0]) {
		case "CAPABILITIES":
			if c.TLS() {
				return over, false
			}
			return plain, false
		case "STARTTLS":
			if c.TLS() {
				if p.twice382 {
					return []string{"382 again"}, false
				}
				return []string{"502 TLS active"}, false
			}
			_ = c.WriteLines("382 go")
			switch {
			case p.inject:
				_ = c.WriteLines("200 injected")
				return nil, true
			case p.execPipe:
				return nil, false // the next line is read and answered in plaintext
			}
			if err := c.StartTLS(cfg); err != nil {
				return nil, true
			}
			if p.greetAfter {
				_ = c.WriteLines("200 hello again")
			}
			return nil, false
		case "DATE":
			return []string{"111 20250101000000"}, false
		}
		return []string{"500 what"}, false
	})})
}

func TestStartTLSHandPeerPasses(t *testing.T) {
	o := Options{Addr: stPeer{}.server(t).Addr(), Deadline: time.Second}
	for _, n := range []string{"starttls", "starttls-twice"} {
		if r := ctCheck(t, o, n); r.Status != Pass {
			t.Errorf("%s: %s %q", n, r.Status, r.Detail)
		}
	}
}

func TestStartTLSMistakes(t *testing.T) {
	cases := []struct {
		name  string
		peer  stPeer
		check string
		want  string
	}{
		{"still advertised", stPeer{capsAfter: []string{"101 caps", "VERSION 2", "READER", "STARTTLS", "."}}, "starttls", `lists "STARTTLS" while TLS is active`},
		{"mode-reader advertised", stPeer{capsAfter: []string{"101 caps", "VERSION 2", "MODE-READER", "."}}, "starttls", `lists "MODE-READER" while TLS is active`},
		{"plaintext after 382", stPeer{inject: true}, "starttls", "plaintext after the 382 line"},
		{"greeting after handshake", stPeer{greetAfter: true}, "starttls", "no greeting is sent"},
		{"second STARTTLS 382", stPeer{twice382: true}, "starttls-twice", `got "382 again", want [502]`},
		{"pipelined executed", stPeer{execPipe: true}, "starttls-pipelined", `answered in plaintext after the 382: "111 `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Addr: tc.peer.server(t).Addr(), Deadline: time.Second}
			ctWant(t, ctCheck(t, o, tc.check), Fail, tc.want)
		})
	}
}

// the Store server drops plaintext sent behind STARTTLS: starttls-pipelined passes, and says
// what it saw.
func TestStartTLSPipelinedStore(t *testing.T) {
	srv, _ := stServer(t, nil, nil)
	r := ctCheck(t, Options{Addr: srv.Addr(), Deadline: 2 * time.Second}, "starttls-pipelined")
	ctWant(t, r, Pass, "DATE sent behind STARTTLS was not answered")
	if srv.Count("DATE") != 0 {
		t.Error("the server executed the DATE")
	}
}

// STARTTLS is not sent after a login: the checks never log in.
func TestStartTLSNoLogin(t *testing.T) {
	ca := tlscert.NewCA(t)
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{User: "u", Pass: "p",
		StartTLS: &fakeserver.StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}}})
	o := Options{Addr: srv.Addr(), User: "u", Pass: "p", Deadline: 2 * time.Second}
	for _, n := range startTLSNames {
		if r := ctCheck(t, o, n); r.Status != Pass {
			t.Errorf("%s: %s %q", n, r.Status, r.Detail)
		}
	}
	if n := srv.Count("AUTHINFO"); n != 0 {
		t.Errorf("%d AUTHINFO sent by the STARTTLS checks", n)
	}
}

func TestRequire(t *testing.T) {
	r := Report{Results: []Result{
		{Name: "a", Status: Pass, RFC: "RFC 1"},
		{Name: "b", Status: Fail, RFC: "RFC 2", Detail: "broke"},
		{Name: "c", Status: Skip, RFC: "RFC 3", Detail: "X not advertised", Missing: "X"},
	}}
	rec := &recordTB{TB: t}
	r.Require(rec, "a")
	if rec.Failed() {
		t.Errorf("Require(a): %q", rec.errors)
	}
	rec = &recordTB{TB: t}
	r.Require(rec, "a", "b", "c", "nosuch", "b")
	if len(rec.errors) != 3 {
		t.Fatalf("errors %q, want one each for b, c and nosuch", rec.errors)
	}
	for i, sub := range []string{"b (RFC 2) is FAIL: broke", "c (RFC 3) is SKIP: X not advertised", `"nosuch" is not in the report`} {
		if !strings.Contains(rec.errors[i], sub) {
			t.Errorf("error %d %q lacks %q", i, rec.errors[i], sub)
		}
	}
	rec = &recordTB{TB: t}
	r.RequireNoFail(rec)
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "FAIL b (RFC 2): broke") {
		t.Errorf("RequireNoFail: %q", rec.errors)
	}
	rec = &recordTB{TB: t}
	Report{Results: r.Results[:1]}.RequireNoFail(rec)
	Report{Results: r.Results[2:]}.RequireNoFail(rec)
	if rec.Failed() {
		t.Errorf("RequireNoFail on pass and skip: %q", rec.errors)
	}
	rec = &recordTB{TB: t}
	Report{}.RequireNoFail(rec)
	if len(rec.errors) != 1 {
		t.Errorf("RequireNoFail on an empty report: %q", rec.errors)
	}
}

// TestCheckStoreCompressStartTLS: a Store server that offers both passes the whole run, the
// STARTTLS and COMPRESS checks included (the stream and auth checks skip as in
// TestCheckStorePasses).
func TestCheckStoreCompressStartTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true,
		StartTLS: &fakeserver.StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}}})
	r := Run(t, Options{Addr: srv.Addr(), PostGroup: "conf.test.b", StartTLS: &tls.Config{RootCAs: ca.Pool}})
	by := r.ByName()
	for _, n := range slices.Concat(startTLSNames, compressNames) {
		if by[n].Status != Pass {
			t.Errorf("%s: %s %q, want PASS", n, by[n].Status, by[n].Detail)
		}
	}
	r.Require(t, slices.Concat(startTLSNames, compressNames)...)
	r.RequireNoFail(t)
}
