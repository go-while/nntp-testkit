package conformance

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file holds the RFC 3977 checks of the commands beyond the basic reader set: LISTGROUP
// with a range, LIST ACTIVE.TIMES, LIST HEADERS and HDR, NEWGROUPS, NEWNEWS and POST.

// pollInterval is how often post asks again for the article it posted (STAT by message-id)
// until the hang guard runs out: a server may file a posting a moment after its 240.
const pollInterval = 100 * time.Millisecond

// newsDate formats t (UTC) as the date and time arguments of NEWGROUPS and NEWNEWS, with a
// four-digit year (§7.3.2: clients SHOULD) or, short, the two-digit form servers still take.
func newsDate(t time.Time, short bool) string {
	if short {
		return t.UTC().Format("060102 150405")
	}
	return t.UTC().Format("20060102 150405")
}

// listedNumbers parses a LISTGROUP block: each line an article number, ascending, within lo..hi.
func listedNumbers(cmd string, lines []string, lo, hi int64) ([]int64, error) {
	var out []int64
	for _, l := range lines {
		n, err := parseNum(l)
		if err != nil {
			return out, fmt.Errorf("%s: line %q is not an article number", cmd, l)
		}
		if n < lo || n > hi {
			return out, fmt.Errorf("%s: number %d is outside the range %d-%d asked for", cmd, n, lo, hi)
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			return out, fmt.Errorf("%s: numbers not ascending: %d after %d", cmd, n, out[len(out)-1])
		}
		out = append(out, n)
	}
	return out, nil
}

// checkListgroupRange: the three range forms of §6.1.2 ("n", "n-m", "n-") list only the
// numbers in the range; "n-m" with m < n lists none; and LISTGROUP with a range leaves the
// current article where GROUP puts it (the first article), even outside the range.
func checkListgroupRange(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	t, r, ok := s.pickTarget(c)
	if !ok {
		return r
	}
	// where GROUP leaves the current article, for the comparison at the end
	if _, err := s.selectGroup(t.g.name); err != nil {
		return c.fail(err.Error())
	}
	_, before, err := s.single([]int{223, 420, 423}, "STAT")
	if err != nil {
		return c.fail(err.Error())
	}
	high := max(t.g.high, t.num)
	cases := []struct {
		rng    string
		lo, hi int64
		exact  []int64 // nil: any subset of lo..hi
	}{
		{strconv.FormatInt(t.num, 10), t.num, t.num, []int64{t.num}},
		{fmt.Sprintf("%d-%d", t.num, t.num), t.num, t.num, []int64{t.num}},
		{fmt.Sprintf("%d-%d", t.num+1, t.num), 1, 0, []int64{}},
		{fmt.Sprintf("%d-", high), high, high, nil},
	}
	for _, tc := range cases {
		cmd := fmt.Sprintf("LISTGROUP %s %s", t.g.name, tc.rng)
		line, lines, err := s.multi(211, "%s", cmd)
		if err != nil {
			return c.fail(err.Error())
		}
		if _, err := parseGroupLine(line, t.g.name); err != nil {
			return c.failf("%s: %v", cmd, err)
		}
		if tc.lo > tc.hi { // the empty range
			if len(lines) != 0 {
				return c.failf("%s: %d numbers %q, want none (the second number is below the first)", cmd, len(lines), lines[:min(5, len(lines))])
			}
			continue
		}
		got, err := listedNumbers(cmd, lines, tc.lo, tc.hi)
		if err != nil {
			return c.fail(err.Error())
		}
		if tc.exact != nil && !slices.Equal(got, tc.exact) {
			return c.failf("%s: %v, want %v (article %d exists: STAT found it)", cmd, got, tc.exact, t.num)
		}
	}
	_, after, err := s.single([]int{223, 420, 423}, "STAT")
	if err != nil {
		return c.fail(err.Error())
	}
	if replyCode(after) != replyCode(before) || (replyCode(after) == 223 && field(after, 1) != field(before, 1)) {
		return c.failf("current article after LISTGROUP %s %d-: STAT gave %q, after GROUP %q (LISTGROUP sets it as GROUP does, whatever the range)", t.g.name, high, after, before)
	}
	return c.pass()
}

