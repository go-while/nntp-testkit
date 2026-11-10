package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// handshake runs a TLS handshake over loopback TCP: server with l, client with cfg; it returns
// the client's error.
func handshake(t *testing.T, l *Leaf, cfg *tls.Config) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		a, err := ln.Accept()
		if err != nil {
			return
		}
		defer a.Close()
		_ = a.SetDeadline(time.Now().Add(10 * time.Second))
		_ = tls.Server(a, l.ServerConfig()).Handshake()
	}()
	b, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(10 * time.Second))
	return tls.Client(b, cfg).Handshake()
}

func TestIssueAndVerify(t *testing.T) {
	ca := NewCA(t)
	for _, kt := range []KeyType{ECDSA, RSA, Ed25519} {
		l := ca.Issue(t, Options{KeyType: kt})
		if err := handshake(t, l, ca.ClientConfig("localhost")); err != nil {
			t.Errorf("key type %d: %v", kt, err)
		}
		if err := handshake(t, l, ca.ClientConfig("127.0.0.1")); err != nil {
			t.Errorf("key type %d, IP: %v", kt, err)
		}
	}
}

func TestOptionFailures(t *testing.T) {
	ca := NewCA(t)
	now := time.Now()
	for _, tc := range []struct {
		name string
		o    Options
		want x509.InvalidReason
		host bool // want a HostnameError
	}{
		{"expired", Options{Expired: true}, x509.Expired, false},
		{"not-yet-valid", Options{NotYetValid: true}, x509.Expired, false},
		{"explicit times", Options{NotBefore: now.Add(-3 * time.Hour), NotAfter: now.Add(-2 * time.Hour)}, x509.Expired, false},
		{"wrong name", Options{DNSNames: []string{"other.example"}}, 0, true},
		{"wrong IP", Options{IPs: []net.IP{net.IPv4(10, 1, 2, 3)}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := handshake(t, ca.Issue(t, tc.o), ca.ClientConfig("localhost"))
			var cve *tls.CertificateVerificationError
			if !errors.As(err, &cve) {
				t.Fatalf("got %v, want a *tls.CertificateVerificationError", err)
			}
			var ci x509.CertificateInvalidError
			var he x509.HostnameError
			switch {
			case tc.host && !errors.As(err, &he):
				t.Errorf("got %v, want x509.HostnameError", err)
			case !tc.host && (!errors.As(err, &ci) || ci.Reason != tc.want):
				t.Errorf("got %v, want x509.CertificateInvalidError %v", err, tc.want)
			}
		})
	}
}

func TestUnknownCA(t *testing.T) {
	l := NewCA(t).Issue(t, Options{})
	err := handshake(t, l, NewCA(t).ClientConfig("localhost"))
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Errorf("got %v, want x509.UnknownAuthorityError", err)
	}
}

func TestSelfSigned(t *testing.T) {
	l := SelfSigned(t, Options{DNSNames: []string{"news.test"}})
	if err := handshake(t, l, l.ClientConfig("news.test")); err != nil {
		t.Fatal(err)
	}
	if !l.Cert.IsCA || string(l.CAPEM) != string(l.CertPEM) {
		t.Error("a self-signed leaf is its own trust anchor")
	}
	if err := handshake(t, l, NewCA(t).ClientConfig("news.test")); err == nil {
		t.Error("a self-signed leaf verified against another CA")
	}
}

func TestWrite(t *testing.T) {
	ca := NewCA(t)
	l := ca.Issue(t, Options{})
	dir := filepath.Join(t.TempDir(), "sub")
	f := l.Write(t, dir)
	for path, mode := range map[string]os.FileMode{f.Cert: 0o644, f.Key: 0o600, f.CA: 0o644} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("%s: mode %v, want %v", filepath.Base(path), fi.Mode().Perm(), mode)
		}
	}
	// what was written loads as a key pair and verifies against the CA file
	if _, err := tls.LoadX509KeyPair(f.Cert, f.Key); err != nil {
		t.Errorf("LoadX509KeyPair: %v", err)
	}
	pemCA, err := os.ReadFile(f.CA)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemCA) {
		t.Fatal("ca.pem is not PEM")
	}
	if _, err := l.Cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Errorf("verify against ca.pem: %v", err)
	}
	// an existing key file with a wider mode is narrowed again
	if err := os.Chmod(f.Key, 0o644); err != nil {
		t.Fatal(err)
	}
	l.Write(t, dir)
	if fi, _ := os.Stat(f.Key); fi.Mode().Perm() != 0o600 {
		t.Errorf("rewritten key mode %v", fi.Mode().Perm())
	}
	// a custom key mode, and a rewrite over a wider file replaces it
	l.WriteKeyMode(t, dir, 0o640)
	if fi, _ := os.Stat(f.Key); fi.Mode().Perm() != 0o640 {
		t.Errorf("WriteKeyMode 0640: %v", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 3 {
		t.Errorf("temp files left in %s: %d entries", dir, len(ents))
	}
	if p := ca.WriteCA(t, ""); p == "" || filepath.Base(p) != "ca.pem" {
		t.Errorf("WriteCA path %q", p)
	}
}

func TestDefaults(t *testing.T) {
	l := NewCA(t).Issue(t, Options{})
	if len(l.Cert.DNSNames) != 1 || l.Cert.DNSNames[0] != "localhost" || len(l.Cert.IPAddresses) != 2 {
		t.Errorf("default SANs %v %v", l.Cert.DNSNames, l.Cert.IPAddresses)
	}
	now := time.Now()
	if l.Cert.NotBefore.After(now) || l.Cert.NotAfter.Before(now) {
		t.Errorf("default validity %v - %v", l.Cert.NotBefore, l.Cert.NotAfter)
	}
	if l.Cert.KeyUsage&x509.KeyUsageKeyEncipherment != 0 {
		t.Error("KeyEncipherment on an EC leaf")
	}
	if r := NewCA(t).Issue(t, Options{KeyType: RSA}); r.Cert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Error("no KeyEncipherment on an RSA leaf")
	}
	c := NewCA(t).Issue(t, Options{ClientAuth: true})
	if len(c.Cert.ExtKeyUsage) != 2 {
		t.Errorf("ClientAuth: %v", c.Cert.ExtKeyUsage)
	}
}
