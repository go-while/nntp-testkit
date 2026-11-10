package article_test

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/article"
	"github.com/go-while/nntp-testkit/client"
)

// named is a testing.TB with another name, to try test names a real one cannot have.
type named struct {
	testing.TB
	name string
}

func (n named) Name() string { return n.name }

func TestMsgIDValidAndSanitized(t *testing.T) {
	cases := []struct {
		name, domain string
		prefix       string
		suffix       string
	}{
		{"TestFoo", "", "<TestFoo.", "@testkit.invalid>"},
		{"TestFoo/sub case#01", "", "<TestFoo_sub_case_01.", "@testkit.invalid>"},
		{"Test<>@\"\\ ]", "", "<Test_______.", "@testkit.invalid>"},
		{"Tëst/日本語", "", "<T_st____.", "@testkit.invalid>"},
		{"", "", "<t.", "@testkit.invalid>"},
		{"a.b..c", "news.example.org", "<a_b__c.", "@news.example.org>"},
		{strings.Repeat("N", 500), "", "<" + strings.Repeat("N", 100) + ".", "@testkit.invalid>"},
	}
	for _, c := range cases {
		t.Run(c.name[:min(len(c.name), 10)], func(t *testing.T) {
			var id string
			if c.domain == "" {
				id = article.MsgID(named{t, c.name})
			} else {
				id = article.MsgIDAt(named{t, c.name}, c.domain)
			}
			if !strings.HasPrefix(id, c.prefix) || !strings.HasSuffix(id, c.suffix) {
				t.Errorf("id %q, want prefix %q and suffix %q", id, c.prefix, c.suffix)
			}
			if !article.ValidMsgID(id) {
				t.Errorf("%q is not valid", id)
			}
		})
	}
	if id := article.NewMsgID("x", "a.b"); !strings.HasSuffix(id, "@a.b>") {
		t.Errorf("NewMsgID domain: %q", id)
	}
}

func TestMsgIDUniqueParallel(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	t.Run("group", func(t *testing.T) {
		for i := range 40 {
			t.Run(fmt.Sprintf("sub/%d", i), func(t *testing.T) {
				t.Parallel()
				for j := range 200 {
					var id string
					if j%2 == 0 {
						id = article.MsgID(t)
					} else {
						id = article.MsgIDAt(t, "other.invalid")
					}
					if !article.ValidMsgID(id) {
						t.Errorf("invalid %q", id)
					}
					mu.Lock()
					if prev, dup := seen[id]; dup {
						t.Errorf("duplicate %q (also from %s)", id, prev)
					}
					seen[id] = t.Name()
					mu.Unlock()
				}
			})
		}
	})
	if len(seen) != 40*200 {
		t.Errorf("%d distinct ids, want %d", len(seen), 40*200)
	}
}

func TestValidMsgID(t *testing.T) {
	for id, want := range map[string]bool{
		"<a@b>":                                  true,
		"<a.b.c@d.e.f>":                          true,
		"<a+b=c!d@host.invalid>":                 true,
		"":                                       false,
		"a@b":                                    false,
		"<a@b":                                   false,
		"<@b>":                                   false,
		"<a@>":                                   false,
		"<ab>":                                   false,
		"<a b@c>":                                false,
		"<a..b@c>":                               false,
		"<.a@c>":                                 false,
		"<a@c.>":                                 false,
		"<a@b@c>":                                false,
		"<a>b@c>":                                false,
		"<a\x00@c>":                              false,
		"<" + strings.Repeat("a", 240) + "@b.c>": true,
		"<" + strings.Repeat("a", 250) + "@b.c>": false,
	} {
		if got := article.ValidMsgID(id); got != want {
			t.Errorf("ValidMsgID(%.40q) = %v, want %v", id, got, want)
		}
	}
}

func TestNewMinimal(t *testing.T) {
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("x", 3600))
	a := article.New(t, article.Options{Newsgroups: "alt.test,misc.test", From: "me@x.invalid", PathHost: "peer.invalid", Date: when, Subject: "hi"})
	want := "Path: peer.invalid!not-for-mail\r\n" +
		"From: me@x.invalid\r\n" +
		"Newsgroups: alt.test,misc.test\r\n" +
		"Subject: hi\r\n" +
		"Message-ID: " + a.MessageID() + "\r\n" +
		"Date: Wed, 04 Mar 2026 04:06:07 +0000\r\n" +
		"\r\n" +
		"This is a test article made by nntp-testkit.\r\n"
	if got := a.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if !article.ValidMsgID(a.MessageID()) || !strings.HasSuffix(a.MessageID(), "@testkit.invalid>") {
		t.Errorf("message id %q", a.MessageID())
	}

	d := article.Minimal(t, "")
	for _, h := range []string{"Path", "From", "Newsgroups", "Subject", "Message-ID", "Date"} {
		if !d.Has(h) {
			t.Errorf("default article lacks %s", h)
		}
	}
	if d.Get("Newsgroups") != article.DefaultGroup || d.Get("From") != article.DefaultFrom ||
		d.Get("Path") != article.DefaultPathHost+"!not-for-mail" {
		t.Errorf("defaults: %q", d)
	}
	if !strings.HasPrefix(d.Get("Subject"), "test article TestNewMinimal") {
		t.Errorf("subject %q", d.Get("Subject"))
	}
	if _, err := time.Parse(article.DateLayout, d.Get("Date")); err != nil {
		t.Errorf("Date %q: %v", d.Get("Date"), err)
	}
	custom := article.New(t, article.Options{MessageID: "<own@x.invalid>", Domain: "ignored.invalid", Body: []string{"a", ".b"}})
	if custom.MessageID() != "<own@x.invalid>" || string(custom.Body) != "a\r\n.b\r\n" {
		t.Errorf("custom: %q", custom)
	}
	if x := article.New(t, article.Options{Domain: "news.example.org"}); !strings.HasSuffix(x.MessageID(), "@news.example.org>") {
		t.Errorf("domain: %q", x.MessageID())
	}
	if d.MessageID() == article.Minimal(t, "").MessageID() {
		t.Error("two articles share a Message-ID")
	}
}

