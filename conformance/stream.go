package conformance

import (
	"fmt"
	"strings"
	"time"
)

// streamChecks are the RFC 4644 streaming checks (MODE STREAM, CHECK, TAKETHIS) and the IHAVE
// checks of RFC 3977 §6.3.2. With Options.User set every check logs in first (transit peers
// such as innd often require it without listing AUTHINFO) and reads CAPABILITIES again, since
// such peers list STREAMING and IHAVE only after the login. For that reason the checks leave
// check.Needs empty and test the capability themselves, on the list read after the login
// (asRun). The checks that post need Options.PostGroup (Posts); their articles carry a
// Message-ID from asNewID.
//
// A legal negative reply (CHECK 431 or 438 for a new id, TAKETHIS 439, IHAVE 435/436/437 for
// a new id) comes from a paused or filtering server, not from a broken one: the check is
// skipped with a Detail naming the reply, after the reply was checked to name the right
// message-id, and the assertions that depended on the transfer are abandoned. Codes outside
// the RFC's set fail.
func streamChecks() []check {
	return []check{
		{Name: "mode-stream", RFC: "RFC 4644 §2.3", Run: asRun("STREAMING", checkModeStream)},
		{Name: "mode-stream-capability", RFC: "RFC 4644 §2.2", Run: asRun("", checkModeStreamCap)},
		{Name: "check-new", RFC: "RFC 4644 §2.4", Run: asRun("STREAMING", checkCheckNew)},
		{Name: "check-pipelined", RFC: "RFC 4644 §2.4", Run: asRun("STREAMING", checkCheckPipelined)},
		{Name: "takethis", RFC: "RFC 4644 §2.5", Posts: true, Run: asRun("STREAMING", checkTakethis)},
		{Name: "check-have", RFC: "RFC 4644 §2.4", Posts: true, Run: asRun("STREAMING", checkCheckHave)},
		{Name: "takethis-duplicate", RFC: "RFC 4644 §2.5", Posts: true, Run: asRun("STREAMING", checkTakethisDup)},
		{Name: "ihave", RFC: "RFC 3977 §6.3.2", Posts: true, Run: asRun("IHAVE", checkIhave)},
		{Name: "ihave-have", RFC: "RFC 3977 §6.3.2", Posts: true, Run: asRun("IHAVE", checkIhaveHave)},
		{Name: "ihave-malformed", RFC: "RFC 3977 §6.3.2", Run: asRun("IHAVE", checkIhaveMalformed)},
	}
}

// asNewID returns the Message-ID of an article or offer of a check (msgID; tests replace it
// with a deterministic one).
var asNewID = msgID

// The legal negative replies and what they mean (see streamChecks).
var (
	asSoftCheck      = map[int]string{431: "server deferred", 438: "server does not want it"}
	asSoftTake       = map[int]string{439: "server rejected the article"}
	asSoftIhaveOffer = map[int]string{435: "server does not want it", 436: "server deferred"}
	asSoftIhaveSent  = map[int]string{436: "server deferred", 437: "server rejected the article"}
)

// asRun wraps a stream check: the Posts skip, the login (asLogin) and then the capability
// test of kw (empty: none) on the list read after the login.
func asRun(kw string, run func(*session, check) Result) func(*session, check) Result {
	return func(s *session, c check) Result {
		if c.Posts && s.o.PostGroup == "" {
			return c.skip("no Options.PostGroup")
		}
		if r := asLogin(s, c); r != nil {
			return *r
		}
		if kw != "" && !s.has(kw) {
			return c.skip(kw + " not advertised")
		}
		return run(s, c)
	}
}

// asLogin logs in with Options.User when it is set, reads CAPABILITIES again after a
// successful login and returns a Fail result (non-nil) when that goes wrong. A refused login
// is only a Fail when the server advertises AUTHINFO: one that does not may simply take no
// credentials, and the check goes on unauthenticated with the first list.
func asLogin(s *session, c check) *Result {
	if s.o.User == "" || s.authed {
		return nil
	}
	refused := func(line string) *Result {
		if capArgs(s.caps, "AUTHINFO") == nil {
			return nil
		}
		r := c.failf("login refused: %q", line)
		return &r
	}
	code, line, err := s.cmd("AUTHINFO USER %s", s.o.User)
	if err != nil {
		r := c.failf("login: AUTHINFO USER: %v", err)
		return &r
	}
	switch code {
	case 281:
	case 381:
		code, line, err = s.cmd("AUTHINFO PASS %s", s.o.Pass)
		if err != nil {
			r := c.failf("login: AUTHINFO PASS: %v", err)
			return &r
		}
		if code != 281 {
			return refused(line)
		}
	default:
		return refused(line)
	}
	s.authed = true
	old := s.caps
	if _, err := s.capabilities(); err != nil && old != nil {
		r := c.failf("CAPABILITIES after login: %v", err)
		return &r
	}
	return nil
}

