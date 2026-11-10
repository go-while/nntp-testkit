package conformance

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// startTLSChecks are the STARTTLS checks of RFC 4642, each on its own connection and each run
// only when CAPABILITIES lists STARTTLS on a plain connection (Skip with Missing "STARTTLS"
// otherwise; with Options.TLS the connection is TLS from the first byte and they are skipped
// without a Missing). They never log in: STARTTLS is not a valid command after AUTHINFO
// (RFC 4642 §2.2.1).
//
// The handshake is not verified unless Options.StartTLS says so (a test server has a
// self-signed certificate; the certificate is not what these checks are about): the Pass
// Detail then says "certificate not verified". A server that answers 580 (it cannot negotiate
// TLS, e.g. it has no certificate, which RFC 4642 §2.2.2 allows) passes check starttls with
// that Detail and skips the others. A 502 or any other code from a server that lists STARTTLS
// on a plain, unauthenticated connection fails.
func startTLSChecks() []check {
	const rfc = "RFC 4642 §2.2.2"
	return []check{
		{Name: "starttls", RFC: rfc, Run: checkStartTLS},
		{Name: "starttls-twice", RFC: rfc + " (502)", Run: checkStartTLSTwice},
		{Name: "starttls-pipelined", RFC: rfc + " (CVE-2011-0411)", Run: checkStartTLSPipelined},
	}
}

// startTLSReady skips the check when the connection is TLS already, or STARTTLS is not
// listed (after MODE READER when the server offers it).
func (s *session) startTLSReady(c check) (Result, bool) {
	if s.c.IsTLS() {
		return c.skip("the connection is TLS from the first byte (Options.TLS): STARTTLS is for plain connections"), false
	}
	return s.capReady(c, "STARTTLS", false)
}

// tlsConfig is the client configuration of the STARTTLS handshake and, for Pass Details, what
// it does about the certificate.
func (s *session) tlsConfig() (cfg *tls.Config, note string) {
	if s.o.StartTLS == nil {
		return &tls.Config{InsecureSkipVerify: true}, "certificate not verified (no Options.StartTLS)" //nolint:gosec // the caller chose not to verify; the Detail says so
	}
	cfg = s.o.StartTLS.Clone()
	if cfg.InsecureSkipVerify {
		return cfg, "certificate not verified (Options.StartTLS skips verification)"
	}
	if cfg.ServerName == "" { // the name the operator dialed, not the IP the connection resolved to
		if host, _, err := net.SplitHostPort(s.o.Addr); err == nil {
			cfg.ServerName = host
		}
	}
	return cfg, ""
}

// upgrade sends STARTTLS and runs the handshake; on success s.c is the TLS connection. When it
// returns false the Result ends the check: refused is set for a 580 (the server cannot
// negotiate TLS), which check starttls passes and the others skip.
func (s *session) upgrade(c check) (r Result, note string, refused, ok bool) {
	cfg, note := s.tlsConfig()
	tc, err := s.c.TryStartTLS(cfg)
	if err == nil {
		s.c = tc
		return Result{}, note, false, true
	}
	var se *client.StartTLSError
	var sterr *client.StrictError
	var verr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &se) && se.Code == 580:
		return c.pass(), "", true, false
	case errors.As(err, &se):
		return c.failf("STARTTLS: got %q, want 382: the server lists STARTTLS on a plain connection that has not authenticated (RFC 4642 §2.2.1: 502 only when TLS is active or authentication has occurred)", se.Line), "", false, false
	case errors.As(err, &sterr) && sterr.Kind == client.Pending:
		return c.fail(describe(err)), "", false, false
	case errors.As(err, &verr):
		return c.failf("STARTTLS handshake: the certificate was not accepted by Options.StartTLS: %v", err), "", false, false
	}
	return c.failf("STARTTLS: %s", describe(err)), "", false, false
}

// refusedResult is the result of check c, other than starttls, for a server that answered 580.
func (s *session) refusedResult(c check) Result {
	return c.skip("STARTTLS refused with 580: the server cannot negotiate TLS (see check starttls)")
}

