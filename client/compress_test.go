package client

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// zserver is the server side of a peer connection after COMPRESS DEFLATE: plain and deflate
// collect bytes in out, send writes them in one Write (so bytes after the 206 line arrive with
// it), zr is the inflated client stream.
type zserver struct {
	p   *peer
	c   net.Conn
	out bytes.Buffer
	zw  *flate.Writer
	zr  *bufio.Reader
}

func (z *zserver) plain(s string) { z.out.WriteString(s) }

// deflate compresses s and sync-flushes it into out.
func (z *zserver) deflate(s string) {
	_, _ = z.zw.Write([]byte(s))
	_ = z.zw.Flush()
}

func (z *zserver) send() {
	_, _ = z.c.Write(z.out.Bytes())
	z.out.Reset()
}

// readLine reads one inflated client line and records it.
func (z *zserver) readLine() (string, bool) { return z.p.readLine(z.zr) }

// compressing returns a peer handler: greeting, then it waits for "COMPRESS DEFLATE" and runs
// fn with the compressed server side; fn writes the 206 itself (z.plain).
func compressing(fn func(z *zserver)) func(*peer, net.Conn, *bufio.Reader) {
	return func(p *peer, c net.Conn, r *bufio.Reader) {
		write(c, "200 hi\r\n")
		if line, ok := p.readLine(r); !ok || line != "COMPRESS DEFLATE" {
			return
		}
		z := &zserver{p: p, c: c, zr: bufio.NewReader(flate.NewReader(r))}
		z.zw, _ = flate.NewWriter(&z.out, flate.BestSpeed)
		fn(z)
	}
}

// TestCompressBufferedAfter206: the first compressed bytes come in the same segment as the
// 206 line and are inflated; the client's commands arrive deflated and flushed.
func TestCompressBufferedAfter206(t *testing.T) {
	p := newPeer(t, compressing(func(z *zserver) {
		z.plain("206 Compression active\r\n")
		z.deflate("200 first\r\n")
		z.send()
		for i := 0; i < 2; i++ {
			if _, ok := z.readLine(); !ok {
				return
			}
			z.deflate("111 20261023000000\r\n")
			z.send()
		}
		drain(z.zr)
	}))
	c := Dial(t, p.addr())
	if code, _ := c.Cmd("COMPRESS DEFLATE"); code != 206 {
		t.Fatalf("COMPRESS = %d", code)
	}
	if c.R.Buffered() == 0 {
		t.Fatal("no compressed bytes buffered after the 206 line")
	}
	if c.Compressed() {
		t.Fatal("Compressed before StartCompress")
	}
	if err := c.StartCompress(); err != nil {
		t.Fatal(err)
	}
	if !c.Compressed() {
		t.Fatal("Compressed false after StartCompress")
	}
	if err := c.StartCompress(); err == nil {
		t.Fatal("a second StartCompress must fail")
	}
	if _, line, err := c.ReadStatus(); err != nil || line != "200 first" {
		t.Fatalf("buffered reply %q %v", line, err)
	}
	if err := c.Send("DATE\r\nDATE\r\n"); err != nil { // pipelined, one flush
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if code, _, err := c.ReadStatus(); err != nil || code != 111 {
			t.Fatalf("DATE %d: %d %v", i, code, err)
		}
	}
	c.Close()
	if err := ExpectLines(p.lines(), []string{"COMPRESS DEFLATE", "DATE", "DATE"}); err != nil {
		t.Fatal(err)
	}
}

