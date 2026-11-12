package fakeserver

import (
	"bufio"
	"bytes"
	"compress/flate"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/internal/inflate"
)

// Tests of CompressShape: the compressed output stream as the client's raw socket sees it.

const (
	okLine = "206 Compression active\r\n"
	marker = "\x00\x00\xff\xff" // the sync flush marker
)

// recConn records every write to the socket (the pieces of the 206 line and of the compressed
// stream), so a test can tell what was one write without relying on read boundaries.
type recConn struct {
	net.Conn
	mu sync.Mutex
	w  [][]byte
}

func (r *recConn) Write(p []byte) (int, error) {
	// recorded before the write, so a client that has read the bytes finds them in writes()
	r.mu.Lock()
	r.w = append(r.w, bytes.Clone(p))
	i := len(r.w) - 1
	r.mu.Unlock()
	n, err := r.Conn.Write(p)
	if n < len(p) {
		r.mu.Lock()
		r.w[i] = r.w[i][:n]
		r.mu.Unlock()
	}
	return n, err
}

func (r *recConn) writes() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.w...)
}

// shapeServer is a Store with Compress and shape; DATE answers dateReply. The recConn sees
// the writes from the greeting on that follow the handler's start, so w[0] is the 206 write.
func shapeServer(t *testing.T, shape CompressShape) (*Server, *recConn) {
	t.Helper()
	rec := &recConn{}
	h := StoreHandler(testStore(), StoreOptions{Compress: true, CompressShape: shape})
	s := New(t, Options{Deadline: 10 * time.Second,
		Faults: Faults{Override: map[string][]string{"DATE": {"111 12345678901234"}}},
		Handler: func(c *Conn) {
			rec.Conn = c.conn
			c.conn = rec
			h(c)
		}})
	return s, rec
}

// zClient is a raw client of a compressed connection: its commands are deflated and sync
// flushed in one write.
type zClient struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader // reads c behind the 206 line; use it once the test reads the compressed stream
	zw *flate.Writer
	b  bytes.Buffer
}

// zDial connects, sends COMPRESS DEFLATE and reads the 206 line byte by byte, so that bytes
// behind it stay in the socket.
func zDial(t *testing.T, s *Server) *zClient {
	t.Helper()
	c := bytesDial(t, s)
	rawSend(t, c, "COMPRESS DEFLATE\r\n")
	var line []byte
	one := make([]byte, 1)
	for !bytes.HasSuffix(line, []byte("\r\n")) {
		if _, err := io.ReadFull(c, one); err != nil {
			t.Fatalf("reading the 206: %v (got %q)", err, line)
		}
		line = append(line, one[0])
	}
	if string(line) != okLine {
		t.Fatalf("COMPRESS: %q", line)
	}
	z := &zClient{t: t, c: c, br: bufio.NewReader(c)}
	z.zw, _ = flate.NewWriter(&z.b, flate.DefaultCompression)
	return z
}

func (z *zClient) send(cmd string) {
	z.t.Helper()
	z.b.Reset()
	_, _ = z.zw.Write([]byte(cmd + "\r\n"))
	_ = z.zw.Flush()
	if _, err := z.c.Write(z.b.Bytes()); err != nil {
		z.t.Fatal(err)
	}
}

// refStream deflates replies the way the default shape does (one default-level flate writer,
// sync flush after each reply), to know the bytes a test expects on the wire.
type refStream struct {
	b  bytes.Buffer
	zw *flate.Writer
}

func newRef() *refStream {
	r := &refStream{}
	r.zw, _ = flate.NewWriter(&r.b, flate.DefaultCompression)
	return r
}

func (r *refStream) flush(p string) []byte {
	r.b.Reset()
	_, _ = r.zw.Write([]byte(p))
	_ = r.zw.Flush()
	return bytes.Clone(r.b.Bytes())
}