// replyCode returns the status code of a reply line (0 when it has none).
func replyCode(line string) int {
	n, _ := strconv.Atoi(field(line, 0))
	return n
}

// field returns the i-th white-space separated field of line, or "".
func field(line string, i int) string {
	f := strings.Fields(line)
	if i < len(f) {
		return f[i]
	}
	return ""
}

// checkListActiveTimes: LIST ACTIVE.TIMES (§7.6.4, optional) when the LIST capability names
// it: each line "name time creator", time a number (seconds since 1970); the wildmat form with
// one listed group's exact name lists that group and no other.
func checkListActiveTimes(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if r, ok := s.listKeyword(c, "ACTIVE.TIMES", ""); !ok {
		return r
	}
	_, lines, err := s.multi(215, "LIST ACTIVE.TIMES")
	if err != nil {
		return c.fail(err.Error())
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 3 {
			return c.failf("LIST ACTIVE.TIMES line %q: want 3 fields \"name time creator\"", l)
		}
		if _, err := parseNum(f[1]); err != nil {
			return c.failf("LIST ACTIVE.TIMES line %q: the time %q is not a number of seconds", l, f[1])
		}
	}
	if len(lines) == 0 {
		r := c.pass()
		r.Detail = "the list is empty (allowed: it may omit groups without the information)"
		return r
	}
	name := strings.Fields(lines[0])[0]
	_, wl, err := s.multi(215, "LIST ACTIVE.TIMES %s", name)
	if err != nil {
		return c.fail(err.Error())
	}
	if len(wl) == 0 {
		return c.failf("LIST ACTIVE.TIMES %s lists nothing, but LIST ACTIVE.TIMES listed %s", name, name)
	}
	for _, l := range wl {
		if f := strings.Fields(l); len(f) == 0 || f[0] != name {
			return c.failf("LIST ACTIVE.TIMES %s: line %q does not match the wildmat", name, l)
		}
	}
	return c.pass()
}

// checkListHeaders: with HDR advertised the LIST capability must name HEADERS (§7.6.2), and
// LIST HEADERS, LIST HEADERS MSGID and LIST HEADERS RANGE answer 215 with one field per line:
// a header name without the colon, a metadata item with its leading colon, or ":" alone (any
// header), in which case no header name may be listed (§8.6.2).
func checkListHeaders(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	if r, ok := s.listKeyword(c, "HEADERS", "HDR"); !ok {
		return r
	}
	for _, cmd := range []string{"LIST HEADERS", "LIST HEADERS MSGID", "LIST HEADERS RANGE"} {
		_, lines, err := s.multi(215, "%s", cmd)
		if err != nil {
			return c.fail(err.Error())
		}
		anyHeader := slices.Contains(lines, ":")
		for _, l := range lines {
			switch {
			case l == "" || strings.ContainsAny(l, " \t"):
				return c.failf("%s: line %q: want one field name, without white space", cmd, l)
			case l == ":":
			case strings.HasPrefix(l, ":"):
				if strings.Contains(l[1:], ":") {
					return c.failf("%s: line %q is not a metadata item (\":name\")", cmd, l)
				}
			case strings.Contains(l, ":"):
				return c.failf("%s: line %q: a header name is listed without its colon", cmd, l)
			case anyHeader:
				return c.failf("%s: header name %q listed together with \":\" (any header): only metadata items may follow \":\"", cmd, l)
			}
		}
	}
	return c.pass()
}

// hdrLine splits one HDR line into the article number and the value ("n value"; the space is
// optional when the value is empty, §8.5.2).
func hdrLine(l string) (int64, string, error) {
	num, val, _ := strings.Cut(l, " ")
	n, err := parseNum(num)
	if err != nil {
		return 0, "", fmt.Errorf("line %q does not start with an article number", l)
	}
	return n, val, nil
}

