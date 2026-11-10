// Package tlscert makes throw-away TLS certificates for tests: a CA that issues leaf
// certificates, a self-signed shortcut, and every result as a tls.Certificate, as PEM and as
// files (the key at mode 0600) for servers that read their certificate from a path (INN's
// tls_cert_file/tls_key_file, diablo, NewsCache). Leaf options make the usual failing
// certificates one field away: Expired, NotYetValid, DNSNames/IPs that do not match the host.
// Standard library only; errors fail tb.
//
// Contract (plan nntp-testkit; implemented in tlscert.go):
//
//	type KeyType int // ECDSA (default, P-256), RSA (2048), Ed25519
//	type CA struct {
//		Cert    *x509.Certificate
//		Key     crypto.Signer
//		CertPEM []byte         // PEM CERTIFICATE
//		Pool    *x509.CertPool // trusts Cert: tls.Config.RootCAs
//	}
//	type Options struct {
//		CommonName  string        // default "nntp-testkit"
//		DNSNames    []string      // SANs; with no IPs too: default {"localhost"}
//		IPs         []net.IP      // SANs; with no DNSNames too: default 127.0.0.1 and ::1
//		NotBefore   time.Time     // default now-1h
//		NotAfter    time.Time     // default now+24h
//		Expired     bool          // NotBefore now-48h, NotAfter now-1h (when the times are not set)
//		NotYetValid bool          // NotBefore now+1h, NotAfter now+48h (when the times are not set)
//		KeyType     KeyType
//		ClientAuth  bool          // also ExtKeyUsageClientAuth (a client certificate)
//	}
//	type Leaf struct {
//		Cert    *x509.Certificate
//		Key     crypto.Signer
//		CertPEM []byte
//		KeyPEM  []byte         // PKCS#8, "PRIVATE KEY"
//		CAPEM   []byte         // PEM of the issuer (the CA, or the leaf itself when self-signed)
//		Pool    *x509.CertPool // trusts the issuer
//	}
//	type Files struct{ Cert, Key, CA string } // paths written by Leaf.Write
//
//	func NewCA(tb testing.TB) *CA
//	func (ca *CA) Issue(tb testing.TB, o Options) *Leaf
//	func (ca *CA) ClientConfig(serverName string) *tls.Config // RootCAs = Pool, ServerName, TLS 1.2+
//	func (ca *CA) WriteCA(tb testing.TB, dir string) string    // dir/ca.pem (mode 0644), its path; dir "" = tb.TempDir()
//	func SelfSigned(tb testing.TB, o Options) *Leaf            // self-signed (it is its own CA certificate)
//	func (l *Leaf) TLSCertificate() tls.Certificate            // Leaf set: for tls.Config.Certificates
//	func (l *Leaf) ServerConfig() *tls.Config                  // Certificates = {TLSCertificate()}, TLS 1.2+
//	func (l *Leaf) ClientConfig(serverName string) *tls.Config // as CA.ClientConfig, trusting the issuer
//	func (l *Leaf) Write(tb testing.TB, dir string) Files      // dir/cert.pem, key.pem (0600), ca.pem; dir "" = tb.TempDir()
//
// A leaf that a test expects to fail verification still carries the issuer's Pool and
// ClientConfig, so a failure comes from the certificate alone: x509.CertificateInvalidError
// with Reason x509.Expired for Expired and NotYetValid (x509.Expired covers both), and
// x509.HostnameError for a name that does not match, both inside a
// *tls.CertificateVerificationError.
package tlscert
