package conformance

import (
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// ctCheck runs the check called name of the STARTTLS and COMPRESS checks the way the runner
// does (runOne: skips, dial, CAPABILITIES, Run).
func ctCheck(t *testing.T, o Options, name string) Result {
	t.Helper()
	for _, c := range append(startTLSChecks(), compressChecks()...) {
		if c.Name == name {
			r := runOne(o, c)
			t.Logf("%-4s %-22s %s", r.Status, r.Name, r.Detail)
			return r
		}
	}
	t.Fatalf("no check %q", name)
	return Result{}
}

// ctWant fails t unless the result has status want and (for a Fail or Skip) a Detail that
// contains sub.
func ctWant(t *testing.T, r Result, want Status, sub string) {
	t.Helper()
	if r.Status != want {
		t.Errorf("%s: %s %q, want %s", r.Name, r.Status, r.Detail, want)
		return
	}
	if !strings.Contains(r.Detail, sub) {
		t.Errorf("%s: Detail %q, want it to contain %q", r.Name, r.Detail, sub)
	}
	if r.RFC == "" {
		t.Errorf("%s: no RFC", r.Name)
	}
}

var compressNames = names(compressChecks())

func TestCompressCheckNames(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range append(startTLSChecks(), compressChecks()...) {
		if !asKebab.MatchString(c.Name) || c.RFC == "" || c.Run == nil || seen[c.Name] {
			t.Errorf("check %+v: want a unique kebab-case name, an RFC and Run", c)
		}
		seen[c.Name] = true
		if c.Needs != "" || c.Auth {
			t.Errorf("%s: Needs %q Auth %v: the check tests its capability itself", c.Name, c.Needs, c.Auth)
		}
		if got, want := c.Posts, strings.HasPrefix(c.Name, "compress-upload"); got != want {
			t.Errorf("%s: Posts %v, want %v", c.Name, got, want)
		}
	}
}

func TestCompressChecksStore(t *testing.T) {
	cases := []struct {
		name string
		so   fakeserver.StoreOptions
		o    Options
	}{
		{"anonymous", fakeserver.StoreOptions{Compress: true}, Options{}},
		// COMPRESS is listed only after the login: the checks log in for it
		{"auth", fakeserver.StoreOptions{Compress: true, User: "u", Pass: "p"}, Options{User: "u", Pass: "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: tc.so})
			o := tc.o
			o.Addr, o.PostGroup = srv.Addr(), "conf.test.b"
			for _, n := range compressNames {
				// compress-twice says what it did not probe (no STARTTLS or AUTHINFO listed)
				if r := ctCheck(t, o, n); r.Status != Pass || (r.Detail != "" && n != "compress-twice") {
					t.Errorf("%s: %s %q, want PASS without a Detail", n, r.Status, r.Detail)
				}
			}
			// both uploads reached the store whole
			if n := len(srv.Articles()); n != 2 {
				t.Errorf("%d articles posted, want 2", n)
			}
			for _, a := range srv.Articles() {
				if len(a) < uploadLines {
					t.Errorf("article of %d lines, want more than %d", len(a), uploadLines)
				}
			}
		})
	}
}

func TestCompressChecksNotAdvertised(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore()})
	for _, n := range compressNames {
		r := ctCheck(t, Options{Addr: srv.Addr(), PostGroup: "conf.test.b"}, n)
		ctWant(t, r, Skip, "COMPRESS not advertised")
		if r.Missing != "COMPRESS" {
			t.Errorf("%s: Missing %q, want COMPRESS", n, r.Missing)
		}
	}
	if srv.Count("COMPRESS") != 0 {
		t.Error("COMPRESS sent to a server that does not list it")
	}
}

// ctPeer is a server written by hand (LineHandler) that offers COMPRESS DEFLATE and POST and
// misbehaves as asked.
type ctPeer struct {
	plain206   bool          // 206 but no compression
	capsAfter  []string      // the lines of CAPABILITIES once compressed (default: the plain list without COMPRESS)
	postHang   time.Duration // POST: wait this long after the article before the 240
	post441    bool
	nonDeflate bool // 206, then the server's output is compressed but the input is read plain
}