func TestCompressRefused(t *testing.T) {
	for _, reply := range []string{
		"403 Unable to activate compression",
		"500 Unknown command",
		"501 Syntax error",
		"502 Compression already active",
		"503 Compression algorithm not supported",
	} {
		t.Run(reply[:3], func(t *testing.T) {
			p := newPeer(t, func(p *peer, c net.Conn, r *bufio.Reader) {
				write(c, "200 hi\r\n")
				for {
					line, ok := p.readLine(r)
					if !ok {
						return
					}
					if line == "DATE" {
						write(c, "111 20261023000000\r\n")
						continue
					}
					write(c, reply+"\r\n")
				}
			})
			c := Dial(t, p.addr())
			code, line, err := c.TryCompress()
			if err == nil || line != reply || code == 206 || !strings.Contains(err.Error(), reply) {
				t.Fatalf("TryCompress = %d %q %v", code, line, err)
			}
			if c.Compressed() {
				t.Fatal("compressed after a refusal")
			}
			if code, _ := c.Cmd("DATE"); code != 111 { // still plain
				t.Fatalf("DATE = %d", code)
			}
			r := &recordTB{TB: t}
			c.tb = r
			r.run(func(testing.TB) { c.Compress() })
			if !r.failed || !strings.Contains(r.msg, reply) {
				t.Fatalf("Compress: failed=%v msg=%q", r.failed, r.msg)
			}
		})
	}
}

// TestCompressPending: an empty sync-flush block inflates to nothing: ExpectNoPending waits
// its duration and returns nil; compressed bytes that arrive later are Pending and stay
// readable.
func TestCompressPending(t *testing.T) {
	gate := make(chan struct{})
	p := newPeer(t, compressing(func(z *zserver) {
		z.plain("206 Compression active\r\n")
		z.send()
		if _, ok := z.readLine(); !ok {
			return
		}
		z.deflate("111 20261023000000\r\n")
		n := z.out.Len()
		_ = z.zw.Flush() // an empty block: bytes on the wire, nothing inflated
		if z.out.Len() == n {
			panic("empty flush wrote nothing")
		}
		z.send()
		<-gate
		z.deflate("STRAY\r\n")
		z.send()
		drain(z.zr)
	}))
	c := Dial(t, p.addr())
	c.Compress()
	if code, _ := c.Cmd("DATE"); code != 111 {
		t.Fatalf("DATE = %d", code)
	}
	const d = 150 * time.Millisecond
	start := time.Now()
	if err := c.ExpectNoPending(d); err != nil {
		t.Fatalf("quiet: %v", err)
	}
	if el := time.Since(start); el < d || el > d+testkit.Deadline()/2 {
		t.Fatalf("ExpectNoPending took %v for %v", el, d)
	}
	close(gate)
	se := strictKind(t, c.ExpectNoPending(testkit.Deadline()), Pending)
	if len(se.Bytes) == 0 || !strings.HasPrefix("STRAY\r\n", string(se.Bytes)) {
		t.Fatalf("Bytes = %q", se.Bytes)
	}
	if line, err := c.ReadLine(); err != nil || line != "STRAY" {
		t.Fatalf("after Pending: %q %v (bytes consumed?)", line, err)
	}
}

// TestCompressStrict: wire mistakes inside the compressed stream are found as without it.
func TestCompressStrict(t *testing.T) {
	cases := []struct {
		name   string
		script string
		close  bool
		op     string
		want   Kind
		line   string
	}{
		{"bare LF", "211 x\n", false, "line", BareLF, "211 x\n"},
		{"bare CR", "211 a\rb\r\n", false, "line", BareCR, "211 a\rb\r\n"},
		{"no CRLF at EOF", "211 partial", true, "line", NoCRLF, "211 partial"},
		{"bad stuffing", "x\r\n.bad\r\n.\r\n", false, "dot", BadDotStuffing, ".bad\r\n"},
		{"missing terminator", "a\r\nb\r\n", true, "dot", NoTerminator, ""},
		{"raw bare LF", "a\r\nb\n.\r\n", false, "raw", BareLF, "b\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPeer(t, compressing(func(z *zserver) {
				z.plain("206 Compression active\r\n")
				z.deflate(tc.script)
				z.send()
				if !tc.close {
					drain(z.zr)
				}
			}))
			c := Dial(t, p.addr())
			c.Compress()
			var err error
			switch tc.op {
			case "line":
				_, err = c.ReadLine()
			case "dot":
				_, err = c.TryReadDotLines()
			case "raw":
				_, err = c.TryRawUntilDot()
			}
			se := strictKind(t, err, tc.want)
			if tc.line != "" && se.Line != tc.line {
				t.Fatalf("Line = %q, want %q", se.Line, tc.line)
			}
			if tc.close && !errors.Is(err, io.EOF) {
				t.Fatalf("err %v must wrap io.EOF", err)
			}
		})
	}
}

