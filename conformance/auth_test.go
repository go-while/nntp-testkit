package conformance

import (
	"regexp"
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// asRunCheck runs one check the way the runner does: the Auth/Posts skips, a fresh session
// with its capability list, the Needs skip, then Run.
func asRunCheck(t *testing.T, o Options, c check) Result {
	t.Helper()
	if c.Auth && o.User == "" {
		return c.skip("no Options.User")
	}
	if c.Posts && o.PostGroup == "" {
		return c.skip("no Options.PostGroup")
	}
	s, err := dial(o)
	if err != nil {
		t.Fatalf("%s: dial: %v", c.Name, err)
	}
	defer s.close()
	if _, err := s.capabilities(); err != nil {
		t.Fatalf("%s: capabilities: %v", c.Name, err)
	}
	if c.Needs != "" && !s.has(c.Needs) {
		return c.skip(c.Needs + " not advertised")
	}
	return c.Run(s, c)
}

// asRunAll runs every check of list and returns the results by name.
func asRunAll(t *testing.T, o Options, list []check) map[string]Result {
	t.Helper()
	out := make(map[string]Result, len(list))
	for _, c := range list {
		res := asRunCheck(t, o, c)
		t.Logf("%-4s %-28s %s %s", res.Status, res.Name, res.RFC, res.Detail)
		out[c.Name] = res
	}
	return out
}

// asFind returns the check called name from list.
func asFind(t *testing.T, list []check, name string) check {
	t.Helper()
	for _, c := range list {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q", name)
	return check{}
}

var asKebab = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestAuthStreamCheckNames(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range append(authChecks(), streamChecks()...) {
		if !asKebab.MatchString(c.Name) || c.RFC == "" || c.Run == nil {
			t.Errorf("check %+v: want a kebab-case name, an RFC and Run", c)
		}
		if seen[c.Name] {
			t.Errorf("duplicate check name %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, c := range authChecks() {
		if !c.Auth {
			t.Errorf("%s: auth check without Auth", c.Name)
		}
	}
}

func TestAuthChecksStore(t *testing.T) {
	st := fakeserver.NewStore()
	st.AddGroup("alt.test", "Testing")
	s := fakeserver.New(t, fakeserver.Options{Store: st, StoreOpts: fakeserver.StoreOptions{User: "u", Pass: "p"}})
	res := asRunAll(t, Options{Addr: s.Addr(), User: "u", Pass: "p", PostGroup: "alt.test"}, authChecks())
	for name, r := range res {
		if r.Status != Pass {
			t.Errorf("%s: %v %s, want PASS", name, r.Status, r.Detail)
		}
	}
	if d := res["authinfo-480"].Detail; d != "" {
		t.Errorf("authinfo-480 Detail %q, want empty (GROUP is restricted)", d)
	}
	for _, l := range s.Lines() {
		if strings.Contains(l, "AUTHINFO PASS p-") {
			return // the wrong and out-of-sequence passwords were sent
		}
	}
	t.Errorf("no wrong AUTHINFO PASS in %q", s.Lines())
}

func TestAuthChecksNoAuth(t *testing.T) {
	st := fakeserver.NewStore()
	st.AddGroup("alt.test", "Testing")
	s := fakeserver.New(t, fakeserver.Options{Store: st})
	// credentials given, but the server lists no AUTHINFO: every check is skipped
	for name, r := range asRunAll(t, Options{Addr: s.Addr(), User: "u", Pass: "p"}, authChecks()) {
		if r.Status != Skip && !(r.Status == Pass && strings.Contains(r.Detail, "not restricted")) {
			t.Errorf("%s: %v %s, want SKIP or PASS not restricted", name, r.Status, r.Detail)
		}
	}
	// no credentials: skipped by the Auth flag
	for name, r := range asRunAll(t, Options{Addr: s.Addr()}, authChecks()) {
		if r.Status != Skip {
			t.Errorf("%s: %v %s, want SKIP", name, r.Status, r.Detail)
		}
	}
}

// asAuthCaps is a capability list that offers AUTHINFO USER.
var asAuthCaps = []string{"101 caps", "VERSION 2", "READER", "AUTHINFO USER", "."}

func TestAuth480NotRestricted(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: asAuthCaps},
		{Expect: "GROUP misc.test", Reply: []string{"211 0 0 0 misc.test"}},
	}})
	r := asRunCheck(t, Options{Addr: s.Addr(), User: "u", Pass: "p"}, asFind(t, authChecks(), "authinfo-480"))
	if r.Status != Pass || !strings.Contains(r.Detail, "not restricted") {
		t.Fatalf("got %v %q, want PASS not restricted", r.Status, r.Detail)
	}
}

