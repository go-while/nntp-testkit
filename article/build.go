package article

import (
	"errors"
	"testing"
	"time"
)

// Defaults of Options.
const (
	DefaultGroup    = "test"
	DefaultPathHost = "testkit.invalid"
	DefaultFrom     = "Test User <test@testkit.invalid>"
)

// DateLayout is the Date header format of the articles New writes (RFC 5536 section 3.1.1,
// RFC 5322 date-time, UTC as "+0000").
const DateLayout = "Mon, 02 Jan 2006 15:04:05 -0700"

// Options describes a valid article; every zero field takes its default.
type Options struct {
	Newsgroups string    // the Newsgroups header, "" = DefaultGroup ("test")
	Subject    string    // "" = "test article <test name>" (the name as in a Message-ID)
	From       string    // "" = DefaultFrom
	PathHost   string    // the Path is "<PathHost>!not-for-mail"; "" = DefaultPathHost
	MessageID  string    // "" = MsgIDAt(tb, Domain)
	Domain     string    // domain of a generated Message-ID, "" = DefaultDomain
	Date       time.Time // zero = now; written in UTC to the second
	Body       []string  // body lines, none = one short line of text
	Approved   string    // when set, an Approved header after Date (moderated groups, control messages)
	Extra      []string  // more header lines, each written as one field after Date (and Approved) with AddRaw
	Omit       []string  // names (case-insensitive) of the generated headers to leave out: Path, From, Newsgroups, Subject, Message-ID, Date, Approved; Extra is not affected
}

// New builds a minimal valid article: the mandatory headers of RFC 5536 section 3 in the order
// Path, From, Newsgroups, Subject, Message-ID, Date, an empty line and a short body, all with
// CRLF. tb only supplies the test name (for the Subject and Message-ID, unless given or
// omitted). A caller that needs the article broken edits the result. A newsreader's POST,
// which has no Path, Date or Message-ID, is
//
//	article.New(t, article.Options{Omit: []string{"Path", "Date", "Message-ID"}})
func New(tb testing.TB, o Options) *Article {
	tb.Helper()
	if o.Subject == "" && !o.omits("Subject") {
		o.Subject = "test article " + sanitize(tb.Name())
	}
	if o.MessageID == "" && !o.omits("Message-ID") {
		o.MessageID = MsgIDAt(tb, o.Domain)
	}
	return build(o)
}

// NewArticle is New without testing: it needs no tb, so a program or a helper outside a test
// can use it. Nothing is derived from a test name, so it returns an error when Subject or
// MessageID is empty and not in Omit; every other field has a default as in New.
func NewArticle(o Options) (*Article, error) {
	if o.Subject == "" && !o.omits("Subject") {
		return nil, errors.New("article: NewArticle needs Options.Subject (or Omit it)")
	}
	if o.MessageID == "" && !o.omits("Message-ID") {
		return nil, errors.New("article: NewArticle needs Options.MessageID (or Omit it)")
	}
	return build(o), nil
}

// omits reports whether name is in Omit.
func (o Options) omits(name string) bool {
	for _, n := range o.Omit {
		if sameName(n, name) {
			return true
		}
	}
	return false
}

// build writes the article of o, whose Subject and MessageID are settled.
func build(o Options) *Article {
	date := o.Date
	if date.IsZero() {
		date = time.Now()
	}
	a := &Article{}
	add := func(name, value string) {
		if !o.omits(name) {
			a.Add(name, value)
		}
	}
	add("Path", orDefault(o.PathHost, DefaultPathHost)+"!not-for-mail")
	add("From", orDefault(o.From, DefaultFrom))
	add("Newsgroups", orDefault(o.Newsgroups, DefaultGroup))
	add("Subject", o.Subject)
	add("Message-ID", o.MessageID)
	add("Date", date.UTC().Format(DateLayout))
	if o.Approved != "" {
		add("Approved", o.Approved)
	}
	for _, raw := range o.Extra {
		a.AddRaw(raw)
	}
	body := o.Body
	if len(body) == 0 {
		body = []string{"This is a test article made by nntp-testkit."}
	}
	return a.SetBodyLines(body...)
}

// Minimal is New with only the group set.
func Minimal(tb testing.TB, group string) *Article {
	tb.Helper()
	return New(tb, Options{Newsgroups: group})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Cancel builds a cancel control message for the article with Message-ID target
// (RFC 5537 section 5.3): the article of New(tb, o) with the extra header
// "Control: cancel <target>", Subject "cmsg cancel <target>" (unless o.Subject is set) and the
// body "cancel <target>" (unless o.Body is set). Newsgroups is o.Newsgroups, "test" when empty:
// give the groups of the target, for a server that looks the target up through them. A server
// honours the cancel only when its From matches the target's, so set o.From to the original's.
func Cancel(tb testing.TB, o Options, target string) *Article {
	tb.Helper()
	return control(tb, o, "cancel "+target, "cancel "+target)
}

// Newgroup builds a newgroup control message (RFC 5537 section 7.1) for group: "Control:
// newgroup <group>" (with " moderated" when moderated), Subject "cmsg newgroup <group>" (unless
// o.Subject is set), Newsgroups the group itself (unless o.Newsgroups is set), and a body in
// the usual format, "For your newsgroups file:" and "<group>\t<description>" (unless o.Body
// is set). The headers only: no Approved header and no PGP signature; add them with Add.
func Newgroup(tb testing.TB, o Options, group string, moderated bool) *Article {
	tb.Helper()
	ctl := "newgroup " + group
	if moderated {
		ctl += " moderated"
	}
	if o.Newsgroups == "" {
		o.Newsgroups = group
	}
	if len(o.Body) == 0 {
		o.Body = []string{"For your newsgroups file:", group + "\tTest group made by nntp-testkit"}
	}
	return control(tb, o, ctl, ctl)
}

// Rmgroup builds an rmgroup control message for group: "Control: rmgroup <group>", Subject
// "cmsg rmgroup <group>" (unless o.Subject is set), Newsgroups the group (unless o.Newsgroups
// is set); headers only like Newgroup.
func Rmgroup(tb testing.TB, o Options, group string) *Article {
	tb.Helper()
	if o.Newsgroups == "" {
		o.Newsgroups = group
	}
	ctl := "rmgroup " + group
	return control(tb, o, ctl, ctl)
}

// control builds the article, adds the Control header after Subject and the defaults of a
// control message.
func control(tb testing.TB, o Options, ctl, body string) *Article {
	tb.Helper()
	if o.Subject == "" {
		o.Subject = "cmsg " + ctl
	}
	if len(o.Body) == 0 {
		o.Body = []string{body}
	}
	a := New(tb, o)
	for i, f := range a.Fields {
		if sameName(f.Name(), "Subject") {
			return a.Insert(i+1, "Control: "+ctl)
		}
	}
	return a.Insert(0, "Control: "+ctl) // Subject omitted
}
