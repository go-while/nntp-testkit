package fakeserver

import (
	"compress/flate"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// COMPRESS DEFLATE (RFC 8054) in Store, Peer and Script mode, with the strict client.

// bigBody returns n pseudo-random lines of 76 base64-like bytes (little to compress), every
// 100th starting with "." so it is dot-stuffed on the wire.
func bigBody(n int) []string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	r := rand.New(rand.NewPCG(1, 2))
	out := make([]string, n)
	b := make([]byte, 76)
	for i := range out {
		for j := range b {
			b[j] = alpha[r.IntN(len(alpha))]
		}
		if i%100 == 0 {
			b[0] = '.'
		}
		out[i] = string(b)
	}
	return out
}

// stuff dot-stuffs lines for the wire.
func stuff(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.HasPrefix(l, ".") {
			l = "." + l
		}
		out[i] = l
	}
	return out
}

func hasCap(caps []string, c string) bool { return slices.Contains(caps, c) }

func TestStoreCompress(t *testing.T) {
	st := testStore()
	s, c := storeServer(t, st, StoreOptions{Compress: true}, Faults{})
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	if !hasCap(caps, "COMPRESS DEFLATE") {
		t.Fatalf("caps before COMPRESS %q", caps)
	}
	_, help := wantBlock(t, c, "HELP", 100)
	if !hasCap(help, "COMPRESS") {
		t.Errorf("HELP %q", help)
	}
	c.Compress()
	if !c.Compressed() {
		t.Fatal("client not compressed")
	}
	_, caps = wantBlock(t, c, "CAPABILITIES", 101)
	if hasCap(caps, "COMPRESS DEFLATE") || !hasCap(caps, "READER") {
		t.Fatalf("caps after COMPRESS %q", caps)
	}

	// RFC 8054 2.2.2: a syntactically valid COMPRESS while active is 502, any algorithm.
	if line := wantCode(t, c, "COMPRESS DEFLATE", 502); line != "502 Compression already active" {
		t.Errorf("second COMPRESS %q", line)
	}
	wantCode(t, c, "COMPRESS SHRINK", 502)
	wantCode(t, c, "COMPRESS deflate", 501)
	wantCode(t, c, "AUTHINFO USER u", 502)

	wantCode(t, c, "GROUP alt.test", 211)
	_, art := wantBlock(t, c, "ARTICLE 1", 220)
	if got := art[len(art)-3:]; !reflect.DeepEqual(got, []string{"line 1", ".dot line", ".."}) {
		t.Errorf("dot-stuffed body after un-stuffing %q", got)
	}
	_, over := wantBlock(t, c, "OVER 1-3", 224)
	if len(over) != 3 {
		t.Errorf("OVER %q", over)
	}

	// pipelined, in one Send and one flush
	if err := c.Send("STAT 1\r\nSTAT 2\r\nSTAT 9\r\nDATE\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{223, 223, 423, 111} {
		wantStatus(t, c, want)
	}

	// a large article both ways: more than the flate window (32 KiB) and the bufio buffers
	// of the server (64 KiB) and the client (1 MiB)
	body := bigBody(40000) // about 3 MiB
	head := []string{"Newsgroups: alt.test", "From: big@x", "Subject: big", "Message-ID: <big@x>"}
	wantCode(t, c, "POST", 340)
	wire := append(append(append([]string{}, head...), ""), stuff(body)...)
	if err := c.Send(strings.Join(wire, "\r\n") + "\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 240)
	_, got := wantBlock(t, c, "BODY <big@x>", 222)
	if !reflect.DeepEqual(got, body) {
		t.Fatalf("BODY of the big article: %d lines, want %d", len(got), len(body))
	}
	raw := s.Articles()
	if len(raw) != 1 || !reflect.DeepEqual(raw[0], wire) {
		t.Fatalf("Articles() did not record the inflated, still stuffed article")
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("after the reply: %v", err)
	}
	wantCode(t, c, "QUIT", 205)
	wantEOF(t, c)
	s.WaitOpen(t, 0, 2*time.Second)

	want := map[string]int{"CAPABILITIES": 2, "HELP": 1, "COMPRESS": 4, "AUTHINFO": 1, "GROUP": 1, "ARTICLE": 1,
		"OVER": 1, "STAT": 3, "DATE": 1, "POST": 1, "BODY": 1, "QUIT": 1}
	wantEqual(t, "Counts", s.Counts(), want)
	if l := s.Lines(); l[2] != "COMPRESS DEFLATE" || l[3] != "CAPABILITIES" || l[len(l)-1] != "QUIT" {
		t.Errorf("Lines() %q", l)
	}
}

// TestCompressReplies: the answers to COMPRESS before compression is on.
func TestCompressReplies(t *testing.T) {
	cases := []struct {
		name string
		so   StoreOptions
		cmd  string
		want string
	}{
		{"off", StoreOptions{}, "COMPRESS DEFLATE", "500 Unknown command"},
		{"no algorithm", StoreOptions{Compress: true}, "COMPRESS", "501 Syntax error"},
		{"two", StoreOptions{Compress: true}, "COMPRESS DEFLATE X", "501 Syntax error"},
		{"lower case", StoreOptions{Compress: true}, "COMPRESS deflate", "501 Syntax error in compression algorithm"},
		{"bad char", StoreOptions{Compress: true}, "COMPRESS DE.FLATE", "501 Syntax error in compression algorithm"},
		{"too long", StoreOptions{Compress: true}, "COMPRESS " + strings.Repeat("A", 21), "501 Syntax error in compression algorithm"},
		{"unsupported", StoreOptions{Compress: true}, "COMPRESS SHRINK", "503 Compression algorithm not supported"},
		{"OFF", StoreOptions{Compress: true}, "COMPRESS OFF", "503 Compression algorithm not supported"},
		{"before auth", StoreOptions{Compress: true, User: "u", Pass: "p"}, "COMPRESS DEFLATE", "480 Authentication required"},
		{"lower-case command", StoreOptions{Compress: true}, "compress DEFLATE", "206 Compression active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := storeServer(t, testStore(), tc.so, Faults{})
			code, line := c.Cmd("%s", tc.cmd)
			if line != tc.want {
				t.Fatalf("%q: %q, want %q", tc.cmd, line, tc.want)
			}
			if code == 206 {
				if err := c.StartCompress(); err != nil {
					t.Fatal(err)
				}
			}
			wantCode(t, c, "DATE", 111) // the connection goes on, compressed only after 206
		})
	}

	// with authentication: COMPRESS DEFLATE is offered once logged in
	_, c := storeServer(t, testStore(), StoreOptions{Compress: true, User: "u", Pass: "p"}, Faults{})
	if _, caps := wantBlock(t, c, "CAPABILITIES", 101); hasCap(caps, "COMPRESS DEFLATE") {
		t.Errorf("COMPRESS offered before auth: %q", caps)
	}
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	if _, caps := wantBlock(t, c, "CAPABILITIES", 101); !hasCap(caps, "COMPRESS DEFLATE") {
		t.Errorf("COMPRESS not offered after auth: %q", caps)
	}
	c.Compress()
	wantCode(t, c, "AUTHINFO USER u", 502)
	wantCode(t, c, "GROUP alt.test", 211)
}

