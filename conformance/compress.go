package conformance

import (
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// compressChecks are the COMPRESS DEFLATE checks of RFC 8054, each on its own connection and
// each run only when CAPABILITIES lists COMPRESS DEFLATE (Skip with Missing "COMPRESS"
// otherwise). The checks leave check.Needs empty and test the capability themselves
// (compressReady): a server may list COMPRESS only after MODE READER or after AUTHINFO.
// A check logs in with Options.User only when COMPRESS is not listed before it (or is answered
// with 480): the capability list of an unauthenticated client is what RFC 8054 §2.2.2 makes
// rules about, so the checks stay unauthenticated whenever the server lets them.
//
// The reply of the server to COMPRESS is judged strictly where the RFC says MUST: 206 for
// DEFLATE, 503 for an algorithm the server does not know, 501 for a missing one, 502 for any
// syntactically valid COMPRESS or STARTTLS or AUTHINFO once compression is active. A 403
// ("unable to activate compression", a legal answer for any server reason) skips the check.
func compressChecks() []check {
	const rfc = "RFC 8054 §2.2.2"
	return []check{
		{Name: "compress", RFC: rfc, Run: checkCompress},
		{Name: "compress-unknown", RFC: rfc + " (501/503)", Run: checkCompressUnknown},
		{Name: "compress-twice", RFC: rfc + " (502)", Run: checkCompressTwice},
		{Name: "compress-upload", RFC: rfc, Posts: true, Run: compressUpload(false)},
		{Name: "compress-upload-lines", RFC: rfc, Posts: true, Run: compressUpload(true)},
	}
}

// unknownAlgorithm is a syntactically valid compression algorithm name no server knows.
const unknownAlgorithm = "NNTPTESTKIT"

// switchReader sends MODE READER when the server lists MODE-READER but not READER, and reads
// CAPABILITIES again (what ready does, without the login).
func (s *session) switchReader() error {
	if s.has("READER") || !s.has("MODE-READER") {
		return nil
	}
	if _, _, err := s.single([]int{200, 201}, "MODE READER"); err != nil {
		return err
	}
	if _, err := s.capabilities(); err != nil {
		return fmt.Errorf("CAPABILITIES after MODE READER: %s", describe(err))
	}
	return nil
}

// capReady makes the session list capability kw: it switches to reader mode when kw is not
// listed and the server offers MODE-READER, then (with login) logs in when kw is still not
// listed and Options.User is set, reading CAPABILITIES again after each. A capability that is
// still missing skips the check (notAdvertised). The login is never done when kw is already
// listed: STARTTLS (RFC 4642) is not valid after it and COMPRESS tests want the
// unauthenticated list.
func (s *session) capReady(c check, kw string, login bool) (Result, bool) {
	if !s.has(kw) {
		if err := s.switchReader(); err != nil {
			return c.fail(err.Error()), false
		}
	}
	if !s.has(kw) && login && s.o.User != "" && !s.authed {
		done, err := s.login()
		if err != nil {
			return c.fail(err.Error()), false
		}
		if done {
			if _, err := s.capabilities(); err != nil {
				return c.failf("CAPABILITIES after AUTHINFO: %s", describe(err)), false
			}
		}
	}
	if !s.has(kw) {
		return c.notAdvertised(kw), false
	}
	return Result{}, true
}

// deflateReady is capReady for COMPRESS plus the DEFLATE argument that RFC 8054 §2.1 makes
// mandatory. A list without it fails check compress and skips the others (so it is reported
// once).
func (s *session) deflateReady(c check) (Result, bool) {
	if r, ok := s.capReady(c, "COMPRESS", true); !ok {
		return r, false
	}
	args := capArgs(s.caps, "COMPRESS")
	if !slices.ContainsFunc(args, func(a string) bool { return strings.EqualFold(a, "DEFLATE") }) {
		why := fmt.Sprintf("COMPRESS is listed with %q, without DEFLATE, which RFC 8054 §2.1 requires every server to support and list", strings.Join(args, " "))
		if c.Name == "compress" {
			return c.fail(why), false
		}
		r := c.skip("COMPRESS DEFLATE not advertised (see check compress): " + why)
		r.Missing = "COMPRESS DEFLATE"
		return r, false
	}
	return Result{}, true
}

// cerr formats err of a command on a compressed session with a hint at the usual cause: a
// stream that does not inflate means the server's output is not (or not only) DEFLATE, a
// timeout that it did not inflate what the client sent.
func (s *session) cerr(err error) string {
	msg := describe(err)
	if !s.c.Compressed() {
		return msg
	}
	var ce flate.CorruptInputError
	switch {
	case errors.As(err, &ce) || strings.Contains(msg, "corrupt compressed stream"):
		return msg + " (after the 206 the server's output is not a DEFLATE stream: it did not compress it)"
	case errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(msg, "i/o timeout"):
		return msg + " (no decodable reply within the hang guard after the 206: the server may not have compressed its output, or may not inflate what the client sends)"
	}
	return msg
}

// compressOn sends COMPRESS DEFLATE and switches the session's client to compression. When it
// returns false the Result ends the check: 403 (the server cannot activate compression) and
// 480 without Options.User skip it, anything but 206 fails it. A 480 with Options.User is
// answered with the login and a second try. Nothing may follow the 206 on the wire before the
// client has sent a command.
func (s *session) compressOn(c check) (Result, bool) {
	code, line, err := s.cmd("COMPRESS DEFLATE")
	if err == nil && code == 480 && s.o.User != "" && !s.authed {
		if _, lerr := s.login(); lerr != nil {
			return c.fail(lerr.Error()), false
		}
		code, line, err = s.cmd("COMPRESS DEFLATE")
	}
	switch {
	case err != nil:
		return c.failf("COMPRESS DEFLATE: %s", describe(err)), false
	case code == 206:
	case code == 403:
		return c.skipf("COMPRESS DEFLATE: %q: the server is unable to activate compression (a legal answer, RFC 8054 §2.2.2)", line), false
	case code == 480:
		return c.skipf("COMPRESS DEFLATE: %q, and no Options.User to authenticate with", line), false
	default:
		return c.failf("COMPRESS DEFLATE: got %q, want 206 (COMPRESS DEFLATE is advertised)", line), false
	}
	if err := s.c.StartCompress(); err != nil {
		return c.failf("COMPRESS DEFLATE: switching the client to compression: %v", err), false
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after the 206: %s", s.cerr(err)), false
	}
	return Result{}, true
}

// stripped lists the capabilities RFC 8054 §2.2.2 (and RFC 4642 for MODE-READER, which both
// RFCs treat alike) forbids once a compression layer is active.
var stripped = []string{"COMPRESS", "STARTTLS", "MODE-READER"}

func checkCompress(s *session, c check) Result {
	if r, ok := s.deflateReady(c); !ok {
		return r
	}
	overviewFmt := slices.ContainsFunc(capArgs(s.caps, "LIST"), func(a string) bool { return strings.EqualFold(a, "OVERVIEW.FMT") })
	if r, ok := s.compressOn(c); !ok {
		return r
	}

	// CAPABILITIES is the first multi-line reply and the first command in each direction
	lines, err := s.capabilities()
	if err != nil {
		return c.failf("CAPABILITIES over COMPRESS: %s", s.cerr(err))
	}
	if _, err := client.ParseCapabilities(lines); err != nil {
		return c.failf("CAPABILITIES over COMPRESS: %v", err)
	}
	for _, kw := range stripped {
		if s.has(kw) {
			return c.failf("CAPABILITIES lists %q while compression is active (RFC 8054 §2.2.2: COMPRESS, STARTTLS and MODE-READER must not be listed)", kw)
		}
	}
	if args := capArgs(s.caps, "AUTHINFO"); !s.authed && len(args) > 0 {
		return c.failf("CAPABILITIES lists \"AUTHINFO %s\" to an unauthenticated client while compression is active (RFC 8054 §2.2.2: AUTHINFO with no arguments, or not at all)", strings.Join(args, " "))
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after CAPABILITIES over COMPRESS: %s", s.cerr(err))
	}

	// more replies: a single line, multi-line blocks
	if _, _, err := s.single([]int{111}, "DATE"); err != nil {
		return c.failf("over COMPRESS: %s", s.cerr(err))
	}
	if _, _, err := s.multi(100, "HELP"); err != nil {
		return c.failf("over COMPRESS: %s", s.cerr(err))
	}
	if overviewFmt {
		if _, _, err := s.multi(215, "LIST OVERVIEW.FMT"); err != nil {
			return c.failf("over COMPRESS: %s", s.cerr(err))
		}
	}

	// commands pipelined in one write (one compressed block with several commands), then the
	// same in separate writes sent back to back (several compressed blocks in one segment)
	if err := compressPipelined(s); err != nil {
		return c.fail(err.Error())
	}
	if err := compressBackToBack(s); err != nil {
		return c.fail(err.Error())
	}

	// QUIT: 205, then the server closes (output it did not flush before closing is lost, so
	// a missing 205 is a server that closes without flushing its compressor)
	if _, _, err := s.expect([]int{205}, "QUIT"); err != nil {
		return c.failf("over COMPRESS: %s", s.cerr(err))
	}
	b, err := s.c.R.ReadByte()
	switch {
	case err == nil:
		return c.failf("over COMPRESS: after 205 the byte %q came instead of the connection closing", b)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed):
		return c.pass()
	case errors.Is(err, os.ErrDeadlineExceeded):
		return c.failf("over COMPRESS: after 205 the server did not close the connection within the hang guard (%v)", s.o.deadline())
	default:
		return c.failf("over COMPRESS: after 205: %s", s.cerr(err))
	}
}

