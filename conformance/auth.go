package conformance

import (
	"fmt"
	"strings"
	"time"
)

// asQuietWindow is how long the auth and stream checks wait for stray bytes after their last
// reply (session.quiet): bytes arriving within it fail the check.
const asQuietWindow = 50 * time.Millisecond

// authChecks are the RFC 4643 AUTHINFO USER/PASS checks. They need Options.User (Auth). They
// do not need AUTHINFO in the capability list: transit servers such as innd often require
// authentication without listing it, so without AUTHINFO the login is attempted anyway; a
// check that works then passes with Detail "AUTHINFO not advertised", and one whose AUTHINFO
// command gets 500, 501 or 502 (the server takes no AUTHINFO) is skipped. A server that lists
// AUTHINFO without the USER argument (only SASL, or USER only after STARTTLS) gets Skip.
func authChecks() []check {
	return []check{
		{Name: "authinfo-user-pass", RFC: "RFC 4643 §2.3", Auth: true, Run: checkAuthUserPass},
		{Name: "authinfo-wrong-pass", RFC: "RFC 4643 §2.3.2", Auth: true, Run: checkAuthWrongPass},
		{Name: "authinfo-pass-without-user", RFC: "RFC 4643 §2.3.2", Auth: true, Run: checkAuthPassFirst},
		{Name: "authinfo-480", RFC: "RFC 4643 §2.2", Auth: true, Run: checkAuth480},
		{Name: "authinfo-after-auth", RFC: "RFC 4643 §2.2", Auth: true, Run: checkAuthTwice},
		{Name: "authinfo-capabilities", RFC: "RFC 4643 §2.1", Auth: true, Run: checkAuthCaps},
	}
}

// asNotAdvertised is the Detail of an auth check that passed without AUTHINFO listed.
const asNotAdvertised = "AUTHINFO not advertised"

// asUserPass reports whether s lists AUTHINFO, and why AUTHINFO USER/PASS cannot be checked
// ("" when it can): no credentials, or AUTHINFO listed without USER.
func asUserPass(s *session) (advertised bool, skip string) {
	if s.o.User == "" {
		return false, "no Options.User"
	}
	args := capArgs(s.caps, "AUTHINFO")
	if args == nil {
		return false, ""
	}
	for _, a := range args {
		if strings.EqualFold(a, "USER") {
			return true, ""
		}
	}
	return true, fmt.Sprintf("AUTHINFO advertised without USER (%q)", strings.Join(args, " "))
}

// asSettle is the end of every auth and stream check: nothing may arrive after the last reply.
func asSettle(s *session, c check) Result {
	if err := s.quiet(asQuietWindow); err != nil {
		return c.failf("after the last reply: %v", err)
	}
	return c.pass()
}

// asDetail sets the Detail of a passed result from the non-empty notes; other results are
// returned unchanged.
func asDetail(res Result, notes ...string) Result {
	if res.Status != Pass {
		return res
	}
	var keep []string
	for _, n := range notes {
		if n != "" {
			keep = append(keep, n)
		}
	}
	res.Detail = strings.Join(keep, "; ")
	return res
}

// asShown is the command as a Detail shows it: the password of AUTHINFO PASS is masked.
func asShown(format string, args ...any) string {
	shown := fmt.Sprintf(format, args...)
	if strings.HasPrefix(strings.ToUpper(shown), "AUTHINFO PASS ") {
		shown = "AUTHINFO PASS ***"
	}
	return shown
}

// asExpect sends one command and returns a Fail result (non-nil) when it fails on the wire or
// the code is not one of want.
func asExpect(s *session, c check, want []int, format string, args ...any) (int, string, *Result) {
	shown := asShown(format, args...)
	code, line, err := s.cmd(format, args...)
	if err != nil {
		r := c.failf("%s: %v", shown, err)
		return code, line, &r
	}
	if asIntIn(code, want) {
		return code, line, nil
	}
	r := c.failf("%s: got %q, want %s", shown, line, asCodes(want))
	return code, line, &r
}

// asAuthCmd sends one AUTHINFO command like asExpect, with two rules of its own: a 480 is a
// Fail (RFC 4643 §2.3.1), and when the server does not list AUTHINFO (advertised false) a 500,
// 501 or 502 is a Skip: it takes no AUTHINFO at all.
func asAuthCmd(s *session, c check, advertised bool, want []int, format string, args ...any) (int, string, *Result) {
	shown := asShown(format, args...)
	code, line, err := s.cmd(format, args...)
	if err != nil {
		r := c.failf("%s: %v", shown, err)
		return code, line, &r
	}
	var r Result
	switch {
	case asIntIn(code, want):
		return code, line, nil
	case code == 480:
		r = c.failf("%s: got %q: RFC 4643 §2.3.1 forbids a 480 in reply to AUTHINFO", shown, line)
	case !advertised && (code == 500 || code == 501 || code == 502):
		r = c.skip(fmt.Sprintf("%s; %s got %q", asNotAdvertised, shown, line))
	default:
		r = c.failf("%s: got %q, want %s", shown, line, asCodes(want))
	}
	return code, line, &r
}