// asArticle returns the wire form of a conformance article for group (headers, blank line,
// a body with one line that needs dot-stuffing, the terminator), CRLF line ends.
func asArticle(group, name, id string) string {
	lines := []string{
		"Path: conformance.invalid!not-for-mail",
		"From: conformance@conformance.invalid",
		"Newsgroups: " + group,
		"Subject: conformance " + name,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: " + id,
		"",
		"nntp-testkit conformance check article, please ignore.",
		".dotted line: sent dot-stuffed, stored with one leading dot",
	}
	var b strings.Builder
	for _, l := range lines {
		if strings.HasPrefix(l, ".") {
			b.WriteByte('.')
		}
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	return b.String()
}

// asIDReply checks that a streaming reply names id as its first argument ("238 <id>").
func asIDReply(c check, line, id string) *Result {
	f := strings.Fields(line)
	if len(f) < 2 || f[1] != id {
		r := c.failf("reply %q does not name %s", line, id)
		return &r
	}
	return nil
}

// asJudge scores the reply line (code) to cmd: nil when the code is one of want, a Skip
// naming the reply when it is one of soft (a legal negative reply), else a Fail. When id is
// not empty the reply must name it first (streaming replies), soft ones included.
func asJudge(c check, cmd string, code int, line, id string, want []int, soft map[int]string) *Result {
	why, isSoft := soft[code]
	if !asIntIn(code, want) && !isSoft {
		r := c.failf("%s: got %q, want %s", cmd, line, asCodes(want))
		return &r
	}
	if id != "" {
		if r := asIDReply(c, line, id); r != nil {
			return r
		}
	}
	if isSoft && !asIntIn(code, want) {
		r := c.skip(fmt.Sprintf("%s: %s", why, line))
		return &r
	}
	return nil
}

// asSend sends one command and scores its reply with asJudge.
func asSend(s *session, c check, id string, want []int, soft map[int]string, format string, args ...any) (int, string, *Result) {
	shown := asShown(format, args...)
	code, line, err := s.cmd(format, args...)
	if err != nil {
		r := c.failf("%s: %v", shown, err)
		return code, line, &r
	}
	return code, line, asJudge(c, shown, code, line, id, want, soft)
}

// asReply reads the status line of cmd (already sent) and scores it with asJudge.
func asReply(s *session, c check, cmd, id string, want []int, soft map[int]string) (int, string, *Result) {
	code, line, err := s.c.ReadStatus()
	if err != nil {
		r := c.failf("%s: %v", cmd, err)
		return code, line, &r
	}
	return code, line, asJudge(c, cmd, code, line, id, want, soft)
}

// asStream sends MODE STREAM, which must answer 203.
func asStream(s *session, c check) *Result {
	_, _, r := asExpect(s, c, []int{203}, "MODE STREAM")
	return r
}

// asTakethis sends TAKETHIS id with its article in one write and scores the reply, which must
// name id: want, or 439 as a Skip.
func asTakethis(s *session, c check, id string, want ...int) *Result {
	if err := s.c.Send("TAKETHIS " + id + "\r\n" + asArticle(s.o.PostGroup, c.Name, id)); err != nil {
		r := c.failf("TAKETHIS %s: %v", id, err)
		return &r
	}
	_, _, r := asReply(s, c, "TAKETHIS "+id, id, want, asSoftTake)
	return r
}

// asIhave runs IHAVE id → 335, the article → 235; 435/436 to the offer and 436/437 to the
// article are Skips.
func asIhave(s *session, c check, id string) *Result {
	if _, _, r := asSend(s, c, "", []int{335}, asSoftIhaveOffer, "IHAVE %s", id); r != nil {
		return r
	}
	if err := s.c.Send(asArticle(s.o.PostGroup, c.Name, id)); err != nil {
		r := c.failf("IHAVE %s: article: %v", id, err)
		return &r
	}
	_, _, r := asReply(s, c, "IHAVE "+id+" (article sent)", "", []int{235}, asSoftIhaveSent)
	return r
}

// asAbortIhave ends an IHAVE that got an unexpected 335: it sends an empty article (the
// terminator alone) and reads the reply, so the server is not left in the middle of the
// command. The Fail result fail is returned, with a wire error appended.
func asAbortIhave(s *session, c check, fail Result) Result {
	if err := s.c.Send(".\r\n"); err != nil {
		fail.Detail += fmt.Sprintf("; aborting with an empty article: %v", err)
		return fail
	}
	if _, line, err := s.c.ReadStatus(); err != nil {
		fail.Detail += fmt.Sprintf("; reply to the empty article: %v", err)
	} else {
		fail.Detail += fmt.Sprintf("; the empty article got %q", line)
	}
	return fail
}

// checkModeStream: MODE STREAM → 203 when STREAMING is advertised.
func checkModeStream(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkModeStreamCap: MODE STREAM agrees with the capability list. Without STREAMING the
// answer must be 500 or 501 (Pass with Detail "no streaming"); a 203 without STREAMING fails.
// With STREAMING advertised but a reply other than 203 the check passes with a Detail: that
// case is mode-stream's to report as a Fail.
func checkModeStreamCap(s *session, c check) Result {
	code, line, err := s.cmd("MODE STREAM")
	if err != nil {
		return c.failf("MODE STREAM: %v", err)
	}
	adv := s.has("STREAMING")
	var detail string
	switch {
	case adv && code != 203:
		detail = fmt.Sprintf("STREAMING advertised, MODE STREAM got %q (see mode-stream)", line)
	case adv:
	case code == 203:
		return c.failf("MODE STREAM got %q, but CAPABILITIES does not list STREAMING", line)
	case code == 500 || code == 501:
		detail = fmt.Sprintf("no streaming: MODE STREAM got %q", line)
	default:
		return c.failf("MODE STREAM without STREAMING advertised: got %q, want 500 or 501", line)
	}
	return asDetail(asSettle(s, c), detail)
}

// checkCheckNew: CHECK of an id the server cannot have → 238 <id> (431/438 <id>: Skip).
func checkCheckNew(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	id := asNewID(c.Name)
	if _, _, r := asSend(s, c, id, []int{238}, asSoftCheck, "CHECK %s", id); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkCheckPipelined: three CHECKs in one write get three replies, in order, each naming its
// id (RFC 4644 lets the client send without waiting for each reply). The order is proven by
// a 431 or 438 as well as by a 238: those pass, with the replies in Detail.
func checkCheckPipelined(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	ids := make([]string, 3)
	var b strings.Builder
	for i := range ids {
		ids[i] = asNewID(fmt.Sprintf("%s-%d", c.Name, i+1))
		b.WriteString("CHECK " + ids[i] + "\r\n")
	}
	if err := s.c.Send(b.String()); err != nil {
		return c.failf("send 3 CHECKs: %v", err)
	}
	var soft []string
	for i, id := range ids {
		code, line, r := asReply(s, c, fmt.Sprintf("CHECK %d of 3 (%s)", i+1, id), id, []int{238, 431, 438}, nil)
		if r != nil {
			return *r
		}
		if code != 238 {
			soft = append(soft, fmt.Sprintf("%q", line))
		}
	}
	detail := ""
	if len(soft) > 0 {
		detail = "in order; not 238: " + strings.Join(soft, ", ")
	}
	return asDetail(asSettle(s, c), detail)
}

// checkTakethis: TAKETHIS of a new article → 239 <id> (439 <id>: Skip).
func checkTakethis(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	if r := asTakethis(s, c, asNewID(c.Name), 239); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkCheckHave: CHECK of an id the server took by TAKETHIS → 438 <id>. A refused TAKETHIS
// (439) or a deferred CHECK (431) is a Skip.
func checkCheckHave(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	id := asNewID(c.Name)
	if r := asTakethis(s, c, id, 239); r != nil {
		return *r
	}
	if _, _, r := asSend(s, c, id, []int{438}, map[int]string{431: "server deferred"}, "CHECK %s", id); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkTakethisDup: TAKETHIS of an id the server took already → 439 <id>. A refused first
// TAKETHIS is a Skip.
func checkTakethisDup(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	id := asNewID(c.Name)
	if r := asTakethis(s, c, id, 239); r != nil {
		return *r
	}
	if r := asTakethis(s, c, id, 439); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkIhave: IHAVE of a new id → 335, the article → 235.
func checkIhave(s *session, c check) Result {
	if r := asIhave(s, c, asNewID(c.Name)); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkIhaveHave: IHAVE of an id the server took by IHAVE → 435 (436: Skip). An unexpected
// 335 fails after the transfer is aborted.
func checkIhaveHave(s *session, c check) Result {
	id := asNewID(c.Name)
	if r := asIhave(s, c, id); r != nil {
		return *r
	}
	code, _, r := asSend(s, c, "", []int{435}, map[int]string{436: "server deferred"}, "IHAVE %s", id)
	if r != nil {
		if code == 335 {
			return asAbortIhave(s, c, *r)
		}
		return *r
	}
	return asSettle(s, c)
}

// checkIhaveMalformed: IHAVE with an argument that is no message-id → 435 (innd) or 501
// (RFC 3977 syntax error); Detail records which. A 335 fails after the transfer is aborted.
func checkIhaveMalformed(s *session, c check) Result {
	code, line, r := asExpect(s, c, []int{435, 501}, "IHAVE conformance-no-message-id")
	if r != nil {
		if code == 335 {
			return asAbortIhave(s, c, *r)
		}
		return *r
	}
	return asDetail(asSettle(s, c), fmt.Sprintf("malformed id answered %q", line))
}