func checkStartTLS(s *session, c check) Result {
	if r, ok := s.startTLSReady(c); !ok {
		return r
	}
	r, note, refused, ok := s.upgrade(c)
	if refused {
		r.Detail = "STARTTLS refused with 580: the server lists STARTTLS but cannot negotiate TLS (RFC 4642 §2.2.2 allows it, e.g. without a certificate); the handshake was not tested"
		return r
	}
	if !ok {
		return r
	}
	// RFC 4642 §2.2.2: the protocol is reset to the state after the greeting, but no greeting
	// is sent: the next step is the client's command
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after the TLS handshake the server sent data before any command (RFC 4642 §2.2.2: no greeting is sent): %s", describe(err))
	}
	lines, err := s.capabilities()
	if err != nil {
		return c.failf("CAPABILITIES after STARTTLS: %s", describe(err))
	}
	if _, err := client.ParseCapabilities(lines); err != nil {
		return c.failf("CAPABILITIES after STARTTLS: %v", err)
	}
	for _, kw := range []string{"STARTTLS", "MODE-READER"} {
		if s.has(kw) {
			return c.failf("CAPABILITIES lists %q while TLS is active (RFC 4642 §2.2.2: neither STARTTLS nor MODE-READER may be listed)", kw)
		}
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after CAPABILITIES over TLS: %s", describe(err))
	}
	res := c.pass()
	res.Detail = note
	return res
}

func checkStartTLSTwice(s *session, c check) Result {
	if r, ok := s.startTLSReady(c); !ok {
		return r
	}
	r, _, refused, ok := s.upgrade(c)
	if refused {
		return s.refusedResult(c)
	}
	if !ok {
		return r
	}
	if _, _, err := s.single([]int{502}, "STARTTLS"); err != nil {
		return c.failf("TLS active: %v (RFC 4642 §2.2.2: 502)", err)
	}
	return c.pass()
}

// pipelinedWait is how long check starttls-pipelined waits for an answer to the command it
// sent behind STARTTLS; a server that executes it in plaintext does so at once, but a slow
// one may take a moment, so the window is longer than the usual quiet window.
const pipelinedWait = 500 * time.Millisecond

// checkStartTLSPipelined sends DATE in the same write as STARTTLS (RFC 4642 §2.2.1 forbids
// pipelining STARTTLS, so what a server does with the command is its own business, except that
// it must not answer it in plaintext: the TLS negotiation begins with the first octet after
// the CRLF of the 382, §2.2.2, and plaintext a man in the middle put in front of the
// handshake must never be executed: CVE-2011-0411). A server that drops the connection, ignores
// the bytes or reads them as a broken handshake and sends a TLS alert passes; one that answers
// DATE with a "111" line fails. The check does not start the handshake: nothing after the
// plaintext is worth testing.
func checkStartTLSPipelined(s *session, c check) Result {
	if r, ok := s.startTLSReady(c); !ok {
		return r
	}
	if err := s.c.Send("STARTTLS\r\nDATE\r\n"); err != nil {
		return c.failf("STARTTLS, DATE: %v", err)
	}
	code, line, err := s.c.ReadStatus()
	switch {
	case err != nil:
		return c.failf("STARTTLS pipelined with DATE: %s", describe(err))
	case code == 580:
		return s.refusedResult(c)
	case code != 382:
		return c.failf("STARTTLS pipelined with DATE: got %q, want 382 (or 580)", line)
	}
	wait := min(pipelinedWait, s.o.deadline())
	err = s.c.ExpectNoPending(wait)
	var se *client.StrictError
	switch {
	case err == nil:
		r := c.pass()
		r.Detail = fmt.Sprintf("the DATE sent behind STARTTLS was not answered (nothing within %v, or the connection was closed)", wait)
		return r
	case errors.As(err, &se) && se.Kind == client.Pending:
		if asciiText(se.Bytes) {
			return c.failf("the DATE sent in the same write as STARTTLS was answered in plaintext after the 382: %q (RFC 4642 §2.2.2: the TLS negotiation begins with the first octet after the 382; CVE-2011-0411)", se.Bytes)
		}
		r := c.pass()
		r.Detail = fmt.Sprintf("the DATE sent behind STARTTLS was not executed: the server sent %d bytes that are not an NNTP reply (a TLS alert?)", len(se.Bytes))
		return r
	}
	return c.failf("after STARTTLS pipelined with DATE: %s", describe(err))
}

// asciiText reports whether the first bytes of b (up to four) are printable ASCII or line ends:
// plaintext, such as an NNTP status line, not the binary record of a TLS alert.
func asciiText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, x := range b[:min(len(b), 4)] {
		if (x < 0x20 || x > 0x7e) && x != '\r' && x != '\n' {
			return false
		}
	}
	return true
}