func (p ctPeer) handler() func(*fakeserver.Conn) {
	plainCaps := []string{"101 caps", "VERSION 2", "READER", "POST", "AUTHINFO", "STARTTLS", "COMPRESS DEFLATE", "."}
	activeCaps := []string{"101 caps", "VERSION 2", "READER", "POST", "."}
	if p.capsAfter != nil {
		activeCaps = p.capsAfter
	}
	return fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		f := strings.Fields(line)
		if len(f) == 0 {
			return []string{"500 what"}, false
		}
		switch strings.ToUpper(f[0]) {
		case "CAPABILITIES":
			if c.Compressed() {
				return activeCaps, false
			}
			return plainCaps, false
		case "COMPRESS":
			if c.Compressed() {
				return []string{"502 already"}, false
			}
			if len(f) != 2 {
				return []string{"501 syntax"}, false
			}
			if f[1] != "DEFLATE" {
				return []string{"503 no such algorithm"}, false
			}
			_ = c.WriteLines("206 compression active")
			switch {
			case p.plain206:
			case p.nonDeflate:
				_ = c.StartCompressOutputOnly()
			default:
				_ = c.StartCompress()
			}
			return nil, false
		case "DATE":
			return []string{"111 20250101000000"}, false
		case "HELP":
			return []string{"100 help", "DATE", "HELP", "."}, false
		case "STARTTLS", "AUTHINFO":
			return []string{"502 not now"}, false
		case "POST":
			if p.post441 {
				return []string{"340 go"}, false
			}
			_ = c.WriteLines("340 go")
			_, _ = c.ReadDotBlock()
			time.Sleep(p.postHang)
			return []string{"240 ok"}, false
		case "STAT":
			return []string{"430 no"}, false
		case "QUIT":
			return []string{"205 bye"}, true
		}
		return []string{"500 what"}, false
	})
}

func ctServer(t *testing.T, p ctPeer) Options {
	t.Helper()
	srv := fakeserver.New(t, fakeserver.Options{Handler: p.handler(), Deadline: 3 * time.Second})
	return Options{Addr: srv.Addr(), PostGroup: "conf.test.b", Deadline: 700 * time.Millisecond}
}

// TestCompressChecksHandPeer: the hand-written peer is right, so every check passes (the
// uploads with the Detail that the article never turned up: the peer has no store).
func TestCompressChecksHandPeer(t *testing.T) {
	o := ctServer(t, ctPeer{})
	for _, n := range compressNames {
		r := ctCheck(t, o, n)
		if r.Status != Pass {
			t.Errorf("%s: %s %q, want PASS", n, r.Status, r.Detail)
		}
	}
}

func TestCompressPlaintextAfter206(t *testing.T) {
	o := ctServer(t, ctPeer{plain206: true})
	for _, n := range []string{"compress", "compress-twice", "compress-upload"} {
		ctWant(t, ctCheck(t, o, n), Fail, "COMPRESS")
	}
	r := ctCheck(t, o, "compress")
	if !strings.Contains(r.Detail, "DEFLATE") && !strings.Contains(r.Detail, "inflate") {
		t.Errorf("compress: Detail %q does not name DEFLATE or inflate", r.Detail)
	}
	// nothing to do with the compressed stream: the algorithm checks still pass
	if r := ctCheck(t, o, "compress-unknown"); r.Status != Pass {
		t.Errorf("compress-unknown: %s %q", r.Status, r.Detail)
	}
}

func TestCompressInputNotInflated(t *testing.T) {
	// the server deflates its output and reads our deflated bytes as plain lines
	o := ctServer(t, ctPeer{nonDeflate: true})
	ctWant(t, ctCheck(t, o, "compress"), Fail, "CAPABILITIES over COMPRESS")
}

