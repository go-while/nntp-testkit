package client

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
)

// StartTLSError is a STARTTLS that the server did not answer with 382: the connection is still
// plain and usable.
type StartTLSError struct {
	Code int    // the status code
	Line string // the status line
}

// Error quotes the status line.
func (e *StartTLSError) Error() string {
	return fmt.Sprintf("STARTTLS refused: %q (want 382)", e.Line)
}

// TryStartTLS sends STARTTLS (RFC 4642) and upgrades the connection to TLS with cfg. It returns
// a new strict Conn over the *tls.Conn (same Deadline, tb and Greeting; no greeting is read
// after STARTTLS: the caller sends CAPABILITIES again, as RFC 4642 2.2.2 says it should); c must
// not be used afterwards.
//
// Errors: a reply other than 382 gives a *StartTLSError (c stays plain and usable, the
// refusal of a stripping attacker or of a server without TLS); bytes already buffered after
// the 382 line fail the upgrade with a *StrictError of Kind Pending, wrapped with the
// explanation: the server wrote plaintext before the handshake, which a client would take for
// part of the TLS session (the STARTTLS command injection of CVE-2011-0411, RFC 4642 2.2.2),
// and c is closed; a handshake error (verification: *tls.CertificateVerificationError, with
// x509.HostnameError, x509.CertificateInvalidError, x509.UnknownAuthorityError inside, or a
// timeout) closes c. The handshake is bounded by the hang guard like every step.
//
// cfg is cloned; when it has no ServerName and does not skip verification, ServerName is the
// host of the remote address of c. TLSState returns the negotiated connection state of the new Conn.
func (c *Conn) TryStartTLS(cfg *tls.Config) (*Conn, error) {
	if err := c.Send("STARTTLS\r\n"); err != nil {
		return nil, fmt.Errorf("STARTTLS: %w", err)
	}
	code, line, err := c.ReadStatus()
	if err != nil {
		return nil, fmt.Errorf("STARTTLS: %w (line %q)", err, line)
	}
	if code != 382 {
		return nil, &StartTLSError{Code: code, Line: line}
	}
	if n := c.R.Buffered(); n > 0 {
		b, _ := c.R.Peek(n)
		c.Close()
		return nil, fmt.Errorf("STARTTLS: %d bytes of plaintext after the 382 line, before the TLS handshake "+
			"(injected by the server or a man in the middle, RFC 4642 2.2.2, CVE-2011-0411): %w",
			n, &StrictError{Kind: Pending, Bytes: append([]byte(nil), b...)})
	}
	cfg = cfg.Clone()
	if cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		if host, _, err := net.SplitHostPort(c.Conn.RemoteAddr().String()); err == nil {
			cfg.ServerName = host
		}
	}
	tc := tls.Client(c.Conn, cfg)
	c.bump()
	if err := tc.Handshake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("STARTTLS handshake: %w", err)
	}
	return &Conn{Conn: tc, R: bufio.NewReaderSize(tc, readerSize), Greeting: c.Greeting,
		Deadline: c.Deadline, Strict: c.Strict, tb: c.tb}, nil
}

// StartTLS is TryStartTLS that fails tb on any error and registers tb.Cleanup(Close) for the
// returned Conn. Use TryStartTLS to look at the error (a refusal, a verification error).
func (c *Conn) StartTLS(cfg *tls.Config) *Conn {
	if c.tb != nil {
		c.tb.Helper()
	}
	n, err := c.TryStartTLS(cfg)
	if err != nil {
		c.fatalf("%v", err)
	}
	if c.tb != nil {
		c.tb.Cleanup(n.Close)
	}
	return n
}

// TLSState returns the TLS connection state when c is a TLS connection (made by TryStartTLS, or
// New over a *tls.Conn): peer certificates, version, negotiated protocol, server name.
func (c *Conn) TLSState() (tls.ConnectionState, bool) {
	if tc, ok := c.Conn.(*tls.Conn); ok {
		return tc.ConnectionState(), true
	}
	return tls.ConnectionState{}, false
}

// IsTLS reports whether c is a TLS connection.
func (c *Conn) IsTLS() bool {
	_, ok := c.Conn.(*tls.Conn)
	return ok
}
