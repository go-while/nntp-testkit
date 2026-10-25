package conformance

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// quietWindow is how long a check waits after a reply for bytes that should not be there (a
// stray NUL, a second status line). It is a hang guard for "nothing pending", not a timing
// assertion: the check never measures how long anything took.
const quietWindow = 100 * time.Millisecond

// maxCandidates bounds the GROUP commands a check sends while it looks for a group with
// articles (or an empty one), so a server with a large active file is not walked whole.
const maxCandidates = 25

// unknownID is a Message-ID no server holds; the 430 checks ask for it. It is fixed (not
// unique per run) so a test can key a fault on the exact command line.
const unknownID = "<no-such-article@conformance.invalid>"

// unknownGroup is a group name no server carries; the 411 checks select it.
const unknownGroup = "conformance.invalid.no-such-group"

// farAbove is added to a group's high water mark for an article number that cannot exist.
const farAbove = 1000000

// maxArticle is the largest article number farNumber gives (2^31-1: servers with 32-bit
// numbers must still parse it).
const maxArticle = 2147483647

// farNumber returns an article number far above high, at most maxArticle.
func farNumber(high int64) int64 { return min(high+farAbove, maxArticle) }

// readerChecks returns the RFC 3977 checks, in report order.
func readerChecks() []check {
	return []check{
		{Name: "greeting", RFC: "RFC 3977 §5.1.1", Run: checkGreeting},
		{Name: "capabilities", RFC: "RFC 3977 §5.2", Run: checkCapabilities},
		{Name: "mode-reader", RFC: "RFC 3977 §5.3", Run: checkModeReader},
		{Name: "group", RFC: "RFC 3977 §6.1.1", Needs: "READER", Run: checkGroup},
		{Name: "listgroup", RFC: "RFC 3977 §6.1.2", Needs: "READER", Run: checkListgroup},
		{Name: "next-last", RFC: "RFC 3977 §6.1.3-6.1.4", Needs: "READER", Run: checkNextLast},
		{Name: "article", RFC: "RFC 3977 §6.2.1", Needs: "READER", Run: articleCheck("ARTICLE", 220)},
		{Name: "head", RFC: "RFC 3977 §6.2.2", Needs: "READER", Run: articleCheck("HEAD", 221)},
		{Name: "body", RFC: "RFC 3977 §6.2.3", Needs: "READER", Run: articleCheck("BODY", 222)},
		{Name: "stat", RFC: "RFC 3977 §6.2.4", Needs: "READER", Run: articleCheck("STAT", 223)},
		{Name: "no-group", RFC: "RFC 3977 §6.1-6.2 (412)", Needs: "READER", Run: checkNoGroup},
		{Name: "no-current-article", RFC: "RFC 3977 §6.1-6.2 (420)", Needs: "READER", Run: checkNoCurrent},
		{Name: "article-out-of-range", RFC: "RFC 3977 §6.2 (423)", Needs: "READER", Run: checkOutOfRange},
		{Name: "article-unknown-id", RFC: "RFC 3977 §6.2 (430)", Needs: "READER", Run: checkUnknownID},
		{Name: "list-active", RFC: "RFC 3977 §7.6.3", Needs: "LIST", Run: checkListActive},
		{Name: "list-newsgroups", RFC: "RFC 3977 §7.6.6", Needs: "LIST", Run: checkListNewsgroups},
		{Name: "list-overview-fmt", RFC: "RFC 3977 §8.4", Needs: "LIST", Run: checkListOverviewFmt},
		{Name: "list-unknown-keyword", RFC: "RFC 3977 §7.6.1", Needs: "LIST", Run: checkListUnknown},
		{Name: "over", RFC: "RFC 3977 §8.3", Needs: "OVER", Run: checkOver},
		{Name: "over-errors", RFC: "RFC 3977 §8.3", Needs: "OVER", Run: checkOverErrors},
		{Name: "date", RFC: "RFC 3977 §7.1", Needs: "READER", Run: checkDate},
		{Name: "help", RFC: "RFC 3977 §7.2", Run: checkHelp},
		{Name: "unknown-command", RFC: "RFC 3977 §3.2.1", Run: checkUnknownCommand},
		{Name: "bad-arguments", RFC: "RFC 3977 §3.2.1", Needs: "READER", Run: checkBadArguments},
		{Name: "quit-eof", RFC: "RFC 3977 §5.4", Run: checkQuit},
	}
}

// sessionFailed is the result of check c when no session could be opened for it (dial gave
// err). A wire mistake in the greeting (bare LF, bare CR, no line end) is the greeting
// check's Fail; every other check is then skipped with the reason, so one broken greeting is
// reported once. Any other dial error fails c.
func sessionFailed(c check, err error) Result {
	var se *client.StrictError
	if errors.As(err, &se) {
		if c.Name == "greeting" {
			return c.fail(describe(err))
		}
		return c.skip("no usable session, the greeting is broken (see check greeting)")
	}
	return c.failf("dial: %v", err)
}

// describe formats err; a strict-mode wire mistake is prefixed with its kind ("BareLF: ...").
func describe(err error) string {
	var se *client.StrictError
	if errors.As(err, &se) {
		return se.Kind.String() + ": " + err.Error()
	}
	return err.Error()
}

