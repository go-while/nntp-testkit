package fakeserver

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"time"
)

// StartTLSOptions makes a Store (StoreOptions.StartTLS) or a PeerHandler (PeerOptions.StartTLS)
// offer STARTTLS (RFC 4642). nil: STARTTLS is an unknown command (500). Compare Options.TLS,
// which makes every connection TLS from the first byte (implicit TLS; STARTTLS then answers
// 502).
//
// STARTTLS is listed in CAPABILITIES while the connection is plain, not compressed and not
// authenticated by AUTHINFO. It answers 382 and the handshake follows the CRLF of that line
// (Conn.StartTLS), after which STARTTLS is no longer listed and answers 502, as it does on an
// implicit TLS connection, after COMPRESS (RFC 8054: TLS first, then compression) and after a
// successful AUTHINFO; arguments are a 501. The connection's state is reset by the upgrade
// (RFC 4642 2.2.2.1: the server discards what it learnt before it: in Store mode the selected
// group and the current article), and bytes the client pipelined after its STARTTLS line
// close the connection (plaintext injected before the handshake).
type StartTLSOptions struct {
	Config *tls.Config // the server config of the upgraded connections; required (nil: no STARTTLS)

	// Refuse is the reply to STARTTLS instead of 382, e.g. "502 STARTTLS not available" or
	// "580 Can not initiate TLS negotiation": a server that refuses, or an attacker that
	// strips STARTTLS so that the client stays in plaintext. The connection stays plain.
	// (In Store mode Faults.Override "STARTTLS" does the same, and a "382 ..." override
	// upgrades.)
	Refuse string

	// HideCapability leaves STARTTLS out of CAPABILITIES (a stripped capability list) while
	// the command itself still works as configured.
	HideCapability bool

	// AuthNeedsTLS makes AUTHINFO before TLS a 483 (RFC 4643 2.2: encryption required), the
	// commands that need authentication a 483 instead of 480 until TLS is on, and lists
	// "AUTHINFO" without "USER" in CAPABILITIES while the connection is plain. Over implicit
	// TLS (Options.TLS) the connection is TLS from the start.
	AuthNeedsTLS bool

	// CloseAfterHandshake > 0: the first N upgrades of the server (counted over all its
	// connections) are followed by closing the connection at once, right after the handshake:
	// a server that drops the session as the client is about to continue (retry tests).
	CloseAfterHandshake int
}

func (o *StartTLSOptions) on() bool { return o != nil && o.Config != nil }

// needsTLS reports whether AuthNeedsTLS is set and the connection is still plain.
func (o *StartTLSOptions) needsTLS(c *Conn) bool { return o.on() && o.AuthNeedsTLS && !c.TLS() }

// advertised reports whether CAPABILITIES lists STARTTLS (also when Refuse will refuse it);
// loggedIn is whether AUTHINFO has succeeded on the connection.
func (o *StartTLSOptions) advertised(c *Conn, loggedIn bool) bool {
	return o.on() && !o.HideCapability && c.startTLSUnavailable(loggedIn) == ""
}

// startTLSUnavailable is the 502 line of a STARTTLS that cannot be taken now (TLS or compression
// is on, or authentication happened), else "".
func (c *Conn) startTLSUnavailable(loggedIn bool) string {
	switch {
	case c.TLS():
		return "502 TLS already active"
	case c.Compressed():
		return "502 Compression active"
	case loggedIn:
		return "502 Already authenticated"
	}
	return ""
}

// startTLSLine is the reply to STARTTLS: the 502 of startTLSUnavailable, o.Refuse when set,
// else "382 Continue with TLS negotiation".
func (c *Conn) startTLSLine(o *StartTLSOptions, loggedIn bool) string {
	switch r := c.startTLSUnavailable(loggedIn); {
	case r != "":
		return r
	case o.Refuse != "":
		return o.Refuse
	}
	return "382 Continue with TLS negotiation"
}

