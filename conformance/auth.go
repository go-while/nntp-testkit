package conformance

import (
	"fmt"
	"strings"
	"time"
)

// asQuietWindow is how long the auth and stream checks wait for stray bytes after their last
// reply (session.quiet): bytes arriving within it fail the check.
const asQuietWindow = 50 * time.Millisecond

// authChecks are the RFC 4643 AUTHINFO USER/PASS checks. They need Options.User (Auth) and an
// AUTHINFO capability in the list read before authentication (Needs); a server that lists
// AUTHINFO without the USER argument (e.g. only SASL, or USER only after STARTTLS) gets Skip.
func authChecks() []check {
	return []check{
		{Name: "authinfo-user-pass", RFC: "RFC 4643 §2.3", Needs: "AUTHINFO", Auth: true, Run: checkAuthUserPass},
		{Name: "authinfo-wrong-pass", RFC: "RFC 4643 §2.3.2", Needs: "AUTHINFO", Auth: true, Run: checkAuthWrongPass},
		{Name: "authinfo-pass-without-user", RFC: "RFC 4643 §2.3.2", Needs: "AUTHINFO", Auth: true, Run: checkAuthPassFirst},
		{Name: "authinfo-480", RFC: "RFC 4643 §2.2", Needs: "AUTHINFO", Auth: true, Run: checkAuth480},
		{Name: "authinfo-after-auth", RFC: "RFC 4643 §2.2", Needs: "AUTHINFO", Auth: true, Run: checkAuthTwice},
		{Name: "authinfo-capabilities", RFC: "RFC 4643 §2.1", Needs: "AUTHINFO", Auth: true, Run: checkAuthCaps},
	}
}

// userPassOffered reports why AUTHINFO USER/PASS cannot be checked on s ("" when it can): no
// credentials, or AUTHINFO advertised without USER.
func userPassOffered(s *session) string {
	if s.o.User == "" {
		return "no Options.User"
	}
	args := capArgs(s.caps, "AUTHINFO")
	if args == nil {
		return "AUTHINFO not advertised"
	}
	for _, a := range args {
		if strings.EqualFold(a, "USER") {
			return ""
		}
	}
	return fmt.Sprintf("AUTHINFO advertised without USER (%q)", strings.Join(args, " "))
}

// asSettle is the end of every auth and stream check: nothing may arrive after the last reply.
func asSettle(s *session, c check) Result {
	if err := s.quiet(asQuietWindow); err != nil {
		return c.failf("after the last reply: %v", err)
	}
	return c.pass()
}

// asExpect sends one command and returns a Fail result (non-nil) when it fails on the wire or
// the code is not one of want.
func asExpect(s *session, c check, want []int, format string, args ...any) (int, string, *Result) {
	shown := fmt.Sprintf(format, args...)
	if strings.HasPrefix(strings.ToUpper(shown), "AUTHINFO PASS ") {
		shown = "AUTHINFO PASS ***"
	}
	code, line, err := s.cmd(format, args...)
	if err != nil {
		r := c.failf("%s: %v", shown, err)
		return code, line, &r
	}
	for _, w := range want {
		if code == w {
			return code, line, nil
		}
	}
	r := c.failf("%s: got %q, want %s", shown, line, asCodes(want))
	return code, line, &r
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

// checkAuthUserPass: AUTHINFO USER → 381 (281 when no password is needed), AUTHINFO PASS → 281.
func checkAuthUserPass(s *session, c check) Result {
	if why := userPassOffered(s); why != "" {
		return c.skip(why)
	}
	code, _, r := asExpect(s, c, []int{381, 281}, "AUTHINFO USER %s", s.o.User)
	if r != nil {
		return *r
	}
	if code == 381 {
		if _, _, r := asExpect(s, c, []int{281}, "AUTHINFO PASS %s", s.o.Pass); r != nil {
			return *r
		}
	}
	return asSettle(s, c)
}

// checkAuthWrongPass: AUTHINFO USER → 381, AUTHINFO PASS with a wrong password → 481.
func checkAuthWrongPass(s *session, c check) Result {
	if why := userPassOffered(s); why != "" {
		return c.skip(why)
	}
	code, _, r := asExpect(s, c, []int{381, 281}, "AUTHINFO USER %s", s.o.User)
	if r != nil {
		return *r
	}
	if code == 281 {
		return c.skip("AUTHINFO USER answered 281: no password is asked for")
	}
	if _, _, r := asExpect(s, c, []int{481}, "AUTHINFO PASS %s", s.o.Pass+"-wrong-conformance"); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkAuthPassFirst: AUTHINFO PASS without AUTHINFO USER before it → 482.
func checkAuthPassFirst(s *session, c check) Result {
	if why := userPassOffered(s); why != "" {
		return c.skip(why)
	}
	if _, _, r := asExpect(s, c, []int{482}, "AUTHINFO PASS %s", s.o.Pass+"-out-of-sequence"); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkAuth480: GROUP before authentication → 480. A server that answers it (211 or 411)
// does not restrict reading: Pass with Detail "not restricted".
func checkAuth480(s *session, c check) Result {
	if capArgs(s.caps, "AUTHINFO") == nil {
		return c.skip("AUTHINFO not advertised")
	}
	group := s.o.PostGroup
	if group == "" {
		group = "misc.test"
	}
	code, line, r := asExpect(s, c, []int{480, 211, 411}, "GROUP %s", group)
	if r != nil {
		return *r
	}
	res := asSettle(s, c)
	if res.Status == Pass && code != 480 {
		res.Detail = fmt.Sprintf("not restricted: GROUP before AUTHINFO got %q", line)
	}
	return res
}

// checkAuthTwice: AUTHINFO USER after a successful authentication → 502.
func checkAuthTwice(s *session, c check) Result {
	if why := userPassOffered(s); why != "" {
		return c.skip(why)
	}
	if err := s.auth(); err != nil {
		return c.failf("login: %v", err)
	}
	if _, _, r := asExpect(s, c, []int{502}, "AUTHINFO USER %s", s.o.User); r != nil {
		return *r
	}
	return asSettle(s, c)
}

// checkAuthCaps: CAPABILITIES before authentication lists AUTHINFO USER, after it no AUTHINFO.
func checkAuthCaps(s *session, c check) Result {
	if s.caps == nil {
		return c.skip(fmt.Sprintf("no capability list: %v", s.capsErr))
	}
	if why := userPassOffered(s); why != "" {
		return c.skip(why)
	}
	if err := s.auth(); err != nil {
		return c.failf("login: %v", err)
	}
	after, err := s.capabilities()
	if err != nil {
		return c.failf("CAPABILITIES after AUTHINFO: %v", err)
	}
	if args := capArgs(after, "AUTHINFO"); args != nil {
		return c.failf("CAPABILITIES after AUTHINFO still lists %q", strings.TrimSpace("AUTHINFO "+strings.Join(args, " ")))
	}
	return asSettle(s, c)
}