// plain reads the compressed stream of c with the kit's inflater, which passes on the bytes as
// soon as they can be decoded (so it also reads partial-flush streams), and returns a reader
// of the plaintext.
func plain(t *testing.T, z *zClient) *bufio.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })
	go func() {
		_ = pw.CloseWithError(inflate.Run(z.br, func(b []byte) error {
			_, err := pw.Write(b)
			return err
		}))
	}()
	return bufio.NewReader(pr)
}

func wantPlainLine(t *testing.T, br *bufio.Reader, c net.Conn, want string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil || line != want {
		t.Fatalf("reply %q, %v; want %q", line, err, want)
	}
}

// The default shape: one socket write per reply, a sync flush at its end.
func TestCompressShapeDefault(t *testing.T) {
	s, rec := shapeServer(t, CompressShape{})
	z := zDial(t, s)
	br := plain(t, z)
	z.send("DATE")
	wantPlainLine(t, br, z.c, dateReply)
	z.send("DATE")
	wantPlainLine(t, br, z.c, dateReply)
	w := rec.writes()
	ref := newRef()
	if len(w) != 3 || string(w[0]) != okLine || !bytes.Equal(w[1], ref.flush(dateReply)) || !bytes.Equal(w[2], ref.flush(dateReply)) {
		t.Fatalf("writes %q", w)
	}
	if !bytes.HasSuffix(w[1], []byte(marker)) {
		t.Errorf("no sync marker at the end of %x", w[1])
	}
}

func TestCompressShapeSplitPieces(t *testing.T) {
	const gap = 120 * time.Millisecond
	ref := newRef()
	first, second := ref.flush(dateReply), ref.flush(dateReply)
	n := len(first)
	for _, tc := range []struct {
		name  string
		split Split
		cuts  []int
	}{
		{"offsets from start and end", Split{At: []int{2, -3}, Gap: gap}, []int{2, n - 3}},
		{"inside the marker", Split{At: []int{-1, -2, -3}, Gap: gap}, []int{n - 3, n - 2, n - 1}},
		{"size", Split{Size: 7, Gap: gap}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cuts := tc.cuts
			if cuts == nil {
				for k := tc.split.Size; k < n; k += tc.split.Size {
					cuts = append(cuts, k)
				}
			}
			var pieces []string
			prev := 0
			for _, k := range append(append([]int{}, cuts...), n) {
				pieces = append(pieces, string(first[prev:k]))
				prev = k
			}
			s, rec := shapeServer(t, CompressShape{Split: tc.split})
			z := zDial(t, s)
			start := time.Now()
			z.send("DATE")
			readPieces(t, z.c, start, pieces, gaps(len(pieces), gap))
			// the pieces are exactly the socket writes of the server
			w := rec.writes()
			if len(w) != 1+len(pieces) {
				t.Fatalf("%d writes, want the 206 and %d pieces", len(w), len(pieces))
			}
			for i, p := range pieces {
				if string(w[1+i]) != p {
					t.Errorf("write %d = %x, want %x", i, w[1+i], p)
				}
			}
			// the stream goes on: the next reply is deflated after the first as the reference does
			start = time.Now()
			z.send("DATE")
			readPieces(t, z.c, start, []string{string(second)}, []time.Duration{0})
		})
	}
}

// A client inflater (compress/flate's reader) must cope with the stream arriving byte by byte.
func TestCompressShapeSplitBytewise(t *testing.T) {
	s, _ := shapeServer(t, CompressShape{Split: Split{Size: 1}})
	z := zDial(t, s)
	zr := flate.NewReader(z.br)
	for range 3 {
		z.send("DATE")
		buf := make([]byte, len(dateReply))
		_ = z.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(zr, buf); err != nil || string(buf) != dateReply {
			t.Fatalf("reply %q, %v", buf, err)
		}
	}
}

