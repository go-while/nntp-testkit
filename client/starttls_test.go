package client

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/fakeserver"
	"github.com/go-while/nntp-testkit/tlscert"
)

func tlsStore() *fakeserver.Store {
	st := fakeserver.NewStore()
	st.AddGroup("alt.test", "Testing")
	st.AddArticle("alt.test", fakeserver.Article{Headers: []string{"Subject: one", "From: a@x", "Message-ID: <a1@x>"}, Body: []string{"hi"}})
	return st
}

// tlsServer starts a Store server that offers STARTTLS with a leaf issued by ca.
func tlsServer(t *testing.T, ca *tlscert.CA, leaf tlscert.Options, o fakeserver.StartTLSOptions, so fakeserver.StoreOptions) *fakeserver.Server {
	t.Helper()
	o.Config = ca.Issue(t, leaf).ServerConfig()
	so.StartTLS = &o
	return fakeserver.New(t, fakeserver.Options{Store: tlsStore(), StoreOpts: so})
}

func hasCap(t *testing.T, c *Conn, name string) bool {
	t.Helper()
	caps, _ := c.Capabilities()
	_, ok := caps[name]
	return ok
}

func TestStartTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{CommonName: "news.test"}, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{})
	c := Dial(t, s.Addr())
	if !hasCap(t, c, "STARTTLS") {
		t.Fatal("STARTTLS not advertised on the plain connection")
	}
	if c.IsTLS() {
		t.Error("plain conn reports TLS")
	}
	tc := c.StartTLS(ca.ClientConfig("localhost"))
	st, ok := tc.TLSState()
	if !ok || !st.HandshakeComplete || st.Version < tls.VersionTLS12 {
		t.Fatalf("state %+v ok=%v", st, ok)
	}
	if len(st.PeerCertificates) != 1 || st.PeerCertificates[0].Subject.CommonName != "news.test" || len(st.VerifiedChains) != 1 {
		t.Errorf("peer certificates: %v", st.PeerCertificates)
	}
	if !tc.Strict || tc.Greeting != c.Greeting {
		t.Errorf("new conn: strict=%v greeting=%q", tc.Strict, tc.Greeting)
	}
	if hasCap(t, tc, "STARTTLS") {
		t.Error("STARTTLS still advertised after the upgrade")
	}
	if code, line := tc.Cmd("STARTTLS"); code != 502 {
		t.Errorf("second STARTTLS: %q", line)
	}
	tc.ExpectCmd(211, "GROUP alt.test")
	tc.ExpectCmd(220, "ARTICLE 1")
	tc.ReadDotLines()
	if s.TLSHandshakes() != 1 || !slices.Equal(s.TLSServerNames(), []string{"localhost"}) || s.TLSFailed() != 0 {
		t.Errorf("server saw %d handshakes, names %q, %d failed", s.TLSHandshakes(), s.TLSServerNames(), s.TLSFailed())
	}
}

// TestStartTLSDefaultServerName: no ServerName in the config means the host of the connection
// (127.0.0.1, an IP SAN of the default leaf; crypto/tls sends no SNI for an IP).
func TestStartTLSDefaultServerName(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{})
	cfg := &tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12}
	tc := Dial(t, s.Addr()).StartTLS(cfg)
	tc.ExpectCmd(215, "LIST")
	tc.ReadDotLines()
	if cfg.ServerName != "" {
		t.Error("the caller's config was changed")
	}
	if !slices.Equal(s.TLSServerNames(), []string{""}) {
		t.Errorf("names %q", s.TLSServerNames())
	}
}

func TestStartTLSPeer(t *testing.T) {
	ca := tlscert.NewCA(t)
	h := fakeserver.PeerHandler(&fakeserver.PeerOptions{StartTLS: &fakeserver.StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}})
	s := fakeserver.New(t, fakeserver.Options{Handler: h})
	c := Dial(t, s.Addr())
	if !hasCap(t, c, "STARTTLS") {
		t.Fatal("STARTTLS not advertised")
	}
	tc := c.StartTLS(ca.ClientConfig("localhost"))
	if hasCap(t, tc, "STARTTLS") {
		t.Error("STARTTLS still advertised")
	}
	tc.ExpectCmd(502, "STARTTLS")
	tc.ExpectCmd(203, "MODE STREAM")
}

