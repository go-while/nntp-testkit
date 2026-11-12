package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
)

// proc is one in-process run of the program, driven as a caller in another language would: the
// config in a file, the ready line on stdout, stdin closed when the client is done.
type proc struct {
	t      *testing.T
	ready  ready
	stdin  *io.PipeWriter
	out    *bufio.Reader
	stderr *bytes.Buffer
	code   chan int
	cancel context.CancelFunc
}

func writeConfig(t *testing.T, cfg string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startProc starts run with the config and waits for the ready line.
func startProc(t *testing.T, cfg string, flags ...string) *proc {
	t.Helper()
	p := launch(t, cfg, flags...)
	line, err := p.out.ReadString('\n')
	if err != nil {
		t.Fatalf("no ready line: %v\nstderr: %s", err, p.stderr.String())
	}
	if err := json.Unmarshal([]byte(line), &p.ready); err != nil {
		t.Fatalf("ready line %q: %v", line, err)
	}
	return p
}

// launch starts run in a goroutine; the caller reads p.out.
func launch(t *testing.T, cfg string, flags ...string) *proc {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{t: t, stdin: inW, out: bufio.NewReader(outR), stderr: &bytes.Buffer{}, code: make(chan int, 1), cancel: cancel}
	args := append(slices.Clone(flags), writeConfig(t, cfg))
	go func() {
		code := run(ctx, args, inR, outW, p.stderr)
		_ = outW.Close()
		p.code <- code
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		_ = outR.Close()
	})
	return p
}

// finish closes stdin like the caller does and returns the summary and the exit status.
func (p *proc) finish() (summary, int) {
	p.t.Helper()
	_ = p.stdin.Close()
	line, err := p.out.ReadString('\n')
	if err != nil {
		p.t.Fatalf("no summary line: %v\nstderr: %s", err, p.stderr.String())
	}
	var s summary
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		p.t.Fatalf("summary %q: %v", line, err)
	}
	select {
	case code := <-p.code:
		return s, code
	case <-time.After(10 * time.Second):
		p.t.Fatal("run did not return")
		return s, -1
	}
}

func (p *proc) dial() *client.Conn {
	p.t.Helper()
	c, err := client.TryDial(p.ready.Addr, 10*time.Second)
	if err != nil {
		p.t.Fatalf("dial: %v", err)
	}
	p.t.Cleanup(c.Close)
	return c
}

func (p *proc) caPool() *x509.CertPool {
	p.t.Helper()
	pem, err := os.ReadFile(p.ready.CA)
	if err != nil {
		p.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		p.t.Fatalf("no certificate in %s", p.ready.CA)
	}
	return pool
}

func cmd(t *testing.T, c *client.Conn, want int, format string, args ...any) string {
	t.Helper()
	code, line, err := c.TryCmd(format, args...)
	if err != nil || code != want {
		t.Fatalf("%s: got %d %q err %v, want %d", format, code, line, err, want)
	}
	return line
}

const storeCfg = `{"Mode": "store",
 "Options": {"Faults": {"DropInMultiline": {"ARTICLE": 2}, "Delay": {"DATE": 300}}},
 "Store": {
  "Groups": [{"Name": "alt.test", "Description": "test group"}, {"Name": "alt.empty"}],
  "Articles": [
   {"Group": "alt.test", "Number": 1, "MessageID": "<1@test>",
    "Headers": ["Subject: one", "From: a@b.example"], "Body": ["text", ".dot"]},
   {"Group": "alt.test", "Number": 2, "MessageID": "<2@test>",
    "Headers": ["Subject: two"], "Body": ["b"], "Date": "2021-03-04T05:06:07Z"}]}}`

// TestStoreWithFaults: the Store is served, a fault applies (a drop in the second line of a
// reply, a delay in ms), and the summary says what the server saw.
func TestStoreWithFaults(t *testing.T) {
	p := startProc(t, storeCfg)
	if p.ready.Host != "127.0.0.1" || p.ready.Addr != net.JoinHostPort(p.ready.Host, strconv.Itoa(p.ready.Port)) || p.ready.CA == "" {
		t.Errorf("ready line %+v", p.ready)
	}
	c := p.dial()
	if line := cmd(t, c, 211, "GROUP alt.test"); line != "211 2 1 2 alt.test" {
		t.Errorf("GROUP: %q", line)
	}
	start := time.Now()
	cmd(t, c, 111, "DATE")
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("Delay DATE 300 ms took %v", d)
	}
	cmd(t, c, 220, "ARTICLE 1")
	lines, err := c.TryReadDotLines()
	if err == nil {
		t.Fatalf("ARTICLE was not cut: %q", lines)
	}
	s, code := p.finish()
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, p.stderr.String())
	}
	if want := []string{"GROUP alt.test", "DATE", "ARTICLE 1"}; !slices.Equal(s.Lines, want) {
		t.Errorf("lines %q, want %q", s.Lines, want)
	}
	if s.Accepted != 1 || s.Counts["GROUP"] != 1 || s.Counts["ARTICLE"] != 1 {
		t.Errorf("accepted %d counts %v", s.Accepted, s.Counts)
	}
	if len(s.Mismatches) != 0 || len(s.Notes) != 0 || len(s.Articles) != 0 || s.TLSHandshakes != 0 {
		t.Errorf("summary %+v", s)
	}
	if _, err := os.Stat(p.ready.CA); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the CA file is still there after exit: %v", err)
	}
}