// compressPipelined sends DATE, HELP and DATE in one write, then a hundred DATEs in one
// write, and checks the replies.
func compressPipelined(s *session) error {
	replies, err := s.c.TryPipeline([]string{"DATE", "HELP", "DATE"}, nil)
	if err != nil {
		return fmt.Errorf("over COMPRESS: DATE, HELP, DATE pipelined in one write: %s", s.cerr(err))
	}
	for i, want := range []int{111, 100, 111} {
		if replies[i].Code != want {
			return fmt.Errorf("over COMPRESS: pipelined %s: got %q, want %d", replies[i].Cmd, replies[i].Line, want)
		}
	}
	if len(replies[1].Lines) == 0 {
		return errors.New("over COMPRESS: pipelined HELP: empty block")
	}
	cmds := slices.Repeat([]string{"DATE"}, 100)
	replies, err = s.c.TryPipeline(cmds, nil)
	if err != nil {
		return fmt.Errorf("over COMPRESS: %d DATE pipelined in one write: %s", len(cmds), s.cerr(err))
	}
	for i, r := range replies {
		if r.Code != 111 {
			return fmt.Errorf("over COMPRESS: pipelined DATE %d of %d: got %q, want 111", i+1, len(cmds), r.Line)
		}
	}
	if err := s.quiet(quietWindow); err != nil {
		return fmt.Errorf("after the pipelined commands over COMPRESS: %s", s.cerr(err))
	}
	return nil
}