// ---- exchange helpers ----

// expect sends one command and checks that the status code is one of want. It does not read
// a block or wait for pending bytes (see single and multi).
func (s *session) expect(want []int, format string, args ...any) (int, string, error) {
	shown := fmt.Sprintf(format, args...)
	code, line, err := s.cmd(format, args...)
	if err != nil {
		if line != "" {
			return code, line, fmt.Errorf("%s: %s (line %q)", shown, describe(err), line)
		}
		return code, line, fmt.Errorf("%s: %s", shown, describe(err))
	}
	if !slices.Contains(want, code) {
		return code, line, fmt.Errorf("%s: got %q, want %v", shown, line, want)
	}
	return code, line, nil
}

// single is expect for a single-line reply, then nothing may be pending.
func (s *session) single(want []int, format string, args ...any) (int, string, error) {
	code, line, err := s.expect(want, format, args...)
	if err != nil {
		return code, line, err
	}
	if err := s.quiet(quietWindow); err != nil {
		return code, line, fmt.Errorf("after %q to %s: %s", line, fmt.Sprintf(format, args...), describe(err))
	}
	return code, line, nil
}

// multi is expect for a multi-line reply with status code want: the block is read (strict:
// line ends, dot-stuffing, terminator), then nothing may be pending.
func (s *session) multi(want int, format string, args ...any) (string, []string, error) {
	shown := fmt.Sprintf(format, args...)
	_, line, err := s.expect([]int{want}, format, args...)
	if err != nil {
		return line, nil, err
	}
	lines, err := s.block()
	if err != nil {
		return line, lines, fmt.Errorf("%s: block after %q: %s", shown, line, describe(err))
	}
	if err := s.quiet(quietWindow); err != nil {
		return line, lines, fmt.Errorf("after the block of %s: %s", shown, describe(err))
	}
	return line, lines, nil
}