// TestCompressCorrupt: bytes that are not DEFLATE surface as a read error wrapping the flate
// error, after the lines inflated before them.
func TestCompressCorrupt(t *testing.T) {
	p := newPeer(t, compressing(func(z *zserver) {
		z.plain("206 Compression active\r\n")
		z.deflate("200 still fine\r\n")
		z.plain("\xff\xff\xff\xffnot deflate\r\n")
		z.send()
		drain(z.zr)
	}))
	c := Dial(t, p.addr())
	c.Compress()
	if line, err := c.ReadLine(); err != nil || line != "200 still fine" {
		t.Fatalf("first line %q %v", line, err)
	}
	_, err := c.ReadLine()
	var ce flate.CorruptInputError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "corrupt compressed stream") {
		t.Fatalf("err = %v (%T), want a flate.CorruptInputError", err, err)
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); !errors.As(err, &ce) {
		t.Fatalf("ExpectNoPending after corruption = %v", err)
	}
}

// TestCompressDeadline: a server that stops in the middle of a compressed reply hits the
// hang guard; the timeout loses nothing, the rest is read once it comes.
func TestCompressDeadline(t *testing.T) {
	gate := make(chan struct{})
	p := newPeer(t, compressing(func(z *zserver) {
		z.plain("206 Compression active\r\n")
		z.deflate("220 ok\r\na\r\n")
		z.send()
		<-gate
		z.deflate("b\r\n.\r\n")
		z.send()
		drain(z.zr)
	}))
	c := Dial(t, p.addr())
	c.Compress()
	c.Deadline = 200 * time.Millisecond
	if code, _, err := c.ReadStatus(); err != nil || code != 220 {
		t.Fatalf("status %d %v", code, err)
	}
	got, err := c.TryReadDotLines()
	strictKind(t, err, NoTerminator)
	if !isTimeout(err) || len(got) != 1 {
		t.Fatalf("err = %v lines %q, want a timeout after one line", err, got)
	}
	close(gate)
	c.Deadline = 0
	if got := c.ReadDotLines(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("after the timeout: %q", got)
	}
	// Writes keep their deadline too: a write to a closed socket fails at once.
	c.Close()
	if err := c.Send("QUIT\r\n"); err == nil {
		t.Fatal("Send after Close must fail")
	}
}

// TestCompressEOF: a server that closes after a flushed reply reads as io.EOF.
func TestCompressEOF(t *testing.T) {
	p := newPeer(t, compressing(func(z *zserver) {
		z.plain("206 Compression active\r\n")
		z.send()
		if _, ok := z.readLine(); ok {
			z.deflate("205 bye\r\n")
			z.send()
		}
	}))
	c := Dial(t, p.addr())
	c.Compress()
	if code, _ := c.Cmd("QUIT"); code != 205 {
		t.Fatalf("QUIT = %d", code)
	}
	if err := c.ExpectNoPending(testkit.Deadline()); err != nil {
		t.Fatalf("closed: %v", err)
	}
	if _, err := c.ReadLine(); err != io.EOF {
		t.Fatalf("after close: %v, want io.EOF", err)
	}
}

// TestCompressPartialFlush: INN's nnrpd ends each response with zlib's Z_PARTIAL_FLUSH (an
// empty fixed block, not 00 00 ff ff); the reply is readable at once. The bytes are what INN
// 2.8.0 sent for DATE.
func TestCompressPartialFlush(t *testing.T) {
	innDate, _ := hex.DecodeString("32343454303230323334303236b434313030e1e50208")
	p := newPeer(t, compressing(func(z *zserver) {
		z.plain("206 Compression now active; enjoy the speed!\r\n")
		z.send()
		if _, ok := z.readLine(); !ok {
			return
		}
		_, _ = z.c.Write(innDate)
		drain(z.zr)
	}))
	c := Dial(t, p.addr())
	c.Compress()
	c.Deadline = 2 * time.Second
	if _, line := c.Cmd("DATE"); line != "111 20261023194004" {
		t.Fatalf("DATE = %q", line)
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