func TestAuthPassBareLF(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: asAuthCaps},
		{Expect: "AUTHINFO USER u", Reply: []string{"381 more"}},
		{Expect: "AUTHINFO PASS p", Reply: []string{"281 ok\n"}}, // written as "281 ok\n\r\n"
	}})
	r := asRunCheck(t, Options{Addr: s.Addr(), User: "u", Pass: "p"}, asFind(t, authChecks(), "authinfo-user-pass"))
	if r.Status != Fail || !strings.Contains(r.Detail, "bare LF") {
		t.Fatalf("got %v %q, want FAIL with the strict bare-LF error", r.Status, r.Detail)
	}
	if strings.Contains(r.Detail, "AUTHINFO PASS p") {
		t.Errorf("Detail %q shows the password", r.Detail)
	}
}

func TestAuthWrongPassAccepted(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: asAuthCaps},
		{Expect: "AUTHINFO USER u", Reply: []string{"381 more"}},
		{Expect: "AUTHINFO PASS .*", Reply: []string{"281 anything goes"}},
	}})
	r := asRunCheck(t, Options{Addr: s.Addr(), User: "u", Pass: "p"}, asFind(t, authChecks(), "authinfo-wrong-pass"))
	if r.Status != Pass || r.Detail != "any password accepted" {
		t.Fatalf("got %v %q, want PASS any password accepted", r.Status, r.Detail)
	}
}

// TestAuth480ToAuthinfo: a 480 in reply to AUTHINFO itself fails (RFC 4643 §2.3.1).
func TestAuth480ToAuthinfo(t *testing.T) {
	for _, tc := range []struct{ name, expect string }{
		{"authinfo-user-pass", "AUTHINFO USER u"},
		{"authinfo-pass-without-user", "AUTHINFO PASS .*"},
	} {
		s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
			{Expect: "CAPABILITIES", Reply: asAuthCaps},
			{Expect: tc.expect, Reply: []string{"480 authenticate first"}},
		}})
		r := asRunCheck(t, Options{Addr: s.Addr(), User: "u", Pass: "p"}, asFind(t, authChecks(), tc.name))
		if r.Status != Fail || !strings.Contains(r.Detail, "§2.3.1") {
			t.Errorf("%s: got %v %q, want FAIL citing §2.3.1", tc.name, r.Status, r.Detail)
		}
	}
}

// TestAuthNotAdvertised: a server that requires AUTHINFO without listing it (PeerHandler, as
// innd) is logged in anyway; the check passes and says so.
func TestAuthNotAdvertised(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.PeerHandler(&fakeserver.PeerOptions{User: "u", Pass: "p"})})
	o := Options{Addr: s.Addr(), User: "u", Pass: "p"}
	for _, name := range []string{"authinfo-user-pass", "authinfo-wrong-pass", "authinfo-capabilities"} {
		r := asRunCheck(t, o, asFind(t, authChecks(), name))
		if r.Status != Pass || r.Detail != asNotAdvertised {
			t.Errorf("%s: got %v %q, want PASS %q", name, r.Status, r.Detail, asNotAdvertised)
		}
	}
}

func TestAuthPendingAfterReply(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: asAuthCaps},
		{Expect: "AUTHINFO PASS .*", Reply: []string{"482 out of sequence", "stray"}},
	}})
	r := asRunCheck(t, Options{Addr: s.Addr(), User: "u", Pass: "p"}, asFind(t, authChecks(), "authinfo-pass-without-user"))
	if r.Status != Fail || !strings.Contains(r.Detail, "pending") {
		t.Fatalf("got %v %q, want FAIL with pending bytes", r.Status, r.Detail)
	}
}