// TestCompressFaults: the faults work on a compressed connection; each write is deflated
// and flushed.
func TestCompressFaults(t *testing.T) {
	opts := StoreOptions{Compress: true}
	t.Run("Delay", func(t *testing.T) {
		_, c := storeServer(t, testStore(), opts, Faults{Delay: map[string]time.Duration{"DATE": 50 * time.Millisecond}})
		c.Compress()
		start := time.Now()
		wantCode(t, c, "DATE", 111)
		if took := time.Since(start); took < 50*time.Millisecond {
			t.Errorf("delayed DATE took %v", took)
		}
	})
	t.Run("OverrideCompress", func(t *testing.T) {
		_, c := storeServer(t, testStore(), opts, Faults{Override: map[string][]string{"COMPRESS": {"403 Unable to activate compression"}}})
		if code, _, err := c.TryCompress(); code != 403 || err == nil || c.Compressed() {
			t.Fatalf("TryCompress = %d %v", code, err)
		}
		wantCode(t, c, "DATE", 111) // uncompressed: the 403 did not start compression
	})
	t.Run("OverrideLine", func(t *testing.T) {
		_, c := storeServer(t, testStore(), opts, Faults{OverrideLine: map[string][]string{"STAT 2": {"430 gone"}}})
		c.Compress()
		wantCode(t, c, "GROUP alt.test", 211)
		wantCode(t, c, "STAT 1", 223)
		wantCode(t, c, "STAT 2", 430)
	})
	t.Run("Drip", func(t *testing.T) {
		_, c := storeServer(t, testStore(), opts, Faults{Drip: map[string]time.Duration{"DATE": 5 * time.Millisecond}})
		c.Compress()
		start := time.Now()
		if line := wantCode(t, c, "DATE", 111); len(line) != 18 {
			t.Errorf("DATE %q", line)
		}
		if took := time.Since(start); took < 80*time.Millisecond {
			t.Errorf("dripped DATE took %v", took)
		}
	})
	t.Run("LineDelay", func(t *testing.T) {
		_, c := storeServer(t, testStore(), opts, Faults{LineDelay: map[string]time.Duration{"LIST": 20 * time.Millisecond}})
		c.Compress()
		start := time.Now()
		_, l := wantBlock(t, c, "LIST", 215)
		if took := time.Since(start); len(l) != 3 || took < 80*time.Millisecond {
			t.Errorf("LIST %q in %v", l, took)
		}
	})
	t.Run("LongLine", func(t *testing.T) {
		const n = 2 << 20
		_, c := storeServer(t, testStore(), opts, Faults{LongLine: map[string]int{"ARTICLE": n}})
		c.Compress()
		wantCode(t, c, "GROUP alt.test", 211)
		if _, l := wantBlock(t, c, "ARTICLE 1", 220); len(l[0]) != n || l[1] != "Subject: one" {
			t.Errorf("long line %d bytes, then %q", len(l[0]), l[1])
		}
	})
	t.Run("DropInMultiline", func(t *testing.T) {
		s, c := storeServer(t, testStore(), opts, Faults{DropInMultiline: map[string]int{"ARTICLE": 3}})
		c.Compress()
		wantCode(t, c, "GROUP alt.test", 211)
		wantCode(t, c, "ARTICLE 1", 220)
		lines, err := c.TryReadDotLines()
		var se *client.StrictError
		if !errors.As(err, &se) || se.Kind != client.NoTerminator || !errors.Is(err, io.EOF) {
			t.Fatalf("cut reply: %v, want NoTerminator at EOF", err)
		}
		wantEqual(t, "lines before the drop", lines, []string{"Subject: one", "From: a@x"})
		s.WaitOpen(t, 0, 2*time.Second)
	})
	t.Run("CloseAfter", func(t *testing.T) {
		_, c := storeServer(t, testStore(), opts, Faults{CloseAfter: 2})
		c.Compress()
		wantCode(t, c, "DATE", 111)
		wantEOF(t, c)
	})
	t.Run("Endless", func(t *testing.T) {
		s, c := storeServer(t, testStore(), opts, Faults{Endless: map[string]bool{"DATE": true}})
		c.Compress()
		wantCode(t, c, "DATE", 111)
		for i := 1; i <= 5000; i++ {
			if l, err := c.ReadLine(); err != nil || l != fmt.Sprintf("%d endless", i) {
				t.Fatalf("filler %d: %q %v", i, l, err)
			}
		}
		c.Close()
		s.WaitOpen(t, 0, 5*time.Second)
	})
}

