package article

import (
	"bytes"
	"strings"
)

// NoEOL is the value of Field.EOL or Article.Sep that writes no line end at all. It is a
// sentinel string that cannot be mistaken for a real line end.
const NoEOL = "\x00none\x00"

// Field is one header field exactly as it is written: Raw is the whole text of the field
// without its final line end, including any folding (a line break followed by space or tab)
// inside it, and EOL is the line end written after it.
type Field struct {
	// Raw is the field as written, e.g. "Subject: hello" or "Path: a\r\n !b". Nothing in it is
	// checked: it may lack a colon, hold a bare CR or LF, or a NUL.
	Raw string
	// EOL is the line end written after Raw: "" is CRLF, "\n" a bare LF, NoEOL nothing; any
	// other string is written as it is.
	EOL string
}

// Name returns the text before the first colon, as written (no trimming, case kept), or ""
// when the field has no colon.
func (f Field) Name() string {
	name, _, ok := strings.Cut(f.Raw, ":")
	if !ok {
		return ""
	}
	return name
}

// HasColon reports whether Raw holds a colon.
func (f Field) HasColon() bool { return strings.Contains(f.Raw, ":") }

// Value returns the unfolded value: the text after the first colon (the whole of Raw when
// there is no colon) with each fold, a CRLF or LF directly before a space or tab, removed
// (RFC 5322 section 2.2.3), and spaces and tabs trimmed from both ends.
func (f Field) Value() string {
	_, v, ok := strings.Cut(f.Raw, ":")
	if !ok {
		v = f.Raw
	}
	return strings.Trim(unfold(v), " \t")
}

