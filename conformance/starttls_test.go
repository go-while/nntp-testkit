package conformance

import (
	"crypto/tls"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
	"github.com/go-while/nntp-testkit/tlscert"
)

// startTLSNames are the checks that need STARTTLS (tls-capabilities is for implicit TLS).
var startTLSNames = slices.DeleteFunc(names(startTLSChecks()), func(n string) bool { return n == "tls-capabilities" })

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
		for _, n := range startTLSNames[:2] {
			ctWant(t, ctCheck(t, o, n), Fail, `got "`+refuse+`", want 382`)
		}
		// the pipelining is the client's violation: nothing to judge
		ctWant(t, ctCheck(t, o, "starttls-pipelined"), Skip, `answered "`+refuse+`", not 382`)
	}
}

// stPeer is a hand-written STARTTLS server (LineHandler) with one mistake at a time.
type stPeer struct {
	capsAfter  []string // CAPABILITIES over TLS (default: without STARTTLS)
	inject     bool     // plaintext right behind the 382
	twice382   bool     // a second STARTTLS over TLS gets 382 again
	greetAfter bool     // a new greeting after the handshake
	execPipe   bool     // reads the command behind STARTTLS as a plaintext command (no handshake)
	after      string   // keeps the DATE behind STARTTLS in the buffer, does the handshake, then: "exec" runs it
	// (CVE-2011-0411), "exec-split" runs it with the reply in two writes, "hello" sends a greeting,
	// "discard" does nothing (the compliant ideal), "reset" resets the connection
	splitPlain bool // with execPipe: the plaintext reply in two writes
	splitInj   bool // with inject: the plaintext in a write of its own, a moment after the 382
	garbage    bool // text that is no NNTP reply behind the 382, then close
	noHandshk  bool // 382, then closes the connection without a handshake
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
			if p.inject && !p.splitInj {
				_, _ = c.Raw().Write([]byte("382 go\r\n200 injected\r\n")) // one segment: the client reads both at once
				return nil, true
			}
			_ = c.WriteLines("382 go")
			switch {
			case p.inject:
				time.Sleep(100 * time.Millisecond) // a read of its own on the client
				_ = c.WriteLines("200 injected")
				return nil, true
			case p.garbage:
				_ = c.WriteLines("hello, this is not NNTP")
				return nil, true
			case p.noHandshk:
				return nil, true
			case p.after != "":
				if _, err := c.ReadLine(); err != nil {
					return nil, true
				}
				tc := tls.Server(c.Raw(), cfg)
				if tc.Handshake() != nil {
					return nil, true
				}
				switch p.after {
				case "exec":
					_, _ = tc.Write([]byte("111 20250101000000\r\n"))
				case "exec-split":
					_, _ = tc.Write([]byte("1"))
					time.Sleep(80 * time.Millisecond)
					_, _ = tc.Write([]byte("11 20250101000000\r\n"))
				case "hello":
					_, _ = tc.Write([]byte("200 hello again\r\n"))
				case "reset":
					if tcp, ok := c.Raw().(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
						_ = tcp.Close()
					}
					return nil, true
				case "discard":
					_, _ = tc.Read(make([]byte, 1)) // until the client closes
					return nil, true
				}
				time.Sleep(200 * time.Millisecond)
				return nil, true
			case p.execPipe && p.splitPlain:
				if _, err := c.ReadLine(); err != nil {
					return nil, true
				}
				_, _ = c.Raw().Write([]byte("1"))
				time.Sleep(80 * time.Millisecond)
				_, _ = c.Raw().Write([]byte("11 20250101000000\r\n"))
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

// A greeting after the handshake is the mistake of check starttls; starttls-pipelined must not
// blame it on the command behind STARTTLS.
func TestStartTLSPipelinedGreetingNotBlamedOnCVE(t *testing.T) {
	o := Options{Addr: stPeer{after: "hello"}.server(t).Addr(), Deadline: time.Second}
	r := ctCheck(t, o, "starttls-pipelined")
	ctWant(t, r, Fail, `"200 hello again`)
	if strings.Contains(r.Detail, "was executed") || strings.Contains(r.Detail, "CVE") {
		t.Errorf("Detail %q blames the DATE for a greeting", r.Detail)
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
		// the plaintext is in the client's buffer at the 382, or arrives in a read of its own and
		// reaches the handshake: both are the same mistake
		{"plaintext after 382", stPeer{inject: true}, "starttls", "plaintext after the 382"},
		{"plaintext after 382, later read", stPeer{inject: true, splitInj: true}, "starttls", "plaintext after the 382"},
		{"greeting after handshake", stPeer{greetAfter: true}, "starttls", "no greeting is sent after the handshake"},
		{"second STARTTLS 382", stPeer{twice382: true}, "starttls-twice", `got "382 again", want [502]`},
		{"pipelined executed before the handshake", stPeer{execPipe: true}, "starttls-pipelined", `answered in plaintext right after the 382: "111 `},
		{"pipelined executed after the handshake", stPeer{after: "exec"}, "starttls-pipelined", `executed after the TLS handshake: the server sent "111 `},
		{"pipelined executed after the handshake, split reply", stPeer{after: "exec-split"}, "starttls-pipelined", `executed after the TLS handshake: the server sent "111 `},
		{"pipelined executed before the handshake, split reply", stPeer{execPipe: true, splitPlain: true}, "starttls-pipelined", `answered in plaintext right after the 382: "111 `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Addr: tc.peer.server(t).Addr(), Deadline: time.Second}
			ctWant(t, ctCheck(t, o, tc.check), Fail, tc.want)
		})
	}
}