func TestPeerCompress(t *testing.T) {
	var log peerLog
	o := &PeerOptions{Compress: true, User: "u", Pass: "p", OnArticle: log.onArticle}
	s := New(t, Options{Handler: PeerHandler(o)})
	c := client.Dial(t, s.Addr())
	wantCode(t, c, "COMPRESS DEFLATE", 480)
	if _, caps := wantBlock(t, c, "CAPABILITIES", 101); hasCap(caps, "COMPRESS DEFLATE") {
		t.Errorf("COMPRESS offered before auth: %q", caps)
	}
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	if _, caps := wantBlock(t, c, "CAPABILITIES", 101); !hasCap(caps, "COMPRESS DEFLATE") {
		t.Fatalf("caps %q", caps)
	}
	wantCode(t, c, "COMPRESS SHRINK", 503)
	wantCode(t, c, "COMPRESS", 501)
	c.Compress()
	if _, caps := wantBlock(t, c, "CAPABILITIES", 101); hasCap(caps, "COMPRESS DEFLATE") || !hasCap(caps, "STREAMING") {
		t.Fatalf("caps after COMPRESS %q", caps)
	}
	wantCode(t, c, "COMPRESS DEFLATE", 502)
	wantCode(t, c, "AUTHINFO USER u", 502)
	wantCode(t, c, "MODE STREAM", 203)

	stuffed := []string{"Message-ID: <z1@x>", "Subject: s", "", "..leading dot", "body"}
	big := append([]string{"Message-ID: <z2@x>", ""}, stuff(bigBody(5000))...)
	// pipelined: CHECK, TAKETHIS with its article, CHECK, in one Send
	if err := c.Send("CHECK <z1@x>\r\nTAKETHIS <z1@x>\r\n" + strings.Join(stuffed, "\r\n") + "\r\n.\r\nCHECK <z1@x>\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 238)
	wantStatus(t, c, 239)
	wantStatus(t, c, 438)
	wantCode(t, c, "IHAVE <z2@x>", 335)
	sendArticle(t, c, big)
	wantStatus(t, c, 235)
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c, "QUIT", 205)
	s.WaitOpen(t, 0, 2*time.Second)

	ids, wire := log.got()
	wantEqual(t, "ids", ids, []string{"<z1@x>", "<z2@x>"})
	if !reflect.DeepEqual(wire[0], stuffed) || !reflect.DeepEqual(wire[1], big) {
		t.Error("OnArticle did not get the inflated wire lines")
	}
	if arts := s.Articles(); len(arts) != 2 || !reflect.DeepEqual(arts[1], big) {
		t.Errorf("Articles() %d", len(arts))
	}
	wantEqual(t, "Counts", s.Counts(), map[string]int{"COMPRESS": 5, "CAPABILITIES": 3, "AUTHINFO": 3,
		"MODE": 1, "CHECK": 2, "TAKETHIS": 1, "IHAVE": 1, "QUIT": 1})

	// without Compress, COMPRESS is unknown
	s2 := New(t, Options{Handler: PeerHandler(&PeerOptions{})})
	c2 := client.Dial(t, s2.Addr())
	wantCode(t, c2, "COMPRESS DEFLATE", 500)
}

