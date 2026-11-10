package tlscert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// KeyType is the key algorithm of a certificate.
type KeyType int

// The key types: ECDSA P-256 is the default (the zero value).
const (
	ECDSA   KeyType = iota // ECDSA P-256
	RSA                    // RSA 2048
	Ed25519                // Ed25519 (the server software needs OpenSSL 1.1.1+ / GnuTLS 3.6+)
)

// CA is a test certificate authority.
type CA struct {
	Cert    *x509.Certificate
	Key     crypto.Signer
	CertPEM []byte         // PEM CERTIFICATE
	Pool    *x509.CertPool // trusts Cert: tls.Config.RootCAs

}

// Options are the settings of a leaf certificate; the zero value is a valid ECDSA server
// certificate for localhost, 127.0.0.1 and ::1, valid from a day ago for two days (so a remote
// test host with a skewed clock still accepts it).
type Options struct {
	CommonName  string    // default "nntp-testkit"
	DNSNames    []string  // subject alternative names; with no IPs too: default {"localhost"}
	IPs         []net.IP  // subject alternative names; with no DNSNames too: default 127.0.0.1 and ::1
	NotBefore   time.Time // default now-24h
	NotAfter    time.Time // default now+48h
	Expired     bool      // NotBefore now-48h, NotAfter now-1h, for a time that is not set
	NotYetValid bool      // NotBefore now+1h, NotAfter now+48h, for a time that is not set
	KeyType     KeyType
	ClientAuth  bool // also ExtKeyUsageClientAuth: a client certificate
}

// Leaf is an issued certificate with its key.
type Leaf struct {
	Cert    *x509.Certificate
	Key     crypto.Signer
	CertPEM []byte
	KeyPEM  []byte         // PKCS#8, "PRIVATE KEY"
	CAPEM   []byte         // PEM of the issuer: the CA, or the leaf itself when self-signed
	Pool    *x509.CertPool // trusts the issuer
}

// Files are the paths Leaf.Write wrote.
type Files struct{ Cert, Key, CA string }

func genKey(kt KeyType) (crypto.Signer, error) {
	switch kt {
	case ECDSA:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case RSA:
		return rsa.GenerateKey(rand.Reader, 2048)
	case Ed25519:
		_, k, err := ed25519.GenerateKey(rand.Reader)
		return k, err
	}
	return nil, fmt.Errorf("unknown key type %d", int(kt))
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
}