// A server that does not run the command behind STARTTLS passes starttls-pipelined, whichever
// way it refuses it, and the Detail says which.
func TestStartTLSPipelinedPasses(t *testing.T) {
	cases := []struct {
		name string
		peer stPeer
		want string
	}{
		{"not NNTP text", stPeer{garbage: true}, "not an NNTP reply"},
		{"closed", stPeer{noHandshk: true}, "did not complete the TLS handshake"},
		{"discarded", stPeer{after: "discard"}, "DATE sent behind STARTTLS was discarded"},
		// a reset after the handshake (a TLS 1.3 client is done before the server has checked
		// the Finished message) is the server dropping the connection, not a verdict
		{"reset after the handshake", stPeer{after: "reset"}, "dropped the connection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Addr: tc.peer.server(t).Addr(), Deadline: time.Second}
			ctWant(t, ctCheck(t, o, "starttls-pipelined"), Pass, tc.want)
		})
	}
}

// the Store server drops plaintext sent behind STARTTLS: starttls-pipelined passes, and says
// what it saw.
func TestStartTLSPipelinedStore(t *testing.T) {
	srv, _ := stServer(t, nil, nil)
	r := ctCheck(t, Options{Addr: srv.Addr(), Deadline: 2 * time.Second}, "starttls-pipelined")
	ctWant(t, r, Pass, "DATE sent behind STARTTLS was not executed")
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

// The default handshake does not judge the certificate and so not the TLS version either: a
// server that speaks only TLS 1.0 passes; a config that verifies keeps Go's default minimum and
// fails on it.
func TestStartTLSDefaultMinVersionTLS10(t *testing.T) {
	ca := tlscert.NewCA(t)
	cfg := ca.Issue(t, tlscert.Options{}).ServerConfig()
	cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS10, tls.VersionTLS10
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{StartTLS: &fakeserver.StartTLSOptions{Config: cfg}}})
	o := Options{Addr: srv.Addr(), Deadline: 2 * time.Second}
	for _, n := range startTLSNames {
		if r := ctCheck(t, o, n); r.Status != Pass {
			t.Errorf("%s with a TLS 1.0 server: %s %q", n, r.Status, r.Detail)
		}
	}
	o.StartTLS = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	ctWant(t, ctCheck(t, o, "starttls"), Pass, "skips verification")
	o.StartTLS = &tls.Config{RootCAs: ca.Pool}
	ctWant(t, ctCheck(t, o, "starttls"), Fail, "handshake")
}

func tlsCapsServer(t *testing.T, caps []string) (*fakeserver.Server, *tlscert.CA) {
	t.Helper()
	ca := tlscert.NewCA(t)
	srv := fakeserver.New(t, fakeserver.Options{TLS: ca.Issue(t, tlscert.Options{}).ServerConfig(), Handler: fakeserver.LineHandler(
		func(c *fakeserver.Conn, line string) ([]string, bool) {
			if strings.HasPrefix(line, "CAPABILITIES") {
				return caps, false
			}
			return []string{"500 what"}, false
		})})
	return srv, ca
}