// TestScriptCompress: a Step with Compress switches the connection after its reply; Lines and
// Mismatches hold the inflated lines.
func TestScriptCompress(t *testing.T) {
	s := New(t, Options{Script: []Step{
		{Expect: "COMPRESS DEFLATE", Reply: []string{"206 Compression active"}, Compress: true},
		{Expect: "DATE", Reply: []string{"111 20260102030405"}},
		{Expect: "QUIT", Reply: []string{"205 bye"}, CloseAfter: true},
	}})
	c := client.Dial(t, s.Addr())
	c.Compress()
	wantCode(t, c, "FOO", 500)
	wantCode(t, c, "DATE", 111)
	wantCode(t, c, "QUIT", 205)
	wantEOF(t, c)
	wantEqual(t, "Lines", s.Lines(), []string{"COMPRESS DEFLATE", "FOO", "DATE", "QUIT"})
	wantEqual(t, "Mismatches", s.Mismatches(), []string{"FOO"})
}

// TestCompressBrokenHandler: wire mistakes and corruption written by a Handler after the 206
// reach the strict client through the compressed stream.
func TestCompressBrokenHandler(t *testing.T) {
	cases := []struct {
		name  string
		write func(c *Conn, raw io.Writer)
		check func(t *testing.T, c *client.Conn)
	}{
		{"bare LF", func(c *Conn, _ io.Writer) { _, _ = c.Raw().Write([]byte("111 bare\n")) },
			func(t *testing.T, c *client.Conn) {
				_, _, err := c.TryCmd("DATE")
				var se *client.StrictError
				if !errors.As(err, &se) || se.Kind != client.BareLF || se.Line != "111 bare\n" {
					t.Fatalf("err %v, want BareLF", err)
				}
			}},
		{"bad stuffing", func(c *Conn, _ io.Writer) { _ = c.WriteLines("101 caps", ".bad", ".") },
			func(t *testing.T, c *client.Conn) {
				wantCode(t, c, "DATE", 101)
				_, err := c.TryReadDotLines()
				var se *client.StrictError
				if !errors.As(err, &se) || se.Kind != client.BadDotStuffing {
					t.Fatalf("err %v, want BadDotStuffing", err)
				}
			}},
		{"corrupt", func(_ *Conn, raw io.Writer) { _, _ = raw.Write([]byte{0xff, 0xff, 0xff, 0xff}) },
			func(t *testing.T, c *client.Conn) {
				_, _, err := c.TryCmd("DATE")
				var ce flate.CorruptInputError
				if !errors.As(err, &ce) {
					t.Fatalf("err %v (%T), want a flate.CorruptInputError", err, err)
				}
			}},
		{"stops mid-reply", func(c *Conn, _ io.Writer) { _ = c.WriteLines("224 overview", "1\tpartial") },
			func(t *testing.T, c *client.Conn) {
				c.Deadline = 200 * time.Millisecond
				wantCode(t, c, "DATE", 224)
				_, err := c.TryReadDotLines()
				var se *client.StrictError
				if !errors.As(err, &se) || se.Kind != client.NoTerminator || !isTimeout(err) {
					t.Fatalf("err %v, want NoTerminator after a timeout", err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(t, Options{Handler: func(c *Conn) {
				if line, err := c.ReadLine(); err != nil || line != "COMPRESS DEFLATE" {
					return
				}
				raw := c.Raw()
				if c.WriteLines("206 Compression active") != nil || c.StartCompress() != nil {
					return
				}
				if c.StartCompress() == nil {
					panic("second StartCompress succeeded")
				}
				if _, err := c.ReadLine(); err != nil {
					return
				}
				tc.write(c, raw)
				c.drain()
			}})
			c := client.Dial(t, s.Addr())
			c.Compress()
			tc.check(t, c)
		})
	}
}

// isTimeout reports whether err wraps a net timeout.
func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// TestCompressCorruptInput: compressed input the server cannot inflate makes it close the
// connection (RFC 8054 2.2.2).
func TestCompressCorruptInput(t *testing.T) {
	s, c := storeServer(t, testStore(), StoreOptions{Compress: true}, Faults{})
	raw := c.Conn
	c.Compress()
	wantCode(t, c, "DATE", 111)
	if _, err := raw.Write([]byte("DATE\r\n")); err != nil { // not DEFLATE
		t.Fatal(err)
	}
	wantEOF(t, c)
	s.WaitOpen(t, 0, 2*time.Second)
	wantEqual(t, "Lines", s.Lines(), []string{"COMPRESS DEFLATE", "DATE"})
}

// TestCompressOutputOnly: a server that deflates its output but never inflates its input, as
// a half implementation does. The strict client compresses its commands as the RFC says; the
// server reads the deflated bytes as they are, so the command never arrives: no answer
// (deflated bytes without a line end) or a 500 for a binary line (Go's deflater stores a
// short command uncompressed, behind a block header).
func TestCompressOutputOnly(t *testing.T) {
	for _, mode := range []string{"store", "peer"} {
		t.Run(mode, func(t *testing.T) {
			o := Options{Store: testStore(), StoreOpts: StoreOptions{CompressOutputOnly: true}}
			if mode == "peer" {
				o = Options{Handler: PeerHandler(&PeerOptions{CompressOutputOnly: true})}
			}
			s := New(t, o)
			c := client.Dial(t, s.Addr())
			if _, caps := wantBlock(t, c, "CAPABILITIES", 101); !hasCap(caps, "COMPRESS DEFLATE") {
				t.Fatalf("caps %q", caps)
			}
			c.Compress()
			c.Deadline = 300 * time.Millisecond
			code, line, err := c.TryCmd("DATE")
			if !(isTimeout(err) || (err == nil && code == 500)) {
				t.Fatalf("DATE on a server that does not inflate: %d %q %v, want a timeout or 500", code, line, err)
			}
			if l := s.Lines(); len(l) < 2 || slices.Contains(l, "DATE") {
				t.Fatalf("Lines() %q: the server must not have read DATE", l)
			}
		})
	}
}

// TestCompressTLS: over TLS the compression layer sits above it (RFC 8054 2.2.2: compress
// first, then TLS); crypto/tls has no TLS-level compression, so COMPRESS is offered.
func TestCompressTLS(t *testing.T) {
	srvCfg, roots := TLSConfig(t)
	s := New(t, Options{TLS: srvCfg, Store: testStore(), StoreOpts: StoreOptions{Compress: true}})
	tc := tls.Client(rawDial(t, s), &tls.Config{RootCAs: roots, ServerName: s.Host(), MinVersion: tls.VersionTLS12})
	c := client.New(t, tc)
	if _, err := c.ReadGreeting(); err != nil {
		t.Fatal(err)
	}
	c.Compress()
	wantCode(t, c, "GROUP alt.test", 211)
	if _, body := wantBlock(t, c, "BODY 1", 222); !reflect.DeepEqual(body, []string{"line 1", ".dot line", ".."}) {
		t.Errorf("BODY over COMPRESS and TLS %q", body)
	}
	wantCode(t, c, "STARTTLS", 500) // not offered by the store handler
}
