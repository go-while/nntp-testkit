package conformance

import (
	"fmt"
	"strings"
	"time"
)

// streamChecks are the RFC 4644 streaming checks (MODE STREAM, CHECK, TAKETHIS) and the IHAVE
// checks of RFC 3977 §6.3.2. With Options.User set every check logs in first (transit peers
// such as innd often require it without listing AUTHINFO). The checks that post need
// Options.PostGroup (Posts); their articles carry a Message-ID from msgID.
func streamChecks() []check {
	return []check{
		{Name: "mode-stream", RFC: "RFC 4644 §2.2", Needs: "STREAMING", Run: checkModeStream},
		{Name: "mode-stream-capability", RFC: "RFC 4644 §2.1", Run: checkModeStreamCap},
		{Name: "check-new", RFC: "RFC 4644 §2.3", Needs: "STREAMING", Run: checkCheckNew},
		{Name: "check-pipelined", RFC: "RFC 4644 §2.3", Needs: "STREAMING", Run: checkCheckPipelined},
		{Name: "takethis", RFC: "RFC 4644 §2.4", Needs: "STREAMING", Posts: true, Run: checkTakethis},
		{Name: "check-have", RFC: "RFC 4644 §2.3", Needs: "STREAMING", Posts: true, Run: checkCheckHave},
		{Name: "takethis-duplicate", RFC: "RFC 4644 §2.4", Needs: "STREAMING", Posts: true, Run: checkTakethisDup},
		{Name: "ihave", RFC: "RFC 3977 §6.3.2", Needs: "IHAVE", Posts: true, Run: checkIhave},
		{Name: "ihave-have", RFC: "RFC 3977 §6.3.2", Needs: "IHAVE", Posts: true, Run: checkIhaveHave},
		{Name: "ihave-malformed", RFC: "RFC 3977 §6.3.2", Needs: "IHAVE", Run: checkIhaveMalformed},
	}
}

// asLogin logs in with Options.User when it is set and returns a Fail result (non-nil) when
// that goes wrong. A refused login is only a Fail when the server advertises AUTHINFO: one
// that does not may simply take no credentials, and the check goes on unauthenticated.
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
		s.authed = true
		return nil
	case 381:
	default:
		return refused(line)
	}
	code, line, err = s.cmd("AUTHINFO PASS %s", s.o.Pass)
	if err != nil {
		r := c.failf("login: AUTHINFO PASS: %v", err)
		return &r
	}
	if code != 281 {
		return refused(line)
	}
	s.authed = true
	return nil
}