// TestStartTLSInjection: the server writes the 382 and plaintext in one write.
func TestStartTLSInjection(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		if line == "STARTTLS" {
			_, _ = c.Raw().Write([]byte("382 go\r\n200 injected\r\n"))
			return nil, false
		}
		return []string{"500 what"}, false
	})})
	c := Dial(t, s.Addr())
	tc, err := c.TryStartTLS(&tls.Config{InsecureSkipVerify: true})
	if err == nil || tc != nil {
		t.Fatalf("TryStartTLS = %v, %v", tc, err)
	}
	var se *StrictError
	if !errors.As(err, &se) || se.Kind != Pending || string(se.Bytes) != "200 injected\r\n" {
		t.Errorf("error %v is not a Pending StrictError holding the injected line", err)
	}
	if !strings.Contains(err.Error(), "plaintext") || !strings.Contains(err.Error(), "382") {
		t.Errorf("unclear error: %v", err)
	}
	// no handshake began: a server-side TLS handshake would have recorded a ClientHello; the
	// handler never calls StartTLS, so also check that the client closed instead of sending one
	if n := s.TLSServerNames(); len(n) != 0 {
		t.Errorf("a ClientHello reached the server: %q", n)
	}
	if code, _, err := c.TryCmd("DATE"); err == nil {
		t.Errorf("connection still usable after the failed upgrade (%d)", code)
	}
}

func TestStartTLSVerifyErrors(t *testing.T) {
	ca := tlscert.NewCA(t)
	for _, tc := range []struct {
		name string
		leaf tlscert.Options
		host bool
		kind x509.InvalidReason
	}{
		{"expired", tlscert.Options{Expired: true}, false, x509.Expired},
		{"not yet valid", tlscert.Options{NotYetValid: true}, false, x509.Expired},
		{"wrong name", tlscert.Options{DNSNames: []string{"other.test"}}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tlsServer(t, ca, tc.leaf, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{})
			c := Dial(t, s.Addr())
			n, err := c.TryStartTLS(ca.ClientConfig("localhost"))
			if n != nil {
				t.Fatal("a Conn with an untrusted certificate")
			}
			var cve *tls.CertificateVerificationError
			var ci x509.CertificateInvalidError
			var he x509.HostnameError
			switch {
			case !errors.As(err, &cve):
				t.Fatalf("error %v is not a verification error", err)
			case tc.host && !errors.As(err, &he):
				t.Errorf("error %v: want x509.HostnameError", err)
			case !tc.host && (!errors.As(err, &ci) || ci.Reason != tc.kind):
				t.Errorf("error %v: want x509.CertificateInvalidError %v", err, tc.kind)
			}
			testkit.WaitFor(t, 0, "failed handshake", func() bool { return s.TLSFailed() == 1 })
			if s.TLSHandshakes() != 0 {
				t.Error("a completed handshake")
			}
		})
	}
}

func TestStartTLSRefused(t *testing.T) {
	ca := tlscert.NewCA(t)
	for _, refuse := range []string{"502 STARTTLS not available", "580 Can not initiate TLS negotiation"} {
		s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{Refuse: refuse}, fakeserver.StoreOptions{})
		c := Dial(t, s.Addr())
		wantCode, _ := ParseCode(refuse)
		n, err := c.TryStartTLS(ca.ClientConfig("localhost"))
		var re *StartTLSError
		if n != nil || !errors.As(err, &re) || re.Line != refuse || re.Code != wantCode {
			t.Errorf("%q: got %v, %v", refuse, n, err)
		}
		// still plain and usable
		if c.IsTLS() || !hasCap(t, c, "STARTTLS") {
			t.Errorf("%q: connection or capabilities changed", refuse)
		}
		c.ExpectCmd(211, "GROUP alt.test")
	}
	// a stripped capability list: not advertised, the command still works
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{HideCapability: true}, fakeserver.StoreOptions{})
	c := Dial(t, s.Addr())
	if hasCap(t, c, "STARTTLS") {
		t.Error("hidden capability listed")
	}
	c.StartTLS(ca.ClientConfig("localhost"))
}