// compressBackToBack sends three commands in three writes without waiting for a reply.
func compressBackToBack(s *session) error {
	for _, cmd := range []string{"DATE", "HELP", "DATE"} {
		if err := s.c.Send(cmd + "\r\n"); err != nil {
			return fmt.Errorf("over COMPRESS: sending %s: %v", cmd, err)
		}
	}
	for i, want := range []int{111, 100, 111} {
		code, line, err := s.c.ReadStatus()
		if err != nil {
			return fmt.Errorf("over COMPRESS: reply %d of 3 commands sent in separate writes: %s", i+1, s.cerr(err))
		}
		if code != want {
			return fmt.Errorf("over COMPRESS: reply %d of 3 commands sent in separate writes: got %q, want %d", i+1, line, want)
		}
		if want == 100 {
			if _, err := s.block(); err != nil {
				return fmt.Errorf("over COMPRESS: HELP block: %s", s.cerr(err))
			}
		}
	}
	if err := s.quiet(quietWindow); err != nil {
		return fmt.Errorf("after the commands sent in separate writes over COMPRESS: %s", s.cerr(err))
	}
	return nil
}

// checkCompressUnknown: before any compression is active an algorithm the server does not
// know is a 503, a missing one a 501, and the connection stays plain and usable.
func checkCompressUnknown(s *session, c check) Result {
	if r, ok := s.deflateReady(c); !ok {
		return r
	}
	if _, _, err := s.single([]int{503}, "COMPRESS %s", unknownAlgorithm); err != nil {
		return c.failf("%v (RFC 8054 §2.2.2: 503 for an algorithm the server does not support)", err)
	}
	if _, _, err := s.single([]int{501}, "COMPRESS"); err != nil {
		return c.failf("%v (RFC 8054 §2.2.2: 501 for a missing algorithm)", err)
	}
	if _, _, err := s.single([]int{111}, "DATE"); err != nil {
		return c.failf("after the refused COMPRESS the connection is not plain and usable: %v", err)
	}
	return c.pass()
}