// TestStoreDates: Date, Arrived and Created are RFC 3339 strings; the defaults are fixed.
func TestStoreDates(t *testing.T) {
	p := startProc(t, storeCfg)
	c := p.dial()
	cmd(t, c, 211, "GROUP alt.test")
	cmd(t, c, 224, "OVER 1-2")
	lines, err := c.TryReadDotLines()
	if err != nil || len(lines) != 2 {
		t.Fatalf("OVER: %q %v", lines, err)
	}
	if !strings.Contains(lines[1], "04 Mar 2021 05:06:07") {
		t.Errorf("article 2 has its own Date: %q", lines[1])
	}
	if !strings.Contains(lines[0], "02 Jan 2020 03:04:05") {
		t.Errorf("article 1 has the default Date: %q", lines[0])
	}
	p.finish()
}

// TestPeerIhave: IHAVE against the peer, with its own PeerOptions, and the article is in the
// summary.
func TestPeerIhave(t *testing.T) {
	p := startProc(t, `{"Mode": "peer", "PeerOptions": {"Have": {"<have@x>": true}, "Defer436Once": {"<def@x>": true}}}`)
	c := p.dial()
	cmd(t, c, 335, "IHAVE <new@x>")
	if err := c.Send("Subject: s\r\n\r\n..dot\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	code, line, err := c.ReadStatus()
	if err != nil || code != 235 {
		t.Fatalf("after the article: %d %q %v", code, line, err)
	}
	cmd(t, c, 435, "IHAVE <have@x>")
	cmd(t, c, 436, "IHAVE <def@x>")
	cmd(t, c, 335, "IHAVE <def@x>")
	s, _ := p.finish()
	if len(s.Articles) != 1 || !slices.Equal(s.Articles[0], []string{"Subject: s", "", "..dot"}) {
		t.Errorf("articles %q", s.Articles)
	}
	if s.Counts["IHAVE"] != 4 {
		t.Errorf("counts %v", s.Counts)
	}
}

// TestPeerBadOption: a PeerOptions key that is a function or does not exist is refused.
func TestPeerBadOption(t *testing.T) {
	for key, want := range map[string]string{
		`"OnArticle": {}`:      "cannot be set from JSON",
		`"HoldGreeting": true`: "cannot be set from JSON",
		`"Have": 3`:            "want an object",
	} {
		p := launch(t, `{"Mode": "peer", "PeerOptions": {`+key+`}}`)
		if code := <-p.code; code != 2 || !strings.Contains(p.stderr.String(), want) {
			t.Errorf("%s: exit %d, stderr %s", key, code, p.stderr.String())
		}
	}
}

// TestScriptMode: the kit's text script.
func TestScriptMode(t *testing.T) {
	p := startProc(t, `{"Mode": "script", "Options": {"Greeting": "200 hi",
	  "Script": [{"Expect": "CAPABILITIES", "Delay": 20, "Reply": ["101 caps", "VERSION 2", "."]},
	  {"Expect": "DATE", "Reply": ["111 20200102030405"]}]}}`)
	c := p.dial()
	if c.Greeting != "200 hi" {
		t.Errorf("greeting %q", c.Greeting)
	}
	cmd(t, c, 101, "CAPABILITIES")
	if _, err := c.TryReadDotLines(); err != nil {
		t.Fatal(err)
	}
	cmd(t, c, 500, "NOPE")
	cmd(t, c, 111, "DATE")
	s, _ := p.finish()
	if !slices.Equal(s.Mismatches, []string{"NOPE"}) {
		t.Errorf("mismatches %q", s.Mismatches)
	}
}

const rawCfg = `{"Mode": "raw", "Raw": [
 {"Send": [{"Text": "200 raw ready\r\n"}]},
 {"Expect": "GROUP .*", "Send": [{"Text": "211 1 1 1 alt.a\r\n", "Split": 4, "SplitGap": 2, "Gap": 5}]},
 {"Expect": "POST", "ReadDot": false, "Send": [{"Text": "340 go\r\n"}]},
 {"Expect": "(?i)subject: .*", "ReadDot": true, "DotMax": 3, "Send": [{"B64": "MjQwIG9rDQo="}]},
 {"Expect": "QUIT", "Send": [{"Text": "205 bye\r\n"}], "Then": "close"}]}`

// TestRawChunks: a reply in pieces arrives whole, base64 chunks are bytes, a dot block is read
// and recorded, a line that does not fit is a mismatch.
func TestRawChunks(t *testing.T) {
	p := startProc(t, rawCfg)
	c := p.dial()
	cmd(t, c, 500, "NOPE")
	cmd(t, c, 211, "GROUP alt.a")
	cmd(t, c, 340, "POST")
	if err := c.Send("Subject: x\r\n\r\nbody\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	if code, line, err := c.ReadStatus(); err != nil || code != 240 {
		t.Fatalf("%d %q %v", code, line, err)
	}
	cmd(t, c, 205, "QUIT")
	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("after the close: %v", err)
	}
	s, code := p.finish()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := []string{"NOPE", "GROUP alt.a", "POST", "Subject: x", "QUIT"}; !slices.Equal(s.Lines, want) {
		t.Errorf("lines %q, want %q", s.Lines, want)
	}
	if !slices.Equal(s.Mismatches, []string{"NOPE"}) {
		t.Errorf("mismatches %q", s.Mismatches)
	}
	if len(s.Articles) != 1 || !slices.Equal(s.Articles[0], []string{"", "body", "."}) {
		t.Errorf("articles %q", s.Articles)
	}
	if s.Counts["GROUP"] != 1 || s.Counts["QUIT"] != 1 {
		t.Errorf("counts %v", s.Counts)
	}
}