// unfold removes every CRLF or LF that is followed by a space or tab.
func unfold(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && i+2 < len(s) && s[i+1] == '\n' && isWSP(s[i+2]) {
			i++
			continue
		}
		if s[i] == '\n' && i+1 < len(s) && isWSP(s[i+1]) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isWSP(c byte) bool { return c == ' ' || c == '\t' }

// sameName compares two header names case-insensitively, for ASCII only (unlike
// strings.EqualFold, which also folds some non-ASCII letters to ASCII ones).
func sameName(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func lowerName(s string) string {
	b := []byte(s)
	for i := range b {
		b[i] = lower(b[i])
	}
	return string(b)
}

// Article is a news article as bytes: header fields in order, the line between head and
// body, and the body. Nothing is normalised: Bytes writes what the fields hold, so an Article
// can be as broken as a test needs. The fields are exported so a test can reorder, insert or
// edit them directly.
type Article struct {
	// Fields are the header fields in the order they are written.
	Fields []Field
	// Sep is what is written between the last field and the body: "" is the empty line CRLF,
	// "\n" a bare-LF empty line, NoEOL nothing (no empty line at all); any other string is
	// written as it is.
	Sep string
	// Body is written as it is after Sep: not dot-stuffed, not terminated, normally ending in
	// CRLF.
	Body []byte
}

// Parse splits raw into head and body at the first empty line (a line that is empty or only a
// CR before its LF: CRLF or LF line ends both count) and keeps everything. Each field keeps
// its exact text; a line that starts with a space or tab continues the field before it (it is
// part of that field's Raw, with the exact line break); a line without a colon is a field of
// its own. The body is everything after the empty line, as is. Without an empty line all of
// raw is head and Sep is NoEOL. Parse never fails, and Parse(b).Bytes() equals b for every b.
// The result does not share memory with raw. Note that a bare LF inside a header splits it
// into two fields (the second usually without a colon), and a bare CR stays inside.
func Parse(raw []byte) *Article {
	a := &Article{}
	for i := 0; i < len(raw); {
		var line []byte
		var eol string
		next := len(raw)
		if j := bytes.IndexByte(raw[i:], '\n'); j < 0 {
			line, eol = raw[i:], NoEOL
		} else {
			line, next = raw[i:i+j], i+j+1
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			} else {
				eol = "\n"
			}
		}
		if len(line) == 0 && eol != NoEOL {
			a.Sep = eol
			if next < len(raw) {
				a.Body = append([]byte(nil), raw[next:]...)
			}
			return a
		}
		if n := len(a.Fields); n > 0 && isWSP(line[0]) {
			prev := &a.Fields[n-1]
			prev.Raw += eolString(prev.EOL) + string(line)
			prev.EOL = eol
		} else {
			a.Fields = append(a.Fields, Field{Raw: string(line), EOL: eol})
		}
		i = next
	}
	a.Sep = NoEOL
	return a
}

// ParseLines builds an article from lines as the client's ReadDotLines returns them
// (un-stuffed, line ends stripped): each line gets a CRLF, then Parse.
func ParseLines(lines []string) *Article {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	return Parse([]byte(b.String()))
}

// eolString is the bytes an EOL value stands for.
func eolString(eol string) string {
	switch eol {
	case "":
		return "\r\n"
	case NoEOL:
		return ""
	}
	return eol
}

// Bytes returns the article in wire form: every field's Raw and EOL, then Sep, then Body.
// Line ends are CRLF unless a field says otherwise. It is not dot-stuffed and has no
// terminating ".": the client's Stuff, Send and TryPost take it that way and add both.
func (a *Article) Bytes() []byte {
	var b bytes.Buffer
	for _, f := range a.Fields {
		b.WriteString(f.Raw)
		b.WriteString(eolString(f.EOL))
	}
	b.WriteString(eolString(a.Sep))
	b.Write(a.Body)
	return b.Bytes()
}

// String returns string(a.Bytes()), for %q in messages.
func (a *Article) String() string { return string(a.Bytes()) }

// Lines returns Bytes split at CRLF, without the line ends: the form client.Stuff takes. A
// final line without CRLF is kept as the last element and nothing follows a final CRLF, so
// joining the lines with CRLF and adding one CRLF gives Bytes back exactly when Bytes ends in
// CRLF. A bare CR or LF stays inside its line. No bytes give no lines.
func (a *Article) Lines() []string { return splitCRLF(a.Bytes()) }

// BodyLines is Lines for the body alone.
func (a *Article) BodyLines() []string { return splitCRLF(a.Body) }

func splitCRLF(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.Split(string(b), "\r\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Clone returns a copy that shares no memory with a.
func (a *Article) Clone() *Article {
	c := &Article{Sep: a.Sep}
	c.Fields = append([]Field(nil), a.Fields...)
	if a.Body != nil {
		c.Body = append([]byte{}, a.Body...)
	}
	return c
}

// Lookup returns the unfolded value of the first field called name (compared
// case-insensitively, ASCII only, with the name exactly as written before the colon: no
// trimming) and whether there is one. A field without a colon has the name "".
func (a *Article) Lookup(name string) (string, bool) {
	for _, f := range a.Fields {
		if sameName(f.Name(), name) {
			return f.Value(), true
		}
	}
	return "", false
}

// Get returns the first value of Lookup, "" when there is none (use Lookup or Has to tell an
// absent field from an empty one).
func (a *Article) Get(name string) string {
	v, _ := a.Lookup(name)
	return v
}

// Values returns the unfolded values of all fields called name, in order.
func (a *Article) Values(name string) []string {
	var vs []string
	for _, f := range a.Fields {
		if sameName(f.Name(), name) {
			vs = append(vs, f.Value())
		}
	}
	return vs
}

// Has reports whether a field called name exists.
func (a *Article) Has(name string) bool { return a.Count(name) > 0 }

// Count returns how many fields are called name.
func (a *Article) Count(name string) int {
	n := 0
	for _, f := range a.Fields {
		if sameName(f.Name(), name) {
			n++
		}
	}
	return n
}

// Duplicates returns the names (lower-cased, in order of first appearance) that more than one
// field has. Fields without a colon are not counted.
func (a *Article) Duplicates() []string {
	count := map[string]int{}
	var order []string
	for _, f := range a.Fields {
		if !f.HasColon() {
			continue
		}
		k := lowerName(f.Name())
		if count[k] == 0 {
			order = append(order, k)
		}
		count[k]++
	}
	var dups []string
	for _, k := range order {
		if count[k] > 1 {
			dups = append(dups, k)
		}
	}
	return dups
}

// MessageID returns Get("Message-ID").
func (a *Article) MessageID() string { return a.Get("Message-ID") }

// Add appends the field "name: value" (the value as given, not folded or checked; an empty
// value gives "name: " with the space). It returns a for chaining.
func (a *Article) Add(name, value string) *Article {
	return a.AddRaw(name + ": " + value)
}

// AddRaw appends raw as one field, byte for byte: use it for what Add cannot write, such as
// "Path:" with no space, a line without colon, or a folded field ("Path: a\r\n !b"). It
// returns a for chaining.
func (a *Article) AddRaw(raw string) *Article {
	a.Fields = append(a.Fields, Field{Raw: raw})
	return a
}

// Set makes "name: value" the only field called name: the first one is replaced in place
// (written as "name: value" with the name's spelling of this call and a CRLF end), the others
// are removed; with none, the field is appended. It returns a for chaining.
func (a *Article) Set(name, value string) *Article {
	out := a.Fields[:0:0]
	done := false
	for _, f := range a.Fields {
		if !sameName(f.Name(), name) {
			out = append(out, f)
			continue
		}
		if !done {
			out = append(out, Field{Raw: name + ": " + value})
			done = true
		}
	}
	if !done {
		out = append(out, Field{Raw: name + ": " + value})
	}
	a.Fields = out
	return a
}

// Del removes every field called name. It returns a for chaining.
func (a *Article) Del(name string) *Article {
	out := a.Fields[:0:0]
	for _, f := range a.Fields {
		if !sameName(f.Name(), name) {
			out = append(out, f)
		}
	}
	a.Fields = out
	return a
}

// SetBody sets the body to a copy of b, as it is: it should end in CRLF unless the test wants
// otherwise, and it is not dot-stuffed. It returns a for chaining.
func (a *Article) SetBody(b []byte) *Article {
	a.Body = append([]byte(nil), b...)
	return a
}

// SetBodyLines sets the body to the lines, each followed by CRLF. A line is written as given
// (a leading "." is not doubled; the client stuffs it on send). No lines give an empty body.
// It returns a for chaining.
func (a *Article) SetBodyLines(lines ...string) *Article {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	a.Body = []byte(b.String())
	return a
}