// startTLSAfter starts TLS after the status line of a STARTTLS command when it is a 382 and the
// connection is plain (the counterpart of compressAfter). upgraded reports a completed
// upgrade; ok is false when the handler must stop serving: handshake failure, plaintext after
// the STARTTLS line, or CloseAfterHandshake.
func (c *Conn) startTLSAfter(o *StartTLSOptions, cmd, status string) (upgraded, ok bool) {
	if !o.on() || cmd != "STARTTLS" || (status != "382" && !strings.HasPrefix(status, "382 ")) || c.TLS() {
		return false, true
	}
	if err := c.StartTLS(o.Config); err != nil {
		return false, false
	}
	if o.CloseAfterHandshake > 0 && c.s.takeTLSClose(o.CloseAfterHandshake) {
		return true, false
	}
	return true, true
}

// StartTLS upgrades the connection to TLS with cfg (RFC 4642): call it right after writing the
// "382" reply; the handshake is the server side, bounded by the server's Deadline. From then on
// ReadLine, WriteLines and Raw use the TLS conn, so Lines, Counts, Articles and Mismatches
// record the client's commands as before. It fails, and the connection should be closed, when
// TLS or compression is already on, when the handshake fails, or when the client's plaintext
// after its STARTTLS line is still buffered (a client that pipelined bytes before the
// handshake). The Server records the client's server name (TLSServerNames) and the handshake
// (TLSHandshakes, TLSFailed).
func (c *Conn) StartTLS(cfg *tls.Config) error {
	switch {
	case c.TLS():
		return errors.New("fakeserver: TLS is already active")
	case c.Compressed():
		return errors.New("fakeserver: STARTTLS after COMPRESS")
	case c.r.Buffered() > 0:
		return errors.New("fakeserver: plaintext pipelined after STARTTLS")
	}
	tc, err := c.s.tlsHandshake(c.conn, cfg)
	if err != nil {
		return err
	}
	c.conn, c.tls = tc, tc
	c.r = newReader(tc)
	return nil
}

// TLS reports whether the connection is TLS: implicit (Options.TLS) or after StartTLS.
func (c *Conn) TLS() bool { return c.tls != nil }

// TLSState returns the state of the TLS handshake of the connection (server name, version,
// negotiated protocol, client certificates), false while the connection is plain.
func (c *Conn) TLSState() (tls.ConnectionState, bool) {
	if c.tls == nil {
		return tls.ConnectionState{}, false
	}
	return c.tls.ConnectionState(), true
}

// tlsHandshake runs the server side of a handshake on raw with cfg and records it: the server
// name of every ClientHello, and the success or failure.
func (s *Server) tlsHandshake(raw net.Conn, cfg *tls.Config) (*tls.Conn, error) {
	hooked := cfg.Clone()
	orig := cfg.GetConfigForClient
	hooked.GetConfigForClient = func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
		s.mu.Lock()
		s.tlsNames = append(s.tlsNames, hi.ServerName)
		s.mu.Unlock()
		if orig != nil {
			return orig(hi)
		}
		return nil, nil
	}
	tc := tls.Server(raw, hooked)
	_ = raw.SetDeadline(time.Now().Add(s.deadline))
	err := tc.Handshake()
	_ = raw.SetDeadline(time.Time{})
	s.mu.Lock()
	if err != nil {
		s.tlsFailed++
	} else {
		s.tlsOK++
	}
	s.mu.Unlock()
	return tc, err
}

// takeTLSClose reports whether this upgrade is among the first n to be closed at once.
func (s *Server) takeTLSClose(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tlsClosed >= n {
		return false
	}
	s.tlsClosed++
	return true
}

// TLSHandshakes returns the number of completed TLS handshakes of the server: implicit TLS
// (Options.TLS) and STARTTLS upgrades.
func (s *Server) TLSHandshakes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tlsOK
}

// TLSFailed returns the number of TLS handshakes that failed (a client that did not trust the
// certificate aborts, a timeout).
func (s *Server) TLSFailed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tlsFailed
}

// TLSServerNames returns the server name (SNI) of every ClientHello received, in order: ""
// for a client that sent none (crypto/tls sends none for an IP address as ServerName), also
// for handshakes that failed.
func (s *Server) TLSServerNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tlsNames...)
}
