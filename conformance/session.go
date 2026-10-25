package conformance

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/client"
)

// readerSize is the bufio size of a session's client (the client package keeps its own
// constant unexported; 64 KiB holds any status line and most overview lines).
const readerSize = 64 << 10

// session is one connection to the server under test: the strict client, the greeting and the
// capability list read right after the greeting (before MODE READER or AUTHINFO, so a check
// can compare the two lists). Every check gets a fresh session; close ends it.
type session struct {
	o        Options
	c        *client.Conn
	greeting string
	code     int      // greeting code (200/201; 400/502 when the server refused)
	caps     []string // CAPABILITIES lines without the 101 line and the terminator; nil when CAPABILITIES failed
	capsErr  error    // why caps is nil
	authed   bool     // AUTHINFO succeeded on this session (see auth)
}

// deadline is the hang guard of this run.
func (o Options) deadline() time.Duration {
	if o.Deadline > 0 {
		return o.Deadline
	}
	return testkit.Deadline()
}

// dial opens a session: TCP or TLS per o.TLS, strict client, greeting read. It does not read
// CAPABILITIES (see capabilities): a check about the greeting wants nothing else on the wire.
func dial(o Options) (*session, error) {
	d := o.deadline()
	var c *client.Conn
	if o.TLS == nil {
		var err error
		if c, err = client.TryDial(o.Addr, d); err != nil {
			return nil, err
		}
	} else {
		nc, err := net.DialTimeout("tcp", o.Addr, d)
		if err != nil {
			return nil, err
		}
		tc := tls.Client(nc, o.TLS)
		_ = tc.SetDeadline(time.Now().Add(d))
		if err := tc.Handshake(); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("tls: %w", err)
		}
		c = &client.Conn{Conn: tc, R: bufio.NewReaderSize(tc, readerSize), Deadline: d, Strict: true}
		if _, err := c.ReadGreeting(); err != nil {
			c.Close()
			return nil, fmt.Errorf("greeting: %w", err)
		}
	}
	s := &session{o: o, c: c, greeting: c.Greeting}
	s.code, _ = client.ParseCode(c.Greeting)
	return s, nil
}

// capabilities sends CAPABILITIES and stores the list (without the status line and the
// terminator). It is called once per session by Run before the check; a check may call it
// again (e.g. after MODE READER or AUTHINFO) and gets the new list.
func (s *session) capabilities() ([]string, error) {
	code, line, err := s.c.TryCmd("CAPABILITIES")
	if err != nil {
		s.caps, s.capsErr = nil, err
		return nil, err
	}
	if code != 101 {
		s.caps, s.capsErr = nil, fmt.Errorf("CAPABILITIES: %s", line)
		return nil, s.capsErr
	}
	lines, err := s.c.TryReadDotLines()
	if err != nil {
		s.caps, s.capsErr = nil, err
		return nil, err
	}
	s.caps, s.capsErr = lines, nil
	return lines, nil
}

// has reports whether the capability list holds keyword kw (first word of a line, case-insensitive).
func (s *session) has(kw string) bool {
	return capArgs(s.caps, kw) != nil
}

// capArgs returns the arguments of capability kw ("" → an empty non-nil slice when listed
// without arguments), or nil when kw is not listed.
func capArgs(caps []string, kw string) []string {
	for _, l := range caps {
		f := strings.Fields(l)
		if len(f) > 0 && strings.EqualFold(f[0], kw) {
			return f[1:]
		}
	}
	return nil
}

// cmd sends one command and returns code and line; a strict-mode or I/O error is returned.
func (s *session) cmd(format string, args ...any) (int, string, error) {
	return s.c.TryCmd(format, args...)
}

// block reads a dot block after a 2xx multi-line reply.
func (s *session) block() ([]string, error) {
	return s.c.TryReadDotLines()
}

// quiet fails when anything arrives within d (a stray byte, a second status line).
func (s *session) quiet(d time.Duration) error {
	return s.c.ExpectNoPending(d)
}

// auth runs AUTHINFO USER/PASS with the session's credentials; nil when the server answered
// 281 (or 381 then 281). Checks that need an authenticated session call it first.
func (s *session) auth() error {
	if s.o.User == "" {
		return fmt.Errorf("no credentials")
	}
	code, line, err := s.cmd("AUTHINFO USER %s", s.o.User)
	if err != nil {
		return err
	}
	switch code {
	case 281:
		s.authed = true
		return nil
	case 381:
	default:
		return fmt.Errorf("AUTHINFO USER: %s", line)
	}
	code, line, err = s.cmd("AUTHINFO PASS %s", s.o.Pass)
	if err != nil {
		return err
	}
	if code != 281 {
		return fmt.Errorf("AUTHINFO PASS: %s", line)
	}
	s.authed = true
	return nil
}

// close ends the session (idempotent).
func (s *session) close() {
	if s != nil && s.c != nil {
		s.c.Close()
	}
}

// msgID returns a unique Message-ID for an article a check posts.
func msgID(check string) string {
	return fmt.Sprintf("<%s-%d@conformance.invalid>", strings.ToLower(check), time.Now().UnixNano())
}
