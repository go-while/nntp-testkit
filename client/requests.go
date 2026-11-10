package client

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StuffBytes returns article as a client sends it after POST, IHAVE or TAKETHIS: every line
// that starts with "." gets a second "." in front, a final CRLF is added when the article does
// not end in one, and the terminator ".\r\n" follows. article is in wire form: CRLF line ends,
// no stuffing, no terminator (what the article package's Bytes returns). Nothing else is
// changed: a bare CR, a bare LF, a NUL or an 8-bit byte stays as it is. A line starts after a
// CRLF only (a lone LF is not a line end for the stuffing, and is not at the start of the
// article either unless the article begins with "."). A server that is lenient and takes a bare
// LF as a line end (innd does) sees a "." after a bare LF as unstuffed: use CRLF in articles
// that are meant to be stuffed. An article that ends in a bare CR gets "\r\n" appended
// ("a\r" becomes "a\r\r\n"): bytes are never changed, only added. An empty article gives
// ".\r\n". An article that ends in "\r\n" gains nothing but the terminator.
func StuffBytes(article []byte) []byte {
	out := make([]byte, 0, len(article)+16)
	rest := article
	for len(rest) > 0 {
		if rest[0] == '.' {
			out = append(out, '.')
		}
		i := bytes.Index(rest, crlf)
		if i < 0 {
			out = append(out, rest...)
			out = append(out, crlf...)
			break
		}
		out = append(out, rest[:i+2]...)
		rest = rest[i+2:]
	}
	return append(out, ".\r\n"...)
}

var crlf = []byte("\r\n")

// sendBytes writes raw to the connection in one write under the hang guard.
func (c *Conn) sendBytes(raw []byte) error {
	c.bump()
	_, err := c.Conn.Write(raw)
	return err
}

// tryTwoStep is POST and IHAVE: send cmd; when the first reply is want (340, 335) send the
// article and return the final reply, else return the first one. A refusal is not an error.
func (c *Conn) tryTwoStep(cmd string, want int, article []byte) (int, string, error) {
	code, line, err := c.TryCmd("%s", cmd)
	if err != nil || code != want {
		return code, line, err
	}
	if err := c.sendBytes(StuffBytes(article)); err != nil {
		return code, line, fmt.Errorf("%s: sending the article: %w", cmd, err)
	}
	code, line, err = c.ReadStatus()
	if err != nil {
		err = fmt.Errorf("%s: reply to the article: %w", cmd, err)
	}
	return code, line, err
}

// TryPost sends POST; on 340 it sends article (see StuffBytes) and returns the final reply
// (240, 441, 440 ...), otherwise the first reply (440, 480, 502 ...). A refusal is a result,
// not an error: err is set only for an I/O, parse or strict-mode error, with the code and line
// of the last reply read.
func (c *Conn) TryPost(article []byte) (code int, line string, err error) {
	return c.tryTwoStep("POST", 340, article)
}

// TryIhave sends "IHAVE id" (id is the message-ID as it goes on the wire, angle brackets
// included, sent as given); on 335 it sends article and returns the final reply (235, 436,
// 437), otherwise the first reply (435, 436, 480 ...). Refusals are results, not errors.
func (c *Conn) TryIhave(id string, article []byte) (code int, line string, err error) {
	return c.tryTwoStep("IHAVE "+id, 335, article)
}

// TryCheck sends "CHECK id" (RFC 4644) and returns the reply (238, 431, 438 ...).
func (c *Conn) TryCheck(id string) (code int, line string, err error) {
	return c.TryCmd("CHECK %s", id)
}

// TryTakethis sends "TAKETHIS id" and the article in one write, without waiting in between
// (RFC 4644), then reads the reply (239, 439 ...).
func (c *Conn) TryTakethis(id string, article []byte) (code int, line string, err error) {
	cmd := append([]byte("TAKETHIS "+id+"\r\n"), StuffBytes(article)...)
	if err := c.sendBytes(cmd); err != nil {
		return 0, "", fmt.Errorf("TAKETHIS %s: %w", id, err)
	}
	return c.ReadStatus()
}