func TestRawBytesSurvive(t *testing.T) {
	// Each raw article is built from pieces and must come out byte for byte.
	big := strings.Repeat("y", 70000)
	tests := []struct {
		name  string
		build func() *article.Article
		want  string
	}{
		{"order and case", func() *article.Article {
			return (&article.Article{}).Add("subject", "s").Add("PATH", "p").Add("Subject", "t").SetBodyLines("b")
		}, "subject: s\r\nPATH: p\r\nSubject: t\r\n\r\nb\r\n"},
		{"empty path no space", func() *article.Article {
			return (&article.Article{}).AddRaw("Path:").SetBodyLines("b")
		}, "Path:\r\n\r\nb\r\n"},
		{"empty value via Add", func() *article.Article {
			return (&article.Article{}).Add("Path", "")
		}, "Path: \r\n\r\n"},
		{"fold before bang", func() *article.Article {
			return (&article.Article{}).AddRaw("Path: a.invalid\r\n !b.invalid!not-for-mail")
		}, "Path: a.invalid\r\n !b.invalid!not-for-mail\r\n\r\n"},
		{"colon-less", func() *article.Article {
			return (&article.Article{}).AddRaw("no colon here").Add("A", "b")
		}, "no colon here\r\nA: b\r\n\r\n"},
		{"bare cr in header", func() *article.Article {
			return (&article.Article{}).Add("Subject", "a\rb")
		}, "Subject: a\rb\r\n\r\n"},
		{"bare lf in header", func() *article.Article {
			return (&article.Article{}).Add("Subject", "a\nb")
		}, "Subject: a\nb\r\n\r\n"},
		{"nul", func() *article.Article {
			return (&article.Article{}).Add("S", "a\x00b").SetBody([]byte("x\x00y"))
		}, "S: a\x00b\r\n\r\nx\x00y"},
		{"latin1 body", func() *article.Article {
			return (&article.Article{}).SetBody([]byte("caf\xe9\r\n"))
		}, "\r\ncaf\xe9\r\n"},
		{"dot lines not stuffed", func() *article.Article {
			return (&article.Article{}).Add("A", "b").SetBodyLines(".", "..", ".x")
		}, "A: b\r\n\r\n.\r\n..\r\n.x\r\n"},
		{"long line", func() *article.Article {
			return (&article.Article{}).Add("A", big).SetBodyLines(big)
		}, "A: " + big + "\r\n\r\n" + big + "\r\n"},
		{"no blank line", func() *article.Article {
			a := (&article.Article{}).Add("A", "b")
			a.Sep = article.NoEOL
			return a
		}, "A: b\r\n"},
		{"bare lf line ends", func() *article.Article {
			a := &article.Article{Fields: []article.Field{{Raw: "A: b", EOL: "\n"}}, Sep: "\n", Body: []byte("x\ny\n")}
			return a
		}, "A: b\n\nx\ny\n"},
		{"unterminated last header", func() *article.Article {
			return &article.Article{Fields: []article.Field{{Raw: "A: b", EOL: article.NoEOL}}, Sep: article.NoEOL}
		}, "A: b"},
		{"empty article", func() *article.Article { return &article.Article{Sep: article.NoEOL} }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.build()
			if got := a.Bytes(); string(got) != tt.want {
				t.Fatalf("Bytes %.200q, want %.200q", got, tt.want)
			}
			// and survive Parse
			if got := article.Parse([]byte(tt.want)).Bytes(); string(got) != tt.want {
				t.Errorf("Parse round trip %.200q, want %.200q", got, tt.want)
			}
		})
	}
}

func TestParseRoundTripAnyBytes(t *testing.T) {
	inputs := []string{
		"", "\r\n", "\n", "\r", "a", "a\r\n", "a\n", "a: b\r\n\r\n", "a: b\n\n", "a: b\n\r\n", "a: b\r\n\nbody",
		"a: b\r\n c\r\n\td\r\n\r\nbody\r\n.\r\n..x\r\n", " leading fold\r\nA: b\r\n\r\n",
		"A: b\r\r\n\r\n", "A: b\r\n\r\r\nx", "\x00\x00\r\n\r\n\x00", "A:\r\nB\r\n\r\n\r\n\r\n", "A: b\r\n \r\n\r\n",
		"A: b\r\n\r\nx\ry\nz\r\n", "A: \xe9\r\n\r\n\xff",
	}
	for _, in := range inputs {
		a := article.Parse([]byte(in))
		if got := a.Bytes(); string(got) != in {
			t.Errorf("Parse(%q).Bytes() = %q", in, got)
		}
	}
	// pseudo random byte strings over an alphabet heavy on the interesting bytes
	alphabet := []byte("\r\n\r\n :\t.aA\x00\xe9")
	seed := uint32(12345)
	for range 5000 {
		n := int(seed>>24) % 40
		var b []byte
		for range n {
			seed = seed*1664525 + 1013904223
			b = append(b, alphabet[int(seed>>16)%len(alphabet)])
		}
		seed = seed*1664525 + 1013904223
		if got := article.Parse(b).Bytes(); !bytes.Equal(got, b) {
			t.Fatalf("Parse(%q).Bytes() = %q", b, got)
		}
	}
}

