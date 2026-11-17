package client

import (
	"bytes"
	"errors"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// endlessServer returns a client Conn on one end of a net.Pipe and the number of bytes the other
// end has written: it writes prefix, then chunk again and again until the Conn is closed. A
// reader without a cap would keep consuming (and holding) them until the hang guard; the
// tests assert that the reader gives up soon after its cap.
func endlessServer(t *testing.T, prefix, chunk string) (*Conn, *atomic.Int64) {
	t.Helper()
	cl, sv := net.Pipe()
	var n atomic.Int64
	go func() {
		defer sv.Close()
		if prefix != "" {
			w, err := sv.Write([]byte(prefix))
			n.Add(int64(w))
			if err != nil {
				return
			}
		}
		for {
			w, err := sv.Write([]byte(chunk))
			n.Add(int64(w))
			if err != nil {
				return
			}
		}
	}()
	c := NewConn(cl)
	c.Deadline = 10 * time.Second // a failure to stop shows as a slow test, not a flake
	t.Cleanup(c.Close)
	return c, &n
}

// bounded fails the test when the endless writer got more than limit bytes through before the
// reader stopped (the cap, the 1 MiB reader buffer and what the pipe held at that moment).
func bounded(t *testing.T, n *atomic.Int64, limit int64) {
	t.Helper()
	if got := n.Load(); got > limit {
		t.Errorf("the reader consumed %d bytes before it gave up, want at most %d", got, limit)
	}
}

const slack = 2 * readerSize

func TestLineCapReaders(t *testing.T) {
	long := strings.Repeat("x", 4096)
	for _, strict := range []bool{true, false} {
		mode := map[bool]string{true: "strict", false: "lax"}[strict]
		t.Run(mode+"/ReadLine", func(t *testing.T) {
			c, n := endlessServer(t, "", long)
			c.Strict = strict
			start := time.Now()
			_, err := c.ReadLine()
			if !errors.Is(err, ErrLineTooLong) {
				t.Fatalf("err = %v, want ErrLineTooLong", err)
			}
			bounded(t, n, DefaultMaxLine+slack)
			if time.Since(start) > 5*time.Second {
				t.Errorf("took %v", time.Since(start))
			}
		})
		t.Run(mode+"/ReadStatus", func(t *testing.T) {
			c, n := endlessServer(t, "211 ", long)
			c.Strict = strict
			c.MaxLine = 1000
			if _, _, err := c.ReadStatus(); !errors.Is(err, ErrLineTooLong) {
				t.Fatalf("err = %v, want ErrLineTooLong", err)
			}
			bounded(t, n, 1000+slack)
		})
		t.Run(mode+"/TryReadDotLines", func(t *testing.T) {
			c, n := endlessServer(t, "a\r\nb\r\n", long)
			c.Strict = strict
			lines, err := c.TryReadDotLines()
			if !errors.Is(err, ErrLineTooLong) {
				t.Fatalf("err = %v, want ErrLineTooLong", err)
			}
			if len(lines) != 2 {
				t.Errorf("lines before the long one: %q", lines)
			}
			bounded(t, n, DefaultMaxLine+slack)
		})
		t.Run(mode+"/TryRawUntilDot", func(t *testing.T) {
			c, n := endlessServer(t, "a\r\n", long)
			c.Strict = strict
			b, err := c.TryRawUntilDot()
			if !errors.Is(err, ErrLineTooLong) {
				t.Fatalf("err = %v, want ErrLineTooLong", err)
			}
			if len(b) > 3+DefaultMaxLine {
				t.Errorf("kept %d bytes", len(b))
			}
			bounded(t, n, DefaultMaxLine+slack)
		})
	}
	t.Run("TryPipeline", func(t *testing.T) {
		// the status line of the first reply is endless; the writer of the commands must not
		// deadlock the call either (the pipe is unbuffered)
		c, n := endlessServer(t, "", long)
		c.MaxLine = 5000
		_, err := c.TryPipeline([]string{"DATE", "DATE"}, nil)
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("err = %v, want ErrLineTooLong", err)
		}
		bounded(t, n, 5000+slack)
	})
	t.Run("TryPipeline block line", func(t *testing.T) {
		c, n := endlessServer(t, "101 caps\r\n", long)
		c.MaxLine = 5000
		replies, err := c.TryPipeline([]string{"CAPABILITIES"}, nil)
		if !errors.Is(err, ErrLineTooLong) || len(replies) != 0 {
			t.Fatalf("replies %v err = %v, want ErrLineTooLong", replies, err)
		}
		bounded(t, n, 5000+slack)
	})
}