// hdrValue returns the value HDR must give for header name in head (the first occurrence,
// unfolded, TABs as spaces, §8.5.2), compared with white space at either end trimmed.
func hdrValue(head []string, name string) string {
	var v strings.Builder
	in := false
	for _, l := range head {
		if in && l != "" && (l[0] == ' ' || l[0] == '\t') {
			v.WriteString(l)
			continue
		}
		if in {
			break
		}
		if k, rest, ok := strings.Cut(l, ":"); ok && strings.EqualFold(k, name) {
			v.WriteString(rest)
			in = true
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(v.String(), "\t", " "))
}

// checkHdr: HDR (§8.5) in its three forms, against the article's own Subject header (read with
// HEAD): the current article (one line, its number), a range (ascending numbers in the range,
// starting with the article) and the message-id (one line, number 0 or the article's number in
// the selected group); 412 without a group, 423 for a range without articles, 430 for an
// unknown message-id. A 503 for Subject passes with a note when LIST HEADERS does not offer it
// (a server MAY limit the fields).
func checkHdr(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	for _, cmd := range []string{"HDR Subject", "HDR Subject 1-2"} {
		if _, _, err := s.single([]int{412}, "%s", cmd); err != nil {
			return c.failf("without a selected group: %v", err)
		}
	}
	t, r, ok := s.pickTarget(c)
	if !ok {
		return r
	}
	_, head, err := s.multi(221, "HEAD %d", t.num)
	if err != nil {
		return c.fail(err.Error())
	}
	want := hdrValue(head, "Subject")
	from, to := t.num, t.num+9
	forms := []struct {
		arg     string
		nums    func(n int64) bool // accepted article numbers
		oneLine bool
	}{
		{"", func(n int64) bool { return n == t.num }, true},
		{fmt.Sprintf("%d-%d", from, to), func(n int64) bool { return n >= from && n <= to }, false},
		{t.id, func(n int64) bool { return n == 0 || n == t.num }, true},
	}
	for _, f := range forms {
		cmd := strings.TrimSpace("HDR Subject " + f.arg)
		code, line, err := s.expect([]int{225, 503}, "%s", cmd)
		if err != nil {
			return c.fail(err.Error())
		}
		if code == 503 {
			return hdrRefused(s, c, cmd, line)
		}
		lines, err := s.block()
		if err != nil {
			return c.failf("%s: block after %q: %s", cmd, line, describe(err))
		}
		if err := s.quiet(quietWindow); err != nil {
			return c.failf("after the block of %s: %s", cmd, describe(err))
		}
		switch {
		case f.oneLine && len(lines) != 1:
			return c.failf("%s: %d lines %q, want 1", cmd, len(lines), lines)
		case len(lines) == 0:
			return c.failf("%s: no lines, but article %d exists", cmd, t.num)
		}
		var prev int64 = -1
		for i, l := range lines {
			n, v, err := hdrLine(l)
			if err != nil {
				return c.failf("%s: %v", cmd, err)
			}
			if !f.nums(n) || n <= prev {
				return c.failf("%s: line %q: article number %d out of range or order", cmd, l, n)
			}
			prev = n
			if i > 0 {
				continue
			}
			if !f.oneLine && n != t.num {
				return c.failf("%s: first line %q, want article %d", cmd, l, t.num)
			}
			if strings.TrimSpace(v) != want {
				return c.failf("%s: line %q: value %q, want the Subject of article %d %q", cmd, l, v, t.num, want)
			}
		}
	}
	far := farNumber(t.g.high)
	if _, _, err := s.single([]int{423}, "HDR Subject %d-%d", far, min(far+10, maxArticle)); err != nil {
		return c.failf("in %s (high %d): %v", t.g.name, t.g.high, err)
	}
	if _, _, err := s.single([]int{430}, "HDR Subject %s", unknownID); err != nil {
		return c.fail(err.Error())
	}
	return c.pass()
}

// hdrRefused is the result of HDR Subject answered with 503: a Fail when LIST HEADERS offers
// any header (":") or Subject, else a Pass with a note (§8.5.2: a server MAY limit the fields,
// and SHOULD then not list them).
func hdrRefused(s *session, c check, cmd, line string) Result {
	_, lines, err := s.multi(215, "LIST HEADERS")
	if err != nil {
		return c.failf("%s: %q; then %v", cmd, line, err)
	}
	if slices.Contains(lines, ":") || slices.ContainsFunc(lines, func(l string) bool { return strings.EqualFold(l, "Subject") }) {
		return c.failf("%s: %q, but LIST HEADERS offers it: %q", cmd, line, lines)
	}
	r := c.pass()
	r.Detail = fmt.Sprintf("Subject not offered: %s answered %q and LIST HEADERS does not list it", cmd, line)
	return r
}

// checkNewgroups: NEWGROUPS (§7.3) answers 231 with LIST ACTIVE lines for a date a year back,
// in both the four- and the two-digit year form, and lists no group for a date two days ahead.
func checkNewgroups(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	past := time.Now().AddDate(-1, 0, 0)
	for _, short := range []bool{false, true} {
		cmd := "NEWGROUPS " + newsDate(past, short) + " GMT"
		_, lines, err := s.multi(231, "%s", cmd)
		if err != nil {
			return c.fail(err.Error())
		}
		for _, l := range lines {
			if _, err := parseActive(l); err != nil {
				return c.failf("%s: %v", cmd, err)
			}
		}
	}
	cmd := "NEWGROUPS " + newsDate(time.Now().Add(48*time.Hour), false) + " GMT"
	_, lines, err := s.multi(231, "%s", cmd)
	if err != nil {
		return c.fail(err.Error())
	}
	if len(lines) != 0 {
		return c.failf("%s (two days ahead): %d groups %q, want none", cmd, len(lines), lines[:min(5, len(lines))])
	}
	return c.pass()
}

// checkNewnews: NEWNEWS (§7.4) for one group: 230 with one message-id per line for a date a
// week back, none for a date two days ahead.
func checkNewnews(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	g, r, ok := s.anyGroup(c)
	if !ok {
		return r
	}
	cmd := fmt.Sprintf("NEWNEWS %s %s GMT", g.name, newsDate(time.Now().AddDate(0, 0, -7), false))
	_, lines, err := s.multi(230, "%s", cmd)
	if err != nil {
		return c.fail(err.Error())
	}
	for _, l := range lines {
		if !validID(l) {
			return c.failf("%s: line %q is not a message-id", cmd, l)
		}
	}
	cmd = fmt.Sprintf("NEWNEWS %s %s GMT", g.name, newsDate(time.Now().Add(48*time.Hour), false))
	_, lines, err = s.multi(230, "%s", cmd)
	if err != nil {
		return c.fail(err.Error())
	}
	if len(lines) != 0 {
		return c.failf("%s (two days ahead): %d message-ids %q, want none", cmd, len(lines), lines[:min(5, len(lines))])
	}
	return c.pass()
}

// checkPost: POST (§6.3.1) into Options.PostGroup: 340, the article (with a dot-stuffed body
// line), 240; then the article is found by its message-id (STAT, asked again until the hang
// guard runs out: filing may lag the 240) and ARTICLE gives the body line back un-stuffed. A
// 441 is a Skip naming the reply (the server refused this article, e.g. by a filter).
func checkPost(s *session, c check) Result {
	if s.o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	if r, ok := s.ready(c); !ok {
		return r
	}
	id := asNewID(c.Name)
	if _, _, err := s.expect([]int{340}, "POST"); err != nil {
		return c.fail(err.Error())
	}
	if err := s.c.Send(asArticle(s.o.PostGroup, c.Name, id)); err != nil {
		return c.failf("POST: article: %v", err)
	}
	code, line, err := s.c.ReadStatus()
	switch {
	case err != nil:
		return c.failf("POST (article sent): %s", describe(err))
	case code == 441:
		return c.skipf("server refused the article: %s", line)
	case code != 240:
		return c.failf("POST (article sent): got %q, want 240", line)
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after %q: %s", line, describe(err))
	}
	stop := time.Now().Add(s.o.deadline())
	for {
		code, sline, err := s.single([]int{223, 430}, "STAT %s", id)
		if err != nil {
			return c.fail(err.Error())
		}
		if code == 223 {
			break
		}
		if time.Now().After(stop) {
			return c.failf("STAT %s: still %q %v after the 240", id, sline, s.o.deadline())
		}
		time.Sleep(pollInterval)
	}
	_, lines, err := s.multi(220, "ARTICLE %s", id)
	if err != nil {
		return c.fail(err.Error())
	}
	const dotted = ".dotted line: sent dot-stuffed, stored with one leading dot"
	if !slices.Contains(lines, dotted) {
		return c.failf("ARTICLE %s: the body has no line %q (sent dot-stuffed)", id, dotted)
	}
	return c.pass()
}
