package yenc_test

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
	"github.com/go-while/nntp-testkit/yenc"
)

// post is what decodePost read from one post.
type post struct {
	name                      string
	size, part, total         int
	begin, end                int // =ypart, 0 if none
	endSize, endPart          int
	crc, pcrc                 uint32
	hasBegin, hasEnd          bool
	hasCRC, hasPCRC, hasYpart bool
	data                      []byte
}

// fields parses "k=v k=v name=rest of line" after the keyword.
func fields(t *testing.T, line, keyword string) map[string]string {
	t.Helper()
	rest, ok := strings.CutPrefix(line, keyword+" ")
	if !ok {
		t.Fatalf("%q does not start with %q", line, keyword)
	}
	m := map[string]string{}
	for rest != "" {
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			t.Fatalf("bad field in %q", line)
		}
		if k == "name" {
			m[k] = v
			break
		}
		v, rest, _ = strings.Cut(v, " ")
		m[k] = v
	}
	return m
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func hex32(t *testing.T, s string) uint32 {
	t.Helper()
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(n)
}

// decodePost is a strict reference decoder for one post's body lines. Besides decoding, it
// checks the rules of o on every data line.
func decodePost(t *testing.T, lines []string, o yenc.Options) post {
	t.Helper()
	L := o.Line
	if L <= 0 {
		L = 128
	}
	var p post
	if len(lines) > 0 && strings.HasPrefix(lines[0], "=ybegin ") {
		f := fields(t, lines[0], "=ybegin")
		p.hasBegin, p.name, p.size = true, f["name"], atoi(t, f["size"])
		if atoi(t, f["line"]) != L {
			t.Fatalf("=ybegin line=%s, want %d", f["line"], L)
		}
		if f["part"] != "" {
			p.part, p.total = atoi(t, f["part"]), atoi(t, f["total"])
		}
		lines = lines[1:]
		if p.part > 0 {
			if len(lines) == 0 || !strings.HasPrefix(lines[0], "=ypart ") {
				t.Fatalf("part %d without =ypart", p.part)
			}
			f := fields(t, lines[0], "=ypart")
			p.hasYpart, p.begin, p.end = true, atoi(t, f["begin"]), atoi(t, f["end"])
			lines = lines[1:]
		}
	}
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "=yend ") {
		f := fields(t, lines[n-1], "=yend")
		p.hasEnd, p.endSize = true, atoi(t, f["size"])
		if f["part"] != "" {
			p.endPart = atoi(t, f["part"])
		}
		if f["crc32"] != "" {
			p.hasCRC, p.crc = true, hex32(t, f["crc32"])
		}
		if f["pcrc32"] != "" {
			p.hasPCRC, p.pcrc = true, hex32(t, f["pcrc32"])
		}
		lines = lines[:n-1]
	}
	for i, l := range lines {
		final := i == len(lines)-1
		if strings.HasPrefix(l, "=y") {
			t.Fatalf("data line %d starts with =y: %q", i, l)
		}
		if l == "" || len(l) > L+1 || !final && len(l) < L {
			t.Fatalf("data line %d has %d characters (line=%d, last=%v): %q", i, len(l), L, final, l)
		}
		var lastEsc bool
		for j := 0; j < len(l); j++ {
			c := l[j]
			switch {
			case c == '=':
				if j+1 == len(l) {
					t.Fatalf("data line %d ends in a lone '='", i)
				}
				j++
				p.data = append(p.data, l[j]-64-42)
				lastEsc = j == len(l)-1
				continue
			case c == 0 || c == '\n' || c == '\r':
				t.Fatalf("data line %d holds a raw %q", i, c)
			case c == '.' && j == 0 && !o.RawDots:
				t.Fatalf("data line %d starts with a raw '.'", i)
			case (c == '\t' || c == ' ') && (j == 0 || j == len(l)-1) && !o.RawSpaces:
				t.Fatalf("data line %d has a raw %q at position %d", i, c, j)
			case strings.IndexByte(o.ExtraEscapes, c) >= 0 && !(j == 0 && c+64 == 'y'):
				t.Fatalf("data line %d has a raw %q from ExtraEscapes", i, c)
			}
			p.data = append(p.data, c-42)
			lastEsc = false
		}
		if len(l) == L+1 && !lastEsc {
			t.Fatalf("data line %d has %d characters without ending in an escape", i, len(l))
		}
	}
	return p
}