// TestRawSplitPieces: Split writes one piece per write, so the client sees several reads when
// it reads between them (a pause of 50 ms is far more than the server needs between writes).
func TestRawSplitPieces(t *testing.T) {
	p := startProc(t, `{"Mode": "raw", "Raw": [{"Send": [{"Text": "200 abcdef\r\n", "Split": 4, "SplitGap": 50}]}]}`)
	nc, err := net.DialTimeout("tcp", p.ready.Addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var reads int
	var got []byte
	buf := make([]byte, 64)
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	for !bytes.HasSuffix(got, []byte("\r\n")) {
		n, err := nc.Read(buf)
		if err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
		reads++
		got = append(got, buf[:n]...)
	}
	if string(got) != "200 abcdef\r\n" || reads < 2 {
		t.Errorf("got %q in %d reads", got, reads)
	}
	p.finish()
}

const rawTLSCfg = `{"Mode": "raw", "Raw": [
 {"Send": [{"Text": "200 raw ready\r\n"}]},
 {"Expect": "STARTTLS", "Send": [{"Text": "382 go\r\n"}], "Then": "starttls"},
 {"Expect": "DATE", "Send": [{"Text": "111 20200102030405\r\n"}]}]}`

// TestRawStartTLS: a raw script upgrades with the program's certificate; the client verifies it
// against the CA file of the ready line, and the summary counts the handshake and the name.
func TestRawStartTLS(t *testing.T) {
	p := startProc(t, rawTLSCfg)
	c := p.dial()
	tc, err := c.TryStartTLS(&tls.Config{RootCAs: p.caPool(), ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if line := cmd(t, tc, 111, "DATE"); line != "111 20200102030405" {
		t.Errorf("DATE over TLS: %q", line)
	}
	s, _ := p.finish()
	if want := []string{"STARTTLS", "DATE"}; !slices.Equal(s.Lines, want) {
		t.Errorf("lines %q, want %q", s.Lines, want)
	}
	if s.TLSHandshakes != 1 || !slices.Equal(s.ServerNames, []string{"localhost"}) || len(s.Mismatches) != 0 {
		t.Errorf("tls_handshakes %d server_names %q mismatches %q notes %q", s.TLSHandshakes, s.ServerNames, s.Mismatches, s.Notes)
	}
}

// TestRawStartTLSPipelined: plaintext sent before the handshake is noted and the connection is
// closed, as the kit does in Store mode.
func TestRawStartTLSPipelined(t *testing.T) {
	p := startProc(t, rawTLSCfg)
	nc, err := net.DialTimeout("tcp", p.ready.Addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(nc)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Both lines go in one write, so the second is already in the server's read buffer when
	// the step for STARTTLS has answered.
	if _, err := io.WriteString(nc, "STARTTLS\r\nDATE\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, br); err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal(err)
	}
	s, _ := p.finish()
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "pipelined") {
		t.Errorf("notes %q", s.Notes)
	}
}

// TestRawRST: Then "rst" resets the connection.
func TestRawRST(t *testing.T) {
	p := startProc(t, `{"Mode": "raw", "Raw": [{"Send": [{"Text": "200 hi\r\n"}]},
	  {"Expect": "QUIT", "Sleep": 20, "Then": "rst"}]}`)
	c := p.dial()
	if err := c.Send("QUIT\r\n"); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadLine()
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("read after rst: %v, want connection reset", err)
	}
	p.finish()
}

// TestRawInflate: inflate reads the client through raw DEFLATE from then on (the replies stay
// plain here).
func TestRawInflate(t *testing.T) {
	p := startProc(t, `{"Mode": "raw", "Raw": [{"Send": [{"Text": "200 hi\r\n"}]},
	  {"Expect": "COMPRESS DEFLATE", "Send": [{"Text": "206 ok\r\n"}], "Then": "inflate"},
	  {"Expect": "DATE", "Send": [{"Text": "111 20200102030405\r\n"}]}]}`)
	c := p.dial()
	cmd(t, c, 206, "COMPRESS DEFLATE")
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("DATE\r\n"))
	if err := fw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(buf.String()); err != nil {
		t.Fatal(err)
	}
	if code, line, err := c.ReadStatus(); err != nil || code != 111 {
		t.Fatalf("DATE through DEFLATE: %d %q %v", code, line, err)
	}
	s, _ := p.finish()
	if want := []string{"COMPRESS DEFLATE", "DATE"}; !slices.Equal(s.Lines, want) || len(s.Mismatches) != 0 {
		t.Errorf("lines %q mismatches %q", s.Lines, s.Mismatches)
	}
}

// TestRawHang: hang reads and never answers, and does not close.
func TestRawHang(t *testing.T) {
	p := startProc(t, `{"Mode": "raw", "Raw": [{"Send": [{"Text": "200 hi\r\n"}]}, {"Expect": "QUIT", "Then": "hang"}]}`)
	nc, err := net.DialTimeout("tcp", p.ready.Addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	br := bufio.NewReader(nc)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	io.WriteString(nc, "QUIT\r\nMORE\r\n")
	_ = nc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var ne net.Error
	if line, err := br.ReadString('\n'); !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("a hanging server answered or closed: %q %v", line, err)
	}
	p.finish()
}

// TestImplicitTLS: Options.TLS makes every connection TLS; the CA file verifies for localhost
// and for 127.0.0.1, and the handshakes and names are in the summary.
func TestImplicitTLS(t *testing.T) {
	p := startProc(t, `{"Mode": "store", "Options": {"TLS": true}, "Store": {"Groups": [{"Name": "a.b"}]}}`)
	pool := p.caPool()
	for _, name := range []string{"localhost", "127.0.0.1"} {
		nc, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", p.ready.Addr, &tls.Config{RootCAs: pool, ServerName: name})
		if err != nil {
			t.Fatalf("verify %s: %v", name, err)
		}
		c := client.New(t, nc)
		if _, err := c.ReadGreeting(); err != nil {
			t.Fatal(err)
		}
		cmd(t, c, 211, "GROUP a.b")
		c.Close()
	}
	// A client that does not trust the certificate fails; the server counts the failure.
	if nc, err := tls.Dial("tcp", p.ready.Addr, &tls.Config{ServerName: "localhost"}); err == nil {
		nc.Close()
		t.Error("the system roots accepted the certificate")
	}
	s, _ := p.finish()
	if s.TLSHandshakes != 2 || s.TLSFailed != 1 {
		t.Errorf("tls_handshakes %d tls_failed %d", s.TLSHandshakes, s.TLSFailed)
	}
	if len(s.ServerNames) != 3 || s.ServerNames[0] != "localhost" || s.ServerNames[1] != "" {
		t.Errorf("server_names %q", s.ServerNames)
	}
}

// TestStoreStartTLS: StoreOpts.StartTLS {} offers STARTTLS with the program's certificate, and
// the knobs of StartTLSOptions are read.
func TestStoreStartTLS(t *testing.T) {
	p := startProc(t, `{"Mode": "store", "Options": {"StoreOpts": {"StartTLS": {}}}}`)
	c := p.dial()
	tc, err := c.TryStartTLS(&tls.Config{RootCAs: p.caPool(), ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	cmd(t, tc, 502, "STARTTLS")
	tc.Close()
	s, _ := p.finish()
	if s.TLSHandshakes != 1 {
		t.Errorf("tls_handshakes %d", s.TLSHandshakes)
	}

	p = startProc(t, `{"Mode": "peer", "PeerOptions": {"StartTLS": {"Refuse": "580 no thanks", "HideCapability": true}}}`)
	c = p.dial()
	var se *client.StartTLSError
	if _, err := c.TryStartTLS(&tls.Config{RootCAs: p.caPool()}); !errors.As(err, &se) || se.Code != 580 {
		t.Errorf("refused STARTTLS: %v", err)
	}
	p.finish()
}

// TestListenFlag: -listen picks the address; Options.Addr is the default.
func TestListenFlag(t *testing.T) {
	addr := freeAddr(t)
	p := startProc(t, `{"Mode": "store"}`, "-listen", addr)
	if p.ready.Addr != addr {
		t.Errorf("addr %q, want %q", p.ready.Addr, addr)
	}
	p.finish()
	addr = freeAddr(t)
	p = startProc(t, `{"Mode": "store", "Options": {"Addr": "`+addr+`"}}`)
	if p.ready.Addr != addr {
		t.Errorf("addr %q, want %q", p.ready.Addr, addr)
	}
	p.finish()
	busy := fakeserver.New(t, fakeserver.Options{})
	p = launch(t, `{"Mode": "store"}`, "-listen", busy.Addr())
	if code := <-p.code; code != 1 || !strings.Contains(p.stderr.String(), "listen") {
		t.Errorf("address in use: exit %d, stderr %s", code, p.stderr.String())
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestSummaryShape: with no client every key is there, empty lists are [] and not null.
func TestSummaryShape(t *testing.T) {
	p := startProc(t, `{"Mode": "store"}`)
	_ = p.stdin.Close()
	line, err := p.out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"lines": "[]", "mismatches": "[]", "articles": "[]", "notes": "[]", "accepted": "0",
		"counts": "{}", "tls_handshakes": "0", "tls_failed": "0", "server_names": "[]",
		"lines_b64": "[]", "mismatches_b64": "[]", "articles_raw": "[]", "max_open": "0", "closed": "0"}
	for k, v := range want {
		if string(m[k]) != v {
			t.Errorf("%s = %s, want %s", k, m[k], v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("keys %v", m)
	}
	if code := <-p.code; code != 0 {
		t.Errorf("exit %d", code)
	}
}

// TestSignal: a cancelled context ends the run like end of file.
func TestSignal(t *testing.T) {
	p := startProc(t, `{"Mode": "store"}`)
	p.cancel()
	if _, err := p.out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if code := <-p.code; code != 0 {
		t.Errorf("exit %d", code)
	}
}

// TestBadConfig: a config error is exit 2 with the place in stderr, before anything listens.
func TestBadConfig(t *testing.T) {
	cases := []struct{ name, cfg, want string }{
		{"unknown-top-key", `{"Mode": "store", "mode": "peer"}`, `config.mode: unknown key`},
		{"unknown-nested", `{"Mode": "store", "Options": {"Faults": {"Delayy": {}}}}`, `config.Options.Faults.Delayy: unknown key (the keys of Faults are Delay,`},
		{"typo-in-store", `{"Mode": "store", "Store": {"Articles": [{"Group": "a", "Bodyy": []}]}}`, `config.Store.Articles[0].Bodyy: unknown key`},
		{"no-mode", `{}`, `config.Mode: want one of store, peer, script, raw`},
		{"bad-mode", `{"Mode": "relay"}`, `"relay"`},
		{"not-json", `{"Mode": `, "unexpected end of JSON input"},
		{"trailing", `{"Mode": "store"} {}`, "invalid character"},
		{"duration-string", `{"Mode": "store", "Options": {"Faults": {"Delay": {"DATE": "2s"}}}}`, `config.Options.Faults.Delay["DATE"]: want a number of milliseconds`},
		{"wrong-type", `{"Mode": "store", "Options": {"Faults": {"CloseAfter": "x"}}}`, "config.Options.Faults.CloseAfter: want int"},
		{"lower-case-command", `{"Mode": "store", "Options": {"Faults": {"Delay": {"date": 2000}}}}`, `config.Options.Faults.Delay["date"]: command names are upper case ("DATE")`},
		{"lower-case-drop", `{"Mode": "store", "Options": {"Faults": {"DropInMultiline": {"Article": 1}}}}`, `DropInMultiline["Article"]`},
		{"both-body-forms", `{"Mode": "store", "Store": {"Articles": [{"Group": "a", "Body": ["x"], "BodyB64": ["eA=="]}]}}`, "Articles[0].Body and BodyB64 are both set"},
		{"bad-body-b64", `{"Mode": "store", "Store": {"Articles": [{"Group": "a", "BodyB64": ["!"]}]}}`, "Articles[0].BodyB64[0]:"},
		{"bad-headers-b64", `{"Mode": "store", "Store": {"Articles": [{"Group": "a", "HeadersB64": ["!!"]}]}}`, "config.Store.Articles[0].HeadersB64[0]:"},
		{"sleep-over-deadline", `{"Mode": "raw", "Raw": [{"Sleep": 30000}]}`, "config.Raw[0].Sleep: 30s is longer than Options.Deadline (10s)"},
		{"gap-over-deadline", `{"Mode": "raw", "Options": {"Deadline": 500}, "Raw": [{"Send": [{"Text": "x", "Gap": 600}]}]}`, "config.Raw[0].Send[0].Gap: 600ms is longer than Options.Deadline (500ms)"},
		{"splitgap-over-deadline", `{"Mode": "raw", "Raw": [{"Send": [{"Text": "xy", "Split": 1, "SplitGap": 10001}]}]}`, "config.Raw[0].Send[0].SplitGap"},
		{"handler", `{"Mode": "store", "Options": {"Handler": {}}}`, "config.Options.Handler: cannot be set from JSON"},
		{"store-object", `{"Mode": "store", "Options": {"Store": {}}}`, "config.Options.Store: cannot be set from JSON"},
		{"peer-in-store", `{"Mode": "store", "PeerOptions": {"User": "u"}}`, `config.PeerOptions is not used in mode "store"`},
		{"faults-in-raw", `{"Mode": "raw", "Options": {"Faults": {"CloseAfter": 1}}}`, `config.Options.Faults is not used in mode "raw"`},
		{"raw-in-store", `{"Mode": "store", "Raw": [{}]}`, `config.Raw is not used in mode "store"`},
		{"bad-regexp", `{"Mode": "raw", "Raw": [{"Expect": "(("}]}`, "config.Raw[0].Expect"},
		{"bad-script-regexp", `{"Mode": "script", "Options": {"Script": [{"Expect": "(("}]}}`, "config.Options.Script[0].Expect"},
		{"bad-then", `{"Mode": "raw", "Raw": [{"Then": "explode"}]}`, `config.Raw[0].Then`},
		{"bad-b64", `{"Mode": "raw", "Raw": [{"Send": [{"B64": "!!"}]}]}`, "config.Raw[0].Send[0]: B64"},
		{"b64-and-text", `{"Mode": "raw", "Raw": [{"Send": [{"B64": "AA==", "Text": "x"}]}]}`, "B64 and Text are both set"},
		{"bad-time", `{"Mode": "store", "Store": {"Articles": [{"Group": "a", "Date": "yesterday"}]}}`, "config.Store.Articles[0].Date"},
		{"empty-group", `{"Mode": "store", "Store": {"Groups": [{}]}}`, "Groups[0].Name is empty"},
		{"msgid-differs", `{"Mode": "store", "Store": {"Articles": [{"Group": "a", "MessageID": "<x@y>", "Headers": ["Message-ID: <z@y>"]}]}}`, "differs from Message-ID header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := launch(t, tc.cfg)
			if code := <-p.code; code != 2 || !strings.Contains(p.stderr.String(), tc.want) {
				t.Errorf("exit %d, want 2 with %q\nstderr: %s", code, tc.want, p.stderr.String())
			}
			if line, _ := p.out.ReadString('\n'); line != "" {
				t.Errorf("stdout %q", line)
			}
		})
	}
}

// TestUsage: arguments.
func TestUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no-arg", nil, 2, "exactly one argument"},
		{"two-args", []string{"a", "b"}, 2, "exactly one argument"},
		{"missing-file", []string{filepath.Join(t.TempDir(), "nope.json")}, 2, "no such file"},
		{"bad-flag", []string{"-nope"}, 2, "-nope"},
		{"help", []string{"-h"}, 0, "serves until stdin reaches end of file"},
		{"help-ready-keys", []string{"-h"}, 0, "{addr, host, port, ca, ca_pem}"},
		{"help-summary-keys", []string{"-h"}, 0, "server_names, max_open, closed}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(context.Background(), tc.args, strings.NewReader(""), &out, &errb); code != tc.code {
				t.Errorf("exit %d, want %d", code, tc.code)
			}
			if !strings.Contains(errb.String(), tc.want) || out.Len() != 0 {
				t.Errorf("stderr %q, stdout %q, want stderr with %q", errb.String(), out.String(), tc.want)
			}
		})
	}
}

// sample returns JSON that decodes into a value of type t, false for a type the decoder cannot
// set.
func sample(t reflect.Type) (string, bool) {
	switch t {
	case reflect.TypeFor[time.Duration]():
		return "1.5", true
	case reflect.TypeFor[time.Time]():
		return `"2020-01-02T03:04:05Z"`, true
	}
	switch t.Kind() {
	case reflect.Bool:
		return "true", true
	case reflect.String:
		return `"x"`, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "1", true
	case reflect.Float32, reflect.Float64:
		return "1.5", true
	case reflect.Map:
		v, ok := sample(t.Elem())
		return `{"K": ` + v + `}`, ok && t.Key().Kind() == reflect.String
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return `"AAE="`, true // []byte is base64
		}
		v, ok := sample(t.Elem())
		return `[` + v + `]`, ok
	case reflect.Pointer:
		if t.Elem() == reflect.TypeFor[tls.Config]() {
			return "true", true
		}
		return sample(t.Elem())
	case reflect.Struct:
		if !hasExported(t) {
			return "", false
		}
		var parts []string // every field that can be set, so a nested struct is exercised too
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				if v, ok := sample(f.Type); ok {
					parts = append(parts, `"`+f.Name+`": `+v)
				}
			}
		}
		return `{` + strings.Join(parts, ", ") + `}`, true
	}
	return "", false
}