func TestStartTLSCloseAfterHandshake(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{CloseAfterHandshake: 2}, fakeserver.StoreOptions{})
	for i := 0; i < 3; i++ {
		tc := Dial(t, s.Addr()).StartTLS(ca.ClientConfig("localhost"))
		code, _, err := tc.TryCmd("DATE")
		if closed := i < 2; closed == (err == nil) || (!closed && code != 111) {
			t.Errorf("connection %d: DATE got %d %v", i+1, code, err)
		}
	}
	if s.TLSHandshakes() != 3 {
		t.Errorf("%d handshakes", s.TLSHandshakes())
	}
}

func TestStartTLSOrder(t *testing.T) {
	ca := tlscert.NewCA(t)
	// COMPRESS then STARTTLS is a 502 and STARTTLS is not offered
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{Compress: true})
	c := Dial(t, s.Addr())
	c.Compress()
	if hasCap(t, c, "STARTTLS") {
		t.Error("STARTTLS offered after COMPRESS")
	}
	if code, line, err := c.TryCmd("STARTTLS"); err != nil || code != 502 {
		t.Errorf("STARTTLS after COMPRESS: %q %v", line, err)
	}

	// TLS then COMPRESS works
	tc := Dial(t, s.Addr()).StartTLS(ca.ClientConfig("localhost"))
	tc.Compress()
	tc.ExpectCmd(211, "GROUP alt.test")

	// the selected group is forgotten by the upgrade
	c = Dial(t, s.Addr())
	c.ExpectCmd(211, "GROUP alt.test")
	tc = c.StartTLS(ca.ClientConfig("localhost"))
	tc.ExpectCmd(412, "STAT")
}

func TestStartTLSAuth(t *testing.T) {
	ca := tlscert.NewCA(t)
	so := fakeserver.StoreOptions{User: "u", Pass: "p"}

	// AuthNeedsTLS: 483 before TLS, USER not advertised, login works after the upgrade
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{AuthNeedsTLS: true}, so)
	c := Dial(t, s.Addr())
	caps, _ := c.Capabilities()
	if a, ok := caps["AUTHINFO"]; !ok || len(a) != 0 {
		t.Errorf("AUTHINFO before TLS: %v", caps["AUTHINFO"])
	}
	c.ExpectCmd(483, "AUTHINFO USER u")
	c.ExpectCmd(483, "GROUP alt.test")
	tc := c.StartTLS(ca.ClientConfig("localhost"))
	if caps, _ = tc.Capabilities(); !slices.Equal(caps["AUTHINFO"], []string{"USER"}) {
		t.Errorf("AUTHINFO after TLS: %v", caps["AUTHINFO"])
	}
	tc.ExpectCmd(381, "AUTHINFO USER u")
	tc.ExpectCmd(281, "AUTHINFO PASS p")
	tc.ExpectCmd(211, "GROUP alt.test")

	// without AuthNeedsTLS: AUTHINFO before STARTTLS works, STARTTLS after it is a 502
	s = tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{}, so)
	c = Dial(t, s.Addr())
	c.ExpectCmd(381, "AUTHINFO USER u")
	c.ExpectCmd(281, "AUTHINFO PASS p")
	if hasCap(t, c, "STARTTLS") {
		t.Error("STARTTLS offered after authentication")
	}
	c.ExpectCmd(502, "STARTTLS")
}

func TestStartTLSImplicitTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	leaf := ca.Issue(t, tlscert.Options{})
	s := fakeserver.New(t, fakeserver.Options{TLS: leaf.ServerConfig(), Store: tlsStore(),
		StoreOpts: fakeserver.StoreOptions{StartTLS: &fakeserver.StartTLSOptions{Config: leaf.ServerConfig()}}})
	nc, err := tls.Dial("tcp", s.Addr(), ca.ClientConfig("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	c := New(t, nc)
	if _, err := c.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	if !c.IsTLS() || hasCap(t, c, "STARTTLS") {
		t.Error("STARTTLS offered on an implicit TLS connection")
	}
	c.ExpectCmd(502, "STARTTLS")
	if s.TLSHandshakes() != 1 || !slices.Equal(s.TLSServerNames(), []string{"localhost"}) {
		t.Errorf("implicit TLS: %d handshakes, names %q", s.TLSHandshakes(), s.TLSServerNames())
	}
}

// TestStartTLSFiles: a leaf written to files and loaded back serves STARTTLS.
func TestStartTLSFiles(t *testing.T) {
	ca := tlscert.NewCA(t)
	f := ca.Issue(t, tlscert.Options{}).Write(t, "")
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	s := fakeserver.New(t, fakeserver.Options{Store: tlsStore(), StoreOpts: fakeserver.StoreOptions{
		StartTLS: &fakeserver.StartTLSOptions{Config: &tls.Config{Certificates: []tls.Certificate{cert}}}}})
	Dial(t, s.Addr()).StartTLS(ca.ClientConfig("localhost")).ExpectCmd(111, "DATE")
}

// TestStartTLSResumption: a client session cache resumes the session on the second
// connection, for STARTTLS and for implicit TLS (the server keeps one ticket key per config).
func TestStartTLSResumption(t *testing.T) {
	ca := tlscert.NewCA(t)
	leaf := ca.Issue(t, tlscert.Options{})
	cfg := func() *tls.Config {
		c := ca.ClientConfig("localhost")
		c.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		return c
	}
	t.Run("STARTTLS", func(t *testing.T) {
		s := fakeserver.New(t, fakeserver.Options{Store: tlsStore(), StoreOpts: fakeserver.StoreOptions{
			StartTLS: &fakeserver.StartTLSOptions{Config: leaf.ServerConfig()}}})
		cc := cfg()
		var resumed []bool
		for i := 0; i < 3; i++ {
			tc := Dial(t, s.Addr()).StartTLS(cc)
			tc.ExpectCmd(111, "DATE") // reads the session ticket
			st, _ := tc.TLSState()
			resumed = append(resumed, st.DidResume)
		}
		if !slices.Equal(resumed, []bool{false, true, true}) {
			t.Errorf("DidResume %v, want [false true true]", resumed)
		}
	})
	t.Run("implicit", func(t *testing.T) {
		s := fakeserver.New(t, fakeserver.Options{TLS: leaf.ServerConfig(), Store: tlsStore()})
		cc := cfg()
		var resumed []bool
		for i := 0; i < 3; i++ {
			nc, err := tls.Dial("tcp", s.Addr(), cc)
			if err != nil {
				t.Fatal(err)
			}
			c := New(t, nc)
			if _, err := c.ReadGreeting(); err != nil {
				t.Fatal(err)
			}
			st, _ := c.TLSState()
			resumed = append(resumed, st.DidResume)
		}
		if !slices.Equal(resumed, []bool{false, true, true}) {
			t.Errorf("DidResume %v, want [false true true]", resumed)
		}
	})
}

// TestStartTLSAuthNeedsTLSWithoutAuth: with no authentication configured the knob changes nothing.
func TestStartTLSAuthNeedsTLSWithoutAuth(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{AuthNeedsTLS: true}, fakeserver.StoreOptions{})
	c := Dial(t, s.Addr())
	c.ExpectCmd(502, "AUTHINFO USER u")
	c.ExpectCmd(211, "GROUP alt.test")
}

// recorder wraps a net.Conn and keeps what is read and written through it.
type recorder struct {
	net.Conn
	mu       sync.Mutex
	in, out  []byte
	deadline int // SetDeadline calls
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.mu.Lock()
	r.in = append(r.in, p[:n]...)
	r.mu.Unlock()
	return n, err
}

func (r *recorder) Write(p []byte) (int, error) {
	n, err := r.Conn.Write(p)
	r.mu.Lock()
	r.out = append(r.out, p[:n]...)
	r.mu.Unlock()
	return n, err
}

func (r *recorder) SetDeadline(t time.Time) error {
	r.mu.Lock()
	r.deadline++
	r.mu.Unlock()
	return r.Conn.SetDeadline(t)
}

func (r *recorder) seen() (in, out []byte, deadlines int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.in...), append([]byte(nil), r.out...), r.deadline
}