// ready prepares a session for a reader check: MODE READER on a mode-switching server (one
// that advertises MODE-READER but not READER), AUTHINFO when Options.User is set and the
// session is not authenticated yet (see login), and the capability list read again after
// either. A check whose Needs the server still does not advertise is skipped.
func (s *session) ready(c check) (Result, bool) {
	if !s.has("READER") && s.has("MODE-READER") {
		if _, _, err := s.single([]int{200, 201}, "MODE READER"); err != nil {
			return c.fail(err.Error()), false
		}
		if _, err := s.capabilities(); err != nil {
			return c.failf("CAPABILITIES after MODE READER: %s", describe(err)), false
		}
	}
	if s.o.User != "" && !s.authed {
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
	if c.Needs != "" && !s.has(c.Needs) {
		return c.skip("server does not advertise " + c.Needs), false
	}
	return Result{}, true
}

// login tries AUTHINFO USER/PASS with the session's credentials, whether or not AUTHINFO is
// advertised: done is true after 281. A 500, 501 or 502 to AUTHINFO USER means the server does
// not offer (or need) it: the check goes on unauthenticated (the auth checks report that case).
// A 481, 482, any other code or a wire mistake is an error.
func (s *session) login() (bool, error) {
	code, line, err := s.single([]int{281, 381, 500, 501, 502}, "AUTHINFO USER %s", s.o.User)
	if err != nil {
		return false, err
	}
	switch code {
	case 281:
		s.authed = true
		return true, nil
	case 500, 501, 502:
		return false, nil
	}
	// not through single: its error messages would quote the password
	code, line, err = s.cmd("AUTHINFO PASS %s", s.o.Pass)
	if err != nil {
		return false, fmt.Errorf("AUTHINFO PASS: %s", describe(err))
	}
	if code != 281 {
		return false, fmt.Errorf("AUTHINFO PASS: got %q, want 281", line)
	}
	if err := s.quiet(quietWindow); err != nil {
		return false, fmt.Errorf("after AUTHINFO PASS: %s", describe(err))
	}
	s.authed = true
	return true, nil
}

// ---- parsing helpers ----

// activeEntry is one LIST ACTIVE line.
type activeEntry struct {
	name      string
	high, low int64
	status    string
}

// usualStatus matches the LIST ACTIVE status values RFC 3977 §7.6.3 names (y, n, m) and the
// common private ones (x, j, =group); §7.6.3.2 allows others, which list-active only notes.
var usualStatus = regexp.MustCompile(`^(?:[ymnxj]|=\S+)$`)

// parseActive parses "name high low status"; status is any token (see usualStatus).
func parseActive(l string) (activeEntry, error) {
	f := strings.Fields(l)
	if len(f) != 4 {
		return activeEntry{}, fmt.Errorf("LIST ACTIVE line %q: want 4 fields \"name high low status\"", l)
	}
	high, err1 := parseNum(f[1])
	low, err2 := parseNum(f[2])
	if err1 != nil || err2 != nil {
		return activeEntry{}, fmt.Errorf("LIST ACTIVE line %q: high and low must be numbers", l)
	}
	return activeEntry{name: f[0], high: high, low: low, status: f[3]}, nil
}

// parseNum parses an unsigned decimal article number (RFC 3977 §3.1: 1 to 16 digits; leading
// zeros are allowed, as INN pads its active file).
func parseNum(s string) (int64, error) {
	if s == "" || len(s) > 16 {
		return 0, fmt.Errorf("bad number %q", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// groupInfo is a parsed "211 count low high name" reply.
type groupInfo struct {
	name             string
	count, low, high int64
}

// parseGroupLine parses a 211 reply of GROUP or LISTGROUP selecting want.
func parseGroupLine(line, want string) (groupInfo, error) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "211" {
		return groupInfo{}, fmt.Errorf("reply %q: want \"211 count low high %s\"", line, want)
	}
	var g groupInfo
	var errs [3]error
	g.count, errs[0] = parseNum(f[1])
	g.low, errs[1] = parseNum(f[2])
	g.high, errs[2] = parseNum(f[3])
	g.name = f[4]
	for _, err := range errs {
		if err != nil {
			return g, fmt.Errorf("reply %q: count, low and high must be numbers", line)
		}
	}
	if g.name != want {
		return g, fmt.Errorf("reply %q: group name %q, want %q", line, g.name, want)
	}
	if g.count > 0 && g.low > g.high {
		return g, fmt.Errorf("reply %q: count %d but low %d > high %d", line, g.count, g.low, g.high)
	}
	if g.count > 0 && g.count > g.high-g.low+1 {
		return g, fmt.Errorf("reply %q: count %d is larger than the range %d-%d", line, g.count, g.low, g.high)
	}
	return g, nil
}

// parseArticleLine parses the "n <id>" after the code of a 220-223 reply.
func parseArticleLine(line string) (int64, string, error) {
	f := strings.Fields(line)
	if len(f) < 3 {
		return 0, "", fmt.Errorf("reply %q: want \"%s n <message-id>\"", line, line[:min(3, len(line))])
	}
	n, err := parseNum(f[1])
	if err != nil {
		return 0, "", fmt.Errorf("reply %q: article number %q", line, f[1])
	}
	if !validID(f[2]) {
		return 0, "", fmt.Errorf("reply %q: %q is not a message-id", line, f[2])
	}
	return n, f[2], nil
}

// validID reports whether id has the form "<...>" (RFC 3977 §3.6: no white space, at least
// one byte inside the brackets).
func validID(id string) bool {
	return len(id) > 2 && id[0] == '<' && id[len(id)-1] == '>' && !strings.ContainsAny(id, " \t\r\n\x00")
}

// checkHeaders checks the header lines of an article (ARTICLE head part or HEAD block): not
// empty, every line a "Name: value" field or a continuation, and a Message-ID field (when
// present) equal to id.
func checkHeaders(head []string, id string) error {
	if len(head) == 0 {
		return fmt.Errorf("no header lines")
	}
	for i, l := range head {
		if l == "" {
			return fmt.Errorf("header line %d is empty", i+1)
		}
		if l[0] == ' ' || l[0] == '\t' {
			if i == 0 {
				return fmt.Errorf("first header line %q is a continuation", l)
			}
			continue
		}
		colon := strings.IndexByte(l, ':')
		if colon <= 0 || strings.ContainsAny(l[:colon], " \t") {
			return fmt.Errorf("header line %q is not \"Name: value\"", l)
		}
		if strings.EqualFold(l[:colon], "Message-ID") {
			if v := strings.TrimSpace(l[colon+1:]); v != id {
				return fmt.Errorf("Message-ID header %q differs from %s in the status line", v, id)
			}
		}
	}
	return nil
}

// ---- group and article lookup ----

// listActive sends LIST ACTIVE and returns the parsable lines (a malformed line is the
// list-active check's business, not every check's).
func (s *session) listActive() ([]activeEntry, error) {
	_, lines, err := s.multi(215, "LIST ACTIVE")
	if err != nil {
		return nil, err
	}
	var out []activeEntry
	for _, l := range lines {
		if e, err := parseActive(l); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// selectGroup sends GROUP name and parses the 211 reply.
func (s *session) selectGroup(name string) (groupInfo, error) {
	_, line, err := s.single([]int{211}, "GROUP %s", name)
	if err != nil {
		return groupInfo{}, err
	}
	return parseGroupLine(line, name)
}

// candidates returns the group names a check may select: Options.PostGroup when set, else
// the LIST ACTIVE groups that keep(e) accepts.
func (s *session) candidates(keep func(activeEntry) bool) ([]string, error) {
	if s.o.PostGroup != "" {
		return []string{s.o.PostGroup}, nil
	}
	list, err := s.listActive()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range list {
		if keep(e) {
			out = append(out, e.name)
		}
	}
	return out, nil
}

// anyGroup selects a group (Options.PostGroup or the first LIST ACTIVE group); ok false
// carries the check's Result.
func (s *session) anyGroup(c check) (groupInfo, Result, bool) {
	names, err := s.candidates(func(activeEntry) bool { return true })
	if err != nil {
		return groupInfo{}, c.fail(err.Error()), false
	}
	if len(names) == 0 {
		return groupInfo{}, c.skip("LIST ACTIVE lists no group"), false
	}
	g, err := s.selectGroup(names[0])
	if err != nil {
		return g, c.fail(err.Error()), false
	}
	return g, Result{}, true
}

// target is an article a check works on, in its selected group.
type target struct {
	g   groupInfo
	num int64  // the article's number in g; the current article after pickTarget
	id  string // its message-id
}

// pickTarget selects a group with articles and makes its first article the current one:
// Options.PostGroup when set, else the first LIST ACTIVE group (of at most maxCandidates that
// look non-empty) whose GROUP reports articles and whose first article STAT (or NEXT) finds.
// ok false carries the check's Result: Skip when the server has no such group.
func (s *session) pickTarget(c check) (target, Result, bool) {
	names, err := s.candidates(func(e activeEntry) bool { return e.high >= e.low && e.high > 0 })
	if err != nil {
		return target{}, c.fail(err.Error()), false
	}
	tried := 0
	for _, name := range names {
		if tried == maxCandidates {
			break
		}
		tried++
		g, err := s.selectGroup(name)
		if err != nil {
			if s.o.PostGroup != "" {
				return target{}, c.failf("PostGroup: %v", err), false
			}
			return target{}, c.fail(err.Error()), false
		}
		if g.count == 0 {
			continue
		}
		code, line, err := s.single([]int{223, 420, 423}, "STAT")
		if err != nil {
			return target{}, c.fail(err.Error()), false
		}
		if code != 223 {
			if code, line, err = s.single([]int{223, 420, 421}, "NEXT"); err != nil {
				return target{}, c.fail(err.Error()), false
			}
			if code != 223 {
				continue
			}
		}
		n, id, err := parseArticleLine(line)
		if err != nil {
			return target{}, c.fail(err.Error()), false
		}
		return target{g: g, num: n, id: id}, Result{}, true
	}
	if s.o.PostGroup != "" {
		return target{}, c.skipf("PostGroup %q has no articles", s.o.PostGroup), false
	}
	return target{}, c.skipf("no group with articles (tried %d of %d LIST ACTIVE groups that look non-empty)", tried, len(names)), false
}

// skipf is skip with a format.
func (c check) skipf(f string, a ...any) Result { return c.skip(fmt.Sprintf(f, a...)) }

// emptyGroup selects an empty group (GROUP count 0) among the LIST ACTIVE groups whose high
// is below low; ok false carries the check's Result (Skip when there is none).
func (s *session) emptyGroup(c check) (groupInfo, Result, bool) {
	list, err := s.listActive()
	if err != nil {
		return groupInfo{}, c.fail(err.Error()), false
	}
	tried := 0
	for _, e := range list {
		// the empty forms of RFC 3977 §6.1.1.2: high below low, or all zeros
		if e.high >= e.low && (e.high != 0 || e.low != 0) {
			continue
		}
		if tried == maxCandidates {
			break
		}
		tried++
		g, err := s.selectGroup(e.name)
		if err != nil {
			return g, c.fail(err.Error()), false
		}
		if g.count == 0 {
			return g, Result{}, true
		}
	}
	return groupInfo{}, c.skip("no empty group (LIST ACTIVE high < low or 0 0, and GROUP count 0) to make the current article invalid"), false
}

// ---- checks ----

func checkGreeting(s *session, c check) Result {
	if _, err := client.ParseCode(s.greeting); err != nil {
		return c.failf("greeting %q: %v", s.greeting, err)
	}
	switch s.code {
	case 200, 201:
	case 400, 502:
		return c.failf("greeting %q: service unavailable (want 200 or 201)", s.greeting)
	default:
		return c.failf("greeting %q: not a greeting code (want 200 or 201)", s.greeting)
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after CAPABILITIES (read right after the greeting): %s", describe(err))
	}
	return c.pass()
}

var capLabel = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.\-]{2,}$`)

func checkCapabilities(s *session, c check) Result {
	if s.capsErr != nil {
		return c.failf("CAPABILITIES right after the greeting: %s", describe(s.capsErr))
	}
	first := slices.Clone(s.caps)
	_, lines, err := s.multi(101, "CAPABILITIES")
	if err != nil {
		return c.fail(err.Error())
	}
	if len(lines) == 0 {
		return c.fail("empty capability list (VERSION is mandatory)")
	}
	f := strings.Fields(lines[0])
	if len(f) < 2 || !strings.EqualFold(f[0], "VERSION") {
		return c.failf("first capability %q: want \"VERSION 2\" (VERSION must come first)", lines[0])
	}
	if !slices.Contains(f[1:], "2") {
		return c.failf("capability %q does not list version 2", lines[0])
	}
	seen := map[string]bool{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 || l != strings.TrimSpace(l) {
			return c.failf("capability line %q: leading/trailing white space or empty", l)
		}
		if !capLabel.MatchString(f[0]) {
			return c.failf("capability line %q: %q is not a capability label", l, f[0])
		}
		kw := strings.ToUpper(f[0])
		if seen[kw] {
			return c.failf("capability %s is listed twice", kw)
		}
		seen[kw] = true
	}
	// §5.2.2: the order need not be the same, the set must
	a, b := slices.Clone(first), slices.Clone(lines)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return c.failf("CAPABILITIES gave %q and then %q in the same state", first, lines)
	}
	r := c.pass()
	if !slices.Equal(first, lines) {
		r.Detail = "the two CAPABILITIES lists are in a different order (allowed)"
	}
	return r
}

func checkModeReader(s *session, c check) Result {
	reader, modeReader := s.has("READER"), s.has("MODE-READER")
	code, line, err := s.single([]int{200, 201, 502}, "MODE READER")
	if err != nil {
		return c.fail(err.Error())
	}
	if code == 502 {
		if reader || modeReader {
			return c.failf("MODE READER: %q although CAPABILITIES lists READER or MODE-READER", line)
		}
		return c.pass()
	}
	caps, err := s.capabilities()
	if err != nil {
		return c.failf("CAPABILITIES after MODE READER: %s", describe(err))
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after CAPABILITIES: %s", describe(err))
	}
	if capArgs(caps, "READER") == nil {
		return c.failf("CAPABILITIES after %q does not list READER", line)
	}
	if capArgs(caps, "MODE-READER") != nil {
		return c.failf("CAPABILITIES after %q still lists MODE-READER", line)
	}
	return c.pass()
}

func checkGroup(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if _, r, ok := s.anyGroup(c); !ok {
		return r
	}
	if _, _, err := s.single([]int{411}, "GROUP %s", unknownGroup); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

func checkListgroup(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	t, r, ok := s.pickTarget(c)
	if !ok {
		return r
	}
	// a window from low, so a large group is not listed whole
	from := max(1, t.g.low)
	to := from + 999
	line, lines, err := s.multi(211, "LISTGROUP %s %d-%d", t.g.name, from, to)
	if err != nil {
		return c.fail(err.Error())
	}
	g, err := parseGroupLine(line, t.g.name)
	if err != nil {
		return c.fail(err.Error())
	}
	var prev int64 = -1
	found := false
	for _, l := range lines {
		n, err := parseNum(l)
		if err != nil {
			return c.failf("LISTGROUP line %q is not an article number", l)
		}
		if n < g.low || n > g.high {
			return c.failf("LISTGROUP number %d is outside low..high %d-%d", n, g.low, g.high)
		}
		if n < from || n > to {
			return c.failf("LISTGROUP number %d is outside the range %d-%d asked for", n, from, to)
		}
		if n <= prev {
			return c.failf("LISTGROUP numbers not ascending: %d after %d", n, prev)
		}
		prev = n
		found = found || n == t.num
	}
	if !found && t.num >= from && t.num <= to {
		return c.failf("LISTGROUP %s %d-%d does not list article %d that STAT found", t.g.name, from, to, t.num)
	}
	if _, _, err := s.single([]int{411}, "LISTGROUP %s", unknownGroup); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

func checkNextLast(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	t, r, ok := s.pickTarget(c)
	if !ok {
		return r
	}
	// pickTarget left the first article current: there is no previous one
	if _, _, err := s.single([]int{422}, "LAST"); err != nil {
		return c.fail(err.Error())
	}
	code, line, err := s.single([]int{223, 421}, "NEXT")
	if err != nil {
		return c.fail(err.Error())
	}
	if code == 421 {
		return c.pass() // a group with one article: NEXT at the end answered 421
	}
	n, _, err := parseArticleLine(line)
	if err != nil {
		return c.fail(err.Error())
	}
	if n <= t.num {
		return c.failf("NEXT from %d gave %q: the number must grow", t.num, line)
	}
	if _, line, err = s.single([]int{223}, "LAST"); err != nil {
		return c.fail(err.Error())
	}
	back, id, err := parseArticleLine(line)
	if err != nil {
		return c.fail(err.Error())
	}
	if back != t.num || id != t.id {
		return c.failf("LAST from %d gave %q, want %d %s", n, line, t.num, t.id)
	}
	// the end: at the high water mark article NEXT must answer 421
	code, line, err = s.single([]int{223, 423}, "STAT %d", t.g.high)
	if err != nil {
		return c.fail(err.Error())
	}
	if code == 423 {
		r := c.pass()
		r.Detail = fmt.Sprintf("421 at the end not checked: the high water mark article %d does not exist (%q)", t.g.high, line)
		return r
	}
	if _, _, err := s.single([]int{421}, "NEXT"); err != nil {
		return c.failf("at the high water mark article %d: %v", t.g.high, err)
	}
	return c.pass()
}

// articleCheck returns the check of ARTICLE, HEAD, BODY or STAT (status code ok) by number,
// as the current article and by message-id; the message-id form must not change the current
// article.
func articleCheck(cmd string, ok int) func(*session, check) Result {
	return func(s *session, c check) Result {
		if r, done := s.ready(c); !done {
			return r
		}
		t, r, done := s.pickTarget(c)
		if !done {
			return r
		}
		for _, arg := range []string{strconv.FormatInt(t.num, 10), "", t.id} {
			command := strings.TrimSpace(cmd + " " + arg)
			var line string
			var lines []string
			var err error
			if ok == 223 {
				_, line, err = s.single([]int{ok}, "%s", command)
			} else {
				line, lines, err = s.multi(ok, "%s", command)
			}
			if err != nil {
				return c.fail(err.Error())
			}
			n, id, err := parseArticleLine(line)
			if err != nil {
				return c.fail(err.Error())
			}
			if id != t.id {
				return c.failf("%s: reply %q, want message-id %s", command, line, t.id)
			}
			if arg != t.id && n != t.num {
				return c.failf("%s: reply %q, want article number %d", command, line, t.num)
			}
			if err := checkArticleBlock(cmd, lines, t.id); err != nil {
				return c.failf("%s: %v", command, err)
			}
		}
		// the message-id form must not have moved the current article
		_, line, err := s.single([]int{223}, "STAT")
		if err != nil {
			return c.failf("current article after %s %s: %v", cmd, t.id, err)
		}
		if n, _, err := parseArticleLine(line); err != nil || n != t.num {
			return c.failf("current article after %s %s: STAT gave %q, want %d (the message-id form must not change it)", cmd, t.id, line, t.num)
		}
		return c.pass()
	}
}

// checkArticleBlock checks the block of an ARTICLE (head, blank line, body), HEAD (head) or
// BODY reply.
func checkArticleBlock(cmd string, lines []string, id string) error {
	switch cmd {
	case "ARTICLE":
		sep := slices.Index(lines, "")
		if sep < 0 {
			return fmt.Errorf("no empty line between head and body")
		}
		return checkHeaders(lines[:sep], id)
	case "HEAD":
		return checkHeaders(lines, id)
	}
	return nil
}

func checkNoGroup(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	for _, cmd := range []string{"ARTICLE", "HEAD", "BODY", "STAT", "ARTICLE 1", "HEAD 1", "BODY 1", "STAT 1", "NEXT", "LAST", "LISTGROUP"} {
		if _, _, err := s.single([]int{412}, "%s", cmd); err != nil {
			return c.failf("without a selected group: %v", err)
		}
	}
	return c.pass()
}

func checkNoCurrent(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	g, r, ok := s.emptyGroup(c)
	if !ok {
		return r
	}
	cmds := []string{"ARTICLE", "HEAD", "BODY", "STAT", "NEXT", "LAST"}
	if s.has("OVER") {
		cmds = append(cmds, "OVER")
	}
	for _, cmd := range cmds {
		if _, _, err := s.single([]int{420}, "%s", cmd); err != nil {
			return c.failf("in the empty group %s: %v", g.name, err)
		}
	}
	return c.pass()
}

func checkOutOfRange(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	g, r, ok := s.anyGroup(c)
	if !ok {
		return r
	}
	n := farNumber(g.high)
	for _, cmd := range []string{"ARTICLE", "HEAD", "BODY", "STAT"} {
		if _, _, err := s.single([]int{423}, "%s %d", cmd, n); err != nil {
			return c.failf("in %s (high %d): %v", g.name, g.high, err)
		}
	}
	return c.pass()
}

func checkUnknownID(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	for _, cmd := range []string{"ARTICLE", "HEAD", "BODY", "STAT"} {
		if _, _, err := s.single([]int{430}, "%s %s", cmd, unknownID); err != nil {
			return c.fail(err.Error())
		}
	}
	return c.pass()
}

// listKeyword reports whether the LIST capability names kw; when it does not, ok is false and
// r fails c when capability mandatory is advertised (RFC 3977 §3.3.2/§8.4: ACTIVE and
// NEWSGROUPS with READER, OVERVIEW.FMT with OVER), else skips it.
func (s *session) listKeyword(c check, kw, mandatory string) (r Result, ok bool) {
	if slices.ContainsFunc(capArgs(s.caps, "LIST"), func(a string) bool { return strings.EqualFold(a, kw) }) {
		return Result{}, true
	}
	if s.has(mandatory) {
		return c.failf("LIST capability does not name %s, which is mandatory with %s", kw, mandatory), false
	}
	return c.skip("LIST capability does not name " + kw), false
}

func checkListActive(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if r, ok := s.listKeyword(c, "ACTIVE", "READER"); !ok {
		return r
	}
	var unusual []string
	for _, cmd := range []string{"LIST", "LIST ACTIVE"} {
		_, lines, err := s.multi(215, "%s", cmd)
		if err != nil {
			return c.fail(err.Error())
		}
		for _, l := range lines {
			e, err := parseActive(l)
			if err != nil {
				return c.failf("%s: %v", cmd, err)
			}
			if !usualStatus.MatchString(e.status) && !slices.Contains(unusual, e.status) && len(unusual) < 5 {
				unusual = append(unusual, e.status)
			}
		}
		if cmd == "LIST" || len(lines) == 0 {
			continue
		}
		// the wildmat form with one group's exact name lists that group
		e, _ := parseActive(lines[0])
		_, wl, err := s.multi(215, "LIST ACTIVE %s", e.name)
		if err != nil {
			return c.fail(err.Error())
		}
		if !slices.ContainsFunc(wl, func(l string) bool { f := strings.Fields(l); return len(f) > 0 && f[0] == e.name }) {
			return c.failf("LIST ACTIVE %s does not list %s: %q", e.name, e.name, wl)
		}
	}
	r := c.pass()
	if len(unusual) > 0 {
		r.Detail = fmt.Sprintf("private status values (allowed by §7.6.3.2): %q", unusual)
	}
	return r
}

func checkListNewsgroups(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if r, ok := s.listKeyword(c, "NEWSGROUPS", "READER"); !ok {
		return r
	}
	_, lines, err := s.multi(215, "LIST NEWSGROUPS")
	if err != nil {
		return c.fail(err.Error())
	}
	for _, l := range lines {
		if l == "" || l[0] == ' ' || l[0] == '\t' {
			return c.failf("LIST NEWSGROUPS line %q: want \"name description\"", l)
		}
	}
	return c.pass()
}

// mandatoryOverview are the first seven OVERVIEW.FMT lines (RFC 3977 §8.4.2), each with the
// legacy spelling a server may use instead.
var mandatoryOverview = [][2]string{
	{"Subject:", "Subject:"}, {"From:", "From:"}, {"Date:", "Date:"}, {"Message-ID:", "Message-ID:"},
	{"References:", "References:"}, {":bytes", "Bytes:"}, {":lines", "Lines:"},
}

// overviewFmt sends LIST OVERVIEW.FMT and checks its lines.
func (s *session) overviewFmt() ([]string, error) {
	_, lines, err := s.multi(215, "LIST OVERVIEW.FMT")
	if err != nil {
		return nil, err
	}
	if len(lines) < len(mandatoryOverview) {
		return lines, fmt.Errorf("LIST OVERVIEW.FMT lists %d fields, want at least the %d mandatory ones: %q", len(lines), len(mandatoryOverview), lines)
	}
	for i, m := range mandatoryOverview {
		if !strings.EqualFold(lines[i], m[0]) && !strings.EqualFold(lines[i], m[1]) {
			return lines, fmt.Errorf("LIST OVERVIEW.FMT line %d is %q, want %q", i+1, lines[i], m[0])
		}
	}
	for _, l := range lines[len(mandatoryOverview):] {
		if !strings.HasPrefix(l, ":") && !strings.HasSuffix(strings.ToLower(l), ":full") {
			return lines, fmt.Errorf("LIST OVERVIEW.FMT line %q: an additional field is \"Name:full\" or \":metadata\"", l)
		}
	}
	return lines, nil
}

func checkListOverviewFmt(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if r, ok := s.listKeyword(c, "OVERVIEW.FMT", "OVER"); !ok {
		return r
	}
	if _, err := s.overviewFmt(); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

func checkListUnknown(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if _, _, err := s.single([]int{501}, "LIST XCONFORMANCE-NO-SUCH-KEYWORD"); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

// checkOverLine checks one OVER line: the field count (number + the 7 mandatory fields, and
// not more than the OVERVIEW.FMT fields; §8.3.2 lets trailing empty fields go), the number (want >= 0), the Message-ID field
// (wantID when not ""), :bytes and :lines, and the "Name: value" form of the full fields.
func checkOverLine(l string, fmtLines []string, want int64, wantID string) (int64, error) {
	f := strings.Split(l, "\t")
	if len(f) < 1+len(mandatoryOverview) {
		return 0, fmt.Errorf("OVER line %q has %d fields, want at least %d: the number and the %d mandatory fields "+
			"(trailing empty fields may be left out, but :bytes and :lines are never empty)", l, len(f), 1+len(mandatoryOverview), len(mandatoryOverview))
	}
	if len(f) > 1+len(fmtLines) {
		return 0, fmt.Errorf("OVER line %q has %d fields, want at most %d (number and the %d of LIST OVERVIEW.FMT)", l, len(f), 1+len(fmtLines), len(fmtLines))
	}
	n, err := parseNum(f[0])
	if err != nil {
		return 0, fmt.Errorf("OVER line %q: first field %q is not an article number", l, f[0])
	}
	if want >= 0 && n != want {
		return n, fmt.Errorf("OVER line %q: article number %d, want %d", l, n, want)
	}
	if wantID != "" && f[4] != wantID {
		return n, fmt.Errorf("OVER line %q: Message-ID field %q, want %s", l, f[4], wantID)
	}
	for i, name := range []string{":bytes", ":lines"} {
		if v := f[6+i]; v != "" {
			if _, err := parseNum(v); err != nil {
				return n, fmt.Errorf("OVER line %q: %s field %q is not a number", l, name, v)
			}
		}
	}
	for i := 1 + len(mandatoryOverview); i < len(f) && i-1 < len(fmtLines); i++ {
		name, full := strings.CutSuffix(strings.ToLower(fmtLines[i-1]), ":full")
		if !full || f[i] == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(f[i]), name+":") {
			return n, fmt.Errorf("OVER line %q: field %d %q does not start with %q", l, i+1, f[i], fmtLines[i-1][:len(name)+1])
		}
	}
	return n, nil
}

func checkOver(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if r, ok := s.listKeyword(c, "OVERVIEW.FMT", "OVER"); !ok {
		return r // OVER is advertised (Needs): a missing OVERVIEW.FMT fails
	}
	fmtLines, err := s.overviewFmt()
	if err != nil {
		return c.fail(err.Error())
	}
	t, r, ok := s.pickTarget(c)
	if !ok {
		return r
	}
	for _, arg := range []string{strconv.FormatInt(t.num, 10), ""} {
		cmd := strings.TrimSpace("OVER " + arg)
		_, lines, err := s.multi(224, "%s", cmd)
		if err != nil {
			return c.fail(err.Error())
		}
		if len(lines) != 1 {
			return c.failf("%s: %d lines, want 1: %q", cmd, len(lines), lines)
		}
		if _, err := checkOverLine(lines[0], fmtLines, t.num, t.id); err != nil {
			return c.failf("%s: %v", cmd, err)
		}
	}
	from, to := t.num, t.num+9
	_, lines, err := s.multi(224, "OVER %d-%d", from, to)
	if err != nil {
		return c.fail(err.Error())
	}
	var prev int64 = -1
	for i, l := range lines {
		wantID := ""
		if i == 0 {
			wantID = t.id
		}
		n, err := checkOverLine(l, fmtLines, -1, wantID)
		if err != nil {
			return c.failf("OVER %d-%d: %v", from, to, err)
		}
		if n < from || n > to || n <= prev {
			return c.failf("OVER %d-%d: article number %d out of range or order (previous %d)", from, to, n, prev)
		}
		prev = n
	}
	if len(lines) == 0 {
		return c.failf("OVER %d-%d: no lines, but article %d exists", from, to, t.num)
	}
	if slices.ContainsFunc(capArgs(s.caps, "OVER"), func(a string) bool { return strings.EqualFold(a, "MSGID") }) {
		_, lines, err := s.multi(224, "OVER %s", t.id)
		if err != nil {
			return c.fail(err.Error())
		}
		if len(lines) != 1 {
			return c.failf("OVER %s: %d lines, want 1: %q", t.id, len(lines), lines)
		}
		if _, err := checkOverLine(lines[0], fmtLines, -1, t.id); err != nil {
			return c.failf("OVER %s: %v", t.id, err)
		}
	}
	return c.pass()
}

func checkOverErrors(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	for _, cmd := range []string{"OVER", "OVER 1-2"} {
		if _, _, err := s.single([]int{412}, "%s", cmd); err != nil {
			return c.failf("without a selected group: %v", err)
		}
	}
	g, r, ok := s.anyGroup(c)
	if !ok {
		return r
	}
	if _, _, err := s.single([]int{423}, "OVER %d-%d", farNumber(g.high), min(farNumber(g.high)+10, maxArticle)); err != nil {
		return c.failf("in %s (high %d): %v", g.name, g.high, err)
	}
	if slices.ContainsFunc(capArgs(s.caps, "OVER"), func(a string) bool { return strings.EqualFold(a, "MSGID") }) {
		if _, _, err := s.single([]int{430}, "OVER %s", unknownID); err != nil {
			return c.fail(err.Error())
		}
	}
	return c.pass()
}

func checkDate(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	_, line, err := s.single([]int{111}, "DATE")
	if err != nil {
		return c.fail(err.Error())
	}
	f := strings.Fields(line)
	if len(f) < 2 || len(f[1]) != 14 {
		return c.failf("reply %q: want \"111 yyyymmddhhmmss\"", line)
	}
	if _, err := parseNum(f[1]); err != nil {
		return c.failf("reply %q: want \"111 yyyymmddhhmmss\"", line)
	}
	if _, err := time.ParseInLocation("20060102150405", f[1], time.UTC); err != nil {
		return c.failf("reply %q: %q is not a valid date and time", line, f[1])
	}
	return c.pass()
}

func checkHelp(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if _, _, err := s.multi(100, "HELP"); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

func checkUnknownCommand(s *session, c check) Result {
	if _, _, err := s.single([]int{500}, "XCONFORMANCENOSUCHCOMMAND"); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

func checkBadArguments(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if _, _, err := s.single([]int{501}, "GROUP"); err != nil {
		return c.failf("GROUP without its argument: %v", err)
	}
	return c.pass()
}

func checkQuit(s *session, c check) Result {
	if _, _, err := s.expect([]int{205}, "QUIT"); err != nil {
		return c.fail(err.Error())
	}
	// after 205 the server closes: the next read must see EOF (a reset counts as closed), not
	// data, and not the hang guard
	b, err := s.c.R.ReadByte()
	switch {
	case err == nil:
		rest, _ := s.c.R.Peek(s.c.R.Buffered())
		return c.failf("after 205: %q pending instead of the connection closing", append([]byte{b}, rest...))
	// syscall.ECONNRESET exists on the platforms the kit runs on (linux, darwin, windows)
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed):
		return c.pass()
	case errors.Is(err, os.ErrDeadlineExceeded):
		return c.failf("after 205: the server did not close the connection within the hang guard (%v)", s.o.deadline())
	default:
		return c.failf("after 205: %v", err)
	}
}