func TestCompressShapeEmptyFlushWith206(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape CompressShape
		empty string
	}{
		{"sync", CompressShape{EmptyFlushWith206: true}, "\x00" + marker},
		{"partial", CompressShape{Flush: FlushPartial, EmptyFlushWith206: true}, "\x02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec := shapeServer(t, tc.shape)
			z := zDial(t, s)
			// the bytes behind the 206 line are already here: nothing was sent after the line
			_ = z.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if got, err := z.br.Peek(len(tc.empty)); err != nil || string(got) != tc.empty {
				t.Fatalf("bytes after the 206: %x, %v; want %x", got, err, tc.empty)
			}
			if w := rec.writes(); len(w) != 1 || string(w[0]) != okLine+tc.empty {
				t.Fatalf("the 206 and the empty flush are not one write: %q", w)
			}
			// the empty block is part of the stream: replies follow it
			br := plain(t, z)
			z.send("DATE")
			wantPlainLine(t, br, z.c, dateReply)
		})
	}
}

// Without the option nothing follows the 206 line.
func TestCompressShapeNoEmptyFlush(t *testing.T) {
	s, rec := shapeServer(t, CompressShape{})
	z := zDial(t, s)
	if !readQuiet(t, z.c, 200*time.Millisecond) {
		t.Fatal("bytes after the 206")
	}
	if w := rec.writes(); len(w) != 1 || string(w[0]) != okLine {
		t.Fatalf("writes %q", w)
	}
}

// With206 pipelines a deflated payload in the write of the 206 line (the client reads it
// before it has sent a command).
func TestCompressShapeWith206(t *testing.T) {
	s, rec := shapeServer(t, CompressShape{With206: []byte("hello\r\n")})
	z := zDial(t, s)
	br := plain(t, z)
	wantPlainLine(t, br, z.c, "hello\r\n")
	z.send("DATE")
	wantPlainLine(t, br, z.c, dateReply)
	w := rec.writes()
	if string(w[0][:len(okLine)]) != okLine || len(w[0]) == len(okLine) {
		t.Fatalf("first write %q", w[0])
	}
}

// Partial flush as zlib's: replies decode, promptly, and the stream has no sync marker.
func TestCompressShapePartialFlush(t *testing.T) {
	s, rec := shapeServer(t, CompressShape{Flush: FlushPartial})
	z := zDial(t, s)
	br := plain(t, z)
	for range 3 {
		z.send("DATE")
		wantPlainLine(t, br, z.c, dateReply) // arrives although the last bits of the flush block are not sent
	}
	z.send("HELP")
	wantPlainLine(t, br, z.c, "100 Help text follows\r\n")
	var stream []byte
	for _, p := range rec.writes()[1:] {
		stream = append(stream, p...)
	}
	if bytes.Contains(stream, []byte(marker)) {
		t.Errorf("sync marker in a partial-flush stream: %x", stream)
	}
}

// The partial-flush encoder against compress/flate's reader, which has no partial flush of its
// own but decodes the stream once its last block is completed.
func TestPartialEncoderDecodes(t *testing.T) {
	e, err := newEncoder(CompressShape{Flush: FlushPartial})
	if err != nil {
		t.Fatal(err)
	}
	var plainText, stream []byte
	all := make([]byte, 256) // every byte value: the 8-bit and the 9-bit codes
	for i := range all {
		all[i] = byte(i)
	}
	for _, p := range [][]byte{[]byte("211 6 1 6 compress.test\r\n"), nil, all, []byte("."), {0xff, 0xff, 0x00}, bytes.Repeat([]byte("xyz\xf0"), 5000)} {
		b, _ := e.flushData(p)
		stream = append(stream, b...)
		plainText = append(plainText, p...)
	}
	if bytes.Contains(stream, []byte(marker)) {
		t.Error("sync marker in the stream")
	}
	// complete the stream: the held bits are the zero tail of the empty block's end code; a
	// final stored block header (BFINAL 1, BTYPE 00) follows them at once, then padding to a
	// byte and the empty block's LEN and NLEN
	unfinished := len(stream)
	e.bw.put(1, 3)
	if e.bw.n > 0 {
		e.bw.put(0, 8-e.bw.n)
	}
	stream = append(append(stream, e.bw.take()...), 0x00, 0x00, 0xff, 0xff)
	got, err := io.ReadAll(flate.NewReader(bytes.NewReader(stream)))
	if err != nil || !bytes.Equal(got, plainText) {
		t.Fatalf("decoded %d bytes, %v; want %d", len(got), err, len(plainText))
	}
	// the kit's inflater decodes the unfinished stream as it is
	var out []byte
	_ = inflate.Run(bufio.NewReader(bytes.NewReader(stream[:unfinished])), func(b []byte) error { out = append(out, b...); return nil })
	if !bytes.Equal(out, plainText) {
		t.Errorf("kit inflater: %d bytes, want %d", len(out), len(plainText))
	}
}