// TryModeStream sends MODE STREAM and returns the reply (203, 501, 480 ...).
func (c *Conn) TryModeStream() (code int, line string, err error) {
	return c.TryCmd("MODE STREAM")
}

// TryLogin sends "AUTHINFO USER user"; on 381 it sends "AUTHINFO PASS pass". It returns the
// final reply (281 success, 481, 482, 502, 483 ...), or the first one when it is not 381 (a
// server that accepts the user alone answers 281, one that wants TLS first 483).
func (c *Conn) TryLogin(user, pass string) (code int, line string, err error) {
	code, line, err = c.TryCmd("AUTHINFO USER %s", user)
	if err != nil || code != 381 {
		return code, line, err
	}
	return c.TryCmd("AUTHINFO PASS %s", pass)
}

// failOn fails tb when err is set; what names the step.
func (c *Conn) failOn(what string, code int, line string, err error) (int, string) {
	if err != nil {
		if c.tb != nil {
			c.tb.Helper()
		}
		c.fatalf("%s: %v (last reply %d %q)", what, err, code, line)
	}
	return code, line
}

// Post is TryPost that fails tb on an I/O or protocol error (not on a refusal) and returns the
// code and line.
func (c *Conn) Post(article []byte) (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryPost(article)
	return c.failOn("POST", code, line, err)
}

// Ihave is TryIhave that fails tb on an I/O or protocol error (not on a refusal).
func (c *Conn) Ihave(id string, article []byte) (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryIhave(id, article)
	return c.failOn("IHAVE "+id, code, line, err)
}

// Check is TryCheck that fails tb on an I/O or protocol error.
func (c *Conn) Check(id string) (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryCheck(id)
	return c.failOn("CHECK "+id, code, line, err)
}

// Takethis is TryTakethis that fails tb on an I/O or protocol error.
func (c *Conn) Takethis(id string, article []byte) (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryTakethis(id, article)
	return c.failOn("TAKETHIS "+id, code, line, err)
}

// ModeStream is TryModeStream that fails tb on an I/O or protocol error.
func (c *Conn) ModeStream() (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryModeStream()
	return c.failOn("MODE STREAM", code, line, err)
}

// Login is TryLogin that fails tb on an I/O or protocol error (not on 481 and the like).
func (c *Conn) Login(user, pass string) (int, string) {
	if c.tb != nil {
		c.tb.Helper()
	}
	code, line, err := c.TryLogin(user, pass)
	return c.failOn("AUTHINFO "+user, code, line, err)
}

// Reply is one answer of Pipeline: the command it answers, the status line and, for a
// multi-line reply, the block (un-stuffed, line ends stripped, without the terminator). Cmd is
// the first line of the pipeline entry (an entry that carries an article, "TAKETHIS <id>\r\n"+
// article, gives "TAKETHIS <id>"), and it is what the multi function gets.
type Reply struct {
	Cmd   string
	Code  int
	Line  string
	Lines []string // nil for a single-line reply
}

// MultiLine reports whether a reply with this status code to cmd is followed by a dot block
// (RFC 3977, 4644 and 2980): 100, 101, 215, 220-222, 224, 225, 230, 231, 282, and 211 after
// LISTGROUP. It is what Pipeline uses when it gets no multi function.
func MultiLine(cmd string, code int) bool {
	switch code {
	case 100, 101, 215, 220, 221, 222, 224, 225, 230, 231, 282:
		return true
	case 211:
		return len(cmd) >= 9 && strings.EqualFold(cmd[:9], "LISTGROUP") && (len(cmd) == 9 || cmd[9] == ' ')
	}
	return false
}

