package fakeserver

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/tlscert"
)

// capDial connects to s and reads the greeting.
func capDial(t *testing.T, s *Server) net.Conn {
	t.Helper()
	nc, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	if _, err := nc.Read(buf); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return nc
}

// closedByServer reads until the server closes the connection (EOF or a reset) and fails when
// it does not within the deadline of capDial.
func closedByServer(t *testing.T, nc net.Conn) {
	t.Helper()
	_, err := io.Copy(io.Discard, nc)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the connection stayed open")
	}
}

// writeIgnoringClose writes b; the write may fail once the server closed (a reset).
func writeIgnoringClose(nc net.Conn, b []byte) { _, _ = nc.Write(b) }

func noteMatches(t *testing.T, s *Server, sub string) {
	t.Helper()
	s.Close() // the connection goroutines are done: the notes are complete
	if n := s.Notes(); len(n) != 1 || !strings.Contains(n[0], sub) {
		t.Errorf("Notes = %q, want one holding %q", n, sub)
	}
}

// TestMaxLineModes: a client line over the cap closes the connection in every mode and is
// noted; the line is not recorded.
func TestMaxLineModes(t *testing.T) {
	modes := map[string]Options{
		"script":      {},
		"script-step": {Script: []Step{{Expect: "HELLO", Reply: []string{"200 x"}}, {}}},
		"store":       {Store: NewStore()},
		"peer":        {Handler: PeerHandler(&PeerOptions{})},
		"linehandler": {Handler: LineHandler(func(*Conn, string) ([]string, bool) { return []string{"500 no"}, false })},
	}
	for name, o := range modes {
		t.Run(name, func(t *testing.T) {
			o.MaxLine = 200
			s := New(t, o)
			nc := capDial(t, s)
			writeIgnoringClose(nc, []byte("A"+strings.Repeat("a", 200)+"\r\n"))
			closedByServer(t, nc)
			noteMatches(t, s, "over Options.MaxLine (200 bytes)")
			if l := s.Lines(); len(l) != 0 || len(s.Mismatches()) != 0 {
				t.Errorf("the long line was recorded: %q %q", l, s.Mismatches())
			}
		})
	}
}

// TestMaxLineNoNewline: a line that never ends is given up on at the cap, not buffered: the
// default cap and a cap above the 64 KiB read buffer (several fragments).
func TestMaxLineNoNewline(t *testing.T) {
	for _, tc := range []struct{ max, want int }{{0, DefaultMaxLine}, {3 << 16, 3 << 16}} {
		s := New(t, Options{MaxLine: tc.max})
		nc := capDial(t, s)
		go func() { // endless: ends when the server closes
			chunk := []byte(strings.Repeat("z", 8<<10))
			for {
				if _, err := nc.Write(chunk); err != nil {
					return
				}
			}
		}()
		closedByServer(t, nc)
		noteMatches(t, s, "over Options.MaxLine")
	}
}

// TestMaxLineBoundary: a line of exactly the cap with its line end passes (CRLF and bare LF),
// one byte more does not; negative lifts the cap.
func TestMaxLineBoundary(t *testing.T) {
	for _, tc := range []struct {
		max, length int // length: the line without its CRLF
		ok          bool
	}{
		{100, 98, true}, {100, 99, false},
		{0, DefaultMaxLine - 2, true}, {0, DefaultMaxLine - 1, false},
		{-1, 5 * DefaultMaxLine, true},
	} {
		got := make(chan error, 2)
		s := New(t, Options{MaxLine: tc.max, Handler: func(c *Conn) {
			line, err := c.ReadLine()
			if err == nil && len(line) != tc.length {
				err = errors.New("wrong length")
			}
			got <- err
			if err == nil {
				_, err = c.ReadLine() // the reader is in step: the next line is read whole
				got <- err
			}
		}})
		nc := capDial(t, s)
		writeIgnoringClose(nc, []byte(strings.Repeat("y", tc.length)+"\r\nnext\r\n"))
		err := <-got
		if tc.ok && err != nil {
			t.Errorf("max %d length %d: %v", tc.max, tc.length, err)
		}
		if !tc.ok && !errors.Is(err, ErrLineTooLong) {
			t.Errorf("max %d length %d: err %v, want ErrLineTooLong", tc.max, tc.length, err)
		}
		if tc.ok {
			if err := <-got; err != nil {
				t.Errorf("max %d length %d: next line: %v", tc.max, tc.length, err)
			}
		}
	}
}

