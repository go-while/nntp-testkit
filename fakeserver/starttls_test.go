package fakeserver

import (
	"crypto/tls"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/tlscert"
)

// A client that pipelines bytes after STARTTLS gets the 382 and a closed connection: the
// server does not read plaintext that was sent before the handshake.
func TestStartTLSPipelinedPlaintext(t *testing.T) {
	ca := tlscert.NewCA(t)
	s := New(t, Options{Store: testStore(), StoreOpts: StoreOptions{StartTLS: &StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}}})
	c := client.Dial(t, s.Addr())
	c.MustSend("STARTTLS\r\nCAPABILITIES\r\n")
	wantStatus(t, c, 382)
	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("after the 382: %v, want EOF", err)
	}
	if s.TLSHandshakes()+s.TLSFailed() != 0 {
		t.Error("handshake started")
	}
}

// Faults.Override works on STARTTLS in Store mode: a refusal, and a 382 that upgrades.
func TestStartTLSOverride(t *testing.T) {
	ca := tlscert.NewCA(t)
	opts := StoreOptions{StartTLS: &StartTLSOptions{Config: ca.Issue(t, tlscert.Options{}).ServerConfig()}}
	s := New(t, Options{Store: testStore(), StoreOpts: opts, Faults: Faults{Override: map[string][]string{"STARTTLS": {"580 no"}}}})
	c := client.Dial(t, s.Addr())
	wantCode(t, c, "STARTTLS", 580)
	if s.TLSHandshakes() != 0 || c.IsTLS() {
		t.Error("upgraded after 580")
	}
	s = New(t, Options{Store: testStore(), StoreOpts: StoreOptions{}, Faults: Faults{Override: map[string][]string{"STARTTLS": {"382 go"}}}})
	c = client.Dial(t, s.Addr())
	wantCode(t, c, "STARTTLS", 382) // no StartTLS configured: unknown command first, Override replaces the reply, no upgrade
	if s.TLSHandshakes() != 0 {
		t.Error("handshake without a config")
	}
}

// A Handler upgrades with Conn.StartTLS; Conn.TLSState shows the handshake.
func TestConnStartTLS(t *testing.T) {
	ca := tlscert.NewCA(t)
	cfg := ca.Issue(t, tlscert.Options{}).ServerConfig()
	var seen []string
	s := New(t, Options{Handler: LineHandler(func(c *Conn, line string) ([]string, bool) {
		if line == "STARTTLS" {
			if c.TLS() {
				return []string{"502 already"}, false
			}
			_ = c.WriteLines("382 go")
			if err := c.StartTLS(cfg); err != nil {
				return nil, true
			}
			st, _ := c.TLSState()
			seen = append(seen, st.ServerName)
			return nil, false
		}
		return []string{"200 " + line}, false
	})})
	c := client.Dial(t, s.Addr())
	tc := c.StartTLS(&tls.Config{RootCAs: ca.Pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	wantCode(t, tc, "HELLO", 200)
	wantCode(t, tc, "STARTTLS", 502)
	s.WaitLines(t, []string{"STARTTLS", "HELLO", "STARTTLS"})
	if !slices.Equal(seen, []string{"localhost"}) {
		t.Errorf("TLSState server names %q", seen)
	}
}