// TestLineCapBoundary: a line of exactly MaxLine bytes with its line end passes, one more fails;
// negative lifts the cap; 0 is DefaultMaxLine.
func TestLineCapBoundary(t *testing.T) {
	for _, tc := range []struct {
		max    int
		length int // line without CRLF
		ok     bool
	}{
		{100, 98, true}, {100, 99, false}, {100, 97, true},
		{0, DefaultMaxLine - 2, true}, {0, DefaultMaxLine - 1, false},
		{-1, 3 * DefaultMaxLine, true},
	} {
		p := newPeer(t, scripted("200 hi\r\n"+strings.Repeat("y", tc.length)+"\r\n"+"211 next\r\n"))
		c := Dial(t, p.addr())
		c.MaxLine = tc.max
		line, err := c.ReadLine()
		switch {
		case tc.ok && (err != nil || len(line) != tc.length):
			t.Errorf("max %d len %d: len %d err %v", tc.max, tc.length, len(line), err)
		case !tc.ok && !errors.Is(err, ErrLineTooLong):
			t.Errorf("max %d len %d: err %v, want ErrLineTooLong", tc.max, tc.length, err)
		}
		if tc.ok {
			if code, _, err := c.ReadStatus(); err != nil || code != 211 {
				t.Errorf("max %d len %d: next line: %d %v (the reader lost sync)", tc.max, tc.length, code, err)
			}
		}
	}
}