// asStream logs in when configured and sends MODE STREAM, which must answer 203.
func asStream(s *session, c check) *Result {
	if r := asLogin(s, c); r != nil {
		return r
	}
	_, _, r := asExpect(s, c, []int{203}, "MODE STREAM")
	return r
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

// asTakethis sends TAKETHIS id with its article in one write and checks the code (one of want)
// and the message-id of the reply.
func asTakethis(s *session, c check, id string, want ...int) *Result {
	if err := s.c.Send("TAKETHIS " + id + "\r\n" + asArticle(s.o.PostGroup, c.Name, id)); err != nil {
		r := c.failf("TAKETHIS %s: %v", id, err)
		return &r
	}
	return asReply(s, c, "TAKETHIS "+id, id, want)
}

// asReply reads the status line of cmd and checks its code (one of want) and, when id is not
// empty, the message-id it names.
func asReply(s *session, c check, cmd, id string, want []int) *Result {
	code, line, err := s.c.ReadStatus()
	if err != nil {
		r := c.failf("%s: %v", cmd, err)
		return &r
	}
	if !asIntIn(code, want) {
		r := c.failf("%s: got %q, want %s", cmd, line, asCodes(want))
		return &r
	}
	if id != "" {
		return asIDReply(c, line, id)
	}
	return nil
}

// asIhave runs IHAVE id → 335, the article → 235.
func asIhave(s *session, c check, id string) *Result {
	if _, _, r := asExpect(s, c, []int{335}, "IHAVE %s", id); r != nil {
		return r
	}
	if err := s.c.Send(asArticle(s.o.PostGroup, c.Name, id)); err != nil {
		r := c.failf("IHAVE %s: article: %v", id, err)
		return &r
	}
	return asReply(s, c, "IHAVE "+id+" (article sent)", "", []int{235})
}

func asIntIn(v int, list []int) bool {
	for _, w := range list {
		if v == w {
			return true
		}
	}
	return false
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
// With STREAMING advertised the reply code is mode-stream's business.
func checkModeStreamCap(s *session, c check) Result {
	if r := asLogin(s, c); r != nil {
		return *r
	}
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
	res := asSettle(s, c)
	if res.Status == Pass {
		res.Detail = detail
	}
	return res
}

// checkCheckNew: CHECK of an id the server cannot have → 238 <id>.
func checkCheckNew(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	id := msgID(c.Name)
	_, line, r := asExpect(s, c, []int{238}, "CHECK %s", id)
	if r != nil {
		return *r
	}
	if r := asIDReply(c, line, id); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkCheckPipelined: three CHECKs in one write get three replies, in order (RFC 4644 §2.3
// lets the client send without waiting for each reply).
func checkCheckPipelined(s *session, c check) Result {
	if r := asStream(s, c); r != nil {
		return *r
	}
	ids := make([]string, 3)
	var b strings.Builder
	for i := range ids {
		ids[i] = msgID(fmt.Sprintf("%s-%d", c.Name, i+1))
		b.WriteString("CHECK " + ids[i] + "\r\n")
	}
	if err := s.c.Send(b.String()); err != nil {
		return c.failf("send 3 CHECKs: %v", err)
	}
	for i, id := range ids {
		if r := asReply(s, c, fmt.Sprintf("CHECK %d of 3 (%s)", i+1, id), id, []int{238}); r != nil {
			return *r
		}
	}
	return asSettle(s, c)
}

// checkTakethis: TAKETHIS of a new article → 239 <id>.
func checkTakethis(s *session, c check) Result {
	if s.o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	if r := asStream(s, c); r != nil {
		return *r
	}
	if r := asTakethis(s, c, msgID(c.Name), 239); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkCheckHave: CHECK of an id the server took by TAKETHIS → 438 <id>.
func checkCheckHave(s *session, c check) Result {
	if s.o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	if r := asStream(s, c); r != nil {
		return *r
	}
	id := msgID(c.Name)
	if r := asTakethis(s, c, id, 239); r != nil {
		return *r
	}
	_, line, r := asExpect(s, c, []int{438}, "CHECK %s", id)
	if r != nil {
		return *r
	}
	if r := asIDReply(c, line, id); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkTakethisDup: TAKETHIS of an id the server took already → 439 <id>.
func checkTakethisDup(s *session, c check) Result {
	if s.o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	if r := asStream(s, c); r != nil {
		return *r
	}
	id := msgID(c.Name)
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
	if s.o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	if r := asLogin(s, c); r != nil {
		return *r
	}
	if r := asIhave(s, c, msgID(c.Name)); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkIhaveHave: IHAVE of an id the server took by IHAVE → 435.
func checkIhaveHave(s *session, c check) Result {
	if s.o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	if r := asLogin(s, c); r != nil {
		return *r
	}
	id := msgID(c.Name)
	if r := asIhave(s, c, id); r != nil {
		return *r
	}
	if _, _, r := asExpect(s, c, []int{435}, "IHAVE %s", id); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkIhaveMalformed: IHAVE with an argument that is no message-id → 435 (innd) or 501
// (RFC 3977 syntax error); Detail records which. A 335 fails and no article is sent.
func checkIhaveMalformed(s *session, c check) Result {
	if r := asLogin(s, c); r != nil {
		return *r
	}
	_, line, r := asExpect(s, c, []int{435, 501}, "IHAVE conformance-no-message-id")
	if r != nil {
		return *r
	}
	res := asSettle(s, c)
	if res.Status == Pass {
		res.Detail = fmt.Sprintf("malformed id answered %q", line)
	}
	return res
}