func TestParseStructure(t *testing.T) {
	raw := "Path: a\r\n !b\r\nFrom: x\r\nsubject: one\r\nSUBJECT:   two  \r\nno colon\r\nX-Empty:\r\n\tfolded value\r\n" +
		"\r\nbody line\r\n.dot\r\n"
	a := article.Parse([]byte(raw))
	if len(a.Fields) != 6 {
		t.Fatalf("%d fields: %q", len(a.Fields), a.Fields)
	}
	if a.Fields[0].Raw != "Path: a\r\n !b" {
		t.Errorf("folded Raw %q", a.Fields[0].Raw)
	}
	if got := a.Get("path"); got != "a !b" {
		t.Errorf("Get(path) = %q", got)
	}
	if got := a.Values("Subject"); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Errorf("Values = %q", got)
	}
	if a.Count("SUBJECT") != 2 || !a.Has("from") || a.Has("Fro") || a.Has("From ") {
		t.Error("Count/Has")
	}
	if got := a.Duplicates(); !reflect.DeepEqual(got, []string{"subject"}) {
		t.Errorf("Duplicates = %q", got)
	}
	if v, ok := a.Lookup("X-Empty"); !ok || v != "folded value" {
		t.Errorf("X-Empty = %q %v", v, ok)
	}
	if v, ok := a.Lookup("Nope"); ok || v != "" {
		t.Errorf("absent = %q %v", v, ok)
	}
	if f := a.Fields[4]; f.HasColon() || f.Name() != "" || f.Value() != "no colon" {
		t.Errorf("colon-less %+v name %q value %q", f, f.Name(), f.Value())
	}
	if string(a.Body) != "body line\r\n.dot\r\n" || a.Sep != "" {
		t.Errorf("body %q sep %q", a.Body, a.Sep)
	}
	if got := a.BodyLines(); !reflect.DeepEqual(got, []string{"body line", ".dot"}) {
		t.Errorf("BodyLines %q", got)
	}

	// LF line ends
	b := article.Parse([]byte("A: 1\nB: 2\n c\n\nbody\n"))
	if b.Get("B") != "2 c" || b.Sep != "\n" || string(b.Body) != "body\n" || len(b.Fields) != 2 {
		t.Errorf("LF parse: %+v", b)
	}
	// no empty line
	c := article.Parse([]byte("A: 1\r\nB: 2"))
	if c.Sep != article.NoEOL || c.Body != nil || len(c.Fields) != 2 || c.Get("B") != "2" {
		t.Errorf("no blank: %+v", c)
	}
	// a bare LF in a header splits it; bare CR stays
	d := article.Parse([]byte("Subject: a\nb\r\nX: p\rq\r\n\r\n"))
	if len(d.Fields) != 3 || d.Get("X") != "p\rq" {
		t.Errorf("bare lf/cr: %q", d.Fields)
	}
	// ParseLines
	e := article.ParseLines([]string{"A: b", "", "x", ".y"})
	if e.Get("a") != "b" || string(e.Body) != "x\r\n.y\r\n" {
		t.Errorf("ParseLines: %q", e)
	}
	// aliasing
	src := []byte("A: b\r\n\r\nbody")
	f := article.Parse(src)
	src[0], src[len(src)-1] = 'X', 'X'
	if f.Get("A") != "b" || string(f.Body) != "body" {
		t.Error("Parse shares memory with its input")
	}
}

func TestEdit(t *testing.T) {
	a := article.Parse([]byte("A: 1\r\nB: 2\r\na: 3\r\nC: 4\r\n\r\nx\r\n"))
	a.Set("a", "new")
	if got := a.Fields; len(got) != 3 || got[0].Raw != "A: new" || got[1].Raw != "B: 2" || got[2].Raw != "C: 4" {
		t.Errorf("Set: %q", got)
	}
	a.Set("D", "5")
	if a.Fields[len(a.Fields)-1].Raw != "D: 5" {
		t.Errorf("Set appends: %q", a.Fields)
	}
	if a.Del("b").Has("B") || len(a.Fields) != 3 {
		t.Errorf("Del: %q", a.Fields)
	}
	a.Del("nothing")
	if len(a.Fields) != 3 {
		t.Error("Del of absent changed fields")
	}
	// Set resets a bare-LF end
	b := article.Parse([]byte("A: 1\nB: 2\n\n"))
	b.Set("A", "x")
	if string(b.Bytes()) != "A: x\r\nB: 2\n\n" {
		t.Errorf("Set eol: %q", b.Bytes())
	}
	// SetBody copies
	src := []byte("hello")
	c := (&article.Article{}).SetBody(src)
	src[0] = 'J'
	if string(c.Body) != "hello" {
		t.Error("SetBody does not copy")
	}
	c.SetBodyLines()
	if len(c.Body) != 0 {
		t.Errorf("no lines: %q", c.Body)
	}
	// Clone is deep
	d := article.Minimal(t, "x")
	e := d.Clone()
	e.Set("Subject", "changed")
	e.Body[0] = 'Z'
	e.Fields[0].Raw = "Path: z"
	if d.Get("Subject") == "changed" || d.Body[0] == 'Z' || d.Get("Path") == "z" {
		t.Error("Clone shares memory")
	}
	if !bytes.Equal(d.Bytes(), d.Clone().Bytes()) {
		t.Error("Clone differs")
	}
	// names are matched ASCII-only, exactly
	k := (&article.Article{}).AddRaw("\u212aey: v") // KELVIN SIGN
	if k.Has("key") {
		t.Error("non-ASCII fold matched")
	}
}