func TestTLSCapabilities(t *testing.T) {
	// the Store lists no STARTTLS over implicit TLS
	ca := tlscert.NewCA(t)
	cfg := ca.Issue(t, tlscert.Options{}).ServerConfig()
	store := fakeserver.New(t, fakeserver.Options{Store: testStore(), TLS: cfg,
		StoreOpts: fakeserver.StoreOptions{StartTLS: &fakeserver.StartTLSOptions{Config: cfg}}})
	r := ctCheck(t, Options{Addr: store.Addr(), TLS: ca.ClientConfig("localhost")}, "tls-capabilities")
	if r.Status != Pass || r.Detail != "" {
		t.Errorf("Store: %s %q", r.Status, r.Detail)
	}
	// without Options.TLS: skipped, without a Missing
	plain := fakeserver.New(t, fakeserver.Options{Store: testStore()})
	r = ctCheck(t, Options{Addr: plain.Addr()}, "tls-capabilities")
	ctWant(t, r, Skip, "no Options.TLS")
	if r.Missing != "" {
		t.Errorf("Missing %q", r.Missing)
	}

	bad, bca := tlsCapsServer(t, []string{"101 caps", "VERSION 2", "READER", "STARTTLS", "."})
	ctWant(t, ctCheck(t, Options{Addr: bad.Addr(), TLS: bca.ClientConfig("localhost")}, "tls-capabilities"), Fail, "lists STARTTLS")

	// SASL EXTERNAL without a client certificate is noted, with one it is not
	sasl, sca := tlsCapsServer(t, []string{"101 caps", "VERSION 2", "READER", "SASL PLAIN EXTERNAL", "."})
	o := Options{Addr: sasl.Addr(), TLS: sca.ClientConfig("localhost")}
	ctWant(t, ctCheck(t, o, "tls-capabilities"), Pass, "SASL EXTERNAL is listed although no client certificate")
	o.TLS = o.TLS.Clone()
	o.TLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	if r := ctCheck(t, o, "tls-capabilities"); r.Status != Pass || r.Detail != "" {
		t.Errorf("with a client certificate: %s %q", r.Status, r.Detail)
	}
}

// Options.Allow turns a Fail whose Detail contains the substring into a Skip and leaves every
// other result alone.
func TestAllow(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(),
		Faults: fakeserver.Faults{Override: map[string][]string{"HELP": {"500 nope"}}}})
	help := asFind(t, readerChecks(), "help")
	run := func(allow map[string]string) Result {
		r := runOne(Options{Addr: srv.Addr(), Allow: allow}, help)
		t.Logf("%s %s", r.Status, r.Detail)
		return r
	}
	if r := run(nil); r.Status != Fail {
		t.Fatalf("without Allow: %s %q, want FAIL", r.Status, r.Detail)
	}
	orig := run(nil).Detail
	r := run(map[string]string{"help": `got "500 nope"`})
	if r.Status != Skip || r.Detail != "allowed: "+orig || r.Missing != "" || r.RFC == "" {
		t.Errorf("allowed: %+v, want SKIP \"allowed: %s\"", r, orig)
	}
	for name, allow := range map[string]map[string]string{
		"other substring": {"help": "501"},
		"other check":     {"date": `got "500 nope"`},
		"empty substring": {"help": ""},
	} {
		if r := run(allow); r.Status != Fail || r.Detail != orig {
			t.Errorf("%s: %+v, want the Fail unchanged", name, r)
		}
	}
	// a passing check is left alone, an allowed panic is a Skip too
	date := asFind(t, readerChecks(), "date")
	if r := runOne(Options{Addr: srv.Addr(), Allow: map[string]string{"date": "x"}}, date); r.Status != Pass {
		t.Errorf("date: %+v", r)
	}
	boom := check{Name: "boom", RFC: "x", Run: func(*session, check) Result { panic("kaboom") }}
	if r := runOne(Options{Addr: srv.Addr(), Allow: map[string]string{"boom": "kaboom"}}, boom); r.Status != Skip || !strings.HasPrefix(r.Detail, "allowed: panic") {
		t.Errorf("panic: %+v", r)
	}
}

