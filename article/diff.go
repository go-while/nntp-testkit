package article

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// DefaultAllowed returns the header fields a server may add or change on its way (they are
// ignored by Compare unless DiffOptions.Strict): Path, Xref, Injection-Info, Injection-Date,
// NNTP-Posting-Host, NNTP-Posting-Date, Lines, X-Trace, X-Complaints-To and Organization
// (INN and others add the configured organization to a posted article that has none). A new
// slice each time.
func DefaultAllowed() []string {
	return []string{"Path", "Xref", "Injection-Info", "Injection-Date", "NNTP-Posting-Host",
		"NNTP-Posting-Date", "Lines", "X-Trace", "X-Complaints-To", "Organization"}
}

// DiffOptions tune Compare.
type DiffOptions struct {
	// Allow lists more field names (case-insensitive) a server may add, lose or change, as well
	// as DefaultAllowed's.
	Allow []string
	// Strict leaves DefaultAllowed out: only Allow is ignored.
	Strict bool
	// IgnoreTrailingSpace compares the bodies line by line without trailing spaces, tabs and
	// CRs, and without empty lines at the end. A body that differs in any other byte still
	// differs. Off: the bodies must be equal byte for byte.
	IgnoreTrailingSpace bool
	// IgnoreOrder does not report Reordered (for servers that rearrange the head).
	IgnoreOrder bool
	// CaseSensitiveNames makes a field whose name is spelled differently (From and FROM) a
	// change; off, names are compared case-insensitively.
	CaseSensitiveNames bool
}

// Change is one field whose values differ: the unfolded values of every field of that name,
// in order, as sent and as received.
type Change struct {
	Name      string
	Sent, Got []string
	// SentNames and GotNames are the spellings of the name, one per field, set only when
	// CaseSensitiveNames found them different.
	SentNames, GotNames []string
}

// Diff is what Compare found between a sent and a received article. All lists keep the order
// of first appearance. Fields that the options allow are in none of them.
type Diff struct {
	Added   []string // names only in the received article
	Lost    []string // names only in the sent article
	Changed []Change // names in both whose values (or number of fields) differ
	// Reordered: the fields that both articles hold in equal number are in another order. The
	// order itself is in OrderSent and OrderGot (lower-cased names, one per field).
	Reordered bool
	OrderSent []string
	OrderGot  []string
	// BodyDiffers is set when the bodies differ; BodyDetail says where.
	BodyDiffers bool
	BodyDetail  string
}

// Empty reports that the articles agree: no field added, lost, changed or reordered, and the
// same body.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Lost) == 0 && len(d.Changed) == 0 && !d.Reordered && !d.BodyDiffers
}

// String describes the differences in a few lines, "" when Empty.
func (d Diff) String() string {
	var b strings.Builder
	if len(d.Added) > 0 {
		fmt.Fprintf(&b, "added by the server: %s\n", strings.Join(d.Added, ", "))
	}
	if len(d.Lost) > 0 {
		fmt.Fprintf(&b, "lost: %s\n", strings.Join(d.Lost, ", "))
	}
	for _, c := range d.Changed {
		fmt.Fprintf(&b, "changed %s: sent %q, got %q\n", c.Name, c.Sent, c.Got)
	}
	if d.Reordered {
		fmt.Fprintf(&b, "reordered: sent %s, got %s\n", strings.Join(d.OrderSent, " "), strings.Join(d.OrderGot, " "))
	}
	if d.BodyDiffers {
		fmt.Fprintf(&b, "body: %s\n", d.BodyDetail)
	}
	return b.String()
}

// Compare compares the article a test sent with the one a server served back. Both are first
// canonicalised (Article.Canon: written out and parsed again), so a bare LF inside a field is
// the line end it is on the wire and an untouched relay shows no difference. Fields are matched
// by name (case-insensitive unless CaseSensitiveNames) and compared by their values with every
// fold, a line break and the spaces and tabs after it, read as one space and spaces and tabs
// trimmed from both ends: a server that folds a long header at another place, or with a tab, is
// no difference, but one that changes a word, re-spaces the inside of a value or re-cases it is.
// For each name outside the allow-list it reports: only in got (Added), only in sent (Lost), in
// both with other values or another number of fields (Changed); and Reordered when the names
// both hold equally often come in another order. The bodies are compared byte for byte, or as
// DiffOptions.IgnoreTrailingSpace says. Fields without a colon have the empty name and their
// whole text as value. A server that normalises bare CR or LF (or line ends) cannot be compared
// byte for byte: the body differs, and IgnoreTrailingSpace covers CRLF against LF only.
func Compare(sent, got *Article, o DiffOptions) Diff {
	allowed := map[string]bool{}
	if !o.Strict {
		for _, n := range DefaultAllowed() {
			allowed[lowerName(n)] = true
		}
	}
	for _, n := range o.Allow {
		allowed[lowerName(n)] = true
	}
	var d Diff
	sent, got = sent.Canon(), got.Canon()
	sg, sNames, sSpell, sSpellings := groupFields(sent, allowed)
	gg, gNames, gSpell, gSpellings := groupFields(got, allowed)
	for _, k := range gNames {
		if _, ok := sg[k]; !ok {
			d.Added = append(d.Added, gSpell[k])
		}
	}
	for _, k := range sNames {
		gv, ok := gg[k]
		if !ok {
			d.Lost = append(d.Lost, sSpell[k])
			continue
		}
		spellDiff := o.CaseSensitiveNames && !equalStrings(sSpellings[k], gSpellings[k])
		if !equalStrings(sg[k], gv) || spellDiff {
			c := Change{Name: sSpell[k], Sent: sg[k], Got: gv}
			if spellDiff {
				c.SentNames, c.GotNames = sSpellings[k], gSpellings[k]
			}
			d.Changed = append(d.Changed, c)
		}
	}
	order := func(a *Article) []string {
		var seq []string
		for _, f := range a.Fields {
			k := lowerName(f.Name())
			if allowed[k] {
				continue
			}
			if sv, ok := sg[k]; ok {
				if gv, ok := gg[k]; ok && len(sv) == len(gv) {
					seq = append(seq, k)
				}
			}
		}
		return seq
	}
	oSent, oGot := order(sent), order(got)
	if !o.IgnoreOrder && !equalStrings(oSent, oGot) {
		d.Reordered, d.OrderSent, d.OrderGot = true, oSent, oGot
	}
	d.BodyDiffers, d.BodyDetail = compareBodies(sent.Body, got.Body, o.IgnoreTrailingSpace)
	return d
}

