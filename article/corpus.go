package article

import (
	"strings"
	"testing"
)

// Case is one entry of the corpus: a named tricky article.
type Case struct {
	// Name is a short identifier fit for t.Run, e.g. "latin1".
	Name string
	// What says what is special about the article.
	What string
	// Invalid is set when the article breaks RFC 5536 (or RFC 3977, for NUL and the like): a
	// server may reject or repair it. Cases without it are legal and a server should take them
	// as they are.
	Invalid bool

	mod func(a *Article)
}

// Article builds the case for group ("" = DefaultGroup): the article of New with a Message-ID
// of tb's name, changed as the case says.
func (c Case) Article(tb testing.TB, group string) *Article {
	tb.Helper()
	a := Minimal(tb, group)
	c.mod(a)
	return a
}

// Corpus returns the tricky articles, in a fixed order, a new slice each call. A suite loops:
//
//	for _, c := range article.Corpus() {
//		t.Run(c.Name, func(t *testing.T) { a := c.Article(t, "test"); ... })
//	}
func Corpus() []Case {
	return []Case{
		{Name: "latin1", What: "8-bit ISO-8859-1 body (not UTF-8), declared with MIME headers", mod: func(a *Article) {
			a.Add("MIME-Version", "1.0").
				Add("Content-Type", "text/plain; charset=ISO-8859-1").
				Add("Content-Transfer-Encoding", "8bit")
			a.SetBody([]byte("caf\xe9 \xfcber \xdf \xa4\r\n\xff\xfe end\r\n"))
		}},
		{Name: "utf8", What: "UTF-8 body with multi-byte characters", mod: func(a *Article) {
			a.Add("MIME-Version", "1.0").
				Add("Content-Type", "text/plain; charset=UTF-8").
				Add("Content-Transfer-Encoding", "8bit")
			a.SetBody([]byte("café über € \U0001F600\r\n"))
		}},
		{Name: "nul", What: "a NUL byte in the body", Invalid: true, mod: func(a *Article) {
			a.SetBody([]byte("before\x00after\r\nsecond\x00\r\n"))
		}},
		{Name: "nul-header", What: "a NUL byte in a header value", Invalid: true, mod: func(a *Article) {
			a.Set("Subject", "nul\x00subject")
		}},
		{Name: "bare-cr", What: "a CR not followed by LF inside a body line", Invalid: true, mod: func(a *Article) {
			a.SetBody([]byte("one\rtwo\r\nthree\r\n"))
		}},
		{Name: "bare-lf", What: "an LF not preceded by CR inside the body", Invalid: true, mod: func(a *Article) {
			a.SetBody([]byte("one\ntwo\r\nthree\r\n"))
		}},
		{Name: "bare-cr-header", What: "a CR not followed by LF inside a header value", Invalid: true, mod: func(a *Article) {
			a.Set("Subject", "bare\rcr")
		}},
		{Name: "bare-lf-header", What: "an LF not preceded by CR inside a header value (Parse sees it as a line end)", Invalid: true, mod: func(a *Article) {
			a.Set("Subject", "bare\nlf")
		}},
		{Name: "dot-lines", What: "body lines that start with a dot: \".\", \"..\", \".hidden\", \"...\"; the client dot-stuffs them", mod: func(a *Article) {
			a.SetBodyLines("first", ".", "..", ".hidden", "...three", "last")
		}},
		{Name: "long-line", What: "a 100000 byte body line", mod: func(a *Article) {
			a.SetBodyLines("short", strings.Repeat("x", 100000), "end")
		}},
		{Name: "long-header", What: "a 5000 byte Subject", mod: func(a *Article) {
			a.Set("Subject", strings.Repeat("long subject ", 385))
		}},
		{Name: "folded-subject", What: "Subject folded over three lines", mod: func(a *Article) {
			a.Set("Subject", "first part\r\n second part\r\n\tthird part")
		}},
		{Name: "folded-path", What: "Path folded before a '!'", mod: func(a *Article) {
			a.Set("Path", "relay.invalid\r\n !injector.invalid!not-for-mail")
		}},
		{Name: "no-colon", What: "a header line without a colon", Invalid: true, mod: func(a *Article) {
			a.AddRaw("This line has no colon")
		}},
		{Name: "space-before-colon", What: "a space between a header name and its colon", Invalid: true, mod: func(a *Article) {
			a.AddRaw("X-Spaced : value")
		}},
		{Name: "dup-header", What: "Subject twice", Invalid: true, mod: func(a *Article) {
			a.Add("Subject", "the second subject")
		}},
		{Name: "empty-path", What: "an empty Path header (\"Path:\")", Invalid: true, mod: func(a *Article) {
			a.Del("Path")
			a.Fields = append([]Field{{Raw: "Path:"}}, a.Fields...)
		}},
		{Name: "empty-body", What: "headers, the empty line and nothing after it", mod: func(a *Article) {
			a.Body = nil
		}},
		{Name: "no-blank-line", What: "no empty line and no body: the article ends right after the headers", Invalid: true, mod: func(a *Article) {
			a.Sep = NoEOL
			a.Body = nil
		}},
		{Name: "header-case", What: "header names in lower and mixed case", mod: func(a *Article) {
			for i, f := range a.Fields {
				switch i % 3 {
				case 0:
					a.Fields[i].Raw = lowerName(f.Name()) + f.Raw[len(f.Name()):]
				case 1:
					a.Fields[i].Raw = strings.ToUpper(f.Name()) + f.Raw[len(f.Name()):]
				}
			}
		}},
		{Name: "no-space-after-colon", What: "header values without the space after the colon", mod: func(a *Article) {
			for i, f := range a.Fields {
				a.Fields[i].Raw = f.Name() + ":" + f.Value()
			}
		}},
		{Name: "latin1-header", What: "8-bit ISO-8859-1 in the Subject (RFC 2047 would encode it)", Invalid: true, mod: func(a *Article) {
			a.Set("Subject", "caf\xe9")
		}},
		{Name: "trailing-space", What: "spaces and tabs at the end of body lines and an empty last line", mod: func(a *Article) {
			a.SetBodyLines("trailing spaces   ", "trailing tab\t", "plain", "")
		}},
		{Name: "blank-lines", What: "empty lines at the start and end of the body", mod: func(a *Article) {
			a.SetBodyLines("", "", "text", "", "")
		}},
	}
}

// CaseNames returns the names of Corpus, in order.
func CaseNames() []string {
	var names []string
	for _, c := range Corpus() {
		names = append(names, c.Name)
	}
	return names
}

// CaseByName returns the corpus case called name.
func CaseByName(name string) (Case, bool) {
	for _, c := range Corpus() {
		if c.Name == name {
			return c, true
		}
	}
	return Case{}, false
}
