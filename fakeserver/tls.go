package fakeserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"
)

// TLSConfig returns a server config with a fresh self-signed ECDSA P-256 certificate for
// 127.0.0.1/localhost (valid ±1h, MinVersion TLS 1.2) and a pool that trusts it, for the
// client side. Errors fail tb.
func TLSConfig(tb testing.TB) (server *tls.Config, roots *x509.CertPool) {
	tb.Helper()
	server, roots, err := NewTLSConfig()
	if err != nil {
		tb.Fatal(err)
	}
	return server, roots
}

// NewTLSConfig is TLSConfig for code without a testing.TB, such as a fake server program: it
// returns the error instead of failing a test. The certificate (server.Certificates[0].Leaf)
// is its own CA; package tlscert has the PEM and file forms (GenerateSelfSigned, WriteFiles).
func NewTLSConfig() (server *tls.Config, roots *x509.CertPool, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("fakeserver: ecdsa key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, nil, fmt.Errorf("fakeserver: serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fakeserver"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("fakeserver: certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("fakeserver: parse certificate: %w", err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	server = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}},
		MinVersion:   tls.VersionTLS12,
	}
	return server, roots, nil
}