// The first sync flush of an empty stream and the first partial flush have the bytes the
// tests above expect (a change of compress/flate would show here first).
func TestEmptyFlushBytes(t *testing.T) {
	e, _ := newEncoder(CompressShape{})
	if b, _ := e.flushData(nil); string(b) != "\x00"+marker {
		t.Errorf("sync empty flush %x", b)
	}
	e, _ = newEncoder(CompressShape{Flush: FlushPartial})
	if b, _ := e.flushData(nil); string(b) != "\x02" {
		t.Errorf("partial empty flush %x", b)
	}
}

func TestCompressShapeFinish(t *testing.T) {
	for _, after := range []string{"", "junk\r\nmore"} {
		t.Run("after="+after, func(t *testing.T) {
			s, rec := shapeServer(t, CompressShape{Flush: FlushFinish, AfterEnd: []byte(after)})
			z := zDial(t, s)
			z.send("DATE")
			br := z.br // a ByteReader: flate reads no byte past the final block
			_ = z.c.SetReadDeadline(time.Now().Add(10 * time.Second))
			got, err := io.ReadAll(flate.NewReader(br))
			if err != nil || string(got) != dateReply {
				t.Fatalf("stream %q, %v", got, err)
			}
			rest := make([]byte, len(after))
			if _, err := io.ReadFull(br, rest); err != nil || string(rest) != after {
				t.Fatalf("after the end %q, %v", rest, err)
			}
			if w := rec.writes(); len(w) != 2 || !bytes.HasSuffix(w[1], []byte(after)) {
				t.Fatalf("the final block and the trailing bytes are not one write: %q", w)
			}
			// the client's input is still inflated, the server's output is plain from here on
			z.send("DATE")
			line, err := br.ReadString('\n')
			if err != nil || line != dateReply {
				t.Fatalf("plain reply %q, %v", line, err)
			}
		})
	}
}

// The shape also applies to a Peer and to a Script step.
func TestCompressShapePeerAndScript(t *testing.T) {
	shape := CompressShape{Flush: FlushPartial, EmptyFlushWith206: true, Split: Split{Size: 2}}
	t.Run("peer", func(t *testing.T) {
		s := New(t, Options{Handler: PeerHandler(&PeerOptions{Compress: true, CompressShape: shape})})
		z := zDial(t, s)
		if _, err := z.br.Peek(1); err != nil { // the empty block
			t.Fatal(err)
		}
		br := plain(t, z)
		z.send("MODE STREAM")
		line, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "203 ") {
			t.Fatalf("%q, %v", line, err)
		}
	})
	t.Run("script", func(t *testing.T) {
		s := New(t, Options{Script: []Step{
			{Expect: "COMPRESS DEFLATE", Reply: []string{"206 Compression active"}, Compress: true, CompressShape: shape},
			{Expect: "DATE", Reply: []string{"111 20260102030405"}},
		}})
		z := zDial(t, s)
		if _, err := z.br.Peek(1); err != nil {
			t.Fatal(err)
		}
		br := plain(t, z)
		z.send("DATE")
		wantPlainLine(t, br, z.c, "111 20260102030405\r\n")
	})
}