// asAuthLogin logs in with AUTHINFO USER (381 or 281) and PASS (281) under the asAuthCmd rules.
func asAuthLogin(s *session, c check, advertised bool) *Result {
	code, _, r := asAuthCmd(s, c, advertised, []int{381, 281}, "AUTHINFO USER %s", s.o.User)
	if r != nil {
		return r
	}
	if code == 381 {
		if _, _, r := asAuthCmd(s, c, advertised, []int{281}, "AUTHINFO PASS %s", s.o.Pass); r != nil {
			return r
		}
	}
	s.authed = true
	return nil
}

// asCodes formats want as "381", "381 or 281", "238, 431 or 438".
func asCodes(want []int) string {
	parts := make([]string, len(want))
	for i, w := range want {
		parts[i] = fmt.Sprint(w)
	}
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}

// asIntIn reports whether v is in list.
func asIntIn(v int, list []int) bool {
	for _, w := range list {
		if v == w {
			return true
		}
	}
	return false
}

// asNotAdv returns asNotAdvertised when advertised is false, else "".
func asNotAdv(advertised bool) string {
	if advertised {
		return ""
	}
	return asNotAdvertised
}

// checkAuthUserPass: AUTHINFO USER → 381 (281 when no password is needed), AUTHINFO PASS → 281.
func checkAuthUserPass(s *session, c check) Result {
	adv, why := asUserPass(s)
	if why != "" {
		return c.skip(why)
	}
	if r := asAuthLogin(s, c, adv); r != nil {
		return *r
	}
	return asDetail(asSettle(s, c), asNotAdv(adv))
}

// checkAuthWrongPass: AUTHINFO USER → 381, AUTHINFO PASS with a wrong password → 481. A server
// that accepts any password (access by address) answers 281: policy, not protocol, so a Pass
// with Detail "any password accepted".
func checkAuthWrongPass(s *session, c check) Result {
	adv, why := asUserPass(s)
	if why != "" {
		return c.skip(why)
	}
	code, _, r := asAuthCmd(s, c, adv, []int{381, 281}, "AUTHINFO USER %s", s.o.User)
	if r != nil {
		return *r
	}
	if code == 281 {
		return c.skip("AUTHINFO USER answered 281: no password is asked for")
	}
	code, _, r = asAuthCmd(s, c, adv, []int{481, 281}, "AUTHINFO PASS %s", s.o.Pass+"-wrong-conformance")
	if r != nil {
		return *r
	}
	policy := ""
	if code == 281 {
		policy = "any password accepted"
	}
	return asDetail(asSettle(s, c), policy, asNotAdv(adv))
}

// checkAuthPassFirst: AUTHINFO PASS without AUTHINFO USER before it → 482.
func checkAuthPassFirst(s *session, c check) Result {
	adv, why := asUserPass(s)
	if why != "" {
		return c.skip(why)
	}
	if _, _, r := asAuthCmd(s, c, adv, []int{482}, "AUTHINFO PASS %s", s.o.Pass+"-out-of-sequence"); r != nil {
		return *r
	}
	return asDetail(asSettle(s, c), asNotAdv(adv))
}

// checkAuth480: GROUP before authentication → 480. A server that answers it (211 or 411)
// does not restrict reading: Pass with Detail "not restricted".
func checkAuth480(s *session, c check) Result {
	adv := capArgs(s.caps, "AUTHINFO") != nil
	group := s.o.PostGroup
	if group == "" {
		group = "misc.test"
	}
	code, line, r := asExpect(s, c, []int{480, 211, 411}, "GROUP %s", group)
	if r != nil {
		return *r
	}
	if code != 480 {
		return asDetail(asSettle(s, c), fmt.Sprintf("not restricted: GROUP before AUTHINFO got %q", line))
	}
	return asDetail(asSettle(s, c), asNotAdv(adv))
}

// checkAuthTwice: AUTHINFO USER after a successful authentication → 502.
func checkAuthTwice(s *session, c check) Result {
	adv, why := asUserPass(s)
	if why != "" {
		return c.skip(why)
	}
	if r := asAuthLogin(s, c, adv); r != nil {
		return *r
	}
	// the server has taken AUTHINFO: a 500/502 is no longer a Skip, 502 is the answer
	if _, _, r := asAuthCmd(s, c, true, []int{502}, "AUTHINFO USER %s", s.o.User); r != nil {
		return *r
	}
	return asDetail(asSettle(s, c), asNotAdv(adv))
}

// checkAuthCaps: CAPABILITIES before authentication lists AUTHINFO USER, after it no AUTHINFO.
func checkAuthCaps(s *session, c check) Result {
	if s.caps == nil {
		return c.skip(fmt.Sprintf("no capability list: %v", s.capsErr))
	}
	adv, why := asUserPass(s)
	if why != "" {
		return c.skip(why)
	}
	if r := asAuthLogin(s, c, adv); r != nil {
		return *r
	}
	after, err := s.capabilities()
	if err != nil {
		return c.failf("CAPABILITIES after AUTHINFO: %v", err)
	}
	if args := capArgs(after, "AUTHINFO"); args != nil {
		return c.failf("CAPABILITIES after AUTHINFO still lists %q", strings.TrimSpace("AUTHINFO "+strings.Join(args, " ")))
	}
	return asDetail(asSettle(s, c), asNotAdv(adv))
}