func hasExported(t reflect.Type) bool {
	for i := range t.NumField() {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}

// TestEveryKitFieldDecodes: every exported field of the kit's option types that is data (not a
// function or a channel) can be set from JSON, so a field added to the kit works without a
// change here; a new field of a shape the decoder cannot read fails this test.
func TestEveryKitFieldDecodes(t *testing.T) {
	d := &decoder{tls: &tls.Config{}}
	for _, v := range []any{fakeserver.Options{}, fakeserver.StoreOptions{}, fakeserver.PeerOptions{}, fakeserver.Faults{}, fakeserver.StartTLSOptions{}, fakeserver.Step{}} {
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			js, ok := sample(f.Type)
			if !ok {
				if k := f.Type.Kind(); k != reflect.Func && k != reflect.Chan && f.Name != "Store" {
					t.Errorf("%s.%s (%s): not settable from JSON, and not a function or channel", typ.Name(), f.Name, f.Type)
				}
				continue
			}
			ptr := reflect.New(typ)
			if err := d.decode(ptr.Elem(), json.RawMessage(`{"`+f.Name+`": `+js+`}`), typ.Name()); err != nil {
				t.Errorf("%s.%s: %v", typ.Name(), f.Name, err)
				continue
			}
			if f.Type.Kind() != reflect.Struct && ptr.Elem().Field(i).IsZero() {
				t.Errorf("%s.%s stayed zero", typ.Name(), f.Name)
			}
		}
	}
}