func TestCompressCapabilitiesAfter(t *testing.T) {
	for _, kw := range []string{"COMPRESS DEFLATE", "STARTTLS", "MODE-READER"} {
		o := ctServer(t, ctPeer{capsAfter: []string{"101 caps", "VERSION 2", "READER", kw, "."}})
		ctWant(t, ctCheck(t, o, "compress"), Fail, "lists \""+strings.Fields(kw)[0])
	}
	o := ctServer(t, ctPeer{capsAfter: []string{"101 caps", "VERSION 2", "READER", "AUTHINFO USER", "."}})
	ctWant(t, ctCheck(t, o, "compress"), Fail, "AUTHINFO USER")
	// AUTHINFO without arguments is what RFC 8054 allows
	o = ctServer(t, ctPeer{capsAfter: []string{"101 caps", "VERSION 2", "READER", "AUTHINFO", "."}})
	if r := ctCheck(t, o, "compress"); r.Status != Pass {
		t.Errorf("AUTHINFO without arguments: %s %q", r.Status, r.Detail)
	}
}

// a second COMPRESS answered 200 (Store with an override from the second use on)
func TestCompressTwiceAnswered200(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true},
		Faults: fakeserver.Faults{Override: map[string][]string{"COMPRESS": {"200 ok"}}, OverrideFrom: map[string]int{"COMPRESS": 2}}})
	o := Options{Addr: srv.Addr(), Deadline: time.Second}
	ctWant(t, ctCheck(t, o, "compress-twice"), Fail, `got "200 ok", want [502]`)
	if r := ctCheck(t, o, "compress"); r.Status != Pass {
		t.Errorf("compress: %s %q", r.Status, r.Detail)
	}
}

func TestCompressUnknownAlgorithm(t *testing.T) {
	cases := map[string]string{ // override of the first COMPRESS NNTPTESTKIT → want in Detail
		"206 compression active": "want [503]", // accepts any algorithm
		"501 syntax":             `got "501 syntax", want [503]`,
	}
	for reply, want := range cases {
		srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true},
			Faults: fakeserver.Faults{OverrideLine: map[string][]string{"COMPRESS " + unknownAlgorithm: {reply}}}})
		o := Options{Addr: srv.Addr(), Deadline: time.Second}
		ctWant(t, ctCheck(t, o, "compress-unknown"), Fail, want)
	}
}

// COMPRESS, STARTTLS and AUTHINFO after compression: a server that lists STARTTLS and
// AUTHINFO must refuse both with 502; the hand peer answers 502 to both (right).
func TestCompressTwiceProbesStartTLSAuthinfo(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Handler: ctPeer{}.handler(), Deadline: 3 * time.Second})
	r := ctCheck(t, Options{Addr: srv.Addr(), Deadline: time.Second}, "compress-twice")
	if r.Status != Pass || r.Detail != "" {
		t.Errorf("%s %q", r.Status, r.Detail)
	}
	for _, l := range srv.Lines() {
		if l == "STARTTLS" {
			goto starttls
		}
	}
	t.Error("STARTTLS not sent after compression although listed")
starttls:
	// not listed: not probed, and the Detail says so
	st := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true}})
	r = ctCheck(t, Options{Addr: st.Addr()}, "compress-twice")
	ctWant(t, r, Pass, "STARTTLS and AUTHINFO not advertised: not probed")
	if st.Count("STARTTLS") != 0 || st.Count("AUTHINFO") != 0 {
		t.Errorf("probed what is not advertised: %v", st.Counts())
	}
}

func TestCompressUploadHang(t *testing.T) {
	o := ctServer(t, ctPeer{postHang: 2500 * time.Millisecond})
	for _, n := range []string{"compress-upload", "compress-upload-lines"} {
		ctWant(t, ctCheck(t, o, n), Fail, "issue #269")
	}
}