// checkCompressTwice: with compression active, a second COMPRESS DEFLATE is a 502, as are
// STARTTLS and AUTHINFO when the server advertised them before (RFC 8054 §2.2.2: the credentials must not travel through a
// compressed stream, and TLS may not be started on top of it). A server that lists neither
// does not owe a 502 for them (500 is a legal answer to an unknown command) and they are not
// probed.
func checkCompressTwice(s *session, c check) Result {
	if r, ok := s.deflateReady(c); !ok {
		return r
	}
	starttls := s.has("STARTTLS") && !s.c.IsTLS()
	authinfo := s.authed || s.has("AUTHINFO")
	if r, ok := s.compressOn(c); !ok {
		return r
	}
	// an unknown algorithm is both "syntactically valid while compression is active" (502) and
	// "invalid" (503) in RFC 8054 §2.2.2: either answer is accepted
	type probe struct {
		cmd  string
		want []int
	}
	probes := []probe{{"COMPRESS DEFLATE", []int{502}}, {"COMPRESS " + unknownAlgorithm, []int{502, 503}}}
	var notProbed []string
	if starttls {
		probes = append(probes, probe{"STARTTLS", []int{502}})
	} else {
		notProbed = append(notProbed, "STARTTLS")
	}
	if authinfo {
		probes = append(probes, probe{"AUTHINFO USER conformance-probe", []int{502}})
	} else {
		notProbed = append(notProbed, "AUTHINFO")
	}
	for _, p := range probes {
		if _, _, err := s.single(p.want, "%s", p.cmd); err != nil {
			return c.failf("compression active: %s (RFC 8054 §2.2.2: %v)", s.cerr(err), p.want)
		}
	}
	r := c.pass()
	if len(notProbed) > 0 {
		r.Detail = strings.Join(notProbed, " and ") + " not advertised: not probed"
	}
	return r
}

// uploadLines and uploadWidth give the article of the upload checks: a few thousand lines of
// a hundred bytes, some starting with a dot (dot-stuffed on the wire), some empty, one a lone
// dot, so that the compressed input the server must inflate is large and has every
// dot-stuffing case. INN had a hang here (issue #269): after the whole article was sent the
// server waited for more socket data while input it had read was still waiting to be
// inflated, so the reply came only after its client timeout.
const (
	uploadLines = 3000
	uploadWidth = 100
)

// uploadBody returns the body lines of the upload article.
func uploadBody() []string {
	body := make([]string, uploadLines)
	for i := range body {
		l := fmt.Sprintf("%d ", i)
		b := strings.TrimRight(strings.Repeat(l, uploadWidth/len(l)+1)[:uploadWidth], " ")
		switch {
		case i == 10:
			b = "."
		case i%11 == 5:
			b = ""
		case i%7 == 0:
			b = "." + b[1:]
		}
		body[i] = b
	}
	return body
}