// TestBlockCaps: a block of short lines that never ends stops at MaxBlock bytes and, trickling,
// at BlockTime; a line that stalls half way stops at BlockTime too, not at the hang guard.
func TestBlockCaps(t *testing.T) {
	t.Run("MaxBlock", func(t *testing.T) {
		c, n := endlessServer(t, "", strings.Repeat("line of text\r\n", 400))
		c.MaxBlock = 100 << 10
		lines, err := c.TryReadDotLines()
		var se *StrictError
		if !errors.Is(err, ErrBlockTooLong) || !errors.As(err, &se) || se.Kind != NoTerminator {
			t.Fatalf("err = %v, want a NoTerminator StrictError with ErrBlockTooLong", err)
		}
		if size := len(lines) * 14; size > c.MaxBlock+10_000 {
			t.Errorf("held %d bytes", size)
		}
		bounded(t, n, int64(c.MaxBlock)+slack+10_000)
	})
	t.Run("MaxBlock raw", func(t *testing.T) {
		c, n := endlessServer(t, "", strings.Repeat("line of text\r\n", 400))
		c.MaxBlock = 100 << 10
		b, err := c.TryRawUntilDot()
		if !errors.Is(err, ErrBlockTooLong) {
			t.Fatalf("err = %v, want ErrBlockTooLong", err)
		}
		if len(b) > c.MaxBlock+10_000 {
			t.Errorf("held %d bytes", len(b))
		}
		bounded(t, n, int64(c.MaxBlock)+slack+10_000)
	})
	t.Run("MaxBlock pipeline", func(t *testing.T) {
		c, _ := endlessServer(t, "101 caps\r\n", strings.Repeat("line of text\r\n", 400))
		c.MaxBlock = 100 << 10
		if _, err := c.TryPipeline([]string{"CAPABILITIES"}, nil); !errors.Is(err, ErrBlockTooLong) {
			t.Fatalf("err = %v, want ErrBlockTooLong", err)
		}
	})
	t.Run("BlockTime trickle", func(t *testing.T) {
		// one short line every 20 ms, forever: the sliding guard (10 s) would never fire
		cl, sv := net.Pipe()
		go func() {
			defer sv.Close()
			for {
				if _, err := sv.Write([]byte("tick\r\n")); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		c := NewConn(cl)
		defer c.Close()
		c.Deadline, c.BlockTime = 10*time.Second, 300*time.Millisecond
		start := time.Now()
		lines, err := c.TryReadDotLines()
		if !errors.Is(err, ErrBlockTooLong) {
			t.Fatalf("err = %v, want ErrBlockTooLong", err)
		}
		if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second || len(lines) < 5 {
			t.Errorf("ended after %v with %d lines", d, len(lines))
		}
	})
	t.Run("BlockTime stalled line", func(t *testing.T) {
		cl, sv := net.Pipe()
		go func() {
			defer sv.Close()
			_, _ = sv.Write([]byte("start of a line that never ends"))
			buf := make([]byte, 1)
			_, _ = sv.Read(buf) // until the client closes
		}()
		c := NewConn(cl)
		defer c.Close()
		c.Deadline, c.BlockTime = 10*time.Second, 200*time.Millisecond
		start := time.Now()
		_, err := c.TryReadDotLines()
		if !errors.Is(err, ErrBlockTooLong) {
			t.Fatalf("err = %v, want ErrBlockTooLong", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("ended after %v, the line's deadline was not clamped to the block's", d)
		}
	})
	t.Run("BlockTime does not hurt a block that ends", func(t *testing.T) {
		var b bytes.Buffer
		for range 5000 {
			b.WriteString("fine\r\n")
		}
		p := newPeer(t, scripted("200 hi\r\n101 caps\r\n"+b.String()+".\r\n"))
		c := Dial(t, p.addr())
		c.ExpectStatus(101, "caps")
		lines, err := c.TryReadDotLines()
		if err != nil || len(lines) != 5000 {
			t.Fatalf("%d lines, %v", len(lines), err)
		}
	})
}

// TestPipelineWriterDuringSlowBlock: the server reads the second command only after a block
// that lasts longer than the hang guard while its lines keep coming. The pipeline's writer is
// blocked all that time and must keep its sliding hang guard, as before the block bound.
func TestPipelineWriterDuringSlowBlock(t *testing.T) {
	cl, sv := net.Pipe()
	go func() {
		defer sv.Close()
		_ = sv.SetDeadline(time.Now().Add(10 * time.Second))
		readCmd := func() bool { // one byte at a time: the writer stays blocked on the rest
			b := make([]byte, 1)
			for {
				if _, err := sv.Read(b); err != nil {
					return false
				}
				if b[0] == '\n' {
					return true
				}
			}
		}
		if !readCmd() {
			return
		}
		if _, err := sv.Write([]byte("215 list follows\r\n")); err != nil {
			return
		}
		for end := time.Now().Add(1200 * time.Millisecond); time.Now().Before(end); {
			if _, err := sv.Write([]byte("alt.test 2 1 y\r\n")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := sv.Write([]byte(".\r\n")); err != nil || !readCmd() {
			return
		}
		_, _ = sv.Write([]byte("111 20261116000000\r\n"))
	}()
	c := NewConn(cl)
	c.Deadline = 400 * time.Millisecond // lines come every 20 ms: a wide margin on a busy host
	c.BlockTime = 5 * time.Second
	t.Cleanup(c.Close)
	replies, err := c.TryPipeline([]string{"LIST", "DATE"}, nil)
	if err != nil {
		t.Fatalf("pipeline: %v (%d replies)", err, len(replies))
	}
	if len(replies) != 2 || replies[0].Code != 215 || len(replies[0].Lines) < 5 || replies[1].Code != 111 {
		t.Errorf("replies = %+v", replies)
	}
}

// TestBlockCapExact: a block of exactly MaxBlock bytes (CRLF lines, before the terminator)
// passes in both block readers, one byte more does not.
func TestBlockCapExact(t *testing.T) {
	for _, tc := range []struct {
		line string // one content line without CRLF
		ok   bool
	}{
		{strings.Repeat("a", 18), true},  // 20 bytes with CRLF
		{strings.Repeat("a", 19), false}, // 21
	} {
		for _, raw := range []bool{false, true} {
			p := newPeer(t, scripted("200 hi\r\n"+tc.line+"\r\n.\r\n"+"211 next\r\n"))
			c := Dial(t, p.addr())
			c.MaxBlock = 20
			var err error
			if raw {
				_, err = c.TryRawUntilDot()
			} else {
				_, err = c.TryReadDotLines()
			}
			switch {
			case tc.ok && err != nil:
				t.Errorf("len %d raw %v: err = %v", len(tc.line), raw, err)
			case !tc.ok && !errors.Is(err, ErrBlockTooLong):
				t.Errorf("len %d raw %v: err = %v, want ErrBlockTooLong", len(tc.line), raw, err)
			}
			if tc.ok {
				if code, _, err := c.ReadStatus(); err != nil || code != 211 {
					t.Errorf("len %d raw %v: next line: %d %v", len(tc.line), raw, code, err)
				}
			}
		}
	}
}

// TestBlockCapWithoutLineCap: with MaxLine < 0, MaxBlock still bounds a block line that never
// ends, in both block readers (council r1).
func TestBlockCapWithoutLineCap(t *testing.T) {
	for _, raw := range []bool{false, true} {
		c, n := endlessServer(t, "", strings.Repeat("x", 8<<10))
		c.MaxLine = -1
		c.MaxBlock = 100 << 10
		var err error
		if raw {
			_, err = c.TryRawUntilDot()
		} else {
			_, err = c.TryReadDotLines()
		}
		if !errors.Is(err, ErrBlockTooLong) {
			t.Errorf("raw %v: err = %v, want ErrBlockTooLong", raw, err)
		}
		bounded(t, n, int64(c.MaxBlock)+slack+10_000)
	}
}

// TestReadUntilQuietDefaultCap: with limit 0, TryReadUntilQuiet is bounded by MaxBlock: an
// endless sender gives ErrBlockTooLong soon after the cap, exactly MaxBlock bytes then quiet is
// fine, and MaxBlock < 0 lifts the bound (the hang guard ends the read).
func TestReadUntilQuietDefaultCap(t *testing.T) {
	t.Run("endless", func(t *testing.T) {
		c, n := endlessServer(t, "", strings.Repeat("x", 8<<10))
		c.MaxBlock = 100 << 10
		got, err := c.TryReadUntilQuiet(time.Second, 0)
		if !errors.Is(err, ErrBlockTooLong) || len(got) > c.MaxBlock+1 {
			t.Errorf("err = %v, %d bytes, want ErrBlockTooLong and at most %d", err, len(got), c.MaxBlock+1)
		}
		bounded(t, n, int64(c.MaxBlock)+slack+10_000)
	})
	for _, tc := range []struct {
		size int
		ok   bool
	}{{1000, true}, {1001, false}} {
		p := newPeer(t, scripted("200 hi\r\n"+strings.Repeat("a", tc.size)))
		c := Dial(t, p.addr())
		c.MaxBlock = 1000
		got, err := c.TryReadUntilQuiet(200*time.Millisecond, 0)
		switch {
		case tc.ok && (err != nil || len(got) != tc.size):
			t.Errorf("size %d: %d bytes, err %v", tc.size, len(got), err)
		case !tc.ok && !errors.Is(err, ErrBlockTooLong):
			t.Errorf("size %d: err = %v, want ErrBlockTooLong", tc.size, err)
		}
	}
}

// TestQuietBound: the bound of TryReadUntilQuiet: an explicit limit, MaxBlock (+1) for limit 0,
// none for MaxBlock < 0 or one that would overflow.
func TestQuietBound(t *testing.T) {
	for _, tc := range []struct {
		maxBlock, limit, want int
		capped                bool
	}{
		{0, 5, 5, false},
		{100, 5, 5, false},
		{0, 0, DefaultMaxBlock + 1, true},
		{1000, 0, 1001, true},
		{1000, -1, 1001, true},
		{-1, 0, 0, false},
		{math.MaxInt, 0, 0, false},
	} {
		c := &Conn{MaxBlock: tc.maxBlock}
		if got, capped := c.quietBound(tc.limit); got != tc.want || capped != tc.capped {
			t.Errorf("MaxBlock %d limit %d: %d %v, want %d %v", tc.maxBlock, tc.limit, got, capped, tc.want, tc.capped)
		}
	}
}

// TestQuoteCut: an error message quotes at most 256 bytes of the data.
func TestQuoteCut(t *testing.T) {
	if got := quote([]byte("ab")); got != `"ab"` {
		t.Errorf("short: %s", got)
	}
	got := quote(bytes.Repeat([]byte{0xff}, 1000))
	if !strings.HasSuffix(got, "... (744 more bytes)") || len(got) > 256*4+40 {
		t.Errorf("long: %d bytes, %.60s", len(got), got)
	}
}

// TestReadUntilQuietMessageCut: ReadUntilQuiet's failure after the cap quotes a short part of
// the data, not all of it (a Conn without tb panics with the message).
func TestReadUntilQuietMessageCut(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n"+strings.Repeat("z", 5000)))
	c, err := TryDial(p.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.MaxBlock = 2000
	var msg string
	func() {
		defer func() { msg, _ = recover().(string) }()
		c.ReadUntilQuiet(200*time.Millisecond, 0)
	}()
	if !strings.Contains(msg, "more than 2000 bytes") || !strings.Contains(msg, "more bytes)") || len(msg) > 1000 {
		t.Errorf("message (%d bytes): %.200q", len(msg), msg)
	}
}