// TestDurations: milliseconds, fractions allowed.
func TestDurations(t *testing.T) {
	var f fakeserver.Faults
	d := &decoder{}
	if err := d.decode(reflect.ValueOf(&f).Elem(), json.RawMessage(`{"Delay": {"A": 2500, "B": 0.5}, "Drip": {"C": 1}}`), "f"); err != nil {
		t.Fatal(err)
	}
	if f.Delay["A"] != 2500*time.Millisecond || f.Delay["B"] != 500*time.Microsecond || f.Drip["C"] != time.Millisecond {
		t.Errorf("%v %v", f.Delay, f.Drip)
	}
}

func b64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestBinarySafe: bytes that are not UTF-8 come back exactly in lines_b64 and articles_raw (the
// plain fields hold U+FFFD), and a Store serves them from HeadersB64 and BodyB64.
func TestBinarySafe(t *testing.T) {
	const art = "Subject: caf\xe9\r\n\r\n\xff\xfe\r\n..dot\r\n"
	p := startProc(t, `{"Mode": "peer"}`)
	c := p.dial()
	cmd(t, c, 335, "IHAVE <bin@x>")
	if err := c.Send(art + ".\r\n"); err != nil {
		t.Fatal(err)
	}
	if code, _, err := c.ReadStatus(); err != nil || code != 235 {
		t.Fatalf("%d %v", code, err)
	}
	c.Send("XYZ caf\xe9\r\n")
	cmd(t, c, 500, "DATE")
	s, _ := p.finish()
	if len(s.ArticlesRaw) != 1 || b64(t, s.ArticlesRaw[0]) != art {
		t.Errorf("articles_raw %q", s.ArticlesRaw)
	}
	if !strings.Contains(s.Articles[0][0], "\ufffd") {
		t.Errorf("the plain form is lossy by nature: %q", s.Articles[0])
	}
	if len(s.LinesB64) != len(s.Lines) || b64(t, s.LinesB64[1]) != "XYZ caf\xe9" {
		t.Errorf("lines_b64 %q lines %q", s.LinesB64, s.Lines)
	}

	// Store: bytes in, bytes out.
	p = startProc(t, `{"Mode": "store", "Store": {"Articles": [{"Group": "a.b", "Number": 1, "MessageID": "<s@x>",
	  "HeadersB64": ["`+base64.StdEncoding.EncodeToString([]byte("Subject: caf\xe9"))+`"],
	  "BodyB64": ["`+base64.StdEncoding.EncodeToString([]byte("\xff\xfe"))+`"]}]}}`)
	c = p.dial()
	cmd(t, c, 222, "BODY <s@x>")
	lines, err := c.TryReadDotLines()
	if err != nil || len(lines) != 1 || lines[0] != "\xff\xfe" {
		t.Errorf("BODY: %q %v", lines, err)
	}
	cmd(t, c, 221, "HEAD <s@x>")
	lines, _ = c.TryReadDotLines()
	if !slices.Contains(lines, "Subject: caf\xe9") {
		t.Errorf("HEAD: %q", lines)
	}
	p.finish()

	// Raw mode: the dot block as the bytes were sent.
	p = startProc(t, `{"Mode": "raw", "Raw": [{"Send": [{"Text": "200 hi\r\n"}]}, {"Expect": "POST", "ReadDot": true, "Send": [{"Text": "240 ok\r\n"}]}]}`)
	c = p.dial()
	c.Send("POST\r\n\xe9\xff\r\n.\r\n")
	if code, _, err := c.ReadStatus(); err != nil || code != 240 {
		t.Fatalf("%d %v", code, err)
	}
	s, _ = p.finish()
	if len(s.ArticlesRaw) != 1 || b64(t, s.ArticlesRaw[0]) != "\xe9\xff\r\n" {
		t.Errorf("raw articles_raw %q", s.ArticlesRaw)
	}
}

// TestRawBoundedByDeadline: a raw step that sleeps for a minute ends with the server, and a
// client that says nothing is let go after the Deadline.
func TestRawBoundedByDeadline(t *testing.T) {
	p := startProc(t, `{"Mode": "raw", "Options": {"Deadline": 600000}, "Raw": [{"Send": [{"Text": "200 hi\r\n"}]},
	  {"Expect": "X", "Sleep": 60000, "Send": [{"Text": "1 a", "Gap": 60000, "Split": 1, "SplitGap": 60000}]}]}`)
	c := p.dial()
	c.Send("X\r\n")
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	p.finish()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("shutdown took %v while a step slept", d)
	}

	p = startProc(t, `{"Mode": "raw", "Options": {"Deadline": 200}, "Raw": [{"Send": [{"Text": "200 hi\r\n"}]}, {"Expect": "X"}]}`)
	c = p.dial()
	if _, err := c.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("a silent client was not let go: %v", err)
	}
	s, _ := p.finish()
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "within the Deadline") {
		t.Errorf("notes %q", s.Notes)
	}
}

