package client

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"time"
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
// timeout) closes c. The handshake is bounded by the hang guard like every step. The part
// after the 382 is TryUpgradeTLS, which a test can call itself after sending STARTTLS its own way.
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
	return c.TryUpgradeTLS(nil, cfg)
}

// TryUpgradeTLS is the second half of TryStartTLS: the TLS handshake and the new Conn, for a
// test that sent STARTTLS itself and read the 382 (c.ReadStatus or c.ExpectStatus(382, ...)).
// Two uses: STARTTLS and a following command in one write (c.Send("STARTTLS\r\nCAPABILITIES\r\n"),
// read the 382, upgrade: the server must not answer the second command in plaintext), and
// recording the encrypted stream between the 382 and the handshake.
//
// raw is the net.Conn the TLS client runs over: nil means c's own connection (c.Conn), which
// is what TryStartTLS passes. A test that wants to see the bytes passes a wrapper around
// c.Conn that records what it reads and writes; the wrapper must read from and write to the
// same socket as c.Conn (the bytes c has buffered are not given to it, so it is only for
// what comes after the 382), and the new Conn's Conn is the *tls.Conn over raw, so
// closing it closes raw. c must not be used afterwards (it shares the socket); the errors,
// the bytes-buffered rule (a *StrictError of Kind Pending: no byte may follow the 382 line
// in c's reader, the server's plaintext injection of CVE-2011-0411; c is closed), the cfg
// cloning and ServerName default (the host of raw's remote address) and the hang guard are
// those of TryStartTLS. Nothing is sent or read before the handshake, so calling it
// without a 382 first is the caller's mistake: the handshake then fails.
func (c *Conn) TryUpgradeTLS(raw net.Conn, cfg *tls.Config) (*Conn, error) {
	if raw == nil {
		raw = c.Conn
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
		if ra := raw.RemoteAddr(); ra != nil {
			if host, _, err := net.SplitHostPort(ra.String()); err == nil {
				cfg.ServerName = host
			}
		}
	}
	tc := tls.Client(raw, cfg)
	c.bump()
	if raw != c.Conn {
		_ = raw.SetDeadline(time.Now().Add(c.deadline()))
	}
	if err := tc.Handshake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("STARTTLS handshake: %w", err)
	}
	return &Conn{Conn: tc, R: bufio.NewReaderSize(tc, readerSize), Greeting: c.Greeting,
		Deadline: c.Deadline, Strict: c.Strict, tb: c.tb}, nil
}

// UpgradeTLS is TryUpgradeTLS that fails tb on any error and registers tb.Cleanup(Close) for
// the returned Conn, like StartTLS.
func (c *Conn) UpgradeTLS(raw net.Conn, cfg *tls.Config) *Conn {
	if c.tb != nil {
		c.tb.Helper()
	}
	n, err := c.TryUpgradeTLS(raw, cfg)
	if err != nil {
		c.fatalf("%v", err)
	}
	if c.tb != nil {
		c.tb.Cleanup(n.Close)
	}
	return n
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