// NewCA creates a CA (ECDSA P-256, valid from three days ago for a year). Errors fail tb.
func NewCA(tb testing.TB) *CA {
	tb.Helper()
	key, err := genKey(ECDSA)
	if err != nil {
		tb.Fatalf("tlscert: CA key: %v", err)
	}
	sn, err := serial()
	if err != nil {
		tb.Fatalf("tlscert: serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "nntp-testkit test CA"},
		NotBefore:             now.Add(-72 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		tb.Fatalf("tlscert: CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("tlscert: parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{Cert: cert, Key: key, CertPEM: pemCert(der), Pool: pool}
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// template returns the certificate template of o with the defaults applied.
func (o Options) template() (*x509.Certificate, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	nb, na := o.NotBefore, o.NotAfter
	switch {
	case o.Expired:
		nb, na = defTime(nb, now.Add(-48*time.Hour)), defTime(na, now.Add(-time.Hour))
	case o.NotYetValid:
		nb, na = defTime(nb, now.Add(time.Hour)), defTime(na, now.Add(48*time.Hour))
	default:
		nb, na = defTime(nb, now.Add(-24*time.Hour)), defTime(na, now.Add(48*time.Hour))
	}
	cn := o.CommonName
	if cn == "" {
		cn = "nntp-testkit"
	}
	dns, ips := o.DNSNames, o.IPs
	if len(dns) == 0 && len(ips) == 0 {
		dns, ips = []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	}
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if o.ClientAuth {
		eku = append(eku, x509.ExtKeyUsageClientAuth)
	}
	ku := x509.KeyUsageDigitalSignature
	if o.KeyType == RSA {
		ku |= x509.KeyUsageKeyEncipherment // RSA key exchange; EC and Ed25519 keys only sign
	}
	return &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             nb,
		NotAfter:              na,
		KeyUsage:              ku,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}, nil
}

func defTime(t, def time.Time) time.Time {
	if t.IsZero() {
		return def
	}
	return t
}

// issue builds a leaf signed by parent/parentKey (nil: self-signed).
func issue(o Options, parent *x509.Certificate, parentKey crypto.Signer, caPEM []byte, pool *x509.CertPool) (*Leaf, error) {
	key, err := genKey(o.KeyType)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	tmpl, err := o.template()
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	self := parent == nil
	if self {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, key.Public(), parentKey)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	l := &Leaf{Cert: cert, Key: key, CertPEM: pemCert(der),
		KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})}
	if self {
		l.CAPEM = l.CertPEM
		l.Pool = x509.NewCertPool()
		l.Pool.AddCert(cert)
	} else {
		l.CAPEM, l.Pool = caPEM, pool
	}
	return l, nil
}

// Issue creates a leaf certificate signed by the CA. Errors fail tb.
func (ca *CA) Issue(tb testing.TB, o Options) *Leaf {
	tb.Helper()
	l, err := issue(o, ca.Cert, ca.Key, ca.CertPEM, ca.Pool)
	if err != nil {
		tb.Fatalf("tlscert: %v", err)
	}
	return l
}

// SelfSigned creates a self-signed certificate that is its own trust anchor (Pool, CAPEM): the
// common case of one server and one client config. Errors fail tb.
func SelfSigned(tb testing.TB, o Options) *Leaf {
	tb.Helper()
	l, err := issue(o, nil, nil, nil, nil)
	if err != nil {
		tb.Fatalf("tlscert: %v", err)
	}
	return l
}

func clientConfig(pool *x509.CertPool, serverName string) *tls.Config {
	return &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// ClientConfig returns a client config that trusts the CA and checks serverName.
func (ca *CA) ClientConfig(serverName string) *tls.Config { return clientConfig(ca.Pool, serverName) }

// ClientConfig returns a client config that trusts the issuer of the leaf and checks serverName.
func (l *Leaf) ClientConfig(serverName string) *tls.Config { return clientConfig(l.Pool, serverName) }

// TLSCertificate returns the leaf as a tls.Certificate (with Leaf set).
func (l *Leaf) TLSCertificate() tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{l.Cert.Raw}, PrivateKey: l.Key, Leaf: l.Cert}
}

// ServerConfig returns a server config holding the certificate (TLS 1.2 and later).
func (l *Leaf) ServerConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{l.TLSCertificate()}, MinVersion: tls.VersionTLS12}
}

func dirOr(tb testing.TB, dir string) string {
	if dir == "" {
		return tb.TempDir()
	}
	return dir
}

// WriteCA writes the CA certificate as dir/ca.pem (mode 0644) and returns the path; dir "" is
// tb.TempDir(). Errors fail tb.
func (ca *CA) WriteCA(tb testing.TB, dir string) string {
	tb.Helper()
	p := filepath.Join(dirOr(tb, dir), "ca.pem")
	write(tb, p, ca.CertPEM, 0o644)
	return p
}

// Write writes dir/cert.pem, dir/key.pem (mode 0600) and dir/ca.pem (the issuer, for servers
// and clients that read a CA file) and returns the paths; dir "" is tb.TempDir(), a missing dir
// is created. Errors fail tb. Files are replaced atomically (temp file, then rename), so a key
// is never visible with a wider mode than requested. A server that runs as another user
// (INN as news) cannot read a 0600 key of the test user: use WriteKeyMode with 0640 and chgrp
// the file, or 0644 for a throw-away test directory.
func (l *Leaf) Write(tb testing.TB, dir string) Files {
	tb.Helper()
	return l.WriteKeyMode(tb, dir, 0o600)
}

// WriteKeyMode is Write with another mode for key.pem (cert.pem and ca.pem stay 0644).
func (l *Leaf) WriteKeyMode(tb testing.TB, dir string, keyMode os.FileMode) Files {
	tb.Helper()
	dir = dirOr(tb, dir)
	f := Files{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem")}
	write(tb, f.Cert, l.CertPEM, 0o644)
	write(tb, f.Key, l.KeyPEM, keyMode)
	write(tb, f.CA, l.CAPEM, 0o644)
	return f
}

// write replaces path with a file of mode perm: it writes a 0600 temp file in the same
// directory, sets the mode and renames it over path, so a rewrite never exposes the new content
// with the old, wider mode (or the umask's).
func write(tb testing.TB, path string, data []byte, perm os.FileMode) {
	tb.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatalf("tlscert: %v", err)
	}
	f, err := os.CreateTemp(dir, ".tlscert-*")
	if err != nil {
		tb.Fatalf("tlscert: %v", err)
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(f.Name(), perm), os.Rename(f.Name(), path)); err != nil {
		_ = os.Remove(f.Name())
		tb.Fatalf("tlscert: %v", err)
	}
}