// TestRawLineLimit: a line over 1 MiB closes the connection and is noted, not held in memory.
func TestRawLineLimit(t *testing.T) {
	p := startProc(t, `{"Mode": "raw", "Raw": [{"Send": [{"Text": "200 hi\r\n"}]}, {"Expect": "X"}]}`)
	c := p.dial()
	go c.Send(strings.Repeat("a", maxLine+1<<16)) // the write may fail when the server closes
	if _, err := c.ReadLine(); err == nil {
		t.Error("the connection stayed open")
	}
	s, _ := p.finish()
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "a line over") || len(s.Lines) != 0 {
		t.Errorf("notes %q lines %d", s.Notes, len(s.Lines))
	}
}

// TestEmptySections: sections that a caller writes empty are fine in any mode.
func TestEmptySections(t *testing.T) {
	for _, cfg := range []string{
		`{"Mode": "raw", "Options": {"Faults": {"Delay": {}}}}`,
		`{"Mode": "store", "Raw": [], "PeerOptions": {"Have": {}}, "Options": {"Script": []}}`,
		`{"Mode": "peer", "Store": {"Groups": [], "Articles": []}, "Options": {"StoreOpts": {"OverviewFmt": []}}}`,
		`{"Mode": "store", "Options": {"Faults": {"OverrideLine": {"article 2": ["430 no"]}}}}`, // the kit normalises
	} {
		if _, err := parseConfig([]byte(cfg), &tls.Config{}); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{
		`{"Mode": "store", "Options": {"StoreOpts": {"StartTLS": {}}}, "Raw": [{}]}`,
		`{"Mode": "raw", "Options": {"StoreOpts": {"StartTLS": {}}}}`, // a StartTLS object is not empty
	} {
		if _, err := parseConfig([]byte(cfg), &tls.Config{}); err == nil {
			t.Errorf("%s: accepted", cfg)
		}
	}
}

// TestStartTLSConfigOff: StartTLS {} offers STARTTLS, "Config": false does not, and the
// certificate of the program is the default only when Config is not named.
func TestStartTLSConfigOff(t *testing.T) {
	for cfg, want := range map[string]int{
		`{"Mode": "store", "Options": {"StoreOpts": {"StartTLS": {}}}}`:                     382,
		`{"Mode": "store", "Options": {"StoreOpts": {"StartTLS": {"Config": true}}}}`:       382,
		`{"Mode": "store", "Options": {"StoreOpts": {"StartTLS": {"Config": false}}}}`:      500,
		`{"Mode": "peer", "PeerOptions": {"StartTLS": {"Config": false, "Refuse": "502"}}}`: 500,
	} {
		p := startProc(t, cfg)
		c := p.dial()
		if code, line, err := c.TryCmd("STARTTLS"); err != nil || code != want {
			t.Errorf("%s: STARTTLS %d %q %v, want %d", cfg, code, line, err, want)
		}
		p.finish()
	}
}

// TestCounters: max_open and closed, and ca_pem in the ready line.
func TestCounters(t *testing.T) {
	p := startProc(t, `{"Mode": "store"}`)
	pem, err := os.ReadFile(p.ready.CA)
	if err != nil || p.ready.CAPEM == "" || p.ready.CAPEM != string(pem) {
		t.Errorf("ca_pem %q, file %q, %v", p.ready.CAPEM, pem, err)
	}
	a, b := p.dial(), p.dial()
	cmd(t, a, 111, "DATE")
	cmd(t, b, 111, "DATE")
	a.Close()
	b.Close()
	s, _ := p.finish()
	if s.MaxOpen != 2 || s.Closed != 2 || s.Accepted != 2 {
		t.Errorf("max_open %d closed %d accepted %d", s.MaxOpen, s.Closed, s.Accepted)
	}
}

// TestIsEmpty: what counts as an empty section.
func TestIsEmpty(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{
		{fakeserver.Faults{}, true},
		{fakeserver.Faults{Delay: map[string]time.Duration{}}, true},
		{fakeserver.Faults{Delay: map[string]time.Duration{"A": 0}}, false},
		{fakeserver.StoreOptions{StartTLS: &fakeserver.StartTLSOptions{}}, false},
		{[]rawStep{}, true},
		{[]rawStep{{}}, false},
	} {
		if got := isEmpty(reflect.ValueOf(tc.v)); got != tc.want {
			t.Errorf("isEmpty(%#v) = %v", tc.v, got)
		}
	}
}

// TestBytesAreBase64: a []byte field takes a base64 string, as in encoding/json.
func TestBytesAreBase64(t *testing.T) {
	var v struct{ B []byte }
	d := &decoder{}
	if err := d.decode(reflect.ValueOf(&v).Elem(), json.RawMessage(`{"B": "AAEC"}`), "v"); err != nil || !bytes.Equal(v.B, []byte{0, 1, 2}) {
		t.Errorf("%v %v", v.B, err)
	}
	if err := d.decode(reflect.ValueOf(&v).Elem(), json.RawMessage(`{"B": [0, 1]}`), "v"); err == nil {
		t.Error("an array was taken for bytes")
	}
}

// TestMain lets TestSignal run the real main in a child process.
func TestMain(m *testing.M) {
	if cfg := os.Getenv("NNTP_FAKESERVER_TEST_CONFIG"); cfg != "" {
		os.Args = []string{"nntp-fakeserver", cfg}
		main()
		return
	}
	os.Exit(m.Run())
}

// TestMainSignal: the real main, as a process: SIGTERM ends the server with its summary and exit
// status 0, and so does EOF on stdin.
func TestMainSignal(t *testing.T) {
	for _, how := range []string{"SIGTERM", "SIGINT", "EOF"} {
		t.Run(how, func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "NNTP_FAKESERVER_TEST_CONFIG="+writeConfig(t, `{"Mode": "store", "Options": {"RawStall": true}}`))
			in, _ := cmd.StdinPipe()
			out, _ := cmd.StdoutPipe()
			var errb bytes.Buffer
			cmd.Stderr = &errb
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(out)
			if _, err := br.ReadString('\n'); err != nil {
				t.Fatalf("%v: %s", err, errb.String())
			}
			switch how {
			case "SIGTERM":
				cmd.Process.Signal(syscall.SIGTERM)
			case "SIGINT":
				cmd.Process.Signal(syscall.SIGINT)
			default:
				in.Close()
			}
			line, err := br.ReadString('\n')
			if err != nil || !strings.Contains(line, `"accepted":0`) {
				t.Errorf("summary %q %v", line, err)
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("exit: %v %s", err, errb.String())
			}
		})
	}
}