// Options.AllowReply matches a Fail by check, command and reply code, not by the Detail text.
func TestAllowReply(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(),
		Faults: fakeserver.Faults{Override: map[string][]string{"HELP": {"500 nope"}}}})
	help := asFind(t, readerChecks(), "help")
	run := func(o Options, c check) Result {
		o.Addr = srv.Addr()
		r := runOne(o, c)
		t.Logf("%s %s [%s %d]", r.Status, r.Detail, r.Cmd, r.Code)
		return r
	}
	failed := run(Options{}, help)
	if failed.Status != Fail || failed.Cmd != "HELP" || failed.Code != 500 {
		t.Fatalf("without AllowReply: %+v, want FAIL with Cmd HELP, Code 500", failed)
	}
	for name, ar := range map[string]AllowReply{
		"check, cmd and code": {"help", "HELP", 500},
		"lower case cmd":      {"help", "help", 500},
		"cmd only":            {"help", "HELP", 0},
		"code only":           {"help", "", 500},
	} {
		r := run(Options{AllowReply: []AllowReply{{"date", "DATE", 111}, ar}}, help)
		if r.Status != Skip || r.Detail != "allowed: "+failed.Detail || r.Missing != "" || r.Cmd != "HELP" || r.Code != 500 {
			t.Errorf("%s: %+v, want SKIP \"allowed: %s\"", name, r, failed.Detail)
		}
	}
	for name, ar := range map[string]AllowReply{
		"other code":    {"help", "HELP", 501},
		"other cmd":     {"help", "DATE", 500},
		"longer cmd":    {"help", "HELP ME", 500},
		"other check":   {"date", "HELP", 500},
		"empty entry":   {"help", "", 0},
		"no check name": {"", "HELP", 500},
	} {
		if r := run(Options{AllowReply: []AllowReply{ar}}, help); r.Status != Fail || r.Detail != failed.Detail {
			t.Errorf("%s: %+v, want the Fail unchanged", name, r)
		}
	}

	// the match survives a rewording of the Detail; the text form does not
	reworded := check{Name: help.Name, RFC: help.RFC, Run: func(s *session, c check) Result {
		r := help.Run(s, c)
		if r.Status == Fail {
			r.Detail = "HELP answered something else than the RFC wants"
		}
		return r
	}}
	if r := run(Options{Allow: map[string]string{"help": `got "500 nope"`}}, reworded); r.Status != Fail {
		t.Errorf("text Allow after a rewording: %+v, want FAIL (the text no longer matches)", r)
	}
	r := run(Options{AllowReply: []AllowReply{{"help", "HELP", 500}}}, reworded)
	if r.Status != Skip || r.Detail != "allowed: HELP answered something else than the RFC wants" {
		t.Errorf("AllowReply after a rewording: %+v, want SKIP", r)
	}
	// Allow and AllowReply side by side: either allows
	if r := run(Options{Allow: map[string]string{"help": "nope"}, AllowReply: []AllowReply{{"help", "HELP", 501}}}, help); r.Status != Skip {
		t.Errorf("Allow with a non-matching AllowReply: %+v, want SKIP", r)
	}

	// only the exchange a check ended on counts: a wrong code that the check went on after
	// is forgotten, and a Fail that is no wrong reply has no Cmd or Code
	stale := check{Name: "help", RFC: "x", Run: func(s *session, c check) Result {
		_, _, _ = s.expect([]int{100}, "HELP") // 500: tolerated
		_, _, _ = s.cmd("DATE")
		return c.fail("another mistake")
	}}
	if r := run(Options{AllowReply: []AllowReply{{"help", "HELP", 500}}}, stale); r.Status != Fail || r.Cmd != "" || r.Code != 0 {
		t.Errorf("stale: %+v, want FAIL without Cmd and Code", r)
	}
}

// AllowReply.matches compares the command word by word, as a prefix.
func TestAllowReplyMatches(t *testing.T) {
	r := Result{Name: "x", Status: Fail, Cmd: "LIST ACTIVE", Code: 215}
	for _, tc := range []struct {
		a    AllowReply
		want bool
	}{
		{AllowReply{"x", "LIST", 0}, true},
		{AllowReply{"x", "list active", 215}, true},
		{AllowReply{"x", "  LIST   ACTIVE ", 0}, true},
		{AllowReply{"x", "LIST ACTIVE.TIMES", 0}, false},
		{AllowReply{"x", "LISTGROUP", 0}, false},
		{AllowReply{"x", "LIST ACTIVE NOW", 0}, false},
		{AllowReply{"x", "", 215}, true},
		{AllowReply{"x", "", 216}, false},
		{AllowReply{"x", "", 0}, false},
	} {
		if got := tc.a.matches(r); got != tc.want {
			t.Errorf("%+v matches %+v: %v, want %v", tc.a, r, got, tc.want)
		}
	}
	if (AllowReply{"x", "LIST", 0}).matches(Result{Name: "x", Status: Fail}) {
		t.Error("an entry matched a Fail without Cmd and Code")
	}
}

// cmdName keeps the leading keywords of a shown command, never an argument or a password.
func TestCmdName(t *testing.T) {
	for in, want := range map[string]string{
		"IHAVE <a@b>":                   "IHAVE",
		"AUTHINFO PASS SECRET":          "AUTHINFO PASS", // masked before: "AUTHINFO PASS ***"
		"LIST OVERVIEW.FMT":             "LIST OVERVIEW.FMT",
		"GROUP alt.test":                "GROUP",
		"MODE STREAM":                   "MODE STREAM",
		"IHAVE <a@b> (article sent)":    "IHAVE",
		"HDR Subject 1-5":               "HDR",
		"STAT 12":                       "STAT",
		"":                              "",
		"<a@b>":                         "",
		"TAKETHIS <id> (article sent)":  "TAKETHIS",
		"AUTHINFO USER fred":            "AUTHINFO USER",
		"over compress: pipelined DATE": "",
	} {
		s := &session{}
		s.mismatch(in, 500)
		if s.bad.cmd != want {
			t.Errorf("mismatch(%q): cmd %q, want %q", in, s.bad.cmd, want)
		}
	}
}
