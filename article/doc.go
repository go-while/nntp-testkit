// Package article builds, parses and compares news articles as raw bytes for tests. It is the
// one place for what each suite used to hand-write: a unique Message-ID per test, a minimal
// valid article, articles that are deliberately broken, header lookup and edit, the wire form
// the client sends, a header diff between a sent and a served article, a corpus of tricky
// articles and the control messages (cancel, newgroup, rmgroup). Only the standard library is
// used.
//
// The central rule: nothing is normalised. An Article keeps its header fields in order with the
// exact case of the names, duplicates, empty values, folded lines, colon-less lines, bare CR or
// LF, NUL and 8-bit bytes, because the point of many tests is to send something a normal
// library would refuse to write. Only the helpers that say so (New, Add, Set, ...) write
// well-formed text, and the test can edit the result into anything.
//
// Contract (plan nntp-testkit). Implemented in article.go, msgid.go, build.go, diff.go and
// corpus.go:
//
//	type Field struct {
//		Raw string // the field as written, without its final line end; folds (line break + space/tab) inside it are part of it
//		EOL string // written after Raw: "" = CRLF, "\n" = bare LF, NoEOL = nothing, anything else as is
//	}
//	func (f Field) Name() string      // text before the first colon, as written; "" when there is no colon
//	func (f Field) HasColon() bool
//	func (f Field) Value() string     // unfolded, spaces and tabs trimmed; the whole Raw when there is no colon
//	const NoEOL = "\x00none\x00"      // sentinel: write no line end
//
//	type Article struct {
//		Fields []Field
//		Sep    string // between head and body: "" = CRLF (the empty line), "\n" = bare LF, NoEOL = no empty line
//		Body   []byte // as is: not dot-stuffed, normally ending in CRLF
//	}
//
// Message-IDs:
//
//	const DefaultDomain = "testkit.invalid"
//	func MsgID(tb testing.TB) string                       // MsgIDAt(tb, DefaultDomain)
//	func MsgIDAt(tb testing.TB, domain string) string      // NewMsgID(tb.Name(), domain)
//	func NewMsgID(name, domain string) string              // "<name.time.seq@domain>"
//	func ValidMsgID(id string) bool
//
// NewMsgID writes the name with every character that is not a letter, digit, '-' or '_' replaced
// by '_' (cut to 100 bytes, "t" when empty), then the time in nanoseconds in base 36, then a
// process-wide sequence number; domain "" is DefaultDomain and a domain is not checked. Two
// calls never return the same string, also from parallel tests, and two runs differ (a server
// with persistent history does not see a duplicate). ValidMsgID checks the form generators
// write: 5 to 250 bytes, "<" dot-atom-text "@" dot-atom-text ">" (RFC 5536 section 3.1.3; the
// quoted and literal forms are not accepted).
//
// Building:
//
//	type Options struct {
//		Newsgroups, Subject, From, PathHost, MessageID, Domain string
//		Date time.Time
//		Body []string
//	}
//	func New(tb testing.TB, o Options) *Article            // minimal valid article
//	func Minimal(tb testing.TB, group string) *Article     // New with the group set
//	const DefaultGroup, DefaultPathHost, DefaultFrom, DateLayout
//
// New writes Path ("<PathHost>!not-for-mail"), From, Newsgroups, Subject, Message-ID and Date
// (RFC 5536 section 3, UTC, to the second) in that order, CRLF line ends, the empty line and a
// short body (Options.Body lines, each plus CRLF). Every zero Options field takes its default
// (group "test", subject "test article <sanitized test name>", a generated Message-ID, now).
// tb is used for its name only.
//
// Editing and reading (all lookups compare names case-insensitively, ASCII only, against the
// name exactly as written before the colon, without trimming):
//
//	func (a *Article) Add(name, value string) *Article    // append "name: value"
//	func (a *Article) AddRaw(raw string) *Article         // append raw as one field, byte for byte
//	func (a *Article) Set(name, value string) *Article    // replace the first field of that name in place, drop the others, append if none
//	func (a *Article) Del(name string) *Article           // drop every field of that name
//	func (a *Article) SetBody(b []byte) *Article          // copy of b, as is
//	func (a *Article) SetBodyLines(lines ...string) *Article // each line plus CRLF; no dot-doubling
//	func (a *Article) Lookup(name string) (string, bool)  // first value, unfolded
//	func (a *Article) Get(name string) string             // Lookup without the bool
//	func (a *Article) Values(name string) []string
//	func (a *Article) Has(name string) bool
//	func (a *Article) Count(name string) int
//	func (a *Article) Duplicates() []string               // lower-cased names that occur more than once
//	func (a *Article) MessageID() string                  // Get("Message-ID")
//	func (a *Article) Clone() *Article
//
// Add writes "name: value", so an empty value gives "name: " with the space; AddRaw("Path:")
// writes the empty Path without it, AddRaw("Path: a\r\n !b") folds before the '!', AddRaw("no
// colon here") writes a line without colon. Set resets the line end of the replaced field to
// CRLF. All the edit methods return the article, so calls chain.
//
// Output:
//
//	func (a *Article) Bytes() []byte     // wire form
//	func (a *Article) String() string
//	func (a *Article) Lines() []string   // Bytes split at CRLF
//	func (a *Article) BodyLines() []string
//
// Bytes writes every field's Raw and EOL, then Sep, then Body: CRLF line ends, NOT dot-stuffed,
// without the terminating ".". That is the form the client's Send and Stuff take; the client
// does the stuffing and the terminator, so a body line that starts with "." goes out doubled
// and the server must hand it back as written. Lines drops the CRLF of each line (a final line
// without CRLF stays; nothing follows a final CRLF); a bare CR or LF stays inside its line, so
// client.Stuff(a.Lines()) is Bytes dot-stuffed plus ".\r\n" whenever Bytes ends in CRLF.
//
// Parsing:
//
//	func Parse(raw []byte) *Article
//	func ParseLines(lines []string) *Article   // each line plus CRLF, then Parse
//
// Parse splits at the first empty line (CRLF or LF line ends both count; an empty line has no
// bytes before its LF except an optional CR). Every header line stays: a line that starts with
// space or tab continues the field before it and becomes part of that field's Raw with its
// exact line break; a line without colon is a field of its own. The body is the rest, as is.
// Sep and the fields' EOL record bare LFs, so Parse(b).Bytes() equals b for every b (also
// NUL, bare CR, input without an empty line, input without a final line end). A bare LF inside
// a header becomes a line end for Parse: the text after it is a field of its own, usually
// without colon; a bare CR stays inside its line. Value unfolds for lookup (removes each CRLF
// or LF directly before a space or tab) and Duplicates and Count find repeated headers. The
// result shares no memory with its input. ParseLines is for the client's ReadDotLines (the
// un-stuffed lines of a served article).
//
// Comparing a sent and a served article:
//
//	func DefaultAllowed() []string
//	type DiffOptions struct {
//		Allow               []string // more names a server may add, lose or change
//		Strict              bool     // do not allow DefaultAllowed
//		IgnoreTrailingSpace bool     // compare bodies without trailing space, tab and CR per line and without empty lines at the end
//	}
//	type Change struct{ Name string; Sent, Got []string }
//	type Diff struct {
//		Added, Lost []string
//		Changed     []Change
//		Reordered   bool
//		OrderSent, OrderGot []string
//		BodyDiffers bool
//		BodyDetail  string
//	}
//	func Compare(sent, got *Article, o DiffOptions) Diff
//	func (d Diff) Empty() bool
//	func (d Diff) String() string
//	func ExpectSame(tb testing.TB, sent, got *Article, o DiffOptions) // tb.Errorf with the Diff
//
// The default allow-list is Path, Xref, Injection-Info, Injection-Date, NNTP-Posting-Host,
// NNTP-Posting-Date, Lines, X-Trace and X-Complaints-To: servers add, trim or rewrite those, so
// they are ignored (also when they differ, are missing or appear only in got). Everything else
// is matched by lower-cased name and compared by unfolded values, so re-folding is no change,
// but a trimmed, re-spaced or re-cased value is. Added: names only in got; Lost: only in sent;
// Changed: in both with other values or another number of fields (duplicates lost or
// invented); Reordered: the fields both articles hold equally often come in another order. A
// header that is missing in sent and got is nothing. The bodies are equal byte for byte unless
// IgnoreTrailingSpace is set; the first difference is named by offset and line.
//
// Corpus of tricky articles:
//
//	type Case struct {
//		Name    string // t.Run-safe identifier
//		What    string
//		Invalid bool   // breaks RFC 5536 (or 3977): a server may reject or repair it
//	}
//	func (c Case) Article(tb testing.TB, group string) *Article // Minimal plus the quirk
//	func Corpus() []Case
//	func CaseNames() []string
//	func CaseByName(name string) (Case, bool)
//
// The cases (names; the Invalid ones marked *): latin1 (8-bit ISO-8859-1 body with MIME
// headers), utf8, nul* (NUL in the body), nul-header*, bare-cr* and bare-lf* (in the body),
// bare-cr-header*, bare-lf-header*, dot-lines (lines ".", "..", ".hidden", "...three"),
// long-line (100000 bytes), long-header (5000 byte Subject), folded-subject, folded-path (a
// fold before '!'), no-colon*, space-before-colon*, dup-header* (Subject twice), empty-path*
// ("Path:"), empty-body, no-blank-line* (no empty line, no body), header-case (lower and upper
// case names), no-space-after-colon, latin1-header*, trailing-space, blank-lines. Each is a
// fresh article of Minimal(tb, group) with the quirk applied, so it has its own Message-ID.
// Parse(c.Article(...).Bytes()).Bytes() equals the bytes for every case.
//
// Control messages (RFC 5537), each the article of New(tb, o) with a Control header added after
// Subject; Options fields set by the caller win over the defaults named here:
//
//	func Cancel(tb testing.TB, o Options, target string) *Article
//	func Newgroup(tb testing.TB, o Options, group string, moderated bool) *Article
//	func Rmgroup(tb testing.TB, o Options, group string) *Article
//
// Cancel: "Control: cancel <target>", Subject "cmsg cancel <target>", body "cancel <target>";
// a server honours it only for the same From (set o.From to the original's) or, with a
// Cancel-Key, a matching key (see package canlock). Newgroup: "Control: newgroup <group>"
// (plus " moderated"), Subject "cmsg newgroup <group>", Newsgroups the group, a body in the
// "For your newsgroups file:" form. Rmgroup: "Control: rmgroup <group>", Subject "cmsg rmgroup
// <group>", Newsgroups the group. Headers only: there is no PGP signing and no Approved
// header; Add one when the server under test wants it.
package article