// StartCompressShaped from a Handler that writes its own 206.
func TestStartCompressShaped(t *testing.T) {
	s := New(t, Options{Handler: func(c *Conn) {
		if _, err := c.ReadLine(); err != nil {
			return
		}
		_ = c.WriteLines("206 Compression active")
		if c.StartCompressShaped(CompressShape{Flush: FlushPartial}) != nil || c.StartCompressShaped(CompressShape{}) == nil {
			return
		}
		_ = c.WriteLines("111 20260102030405")
		c.drain()
	}})
	z := zDial(t, s)
	br := plain(t, z)
	wantPlainLine(t, br, z.c, "111 20260102030405\r\n")
}

// The pipelined tail is armed only when compression starts: a Script step without Compress and
// a Peer without Compress (a forced 206) stay plain.
func TestCompressShapeNotArmedWithoutCompress(t *testing.T) {
	shape := CompressShape{EmptyFlushWith206: true, With206: []byte("x")}
	check := func(t *testing.T, s *Server, cmd, wantPrefix string) {
		t.Helper()
		c := bytesDial(t, s)
		rawSend(t, c, cmd+"\r\n")
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "206 ") {
			t.Fatalf("%q, %v", line, err)
		}
		if br.Buffered() != 0 || !readQuiet(t, c, 200*time.Millisecond) {
			t.Fatal("bytes after the 206 of a connection that stays plain")
		}
		rawSend(t, c, "MODE STREAM\r\n")
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if line, err = br.ReadString('\n'); err != nil || !strings.HasPrefix(line, wantPrefix) {
			t.Fatalf("plain command after the 206: %q, %v", line, err)
		}
	}
	t.Run("script", func(t *testing.T) {
		s := New(t, Options{Script: []Step{
			{Expect: "COMPRESS DEFLATE", Reply: []string{"206 ok"}, CompressShape: shape},
			{Expect: "MODE STREAM", Reply: []string{"203 ok"}},
		}})
		check(t, s, "COMPRESS DEFLATE", "203 ")
	})
	t.Run("peer", func(t *testing.T) {
		s := New(t, Options{Handler: PeerHandler(&PeerOptions{CompressShape: shape}),
			Faults: Faults{Override: map[string][]string{"COMPRESS": {"206 ok"}}}})
		check(t, s, "COMPRESS DEFLATE", "203 ")
	})
}

// A write deadline that the caller set (writeKind does for its pauses) survives a Write with a
// Split gap; without one, the deadline set for the pauses is cleared again.
type dlConn struct {
	net.Conn
	dl []time.Time
}

func (c *dlConn) Write(p []byte) (int, error)        { return len(p), nil }
func (c *dlConn) SetWriteDeadline(t time.Time) error { c.dl = append(c.dl, t); return nil }

func TestCompressSplitKeepsCallerDeadline(t *testing.T) {
	for _, set := range []bool{true, false} {
		sc := &dlConn{}
		enc, _ := newEncoder(CompressShape{Split: Split{Size: 3, Gap: time.Millisecond}})
		d := &deflateConn{Conn: sc, enc: enc, sleep: func(time.Duration) bool { return true }, deadline: time.Hour}
		var want time.Time
		if set {
			want = time.Now().Add(time.Minute)
			_ = d.SetWriteDeadline(want)
		}
		if _, err := d.Write([]byte("hello\r\n")); err != nil {
			t.Fatal(err)
		}
		if last := sc.dl[len(sc.dl)-1]; !last.Equal(want) {
			t.Errorf("caller deadline set=%v: after Write the deadline is %v, want %v", set, last, want)
		}
	}
}
