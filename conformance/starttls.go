package conformance

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// startTLSChecks are the STARTTLS checks of RFC 4642 and the capability check of implicit TLS.
// Each runs on its own connection. The three "starttls" checks run only when CAPABILITIES lists
// STARTTLS on a plain connection (Skip with Missing "STARTTLS" otherwise; with Options.TLS the
// connection is TLS from the first byte and they are skipped without a Missing);
// tls-capabilities is the reverse: it runs only with Options.TLS. They never log in: STARTTLS is not a valid command after AUTHINFO
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
		{Name: "tls-capabilities", RFC: "RFC 4642 §2.2.2, RFC 8143 §2", Run: checkTLSCapabilities},
	}
}

// checkTLSCapabilities is the one check of this file for implicit TLS (Options.TLS): the
// capability list read on a connection that is TLS from the first byte must not list STARTTLS
// (RFC 4642 §2.2.2: not once a TLS layer is active; RFC 8143 uses the same rule for port 563).
// A SASL EXTERNAL mechanism listed to a client that presented no certificate (Options.TLS has no
// Certificates and no GetClientCertificate) is only noted in the Detail: RFC 4642 §2.2.2 says a
// server might not want to advertise it then, which is no requirement. Skipped without
// Options.TLS.
func checkTLSCapabilities(s *session, c check) Result {
	if s.o.TLS == nil || !s.c.IsTLS() {
		return c.skip("no Options.TLS: the connection is plain")
	}
	if s.caps == nil {
		return c.failf("CAPABILITIES over TLS: %v", s.capsErr)
	}
	if s.has("STARTTLS") {
		return c.fail("CAPABILITIES on a connection that is TLS from the first byte lists STARTTLS (RFC 4642 §2.2.2: not once a TLS layer is active)")
	}
	r := c.pass()
	hasCert := len(s.o.TLS.Certificates) > 0 || s.o.TLS.GetClientCertificate != nil
	if !hasCert && slices.ContainsFunc(capArgs(s.caps, "SASL"), func(a string) bool { return strings.EqualFold(a, "EXTERNAL") }) {
		r.Detail = "SASL EXTERNAL is listed although no client certificate was presented (RFC 4642 §2.2.2: a server might not advertise it then)"
	}
	return r
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
// it does about the certificate. Without verification (nil Options.StartTLS, or one that skips
// it) the certificate is not judged, so neither is the protocol version: MinVersion is TLS 1.0
// unless the config sets one (RFC 4642 asks for nothing newer, and an old server under test
// should not fail these checks for its TLS version). A config that verifies is used as given,
// so a version mismatch with it is a handshake failure, hence a Fail.
func (s *session) tlsConfig() (cfg *tls.Config, note string) {
	if s.o.StartTLS == nil {
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}, "certificate not verified (no Options.StartTLS)" //nolint:gosec // the caller chose not to verify; the Detail says so
	}
	cfg = s.o.StartTLS.Clone()
	if cfg.InsecureSkipVerify {
		if cfg.MinVersion == 0 {
			cfg.MinVersion = tls.VersionTLS10
		}
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
	var rerr tls.RecordHeaderError
	switch {
	case errors.As(err, &se) && se.Code == 580:
		return c.pass(), "", true, false
	case errors.As(err, &se):
		return c.failf("STARTTLS: got %q, want 382: the server lists STARTTLS on a plain connection that has not authenticated (RFC 4642 §2.2.1: 502 only when TLS is active or authentication has occurred)", se.Line), "", false, false
	case errors.As(err, &sterr) && sterr.Kind == client.Pending:
		return c.fail(describe(err)), "", false, false
	case errors.As(err, &verr):
		return c.failf("STARTTLS handshake: the certificate was not accepted by Options.StartTLS: %v", err), "", false, false
	case errors.As(err, &rerr):
		// the plaintext of an injection that arrived in a later TCP read than the 382
		return c.failf("the server sent plaintext after the 382 instead of starting the TLS handshake (record header %q): %v", rerr.RecordHeader[:], err), "", false, false
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
		return c.failf("after the TLS handshake the server sent data before any command (RFC 4642 §2.2.2: no greeting is sent after the handshake): %s", describe(err))
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
// sent behind STARTTLS, before and after the handshake; a server that executes it does so at
// once, but a slow one may take a moment, so the window is longer than the usual quiet window.
const pipelinedWait = 500 * time.Millisecond

// checkStartTLSPipelined sends DATE in the same write as STARTTLS and judges what the server
// does with it. RFC 4642 §2.2.1 forbids pipelining STARTTLS, so what a server does with the
// command is its own business, except that RFC 8143 §4 says it MUST discard any NNTP command
// received between STARTTLS and the end of the TLS negotiation (the STARTTLS command
// injection, CVE-2011-0411, RFC 7457 §2.2). There are two ways to get it wrong, and the check
// looks for both:
//
//   - the server answers the DATE in plaintext right after the 382 (it ran the command before
//     the handshake): looked for in the first 500 ms; a status line there is a Fail;
//   - the server keeps the DATE in its input buffer, completes the handshake and runs it then
//     (the flaw of the real CVE): the check does the handshake and waits another 500 ms; any
//     reply over TLS before the client sent a command is a Fail.
//
// A server that closes the connection, sends something that is not an NNTP reply (a TLS alert)
// or fails the handshake has not run the command: Pass, with a Detail saying which. A reply
// other than 382 or 580 to the pipelined STARTTLS is a Skip: the plain STARTTLS is judged by
// check starttls, and the pipelining is the client's violation.
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
		return c.skipf("STARTTLS pipelined with DATE was answered %q, not 382: nothing to test (the plain STARTTLS is judged by check starttls)", line)
	}
	wait := min(pipelinedWait, s.o.deadline())

	// before the handshake
	err = s.c.ExpectNoPending(wait)
	var se *client.StrictError
	switch {
	case errors.As(err, &se) && se.Kind == client.Pending:
		time.Sleep(50 * time.Millisecond) // the rest of a reply that arrived in pieces
		b, _ := s.c.R.Peek(s.c.R.Buffered())
		if plaintextNNTP(b) {
			return c.failf("the DATE sent in the same write as STARTTLS was answered in plaintext right after the 382: %q (RFC 8143 §4: a server MUST discard any NNTP command received between STARTTLS and the end of the TLS negotiation; CVE-2011-0411)", b)
		}
		r := c.pass()
		r.Detail = fmt.Sprintf("the DATE sent behind STARTTLS was not executed: the server sent %d bytes that are not an NNTP reply (a TLS alert?) instead of waiting for the handshake", len(b))
		return r
	case err != nil:
		r := c.pass() // a reset: the server dropped the connection
		r.Detail = fmt.Sprintf("the DATE sent behind STARTTLS was not executed: the server dropped the connection (%v)", err)
		return r
	}

	// the handshake, then anything the server says before the client sends a command
	cfg, note := s.tlsConfig()
	tc := tls.Client(s.c.Conn, cfg)
	_ = tc.SetDeadline(time.Now().Add(s.o.deadline()))
	if err := tc.Handshake(); err != nil {
		var verr *tls.CertificateVerificationError
		if errors.As(err, &verr) {
			return c.failf("STARTTLS handshake: the certificate was not accepted by Options.StartTLS: %v", err)
		}
		r := c.pass()
		r.Detail = fmt.Sprintf("the DATE sent behind STARTTLS was not executed: the server did not complete the TLS handshake after it (%v)", err)
		return r
	}
	s.c = &client.Conn{Conn: tc, R: bufio.NewReaderSize(tc, readerSize), Greeting: s.c.Greeting, Deadline: s.c.Deadline, Strict: true}
	err = s.c.ExpectNoPending(wait)
	switch {
	case errors.As(err, &se) && se.Kind == client.Pending:
		time.Sleep(50 * time.Millisecond)
		b, _ := s.c.R.Peek(s.c.R.Buffered())
		return c.failf("the DATE sent in the same write as STARTTLS was executed after the TLS handshake: the server sent %q before the client sent a command (RFC 8143 §4: a server MUST discard any NNTP command received between STARTTLS and the end of the TLS negotiation; CVE-2011-0411)", b)
	case err != nil && !errors.Is(err, os.ErrDeadlineExceeded):
		return c.failf("after the TLS handshake: %s", describe(err))
	}
	r := c.pass()
	r.Detail = fmt.Sprintf("the DATE sent behind STARTTLS was discarded: nothing came before or after the handshake within %v each", wait)
	if note != "" {
		r.Detail += "; " + note
	}
	return r
}

// plaintextNNTP reports whether b starts like an NNTP status line: three digits and then a
// space, a line end or nothing more.
func plaintextNNTP(b []byte) bool {
	if len(b) < 3 {
		return false
	}
	for _, x := range b[:3] {
		if x < '0' || x > '9' {
			return false
		}
	}
	return len(b) == 3 || b[3] == ' ' || b[3] == '\r' || b[3] == '\n'
}