// groupFields collects the comparison values of the fields that are not allowed by
// lower-cased name, with the names in order of first appearance, the first spelling of each
// and all the spellings.
func groupFields(a *Article, allowed map[string]bool) (map[string][]string, []string, map[string]string, map[string][]string) {
	vals := map[string][]string{}
	spell := map[string]string{}
	spellings := map[string][]string{}
	var names []string
	for _, f := range a.Fields {
		k := lowerName(f.Name())
		if allowed[k] {
			continue
		}
		if _, ok := vals[k]; !ok {
			names = append(names, k)
			spell[k] = f.Name()
		}
		vals[k] = append(vals[k], compareValue(f))
		spellings[k] = append(spellings[k], f.Name())
	}
	return vals, names, spell, spellings
}

// compareValue is the value Compare compares: Value with every fold (a line break and the
// spaces and tabs after it) read as one space, so a server that re-folds a long header at
// another place, or with a tab, shows no change.
func compareValue(f Field) string {
	_, v, ok := strings.Cut(f.Raw, ":")
	if !ok {
		v = f.Raw
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if (v[i] == '\r' && i+2 < len(v) && v[i+1] == '\n' && isWSP(v[i+2])) || (v[i] == '\n' && i+1 < len(v) && isWSP(v[i+1])) {
			for i < len(v) && (v[i] == '\r' || v[i] == '\n' || isWSP(v[i])) {
				i++
			}
			i--
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(v[i])
	}
	return strings.Trim(b.String(), " \t")
}

// PathExtends reports whether got's Path is sent's Path with entries added in front, as a
// server does ("relay!peer!not-for-mail" extends "peer!not-for-mail"), or is the same: the
// PathEntries of sent are the last entries of got's, entry for entry. Folds around a '!' are
// ignored, but a space inside an entry is part of it, so a Path that gained a token such as
// "peer junk!not-for-mail" does not extend. It is true when sent has no
// Path (nothing to keep) and false when only got lacks one. Compare allows Path to change, so
// use this to check what a server did to it.
func PathExtends(sent, got *Article) bool {
	sent, got = sent.Canon(), got.Canon()
	if _, ok := sent.Lookup("Path"); !ok {
		return true
	}
	if _, ok := got.Lookup("Path"); !ok {
		return false
	}
	s, g := sent.PathEntries(), got.PathEntries()
	return len(g) >= len(s) && equalStrings(g[len(g)-len(s):], s)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func compareBodies(sent, got []byte, ignoreTrailing bool) (bool, string) {
	if ignoreTrailing {
		sent, got = normBody(sent), normBody(got)
	}
	if bytes.Equal(sent, got) {
		return false, ""
	}
	i := 0
	for i < len(sent) && i < len(got) && sent[i] == got[i] {
		i++
	}
	line := 1 + bytes.Count(sent[:i], []byte("\n"))
	return true, fmt.Sprintf("sent %d bytes, got %d; first difference at byte %d (line %d): sent %q, got %q",
		len(sent), len(got), i, line, excerpt(sent, i), excerpt(got, i))
}

// normBody drops trailing spaces, tabs and CRs of every line and empty lines at the end.
func normBody(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i, l := range lines {
		lines[i] = bytes.TrimRight(l, " \t\r")
	}
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return bytes.Join(lines, []byte("\n"))
}

// excerpt returns up to 24 bytes of b from offset i.
func excerpt(b []byte, i int) []byte {
	if i >= len(b) {
		return nil
	}
	end := min(len(b), i+24)
	return b[i:end]
}

// ExpectSame fails tb with the differences when Compare finds any between the sent and the
// received article.
func ExpectSame(tb testing.TB, sent, got *Article, o DiffOptions) {
	tb.Helper()
	if d := Compare(sent, got, o); !d.Empty() {
		tb.Errorf("received article differs from the sent one:\n%s", d)
	}
}