func TestLinesAndStuffing(t *testing.T) {
	a := article.Minimal(t, "x").SetBodyLines("one", ".", "..two", "")
	lines := a.Lines()
	if len(lines) != 11 || lines[6] != "" || lines[7] != "one" || lines[len(lines)-1] != "" {
		t.Errorf("lines %q", lines)
	}
	wire := client.Stuff(lines)
	want := strings.ReplaceAll(string(a.Bytes()), "\r\n.", "\r\n..") + ".\r\n"
	if wire != want {
		t.Errorf("Stuff(Lines) = %q, want %q", wire, want)
	}
	if !strings.Contains(wire, "\r\n..\r\n...two\r\n") {
		t.Errorf("stuffing: %q", wire)
	}
	// round trip through the client's un-stuffing and ParseLines
	back := article.ParseLines(client.Unstuff(strings.Split(strings.TrimSuffix(wire, ".\r\n"), "\r\n")[:len(lines)]))
	if !bytes.Equal(back.Bytes(), a.Bytes()) {
		t.Errorf("round trip %q", back.Bytes())
	}
	// edge cases of Lines
	for _, tt := range []struct {
		body string
		want []string
	}{
		{"", nil}, {"a", []string{"a"}}, {"a\r\n", []string{"a"}}, {"a\r\n\r\n", []string{"a", ""}},
		{"a\nb\rc\r\nd", []string{"a\nb\rc", "d"}},
	} {
		b := (&article.Article{}).SetBody([]byte(tt.body))
		if got := b.BodyLines(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("BodyLines(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
	if got := (&article.Article{Sep: article.NoEOL}).Lines(); got != nil {
		t.Errorf("empty Lines %q", got)
	}
}

func TestCorpus(t *testing.T) {
	cases := article.Corpus()
	if len(cases) < 20 {
		t.Fatalf("only %d cases", len(cases))
	}
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, c := range cases {
		if names[c.Name] || c.Name == "" || c.What == "" || strings.ContainsAny(c.Name, " /") {
			t.Errorf("bad case %q", c.Name)
		}
		names[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			a := c.Article(t, "alt.test")
			raw := a.Bytes()
			if ids[a.MessageID()] {
				t.Errorf("Message-ID %q reused", a.MessageID())
			}
			ids[a.MessageID()] = true
			// round trip: bytes survive exactly
			p := article.Parse(raw)
			if !bytes.Equal(p.Bytes(), raw) {
				t.Fatalf("Parse(Bytes()) changed the bytes:\n got %.300q\nwant %.300q", p.Bytes(), raw)
			}
			// a second build of the same case differs only in its Message-ID
			b := c.Article(t, "alt.test")
			if b.MessageID() == a.MessageID() {
				t.Error("same Message-ID twice")
			}
			if !strings.HasPrefix(string(a.Bytes()), "Path:") && !strings.HasPrefix(strings.ToLower(string(a.Bytes())), "path:") {
				t.Errorf("does not start with Path: %.40q", a.Bytes())
			}
			if !c.Invalid && !hasRequired(p) {
				t.Errorf("valid case lacks mandatory headers: %.300q", raw)
			}
			// does not mutate shared state: the case builds again the same way
			if again := c.Article(t, "alt.test"); len(again.Bytes()) != len(raw) {
				t.Errorf("length changed from %d to %d", len(raw), len(again.Bytes()))
			}
		})
	}
	if got := article.CaseNames(); len(got) != len(cases) || got[0] != cases[0].Name {
		t.Errorf("CaseNames %q", got)
	}
	if _, ok := article.CaseByName("latin1"); !ok {
		t.Error("CaseByName(latin1)")
	}
	if _, ok := article.CaseByName("nope"); ok {
		t.Error("CaseByName(nope)")
	}
}

func hasRequired(a *article.Article) bool {
	for _, h := range []string{"Path", "From", "Newsgroups", "Subject", "Message-ID", "Date"} {
		if a.Count(h) != 1 {
			return false
		}
	}
	return true
}

func TestCorpusContents(t *testing.T) {
	get := func(name string) *article.Article {
		c, ok := article.CaseByName(name)
		if !ok {
			t.Fatalf("no case %s", name)
		}
		return c.Article(t, "")
	}
	check := func(name string, ok bool, what string) {
		t.Helper()
		if !ok {
			t.Errorf("%s: %s", name, what)
		}
	}
	a := get("latin1")
	check("latin1", bytes.Contains(a.Body, []byte("caf\xe9")), "no Latin-1 byte")
	check("latin1", a.Get("Content-Type") == "text/plain; charset=ISO-8859-1", "content type")
	check("nul", bytes.IndexByte(get("nul").Body, 0) >= 0, "no NUL")
	check("bare-cr", bytes.Contains(get("bare-cr").Body, []byte("one\rtwo")), "no bare CR")
	check("bare-lf", bytes.Contains(get("bare-lf").Body, []byte("one\ntwo")), "no bare LF")
	check("dot-lines", strings.Contains(string(get("dot-lines").Body), "\r\n.\r\n"), "no lone dot")
	check("long-line", len(get("long-line").BodyLines()[1]) == 100000, "line length")
	check("long-header", len(get("long-header").Get("Subject")) >= 5000, "subject length")
	check("folded-subject", strings.Count(get("folded-subject").Fields[3].Raw, "\r\n") == 2 &&
		get("folded-subject").Get("Subject") == "first part second part\tthird part", "fold")
	check("folded-path", strings.Contains(get("folded-path").Fields[0].Raw, "\r\n !injector"), "fold before !")
	check("no-colon", !get("no-colon").Fields[len(get("no-colon").Fields)-1].HasColon(), "colon")
	check("dup-header", get("dup-header").Count("Subject") == 2, "dup")
	check("empty-path", get("empty-path").Fields[0].Raw == "Path:" && get("empty-path").Get("Path") == "", "path")
	check("empty-body", len(get("empty-body").Body) == 0 && bytes.HasSuffix(get("empty-body").Bytes(), []byte("\r\n\r\n")), "body")
	check("no-blank-line", !bytes.Contains(get("no-blank-line").Bytes(), []byte("\r\n\r\n")), "has blank line")
	check("header-case", get("header-case").Has("PATH") && get("header-case").Fields[0].Name() == "path", "case")
	check("header-case", get("header-case").Fields[1].Name() == "FROM", "upper")
	check("no-space-after-colon", strings.HasPrefix(get("no-space-after-colon").Fields[1].Raw, "From:Test"), "space")
	check("space-before-colon", get("space-before-colon").Has("X-Spaced ") && !get("space-before-colon").Has("X-Spaced"), "name")
	check("trailing-space", bytes.Contains(get("trailing-space").Body, []byte("spaces   \r\n")), "trailing")
	check("utf8", bytes.Contains(get("utf8").Body, []byte("\xe2\x82\xac")), "euro")
	check("nul-header", strings.Contains(get("nul-header").Get("Subject"), "\x00"), "nul header")
	check("latin1-header", strings.Contains(get("latin1-header").Get("Subject"), "\xe9"), "latin1 header")
	check("bare-cr-header", strings.Contains(get("bare-cr-header").Get("Subject"), "\r"), "bare cr header")
	// bare LF in a header: Parse sees an extra field, bytes still survive
	blf := get("bare-lf-header")
	check("bare-lf-header", len(article.Parse(blf.Bytes()).Fields) == len(blf.Fields)+1 && len(blf.Canon().Fields) == len(blf.Fields)+1, "extra field")
	check("long-line", get("long-line").BodyLines()[1] != "" && len(get("long-line").BodyLines()[1]) > 998, "over 998")
	check("long-header", len(get("long-header").Get("Subject")) == 5000 && !strings.HasSuffix(get("long-header").Get("Subject"), " "), "5000, no trailing space")
	ml := get("max-line")
	for _, l := range ml.Lines() {
		check("max-line", len(l) <= 998, "line over 998")
	}
	check("max-line", len(ml.BodyLines()[1]) == 998 && len(ml.Fields[3].Raw) == 998, "998 exactly")
	lf := get("lf-only")
	check("lf-only", !bytes.Contains(lf.Bytes(), []byte("\r")) && bytes.Count(lf.Bytes(), []byte("\n")) == len(lf.Fields)+3, "only LF")
	check("empty-subject", get("empty-subject").Has("Subject") && get("empty-subject").Get("Subject") == "", "empty subject")
	lm := get("long-message-id")
	check("long-message-id", len(lm.MessageID()) == 250 && article.ValidMsgID(lm.MessageID()), "250 and valid")
	check("long-message-id", lm.Fields[4].Name() == "Message-ID", "position")
	// the Invalid flags
	valid := map[string]bool{"latin1": true, "utf8": true, "dot-lines": true, "max-line": true, "long-message-id": true,
		"folded-subject": true, "folded-path": true, "empty-body": true, "header-case": true,
		"trailing-space": true, "blank-lines": true}
	for _, c := range article.Corpus() {
		if c.Invalid == valid[c.Name] {
			t.Errorf("%s: Invalid = %v", c.Name, c.Invalid)
		}
	}
}

func TestCompare(t *testing.T) {
	base := func() *article.Article {
		return article.Parse([]byte("Path: a\r\nFrom: x@y\r\nSubject: hello\r\nMessage-ID: <1@x>\r\nX-Dup: 1\r\nX-Dup: 2\r\n\r\nbody\r\n"))
	}
	tests := []struct {
		name string
		got  func() *article.Article
		opt  article.DiffOptions
		// expected
		added, lost []string
		changed     []string
		reordered   bool
		body        bool
	}{
		{name: "identical", got: base},
		{name: "allowed changes", got: func() *article.Article {
			a := base()
			a.Set("Path", "relay!a").Add("Xref", "h g:1").Add("Injection-Info", "x").Add("NNTP-Posting-Host", "1.2.3.4").
				Add("Lines", "1").Add("X-Trace", "t").Add("X-Complaints-To", "abuse@x").Add("Injection-Date", "d").
				Add("NNTP-Posting-Date", "d")
			return a
		}},
		{name: "allowed lost", got: func() *article.Article { return base().Del("Path") }},
		{name: "strict path", opt: article.DiffOptions{Strict: true}, got: func() *article.Article { return base().Set("Path", "x") },
			changed: []string{"Path"}},
		{name: "extra allow", opt: article.DiffOptions{Allow: []string{"x-dup"}}, got: func() *article.Article { return base().Del("X-Dup").Add("X-New", "1") },
			added: []string{"X-New"}},
		{name: "added", got: func() *article.Article { return base().Add("Organization", "o") }, added: []string{"Organization"}},
		{name: "lost", got: func() *article.Article { return base().Del("From") }, lost: []string{"From"}},
		{name: "changed", got: func() *article.Article { return base().Set("Subject", "Hello") }, changed: []string{"Subject"}},
		{name: "case of name is not a change", got: func() *article.Article {
			return article.Parse([]byte(strings.Replace(base().String(), "Subject", "SUBJECT", 1)))
		}},
		{name: "refolded", got: func() *article.Article {
			return article.Parse([]byte(strings.Replace(base().String(), "Subject: hello", "Subject:\r\n hello", 1)))
		}},
		{name: "duplicate lost", got: func() *article.Article {
			a := base()
			a.Fields = append(a.Fields[:5], a.Fields[6:]...)
			return a
		}, changed: []string{"X-Dup"}},
		{name: "duplicate order inside name", got: func() *article.Article {
			a := base()
			a.Fields[4], a.Fields[5] = a.Fields[5], a.Fields[4]
			return a
		}, changed: []string{"X-Dup"}, reordered: false},
		{name: "reordered", got: func() *article.Article {
			a := base()
			a.Fields[1], a.Fields[2] = a.Fields[2], a.Fields[1]
			return a
		}, reordered: true},
		{name: "allowed field moved is not a reorder", got: func() *article.Article {
			a := base()
			a.Fields = append(a.Fields[1:], a.Fields[0])
			return a
		}},
		{name: "body changed", got: func() *article.Article { return base().SetBodyLines("bodY") }, body: true},
		{name: "body trailing space exact", got: func() *article.Article { return base().SetBodyLines("body ") }, body: true},
		{name: "body trailing space ignored", opt: article.DiffOptions{IgnoreTrailingSpace: true},
			got: func() *article.Article { return base().SetBody([]byte("body \t\r\n\r\n\r\n")) }},
		{name: "body leading space still differs", opt: article.DiffOptions{IgnoreTrailingSpace: true},
			got: func() *article.Article { return base().SetBodyLines(" body") }, body: true},
		{name: "body lf vs crlf ignored with option", opt: article.DiffOptions{IgnoreTrailingSpace: true},
			got: func() *article.Article { return base().SetBody([]byte("body\n")) }},
		{name: "body lf vs crlf exact", got: func() *article.Article { return base().SetBody([]byte("body\n")) }, body: true},
		{name: "everything", got: func() *article.Article {
			a := base().Del("From").Add("X-Extra", "1").Set("Subject", "s")
			a.Fields[0], a.Fields[1] = a.Fields[1], a.Fields[0]
			a.SetBodyLines("other")
			return a
		}, added: []string{"X-Extra"}, lost: []string{"From"}, changed: []string{"Subject"}, body: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := article.Compare(base(), tt.got(), tt.opt)
			var changed []string
			for _, c := range d.Changed {
				changed = append(changed, c.Name)
			}
			if !reflect.DeepEqual(d.Added, tt.added) || !reflect.DeepEqual(d.Lost, tt.lost) ||
				!reflect.DeepEqual(changed, tt.changed) || d.Reordered != tt.reordered || d.BodyDiffers != tt.body {
				t.Errorf("diff:\n%s\n added %q lost %q changed %q reordered %v body %v", d, d.Added, d.Lost, changed, d.Reordered, d.BodyDiffers)
			}
			wantEmpty := tt.added == nil && tt.lost == nil && tt.changed == nil && !tt.reordered && !tt.body
			if d.Empty() != wantEmpty || (d.String() == "") != wantEmpty {
				t.Errorf("Empty %v String %q", d.Empty(), d.String())
			}
		})
	}

	d := article.Compare(base(), base().Set("Subject", "Hello").SetBodyLines("bXdy"), article.DiffOptions{})
	if len(d.Changed) != 1 || !reflect.DeepEqual(d.Changed[0].Sent, []string{"hello"}) || !reflect.DeepEqual(d.Changed[0].Got, []string{"Hello"}) {
		t.Errorf("Change values: %+v", d.Changed)
	}
	if !strings.Contains(d.BodyDetail, "byte 1 (line 1)") || !strings.Contains(d.String(), "changed Subject") {
		t.Errorf("detail: %q / %q", d.BodyDetail, d)
	}
	r := article.Compare(base(), func() *article.Article {
		a := base()
		a.Fields[1], a.Fields[2] = a.Fields[2], a.Fields[1]
		return a
	}(), article.DiffOptions{})
	if !reflect.DeepEqual(r.OrderSent, []string{"from", "subject", "message-id", "x-dup", "x-dup"}) ||
		!reflect.DeepEqual(r.OrderGot, []string{"subject", "from", "message-id", "x-dup", "x-dup"}) {
		t.Errorf("order: %q / %q", r.OrderSent, r.OrderGot)
	}
	// colon-less lines compare by their text
	cl := article.Compare(base().AddRaw("junk one"), base().AddRaw("junk two"), article.DiffOptions{})
	if len(cl.Changed) != 1 || cl.Changed[0].Name != "" {
		t.Errorf("colon-less: %+v", cl)
	}
}

func TestDefaultAllowed(t *testing.T) {
	got := article.DefaultAllowed()
	want := []string{"Path", "Xref", "Injection-Info", "Injection-Date", "NNTP-Posting-Host", "NNTP-Posting-Date", "Lines", "X-Trace", "X-Complaints-To"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultAllowed %q", got)
	}
	got[0] = "x"
	if article.DefaultAllowed()[0] != "Path" {
		t.Error("DefaultAllowed shares its slice")
	}
}

// recorder is a testing.TB that records Errorf instead of failing.
type recorder struct {
	testing.TB
	msgs []string
}

func (r *recorder) Errorf(f string, a ...any) { r.msgs = append(r.msgs, fmt.Sprintf(f, a...)) }

func TestExpectSame(t *testing.T) {
	a := article.Minimal(t, "x")
	r := &recorder{TB: t}
	article.ExpectSame(r, a, a.Clone(), article.DiffOptions{})
	if len(r.msgs) != 0 {
		t.Errorf("identical: %q", r.msgs)
	}
	article.ExpectSame(r, a, a.Clone().Set("Subject", "other"), article.DiffOptions{})
	if len(r.msgs) != 1 || !strings.Contains(r.msgs[0], "changed Subject") {
		t.Errorf("msgs %q", r.msgs)
	}
}

func TestControl(t *testing.T) {
	c := article.Cancel(t, article.Options{Newsgroups: "alt.test", From: "me@x.invalid"}, "<victim@x.invalid>")
	if c.Get("Control") != "cancel <victim@x.invalid>" || c.Get("Subject") != "cmsg cancel <victim@x.invalid>" ||
		c.Get("Newsgroups") != "alt.test" || c.Get("From") != "me@x.invalid" || string(c.Body) != "cancel <victim@x.invalid>\r\n" {
		t.Errorf("cancel: %q", c)
	}
	// Control comes right after Subject
	names := []string{}
	for _, f := range c.Fields {
		names = append(names, f.Name())
	}
	if !reflect.DeepEqual(names, []string{"Path", "From", "Newsgroups", "Subject", "Control", "Message-ID", "Date"}) {
		t.Errorf("order %q", names)
	}
	if !article.ValidMsgID(c.MessageID()) || !hasRequired(c) {
		t.Errorf("not valid: %q", c)
	}

	n := article.Newgroup(t, article.Options{}, "alt.new.group", false)
	if n.Get("Control") != "newgroup alt.new.group" || n.Get("Subject") != "cmsg newgroup alt.new.group" ||
		n.Get("Newsgroups") != "alt.new.group" || !strings.HasPrefix(string(n.Body), "For your newsgroups file:\r\nalt.new.group\t") {
		t.Errorf("newgroup: %q", n)
	}
	m := article.Newgroup(t, article.Options{Newsgroups: "control.cancel", Subject: "own", Body: []string{"x"}}, "alt.mod", true)
	if m.Get("Control") != "newgroup alt.mod moderated" || m.Get("Subject") != "own" || m.Get("Newsgroups") != "control.cancel" || string(m.Body) != "x\r\n" {
		t.Errorf("moderated newgroup: %q", m)
	}
	r := article.Rmgroup(t, article.Options{}, "alt.old")
	if r.Get("Control") != "rmgroup alt.old" || r.Get("Subject") != "cmsg rmgroup alt.old" || r.Get("Newsgroups") != "alt.old" {
		t.Errorf("rmgroup: %q", r)
	}
	for _, a := range []*article.Article{c, n, m, r} {
		if a.Count("Control") != 1 || !bytes.Equal(article.Parse(a.Bytes()).Bytes(), a.Bytes()) {
			t.Errorf("control article: %q", a)
		}
	}
}

func TestCaseZero(t *testing.T) {
	z := article.Case{}.Article(t, "alt.x")
	if !hasRequired(z) || z.Get("Newsgroups") != "alt.x" {
		t.Errorf("zero case: %q", z)
	}
}

func TestMsgIDHasProcessNonce(t *testing.T) {
	// two ids of the same name and domain differ in the sequence and share the nonce
	a, b := article.NewMsgID("n", "d.invalid"), article.NewMsgID("n", "d.invalid")
	pa, pb := strings.Split(a[1:len(a)-1], "@")[0], strings.Split(b[1:len(b)-1], "@")[0]
	fa, fb := strings.Split(pa, "."), strings.Split(pb, ".")
	if len(fa) != 4 || len(fb) != 4 || fa[2] != fb[2] || fa[2] == "" || fa[3] == fb[3] {
		t.Errorf("fields %q %q", fa, fb)
	}
	if !strings.Contains(fa[2], "-") || !article.ValidMsgID(a) {
		t.Errorf("nonce %q in %q", fa[2], a)
	}
}

func TestCanon(t *testing.T) {
	a := article.Minimal(t, "x").Set("Subject", "a\nX-Evil: y")
	if a.Count("X-Evil") != 0 {
		t.Fatal("before Canon")
	}
	c := a.Canon()
	if c.Count("X-Evil") != 1 || c.Get("X-Evil") != "y" || !bytes.Equal(c.Bytes(), a.Bytes()) {
		t.Errorf("Canon: %q", c)
	}
	if c.Count("X-Evil") != article.Parse(a.Bytes()).Count("X-Evil") {
		t.Error("Canon differs from Parse(Bytes())")
	}
}

func TestCompareRelayOfBrokenArticles(t *testing.T) {
	// an untouched relay of every corpus case shows no difference, whatever its line ends
	for _, c := range article.Corpus() {
		t.Run(c.Name, func(t *testing.T) {
			sent := c.Article(t, "x")
			got := article.ParseDotBlock([]byte(stuffed(sent.Bytes())))
			if !bytes.HasSuffix(sent.Bytes(), []byte("\r\n")) {
				// the terminator needs a CRLF before it, so the wire adds one to such a body
				sent = article.Parse(append(sent.Bytes(), "\r\n"...))
			}
			if d := article.Compare(sent, got, article.DiffOptions{}); !d.Empty() {
				t.Errorf("relay shows a difference:\n%s", d)
			}
			if d := article.Compare(sent, got, article.DiffOptions{Strict: true, CaseSensitiveNames: true}); !d.Empty() {
				t.Errorf("strict relay shows a difference:\n%s", d)
			}
			if !bytes.Equal(got.Bytes(), sent.Bytes()) {
				t.Errorf("bytes changed")
			}
		})
	}
	a := article.Minimal(t, "x").Set("Subject", "bare\nlf")
	if d := article.Compare(a, article.Parse(a.Bytes()), article.DiffOptions{}); !d.Empty() {
		t.Errorf("bare LF: %s", d)
	}
}

// stuffed is raw as a client sends it: dot-stuffed at every CRLF line start, then ".\r\n".
func stuffed(raw []byte) string {
	s := string(raw)
	if strings.HasPrefix(s, ".") {
		s = "." + s
	}
	s = strings.ReplaceAll(s, "\r\n.", "\r\n..")
	if s != "" && !strings.HasSuffix(s, "\r\n") {
		s += "\r\n"
	}
	return s + ".\r\n"
}

func TestParseDotBlock(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{".\r\n", ""},
		{"A: b\r\n\r\nx\r\n.\r\n", "A: b\r\n\r\nx\r\n"},
		{"A: b\r\n\r\n..\r\n...x\r\n..\r\n.\r\n", "A: b\r\n\r\n.\r\n..x\r\n.\r\n"},
		{"..A: b\r\n\r\n.\r\n", ".A: b\r\n\r\n"},
		{"A: b\r\n\r\nbare\nlf\rcr\r\r\n.\r\n", "A: b\r\n\r\nbare\nlf\rcr\r\r\n"},
		{"x\n..y\r\n.\r\n", "x\n..y\r\n"},
		{"no terminator\r\n", "no terminator\r\n"},
		{"", ""},
	} {
		if got := article.ParseDotBlock([]byte(tt.in)).Bytes(); string(got) != tt.want {
			t.Errorf("ParseDotBlock(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// agrees with client.Stuff
	a := article.Minimal(t, "x").SetBodyLines(".", "..", ".x", "")
	if got := article.ParseDotBlock([]byte(client.Stuff(a.Lines()))); !bytes.Equal(got.Bytes(), a.Bytes()) {
		t.Errorf("round trip through Stuff: %q", got.Bytes())
	}
}

func TestSetRawInsert(t *testing.T) {
	a := article.Minimal(t, "x")
	a.SetRaw("path", "Path:")
	if a.Fields[0].Raw != "Path:" || a.Get("Path") != "" || len(a.Fields) != 6 {
		t.Errorf("SetRaw: %q", a.Fields)
	}
	a.SetRaw("X-New", "X-New:v")
	if a.Fields[len(a.Fields)-1].Raw != "X-New:v" {
		t.Errorf("SetRaw appends: %q", a.Fields)
	}
	a.Add("X-Dup", "1").Add("x-dup", "2").SetRaw("X-Dup", "x-DUP : z")
	if a.Count("X-Dup") != 0 || a.Count("x-dup ") != 1 {
		t.Errorf("SetRaw dup: %q", a.Fields)
	}
	// Set keeps the existing spelling
	b := article.Parse([]byte("SUBJECT: one\r\nsubject: two\r\n\r\n"))
	b.Set("Subject", "new")
	if string(b.Bytes()) != "SUBJECT: new\r\n\r\n" {
		t.Errorf("Set spelling: %q", b.Bytes())
	}
	b.Set("Other", "v")
	if b.Fields[1].Raw != "Other: v" {
		t.Errorf("Set new name: %q", b.Fields)
	}
	c := (&article.Article{}).AddRaw("a: 1").AddRaw("c: 3")
	c.Insert(1, "b: 2").Insert(0, "z: 0").Insert(99, "e: 5").Insert(-4, "y: -")
	var raws []string
	for _, f := range c.Fields {
		raws = append(raws, f.Raw)
	}
	if !reflect.DeepEqual(raws, []string{"y: -", "z: 0", "a: 1", "b: 2", "c: 3", "e: 5"}) {
		t.Errorf("Insert: %q", raws)
	}
}

func TestOptionsApprovedExtra(t *testing.T) {
	a := article.New(t, article.Options{Approved: "mod@x.invalid", Extra: []string{"X-One: 1", "no colon"}})
	n := len(a.Fields)
	if a.Fields[n-3].Raw != "Approved: mod@x.invalid" || a.Fields[n-2].Raw != "X-One: 1" || a.Fields[n-1].Raw != "no colon" || a.Fields[n-4].Name() != "Date" {
		t.Errorf("fields %q", a.Fields)
	}
	if article.Minimal(t, "x").Has("Approved") {
		t.Error("Approved by default")
	}
	g := article.Newgroup(t, article.Options{Approved: "a@b"}, "alt.g", false)
	if g.Get("Approved") != "a@b" || g.Count("Control") != 1 {
		t.Errorf("newgroup: %q", g)
	}
	// Cancel's group default
	if c := article.Cancel(t, article.Options{}, "<a@b>"); c.Get("Newsgroups") != "test" {
		t.Errorf("cancel default group %q", c.Get("Newsgroups"))
	}
}

func TestComparePathExtends(t *testing.T) {
	mk := func(p string) *article.Article { return (&article.Article{}).AddRaw("Path: " + p) }
	for _, tt := range []struct {
		sent, got string
		none      string // "sent" or "got": that side has no Path
		want      bool
	}{
		{"peer!not-for-mail", "peer!not-for-mail", "", true},
		{"peer!not-for-mail", "me!peer!not-for-mail", "", true},
		{"peer!not-for-mail", "a!b!peer!not-for-mail", "", true},
		{"peer!not-for-mail", "me!speer!not-for-mail", "", false},
		{"peer!not-for-mail", "me!peer!not-for-mail!x", "", false},
		{"peer!not-for-mail", "not-for-mail", "", false},
		{"peer!not-for-mail", "me!other!not-for-mail", "", false},
		{"peer\r\n !not-for-mail", "me!peer!\r\n\tnot-for-mail", "", true},
		{"", "", "sent", true},
		{"peer!x", "", "got", false},
	} {
		s, g := mk(tt.sent), mk(tt.got)
		switch tt.none {
		case "sent":
			s = &article.Article{}
		case "got":
			g = &article.Article{}
		}
		if got := article.PathExtends(s, g); got != tt.want {
			t.Errorf("PathExtends(%q, %q) = %v", tt.sent, tt.got, got)
		}
	}
}

func TestCompareRefoldAndNameCase(t *testing.T) {
	sent := article.Parse([]byte("Subject: one two three\r\nFrom: x\r\n\r\nb\r\n"))
	got := article.Parse([]byte("Subject: one\r\n\ttwo\r\n  three\r\nFrom: x\r\n\r\nb\r\n"))
	if d := article.Compare(sent, got, article.DiffOptions{}); !d.Empty() {
		t.Errorf("refold with tab: %s", d)
	}
	if d := article.Compare(got, sent, article.DiffOptions{}); !d.Empty() {
		t.Errorf("fold removed: %s", d)
	}
	// but a changed inner space is a change, trimmed ends are not
	if d := article.Compare(sent, sent.Clone().Set("Subject", "one  two three"), article.DiffOptions{}); len(d.Changed) != 1 {
		t.Errorf("inner space: %s", d)
	}
	if d := article.Compare(sent, sent.Clone().Set("Subject", "  one two three  "), article.DiffOptions{}); !d.Empty() {
		t.Errorf("trimmed ends: %s", d)
	}
	re := sent.Clone().SetRaw("From", "FROM: x")
	if d := article.Compare(sent, re, article.DiffOptions{}); !d.Empty() {
		t.Errorf("name case by default: %s", d)
	}
	d := article.Compare(sent, re, article.DiffOptions{CaseSensitiveNames: true})
	if len(d.Changed) != 1 || !reflect.DeepEqual(d.Changed[0].SentNames, []string{"From"}) || !reflect.DeepEqual(d.Changed[0].GotNames, []string{"FROM"}) {
		t.Errorf("CaseSensitiveNames: %+v", d)
	}
}