// TestUpgradeTLS: the test sends STARTTLS itself, reads the 382 and upgrades over a recording
// wrapper, which sees the TLS records and nothing of the plaintext before them.
func TestUpgradeTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{CommonName: "news.test"}, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{})
	c := Dial(t, s.Addr())
	c.MustSend("STARTTLS\r\n")
	c.ExpectStatus(382, "STARTTLS")

	rec := &recorder{Conn: c.Conn}
	tc := c.UpgradeTLS(rec, ca.ClientConfig("localhost"))
	if !tc.IsTLS() || !tc.Strict || tc.Greeting != c.Greeting {
		t.Errorf("new conn: tls=%v strict=%v greeting=%q", tc.IsTLS(), tc.Strict, tc.Greeting)
	}
	tc.ExpectCmd(211, "GROUP alt.test")
	in, out, dl := rec.seen()
	// a TLS handshake record starts with 0x16 0x03; the plaintext "STARTTLS" and "382" went through c.Conn
	if len(out) < 5 || out[0] != 0x16 || out[1] != 0x03 || bytes.Contains(out, []byte("GROUP")) || bytes.HasPrefix(in, []byte("211 ")) {
		t.Errorf("recorder out %x in %x: not the raw TLS stream", out[:min(len(out), 8)], in[:min(len(in), 8)])
	}
	if len(in) < 5 || in[0] != 0x16 || dl == 0 {
		t.Errorf("recorder in starts %x, %d deadline calls", in[:min(len(in), 8)], dl)
	}
	if s.TLSHandshakes() != 1 {
		t.Errorf("server saw %d handshakes", s.TLSHandshakes())
	}
}

// TestUpgradeTLSNilRaw: nil raw is c's own connection, TryUpgradeTLS(nil, cfg) after the 382
// is what TryStartTLS does.
func TestUpgradeTLSNilRaw(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{})
	c := Dial(t, s.Addr())
	c.MustSend("STARTTLS\r\n")
	c.ExpectStatus(382, "STARTTLS")
	tc, err := c.TryUpgradeTLS(nil, &tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if _, ok := tc.TLSState(); !ok {
		t.Error("not TLS")
	}
	if code, _, err := tc.TryCmd("DATE"); err != nil || code != 111 {
		t.Errorf("DATE over TLS = %d %v", code, err)
	}
}

// TestUpgradeTLSPipelined: STARTTLS and another command in one write; the server answers 382 and
// closes (it will not read plaintext as part of the handshake), so the upgrade fails with a
// handshake error and c is closed.
func TestUpgradeTLSPipelined(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := tlsServer(t, ca, tlscert.Options{}, fakeserver.StartTLSOptions{}, fakeserver.StoreOptions{})
	c := Dial(t, s.Addr())
	c.MustSend("STARTTLS\r\nCAPABILITIES\r\n")
	c.ExpectStatus(382, "pipelined STARTTLS")
	tc, err := c.TryUpgradeTLS(nil, ca.ClientConfig("localhost"))
	if err == nil || tc != nil {
		t.Fatalf("TryUpgradeTLS = %v, %v", tc, err)
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("error %v", err)
	}
}

// TestUpgradeTLSPending: plaintext buffered after the 382 fails the upgrade like StartTLS, over
// a wrapper as well.
func TestUpgradeTLSPending(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		_, _ = c.Raw().Write([]byte("382 go\r\n200 injected\r\n"))
		return nil, false
	})})
	c := Dial(t, s.Addr())
	c.MustSend("STARTTLS\r\n")
	c.ExpectStatus(382, "STARTTLS")
	// the injected line has to be in the reader's buffer: peek until it is
	if _, err := c.R.Peek(len("200 injected\r\n")); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{Conn: c.Conn}
	tc, err := c.TryUpgradeTLS(rec, &tls.Config{InsecureSkipVerify: true})
	var se *StrictError
	if tc != nil || !errors.As(err, &se) || se.Kind != Pending || string(se.Bytes) != "200 injected\r\n" {
		t.Errorf("TryUpgradeTLS = %v, %v", tc, err)
	}
	if _, out, _ := rec.seen(); len(out) != 0 {
		t.Errorf("a handshake began: %x", out)
	}
}

// TestUpgradeTLSFailsTB: the plain form fails tb (and panics without one).
func TestUpgradeTLSFailsTB(t *testing.T) {
	p := newPeer(t, scriptedClose("200 hi\r\n"))
	c, err := TryDial(p.addr(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer func() {
		if v := recover(); v == nil || !strings.Contains(fmt.Sprint(v), "handshake") {
			t.Fatalf("recover = %v", v)
		}
	}()
	c.UpgradeTLS(nil, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
}