var profiles = []yenc.Profile{yenc.Random, yenc.Plain, yenc.Escapes, yenc.Edges}

func TestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 2, 3, 63, 64, 65, 127, 128, 129, 130, 255, 256, 257, 1000, 4096, 65543}
	opts := []yenc.Options{{}, {RawDots: true, RawSpaces: true}, {Line: 64}, {Line: 997}, {Line: 1}, {Line: 2}, {ExtraEscapes: "\t.9"}}
	for _, pr := range profiles {
		for _, n := range sizes {
			for _, o := range opts {
				data := yenc.Data(pr, n, uint64(n))
				p := decodePost(t, yenc.Encode("f 1.bin", data, o), o)
				if !p.hasBegin || !p.hasEnd || !p.hasCRC || p.name != "f 1.bin" || p.size != n || p.endSize != n {
					t.Fatalf("%s, %d bytes, %+v: header or trailer wrong: %+v", pr, n, o, p)
				}
				if !bytes.Equal(p.data, data) || p.crc != crc32.ChecksumIEEE(data) {
					t.Fatalf("%s, %d bytes, %+v: data or CRC differ", pr, n, o)
				}
			}
		}
	}
}

func TestEncodeGolden(t *testing.T) {
	// '.', 'a', NUL, space, '=', 'b', tab: each rule once on lines of 4
	data := []byte{'.' - 42, 'a' - 42, 256 - 42, ' ' + 256 - 42, '=' - 42, 'b' - 42, '\t' + 256 - 42}
	crc := fmt.Sprintf("%08x", crc32.ChecksumIEEE(data))
	for _, tc := range []struct {
		o    yenc.Options
		want []string
	}{
		{yenc.Options{Line: 4}, []string{"=ybegin line=4 size=7 name=g", "=na=@", "=`=}", "b=I", "=yend size=7 crc32=" + crc}},
		{yenc.Options{Line: 4, RawDots: true, RawSpaces: true}, []string{"=ybegin line=4 size=7 name=g", ".a=@", " =}b", "\t", "=yend size=7 crc32=" + crc}},
	} {
		if got := yenc.Encode("g", data, tc.o); !slicesEqual(got, tc.want) {
			t.Errorf("%+v:\n got %q\nwant %q", tc.o, got, tc.want)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEncodeParts(t *testing.T) {
	data := yenc.Data(yenc.Random, 1000, 1)
	posts := yenc.EncodeParts("p.bin", data, 300, yenc.Options{})
	if len(posts) != 4 {
		t.Fatalf("%d parts, want 4", len(posts))
	}
	var all []byte
	for i, lines := range posts {
		p := decodePost(t, lines, yenc.Options{})
		begin, end := i*300+1, min(i*300+300, 1000)
		part := data[begin-1 : end]
		if p.part != i+1 || p.total != 4 || p.size != 1000 || !p.hasYpart || p.begin != begin || p.end != end ||
			p.endPart != i+1 || p.endSize != len(part) || !p.hasPCRC || p.pcrc != crc32.ChecksumIEEE(part) {
			t.Fatalf("part %d: %+v", i+1, p)
		}
		if last := i == 3; p.hasCRC != last || last && p.crc != crc32.ChecksumIEEE(data) {
			t.Fatalf("part %d: crc32 %v %08x", i+1, p.hasCRC, p.crc)
		}
		if !bytes.Equal(p.data, part) {
			t.Fatalf("part %d: data differ", i+1)
		}
		all = append(all, p.data...)
	}
	if !bytes.Equal(all, data) {
		t.Fatal("parts don't add up to the data")
	}
}

func TestBrokenPosts(t *testing.T) {
	data := yenc.Data(yenc.Random, 200, 2)
	crc := crc32.ChecksumIEEE(data)
	p := decodePost(t, yenc.Encode("b", data, yenc.Options{NoBegin: true}), yenc.Options{})
	if p.hasBegin || !p.hasEnd || !bytes.Equal(p.data, data) {
		t.Errorf("NoBegin: %+v", p)
	}
	p = decodePost(t, yenc.Encode("b", data, yenc.Options{NoEnd: true}), yenc.Options{})
	if !p.hasBegin || p.hasEnd || !bytes.Equal(p.data, data) {
		t.Errorf("NoEnd: %+v", p)
	}
	p = decodePost(t, yenc.Encode("b", data, yenc.Options{BadSize: true, BadCRC: true}), yenc.Options{})
	if p.endSize != 201 || p.crc != crc^1 || p.size != 200 {
		t.Errorf("BadSize, BadCRC: %+v", p)
	}
	posts := yenc.EncodeParts("b", data, 150, yenc.Options{BadCRC: true})
	p = decodePost(t, posts[1], yenc.Options{})
	if p.pcrc != crc32.ChecksumIEEE(data[150:])^1 || p.crc != crc^1 {
		t.Errorf("parts, BadCRC: %+v", p)
	}
}

func TestData(t *testing.T) {
	// the stream is part of the contract: tests may keep expectations
	if got := fmt.Sprintf("%x", yenc.Data(yenc.Random, 8, 7)); got != "b1d0756fb63cb463" {
		t.Errorf("Data(Random, 8, 7) = %s", got)
	}
	if !bytes.Equal(yenc.Data(yenc.Edges, 100, 3), yenc.Data(yenc.Edges, 100, 3)) || bytes.Equal(yenc.Data(yenc.Edges, 100, 3), yenc.Data(yenc.Edges, 100, 4)) {
		t.Error("Data is not a function of its arguments")
	}
	special := func(e byte) bool { return strings.IndexByte("\x00\n\r=.\t ", e) >= 0 }
	for i, c := range yenc.Data(yenc.Plain, 30000, 5) {
		if special(c + 42) {
			t.Fatalf("Plain byte %d encodes to %q", i, c+42)
		}
	}
	for i, c := range yenc.Data(yenc.Escapes, 30000, 5) {
		if e := c + 42; i%2 == 1 && strings.IndexByte("\x00\n\r=", e) < 0 || i%2 == 0 && special(e) {
			t.Fatalf("Escapes byte %d encodes to %q", i, e)
		}
	}
	edges := 0
	for _, c := range yenc.Data(yenc.Edges, 30000, 5) {
		if strings.IndexByte(".\t ", c+42) >= 0 {
			edges++
		}
	}
	if edges < 9000 || edges > 11500 {
		t.Errorf("Edges: %d of 30000 bytes encode to '.', tab or space, want about a third", edges)
	}
}

// TestGenerator serves a generated group through fakeserver and reads it with the strict
// client: dot-stuffing and line ends survive the round trip.
func TestGenerator(t *testing.T) {
	o := yenc.Options{RawDots: true, RawSpaces: true}
	size := func(n int64) int { return int(n) * 700 }
	st := fakeserver.NewStore()
	st.AddGenerated("test.yenc", "yEnc posts", yenc.Generator(yenc.Edges, 1, 5, size, o))
	s := fakeserver.New(t, fakeserver.Options{Store: st})
	c := client.Dial(t, s.Addr())
	if code, line := c.Cmd("GROUP test.yenc"); code != 211 {
		t.Fatalf("GROUP: %s", line)
	}
	for n := int64(1); n <= 5; n++ {
		if code, line := c.Cmd("HEAD %d", n); code != 221 {
			t.Fatalf("HEAD %d: %s", n, line)
		}
		want := fmt.Sprintf(`Subject: test.yenc - "edges-%d.bin" yEnc (1/1) %d`, n, size(n))
		if h := c.ReadDotLines(); !contains(h, want) {
			t.Errorf("HEAD %d: no %q in %q", n, want, h)
		}
		if code, line := c.Cmd("BODY %d", n); code != 222 {
			t.Fatalf("BODY %d: %s", n, line)
		}
		p := decodePost(t, c.ReadDotLines(), o)
		if p.name != fmt.Sprintf("edges-%d.bin", n) || !bytes.Equal(p.data, yenc.Data(yenc.Edges, size(n), uint64(n))) {
			t.Errorf("BODY %d: %s, data differ", n, p.name)
		}
	}
}

func contains(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}