// compressUpload is the checks compress-upload (the article in one write) and
// compress-upload-lines (line by line): POST over COMPRESS with a large dot-stuffed article is
// answered within the hang guard and the article reads back intact.
func compressUpload(lineByLine bool) func(*session, check) Result {
	return func(s *session, c check) Result {
		if s.o.PostGroup == "" {
			return c.skip("no Options.PostGroup")
		}
		if r, ok := s.ready(c); !ok {
			return r
		}
		if r, ok := s.deflateReady(c); !ok {
			return r
		}
		if !s.has("POST") {
			return c.notAdvertised("POST")
		}
		if r, ok := s.compressOn(c); !ok {
			return r
		}
		pcode, pline, err := s.expect([]int{340, 440, 480}, "POST")
		if err != nil {
			return c.failf("over COMPRESS: %s", s.cerr(err))
		}
		if pcode != 340 {
			return c.skipf("POST: %q", pline)
		}
		id := asNewID(c.Name)
		body := uploadBody()
		head := []string{
			"Path: conformance.invalid!not-for-mail",
			"From: conformance@conformance.invalid",
			"Newsgroups: " + s.o.PostGroup,
			"Subject: conformance " + c.Name,
			"Date: " + time.Now().UTC().Format(time.RFC1123Z),
			"Message-ID: " + id,
			"",
		}
		wire := client.Stuff(append(head, body...))
		sent := "in one write"
		if lineByLine {
			sent = "line by line"
			for l := range strings.Lines(wire) {
				if err := s.c.Send(l); err != nil {
					return c.failf("POST over COMPRESS: sending the article %s: %v", sent, err)
				}
			}
		} else if err := s.c.Send(wire); err != nil {
			return c.failf("POST over COMPRESS: sending the article %s: %v", sent, err)
		}
		code, line, err := s.c.ReadStatus()
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			return c.failf("POST over COMPRESS: no reply within %v after the article (%d lines, %d bytes before compression, sent %s): "+
				"the server waits for socket data while compressed input it has read is still waiting to be inflated (INN issue #269)",
				s.o.deadline(), len(body), len(wire), sent)
		case err != nil:
			return c.failf("POST over COMPRESS: after the article (sent %s): %s", sent, s.cerr(err))
		case code == 441:
			return c.skipf("server refused the article (%d lines, %d bytes) with %q: this may be a size or policy limit, or the server's inflate may have corrupted the article (not told apart: see check compress for the compression itself)", len(body), len(wire), line)
		case code != 240:
			return c.failf("POST over COMPRESS: article sent %s: got %q, want 240", sent, line)
		}
		if err := s.quiet(quietWindow); err != nil {
			return c.failf("after %q over COMPRESS: %s", line, s.cerr(err))
		}
		return compressRoundTrip(s, c, id, body)
	}
}

// compressRoundTrip waits until the posted article id is there (STAT), then reads its body
// over the compressed stream and compares it with body.
func compressRoundTrip(s *session, c check, id string, body []string) Result {
	stop := time.Now().Add(s.o.deadline())
	for {
		code, sline, err := s.single([]int{223, 430}, "STAT %s", id)
		if err != nil {
			return c.failf("over COMPRESS: %s", s.cerr(err))
		}
		if code == 223 {
			break
		}
		if time.Now().After(stop) {
			r := c.pass()
			r.Detail = fmt.Sprintf("240, but STAT %s still %q %v later: the server may have assigned its own message-id or queued the article for a moderator; the stored body was not compared", id, sline, s.o.deadline())
			return r
		}
		time.Sleep(pollInterval)
	}
	_, got, err := s.multi(222, "BODY %s", id)
	if err != nil {
		return c.failf("over COMPRESS: %s", s.cerr(err))
	}
	if len(got) != len(body) {
		return c.failf("BODY %s over COMPRESS: %d lines, the article posted had %d", id, len(got), len(body))
	}
	for i := range body {
		if got[i] != body[i] {
			return c.failf("BODY %s over COMPRESS: line %d is %q, the article posted had %q", id, i+1, got[i], body[i])
		}
	}
	return c.pass()
}