// TestByteFaults: the byte-level Faults (Split, Trailing, DropAfterBytes, Abort, OverrideFinal,
// DelayFinal, DropInUpload) come through with their nested fields and durations in ms.
func TestByteFaults(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"Mode": "store", "Options": {"Faults": {
	  "Split": {"DATE": {"At": [1, 5], "Size": 3, "Gap": 2.5}},
	  "Trailing": {"DATE": {"Lines": ["111 x", "."], "Gap": 40}},
	  "DropAfterBytes": {"GROUP": 4}, "Abort": {"GROUP": true},
	  "OverrideFinal": {"POST": ["441 no"]}, "DelayFinal": {"POST": 15}, "DropInUpload": {"POST": 2}}}}`), &tls.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Options.Faults
	if sp := f.Split["DATE"]; !slices.Equal(sp.At, []int{1, 5}) || sp.Size != 3 || sp.Gap != 2500*time.Microsecond {
		t.Errorf("Split %+v", sp)
	}
	if tr := f.Trailing["DATE"]; !slices.Equal(tr.Lines, []string{"111 x", "."}) || tr.Gap != 40*time.Millisecond {
		t.Errorf("Trailing %+v", tr)
	}
	if f.DropAfterBytes["GROUP"] != 4 || !f.Abort["GROUP"] || f.OverrideFinal["POST"][0] != "441 no" ||
		f.DelayFinal["POST"] != 15*time.Millisecond || f.DropInUpload["POST"] != 2 {
		t.Errorf("faults %+v", f)
	}

	// End to end: the status line of DATE in two writes with a pause, then the trailing line.
	p := startProc(t, `{"Mode": "store", "Options": {"Faults": {
	  "Split": {"DATE": {"At": [3], "Gap": 100}}, "Trailing": {"DATE": {"Lines": ["TRAILER"], "Gap": 50}}}}}`)
	nc, err := net.DialTimeout("tcp", p.ready.Addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	br := bufio.NewReader(nc)
	br.ReadString('\n')
	io.WriteString(nc, "DATE\r\n")
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []string
	for range 2 {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.TrimSpace(l))
	}
	if !strings.HasPrefix(got[0], "111 ") || got[1] != "TRAILER" {
		t.Errorf("got %q", got)
	}
	p.finish()
}
