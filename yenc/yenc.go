package yenc

import (
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"strings"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// Options are the encoder choices; see the package doc.
type Options struct {
	Line         int
	RawDots      bool
	RawSpaces    bool
	ExtraEscapes string
	NoBegin      bool
	NoEnd        bool
	BadSize      bool
	BadCRC       bool
}

func (o Options) line() int {
	if o.Line <= 0 {
		return 128
	}
	return o.Line
}

func (o Options) size(n int) int {
	if o.BadSize {
		return n + 1
	}
	return n
}

func (o Options) crc(c uint32) uint32 {
	if o.BadCRC {
		return c ^ 1
	}
	return c
}

// Encode returns the body lines of a single-part post of data.
func Encode(name string, data []byte, o Options) []string {
	var lines []string
	if !o.NoBegin {
		lines = append(lines, fmt.Sprintf("=ybegin line=%d size=%d name=%s", o.line(), len(data), name))
	}
	lines = append(lines, encodeData(data, o)...)
	if !o.NoEnd {
		lines = append(lines, fmt.Sprintf("=yend size=%d crc32=%08x", o.size(len(data)), o.crc(crc32.ChecksumIEEE(data))))
	}
	return lines
}

// EncodeParts returns one post per part of partSize bytes of data.
func EncodeParts(name string, data []byte, partSize int, o Options) [][]string {
	if partSize <= 0 || len(data) == 0 {
		panic("yenc: EncodeParts needs partSize > 0 and data")
	}
	total := (len(data) + partSize - 1) / partSize
	whole := crc32.ChecksumIEEE(data)
	posts := make([][]string, 0, total)
	for p := 1; p <= total; p++ {
		begin := (p - 1) * partSize
		end := min(begin+partSize, len(data))
		part := data[begin:end]
		var lines []string
		if !o.NoBegin {
			lines = append(lines,
				fmt.Sprintf("=ybegin part=%d total=%d line=%d size=%d name=%s", p, total, o.line(), len(data), name),
				fmt.Sprintf("=ypart begin=%d end=%d", begin+1, end))
		}
		lines = append(lines, encodeData(part, o)...)
		if !o.NoEnd {
			l := fmt.Sprintf("=yend size=%d part=%d pcrc32=%08x", o.size(len(part)), p, o.crc(crc32.ChecksumIEEE(part)))
			if p == total {
				l += fmt.Sprintf(" crc32=%08x", o.crc(whole))
			}
			lines = append(lines, l)
		}
		posts = append(posts, lines)
	}
	return posts
}

// encodeData returns the data lines: each byte plus 42; NUL, LF, CR, '=' and the characters o
// asks for written as '=' and the character plus 64; a new line once one holds Line characters.
func encodeData(data []byte, o Options) []string {
	line := o.line()
	var lines []string
	b := make([]byte, 0, line+1)
	col := 0
	for i, c := range data {
		e := c + 42
		last := i == len(data)-1 || col >= line-1 // the last character of its line
		var esc bool
		switch e {
		case 0, '\n', '\r', '=':
			esc = true
		case '\t', ' ':
			esc = !o.RawSpaces && (col == 0 || last)
		case '.':
			esc = !o.RawDots && col == 0
		}
		if !esc && strings.IndexByte(o.ExtraEscapes, e) >= 0 {
			// never "=y" at the start of a line: that starts a control line
			esc = !(col == 0 && e+64 == 'y')
		}
		if esc {
			b = append(b, '=', e+64)
			col += 2
		} else {
			b = append(b, e)
			col++
		}
		if col >= line && i != len(data)-1 {
			lines = append(lines, string(b))
			b, col = b[:0], 0
		}
	}
	if len(data) > 0 {
		lines = append(lines, string(b))
	}
	return lines
}

// Profile names a kind of test data; see the package doc.
type Profile string

// The profiles of Data.
const (
	Random  Profile = "random"
	Plain   Profile = "plain"
	Escapes Profile = "escapes"
	Edges   Profile = "edges"
)

// critical are the bytes that encode to NUL, LF, CR and '=', edge those that encode to '.', tab
// and space.
var (
	critical = [...]byte{256 - 42, '\n' + 256 - 42, '\r' + 256 - 42, '=' - 42}
	edge     = [...]byte{'.' - 42, '\t' + 256 - 42, ' ' + 256 - 42}
)

// Data returns size bytes of profile p; the same arguments give the same bytes.
func Data(p Profile, size int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1))
	data := make([]byte, size)
	switch p {
	case Random:
		for i := range data {
			data[i] = byte(r.Uint32())
		}
	case Plain:
		for i := range data {
			data[i] = plainByte(r)
		}
	case Escapes:
		for i := range data {
			if i%2 == 1 {
				data[i] = critical[r.IntN(len(critical))]
			} else {
				data[i] = plainByte(r)
			}
		}
	case Edges:
		for i := range data {
			if r.IntN(3) == 0 {
				data[i] = edge[r.IntN(len(edge))]
			} else {
				data[i] = byte(r.Uint32())
			}
		}
	default:
		panic("yenc: unknown profile " + string(p))
	}
	return data
}

// plainByte returns a byte that encodes to none of NUL, LF, CR, '=', '.', tab and space.
func plainByte(r *rand.Rand) byte {
	for {
		c := byte(r.Uint32())
		switch c + 42 {
		case 0, '\n', '\r', '=', '.', '\t', ' ':
			continue
		}
		return c
	}
}

// Generator returns a fakeserver.Generator of single-part posts of profile p.
func Generator(p Profile, low, high int64, size func(n int64) int, o Options) fakeserver.Generator {
	if size == nil {
		size = func(int64) int { return 750_000 }
	}
	return fakeserver.Generator{
		Low:  low,
		High: high,
		Article: func(group string, n int64) (fakeserver.Article, bool) {
			a, _ := fakeserver.GeneratedArticle(group, n)
			name := fmt.Sprintf("%s-%d.bin", p, n)
			data := Data(p, size(n), uint64(n))
			for i, h := range a.Headers {
				if strings.HasPrefix(h, "Subject: ") {
					a.Headers[i] = fmt.Sprintf(`Subject: %s - "%s" yEnc (1/1) %d`, group, name, len(data))
				}
			}
			a.Body = Encode(name, data, o)
			return a, true
		},
	}
}