func TestCompressUploadRefused(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true, ReaderOnly: true}})
	// a reader-only server lists no POST: skipped for that
	r := ctCheck(t, Options{Addr: srv.Addr(), PostGroup: "conf.test.b"}, "compress-upload")
	ctWant(t, r, Skip, "POST not advertised")
	if r.Missing != "POST" {
		t.Errorf("Missing %q, want POST", r.Missing)
	}
	// no PostGroup: skipped before the dial
	srv2 := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true}})
	ctWant(t, ctCheck(t, Options{Addr: srv2.Addr()}, "compress-upload-lines"), Skip, "no Options.PostGroup")
}

// a server that stores the article with a changed line fails the round trip
func TestCompressUploadBodyChanged(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true},
		Faults: fakeserver.Faults{Override: map[string][]string{"BODY": {"222 0 <x>", "short", "."}}}})
	r := ctCheck(t, Options{Addr: srv.Addr(), PostGroup: "conf.test.b"}, "compress-upload")
	ctWant(t, r, Fail, "1 lines, the article posted had")
}

func TestCompressDeflateMissing(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		if strings.HasPrefix(line, "CAPABILITIES") {
			return []string{"101 caps", "VERSION 2", "READER", "COMPRESS SHRINK", "."}, false
		}
		return []string{"500 what"}, false
	})})
	o := Options{Addr: srv.Addr()}
	ctWant(t, ctCheck(t, o, "compress"), Fail, "without DEFLATE")
	r := ctCheck(t, o, "compress-twice")
	ctWant(t, r, Skip, "COMPRESS DEFLATE not advertised")
	if r.Missing != "COMPRESS DEFLATE" {
		t.Errorf("Missing %q", r.Missing)
	}
}

func TestCompress403Skips(t *testing.T) {
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true},
		Faults: fakeserver.Faults{Override: map[string][]string{"COMPRESS": {"403 busy"}}}})
	r := ctCheck(t, Options{Addr: srv.Addr()}, "compress")
	ctWant(t, r, Skip, "unable to activate compression")
	if r.Missing != "" {
		t.Errorf("Missing %q, want empty (it is advertised)", r.Missing)
	}
}

func TestCompress206Wanted(t *testing.T) {
	// advertised, but COMPRESS DEFLATE answered 502 on a fresh, unauthenticated connection
	srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true},
		Faults: fakeserver.Faults{Override: map[string][]string{"COMPRESS": {"502 no"}}}})
	ctWant(t, ctCheck(t, Options{Addr: srv.Addr()}, "compress"), Fail, `got "502 no", want 206`)
}

func TestUploadBody(t *testing.T) {
	b := uploadBody()
	if len(b) != uploadLines {
		t.Fatalf("%d lines", len(b))
	}
	var dots, empty, lone int
	for _, l := range b {
		switch {
		case l == ".":
			lone++
		case l == "":
			empty++
		case strings.HasPrefix(l, "."):
			dots++
		}
		if len(l) > uploadWidth || strings.HasSuffix(l, " ") {
			t.Fatalf("line %q: too long or trailing space", l)
		}
	}
	if dots < 100 || empty < 100 || lone != 1 {
		t.Errorf("dotted %d, empty %d, lone dot %d", dots, empty, lone)
	}
}

// the unknown algorithm while compression is active: 502 or 503 (two MUSTs of RFC 8054 §2.2.2
// overlap), anything else fails
func TestCompressTwiceUnknownAlgorithm(t *testing.T) {
	for reply, want := range map[string]Status{"503 no such algorithm": Pass, "200 ok": Fail} {
		srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: fakeserver.StoreOptions{Compress: true},
			Faults: fakeserver.Faults{OverrideLine: map[string][]string{"COMPRESS " + unknownAlgorithm: {reply}}}})
		r := ctCheck(t, Options{Addr: srv.Addr(), Deadline: time.Second}, "compress-twice")
		if r.Status != want {
			t.Errorf("%q: %s %q, want %s", reply, r.Status, r.Detail, want)
		}
	}
}