// TryPipeline sends all cmds (each followed by CRLF; an entry that already ends in CRLF goes
// out as it is, so "TAKETHIS <id>\r\n"+StuffBytes(article) pipelines an article) and reads one
// reply per entry, in order. The commands are written by another goroutine while this one
// reads, so a server that stops reading until its replies are read (a full socket buffer in
// both directions) cannot deadlock the test. multi tells whether the reply with that code to
// that command is followed by a dot block (nil: MultiLine). The hang guard is the Conn's
// deadline, moved on with every line read, so a stalled server fails after one deadline. A
// block has an absolute bound as well, so a server that never sends the "." cannot keep the
// test alive by trickling lines: it must end within 10 times the deadline and hold at most
// 64 MiB, else the call fails. On an error the replies read so far are returned with it.
func (c *Conn) TryPipeline(cmds []string, multi func(cmd string, code int) bool) ([]Reply, error) {
	if multi == nil {
		multi = MultiLine
	}
	var buf bytes.Buffer
	for _, cmd := range cmds {
		buf.WriteString(cmd)
		if !bytes.HasSuffix([]byte(cmd), crlf) {
			buf.WriteString("\r\n")
		}
	}
	c.bump()
	werr := make(chan error, 1)
	go func() {
		_, err := c.Conn.Write(buf.Bytes())
		werr <- err
	}()
	replies := make([]Reply, 0, len(cmds))
	var rerr error
	for _, entry := range cmds {
		cmd := entry
		if i := strings.Index(cmd, "\r\n"); i >= 0 {
			cmd = cmd[:i]
		}
		code, line, err := c.ReadStatus()
		if err != nil {
			rerr = fmt.Errorf("pipeline: reply %d (%q): %w", len(replies)+1, cmd, err)
			break
		}
		r := Reply{Cmd: cmd, Code: code, Line: line}
		if multi(cmd, code) {
			lines, err := c.readBlockBounded()
			if err != nil {
				rerr = fmt.Errorf("pipeline: block of reply %d (%q): %w", len(replies)+1, cmd, err)
				break
			}
			r.Lines = lines
			if r.Lines == nil {
				r.Lines = []string{}
			}
		}
		replies = append(replies, r)
	}
	if rerr != nil {
		_ = c.Conn.SetWriteDeadline(time.Now()) // unblock the writer
	}
	wr := <-werr
	c.bump()
	if rerr != nil {
		return replies, rerr
	}
	if wr != nil {
		return replies, fmt.Errorf("pipeline: writing the commands: %w", wr)
	}
	return replies, nil
}

// Pipeline is TryPipeline that fails tb on any error.
func (c *Conn) Pipeline(cmds []string, multi func(cmd string, code int) bool) []Reply {
	if c.tb != nil {
		c.tb.Helper()
	}
	replies, err := c.TryPipeline(cmds, multi)
	if err != nil {
		c.fatalf("%v (%d of %d replies read)", err, len(replies), len(cmds))
	}
	return replies
}

// maxBlockBytes and blockFactor are the absolute bounds of a dot block in TryPipeline.
const (
	maxBlockBytes = 64 << 20
	blockFactor   = 10
)

// readBlockBounded is TryReadDotLines with an absolute bound: the block must end within
// blockFactor hang guards and hold at most maxBlockBytes, whatever the sliding guard says.
func (c *Conn) readBlockBounded() ([]string, error) {
	limit := time.Now().Add(blockFactor * c.deadline())
	size := 0
	var out []string
	for {
		if time.Now().After(limit) || size > maxBlockBytes {
			return out, &StrictError{Kind: NoTerminator, Err: fmt.Errorf(
				"block still going after %d lines, %d bytes, %v (bound: %d times the deadline, %d bytes)",
				len(out), size, blockFactor*c.deadline(), blockFactor, maxBlockBytes)}
		}
		line, err := c.ReadLine()
		if err != nil {
			var se *StrictError
			if errors.As(err, &se) {
				if se.Kind == NoCRLF {
					return out, &StrictError{Kind: NoTerminator, Line: se.Line,
						Err: fmt.Errorf("after %d lines: %w", len(out), se.Err)}
				}
				return out, err
			}
			err = fmt.Errorf("after %d lines: %w", len(out), err)
			if c.Strict {
				return out, &StrictError{Kind: NoTerminator, Line: line, Err: err}
			}
			return out, err
		}
		if line == "." {
			return out, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		} else if c.Strict && strings.HasPrefix(line, ".") {
			return out, &StrictError{Kind: BadDotStuffing, Line: line + "\r\n"}
		}
		size += len(line) + 2
		out = append(out, line)
	}
}