// TestMaxLineBareLF: a bare LF line end counts as one byte.
func TestMaxLineBareLF(t *testing.T) {
	for _, tc := range []struct {
		length int
		ok     bool
	}{{99, true}, {100, false}} {
		got := make(chan error, 1)
		s := New(t, Options{MaxLine: 100, Handler: func(c *Conn) {
			_, err := c.ReadLine()
			got <- err
		}})
		nc := capDial(t, s)
		writeIgnoringClose(nc, []byte(strings.Repeat("y", tc.length)+"\n"))
		if err := <-got; (err == nil) != tc.ok {
			t.Errorf("length %d + LF: err %v, want ok=%v", tc.length, err, tc.ok)
		}
	}
}

// TestMaxLineInBlock: the lines of a dot block (POST, TAKETHIS) are capped too, and so is the
// block as a whole (MaxBlock); both end ReadDotBlock with an error and a note.
func TestMaxLineInBlock(t *testing.T) {
	t.Run("long line", func(t *testing.T) {
		errc := make(chan error, 1)
		s := New(t, Options{MaxLine: 100, Handler: func(c *Conn) {
			_, err := c.ReadDotBlock()
			errc <- err
		}})
		nc := capDial(t, s)
		writeIgnoringClose(nc, []byte("ok line\r\n"+strings.Repeat("b", 100)+"\r\n"))
		if err := <-errc; !errors.Is(err, ErrLineTooLong) {
			t.Errorf("err = %v", err)
		}
		closedByServer(t, nc)
		noteMatches(t, s, "over Options.MaxLine")
		if len(s.Articles()) != 0 {
			t.Errorf("a cut block was recorded: %q", s.Articles())
		}
	})
	t.Run("endless block", func(t *testing.T) {
		errc := make(chan error, 1)
		s := New(t, Options{MaxBlock: 10_000, Handler: func(c *Conn) {
			_, raw, err := c.ReadDotBlockRaw()
			if err != nil && len(raw) > 10_000 {
				err = errors.New("held more than the cap")
			}
			errc <- err
		}})
		nc := capDial(t, s)
		go func() {
			chunk := []byte(strings.Repeat("a short line\r\n", 100))
			for {
				if _, err := nc.Write(chunk); err != nil {
					return
				}
			}
		}()
		if err := <-errc; !errors.Is(err, ErrBlockTooLong) {
			t.Errorf("err = %v", err)
		}
		closedByServer(t, nc)
		noteMatches(t, s, "over Options.MaxBlock (10000 bytes)")
	})
	t.Run("block at the cap", func(t *testing.T) {
		errc := make(chan error, 1)
		s := New(t, Options{MaxBlock: 20, Handler: func(c *Conn) {
			_, _, err := c.ReadDotBlockRaw()
			errc <- err
		}})
		nc := capDial(t, s)
		writeIgnoringClose(nc, []byte("0123456789012345\r\n.\r\n")) // 18 bytes before the terminator
		if err := <-errc; err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("exactly MaxBlock", func(t *testing.T) {
		// MaxBlock 20 counts the wire bytes before the terminator: 20 pass, 21 do not, with CRLF
		// and with bare LF line ends
		for _, tc := range []struct {
			block string
			ok    bool
		}{
			{"012345678901234567\r\n.\r\n", true},
			{"0123456789012345678\r\n.\r\n", false},
			{"0123456789012345678\n.\n", true},
			{"01234567890123456789\n.\n", false},
			{"012345678\r\n0123456\r\n.\r\n", true},   // 11 + 9 bytes
			{"012345678\r\n01234567\r\n.\r\n", false}, // 11 + 10
		} {
			errc := make(chan error, 1)
			s := New(t, Options{MaxBlock: 20, Handler: func(c *Conn) {
				_, _, err := c.ReadDotBlockRaw()
				errc <- err
			}})
			nc := capDial(t, s)
			writeIgnoringClose(nc, []byte(tc.block))
			var err error
			select {
			case err = <-errc:
			case <-time.After(10 * time.Second):
				t.Fatalf("%q: the block read did not end", tc.block)
			}
			if tc.ok && err != nil || !tc.ok && !errors.Is(err, ErrBlockTooLong) {
				t.Errorf("%q: err = %v, want ok %v", tc.block, err, tc.ok)
			}
		}
	})
	t.Run("store POST", func(t *testing.T) {
		s := New(t, Options{Store: testStore(), MaxBlock: 1000})
		c := client.Dial(t, s.Addr())
		c.ExpectCmd(340, "POST")
		c.MustSend("Subject: " + strings.Repeat("s", 2000) + "\r\n")
		if _, err := c.ReadLine(); err == nil {
			t.Error("the connection stayed open")
		}
		noteMatches(t, s, "over Options.MaxBlock")
	})
}

// TestMaxLineCompressed: the cap applies to the inflated stream as well (a deflate bomb of one
// endless line).
func TestMaxLineCompressed(t *testing.T) {
	s := New(t, Options{Store: testStore(), StoreOpts: StoreOptions{Compress: true}, MaxLine: 1000})
	c := client.Dial(t, s.Addr())
	c.Compress()
	if err := c.SendBytes([]byte(strings.Repeat("x", 5000) + "\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadLine(); err == nil {
		t.Error("the connection stayed open")
	}
	noteMatches(t, s, "over Options.MaxLine (1000 bytes)")
}

// TestNotesEmpty: a client within the caps leaves no note.
func TestNotesEmpty(t *testing.T) {
	s := New(t, Options{Store: testStore()})
	c := client.Dial(t, s.Addr())
	c.ExpectCmd(111, "DATE")
	c.Close()
	s.Close()
	if n := s.Notes(); len(n) != 0 {
		t.Errorf("Notes = %q", n)
	}
}

// TestMaxBlockBoundsLineWithoutCap: with the line cap lifted (MaxLine < 0), MaxBlock still
// bounds a block line that never ends (council r1).
func TestMaxBlockBoundsLineWithoutCap(t *testing.T) {
	errc := make(chan error, 1)
	s := New(t, Options{MaxLine: -1, MaxBlock: 10_000, Handler: func(c *Conn) {
		_, _, err := c.ReadDotBlockRaw()
		errc <- err
	}})
	nc := capDial(t, s)
	go func() { // endless, no LF: ends when the server closes
		chunk := []byte(strings.Repeat("x", 4096))
		for {
			if _, err := nc.Write(chunk); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrBlockTooLong) {
			t.Errorf("err = %v, want ErrBlockTooLong", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the block read did not stop")
	}
	closedByServer(t, nc)
	noteMatches(t, s, "over Options.MaxBlock (10000 bytes)")
}

// TestMaxLineNoWait: a line over a small cap is given up as soon as its bytes arrive, without
// waiting for an LF or a full 64 KiB buffer, while the client stays connected (council r1).
func TestMaxLineNoWait(t *testing.T) {
	s := New(t, Options{MaxLine: 100})
	nc := capDial(t, s)
	_ = nc.SetDeadline(time.Now().Add(3 * time.Second))
	writeIgnoringClose(nc, []byte(strings.Repeat("y", 101))) // no LF
	closedByServer(t, nc)
	noteMatches(t, s, "over Options.MaxLine (100 bytes)")
}

// TestCapErrorSticky: a custom Handler that ignores the cap error and reads on gets the same
// error again; the rest of the oversized line is never read as commands, and nothing it
// writes reaches the client (council r1).
func TestCapErrorSticky(t *testing.T) {
	done := make(chan []error, 1)
	s := New(t, Options{MaxLine: 100, Handler: func(c *Conn) {
		var errs []error
		for range 3 {
			_, err := c.ReadLine()
			errs = append(errs, err)
			_ = c.WriteLines("200 still here")
		}
		done <- errs
	}})
	nc := capDial(t, s)
	// the line arrives in two parts: the first (under the cap, no LF) is consumed, the cap trips
	// on the second, which stays unread; only a sticky error keeps a second read from returning
	// that tail as a command line and "DATE" as the next one
	writeIgnoringClose(nc, []byte(strings.Repeat("z", 60)))
	time.Sleep(200 * time.Millisecond)
	writeIgnoringClose(nc, []byte(strings.Repeat("z", 60)+"\r\nDATE\r\n"))
	got, _ := io.ReadAll(nc)
	var errs []error
	select {
	case errs = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler is still reading: the cap error did not stick")
	}
	for i, err := range errs {
		if !errors.Is(err, ErrLineTooLong) {
			t.Errorf("read %d: err = %v, want ErrLineTooLong", i, err)
		}
	}
	if strings.Contains(string(got), "still here") {
		t.Errorf("the handler's reply reached the client: %q", got)
	}
	if l := s.Lines(); len(l) != 0 {
		t.Errorf("lines recorded after the cap: %q", l)
	}
}

// TestCapErrorStickyUpgrades: the sticky cap error holds after COMPRESS and after STARTTLS too: a
// line that arrives in two parts is cut, a custom Handler that ignores the error and reads on
// gets it again (not the tail of the line or "DATE"), nothing it writes reaches the client, and
// no line after the upgrade is recorded.
func TestCapErrorStickyUpgrades(t *testing.T) {
	ca := tlscert.NewCA(t)
	cfg := ca.Issue(t, tlscert.Options{}).ServerConfig()
	for _, mode := range []string{"compress", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan []error, 1)
			s := New(t, Options{MaxLine: 100, Handler: func(c *Conn) {
				line, err := c.ReadLine()
				switch {
				case err != nil:
				case mode == "compress" && line == "COMPRESS DEFLATE":
					_ = c.WriteLines("206 compressing")
					err = c.StartCompress()
				case mode == "starttls" && line == "STARTTLS":
					_ = c.WriteLines("382 go ahead")
					err = c.StartTLS(cfg)
				default:
					err = errors.New("unexpected line " + line)
				}
				if err != nil {
					done <- []error{err}
					return
				}
				var errs []error
				for range 3 {
					_, err := c.ReadLine()
					errs = append(errs, err)
					_ = c.WriteLines("200 still here")
				}
				done <- errs
			}})
			cl := client.Dial(t, s.Addr())
			if mode == "compress" {
				cl.Compress()
			} else {
				cl = cl.StartTLS(&tls.Config{RootCAs: ca.Pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
			}
			// in two parts, as in TestCapErrorSticky: only a sticky error keeps the tail of the
			// line and "DATE" from being read as commands after the cut
			if err := cl.Send(strings.Repeat("z", 60)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
			_ = cl.Send(strings.Repeat("z", 60) + "\r\nDATE\r\n") // may fail once the server closed
			var errs []error
			select {
			case errs = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the handler is still reading: the cap error did not stick")
			}
			// the handler has written whatever it could before done; read it now
			got, _ := cl.TryReadUntilQuiet(200*time.Millisecond, 4096)
			if len(errs) != 3 {
				t.Fatalf("handler: %v", errs)
			}
			for i, err := range errs {
				if !errors.Is(err, ErrLineTooLong) {
					t.Errorf("read %d: err = %v, want ErrLineTooLong", i, err)
				}
			}
			if strings.Contains(string(got), "still here") {
				t.Errorf("the handler's reply reached the client: %q", got)
			}
			want := map[string]string{"compress": "COMPRESS DEFLATE", "starttls": "STARTTLS"}[mode]
			if l := s.Lines(); len(l) != 1 || l[0] != want {
				t.Errorf("lines %q, want only %q (nothing after the upgrade)", l, want)
			}
		})
	}
}
