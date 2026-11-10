package proc

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

// cutMarker is the line that replaces the bytes left out of a capped output.
func cutMarker(n int64) string { return fmt.Sprintf("\n[... %d bytes cut ...]\n", n) }

// cutMiddle returns s unchanged when it has at most max bytes, else its first and last max/2
// bytes around a cut marker.
func cutMiddle(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	h := max / 2
	t := max - h
	return s[:h] + cutMarker(int64(len(s)-max)) + s[len(s)-t:]
}

// capWriter is an io.Writer that keeps the first and the last bytes of what is written, within
// max in all, and counts the rest. It never holds more than about 2*max/2 + max bytes, however
// much is written. Safe for concurrent Write and String. It must not embed bytes.Buffer, whose
// ReadFrom would let io.Copy bypass Write.
type capWriter struct {
	mu      sync.Mutex
	headMax int
	tailMax int
	head    []byte
	tail    []byte // may exceed tailMax between trims
	total   int64
}

// newCapWriter keeps at most max bytes (max <= 0: everything).
func newCapWriter(max int) *capWriter {
	if max <= 0 {
		return &capWriter{headMax: math.MaxInt}
	}
	h := max / 2
	return &capWriter{headMax: h, tailMax: max - h}
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.total += int64(n)
	if room := c.headMax - len(c.head); room > 0 {
		take := min(room, len(p))
		c.head = append(c.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if len(c.tail) > 2*c.tailMax+4096 {
			c.tail = append(c.tail[:0], c.tail[len(c.tail)-c.tailMax:]...)
		}
	}
	return n, nil
}

// String returns what was kept, with a "[... N bytes cut ...]" marker between head and tail
// when bytes were dropped.
func (c *capWriter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tail := c.tail
	if len(tail) > c.tailMax {
		tail = tail[len(tail)-c.tailMax:]
	}
	cut := c.total - int64(len(c.head)) - int64(len(tail))
	var b strings.Builder
	b.Grow(len(c.head) + len(tail) + 40)
	b.Write(c.head)
	if cut > 0 {
		b.WriteString(cutMarker(cut))
	}
	b.Write(tail)
	return b.String()
}
