package fakeserver

import (
	"fmt"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultOverviewFmt is the LIST OVERVIEW.FMT answer when StoreOptions.OverviewFmt is empty; it
// is also the field order of every OVER/XOVER line.
var defaultOverviewFmt = []string{"Subject:", "From:", "Date:", "Message-ID:", "References:", ":bytes", ":lines", "Xref:full"}

// xrefHost is the host name of a generated Xref field.
const xrefHost = "fakeserver"

func init() { storeHandler = StoreHandler }

// StoreHandler returns the handler of Store mode: an RFC 3977 reader that serves st. Options.Store
// runs it (built once per Server, with Options.StoreOpts); it can also be passed as
// Options.Handler. It keeps per-connection state (selected group, current article,
// authentication) and applies the server's Faults to every reply, keyed by the upper-cased
// command (for POST: to the first reply; an Override that does not start with "340" is final
// and no article is read).
//
// Commands and replies:
//
//	CAPABILITIES [kw]   101 VERSION 2, READER, LIST ACTIVE NEWSGROUPS OVERVIEW.FMT, NEWNEWS (not NoNewnews), OVER MSGID (not NoOver), POST (not ReaderOnly), AUTHINFO USER (auth required and not done), COMPRESS DEFLATE (Compress or CompressOutputOnly, authenticated, compression off), IMPLEMENTATION fakeserver
//	MODE READER         200 (201 when ReaderOnly); other MODE 501
//	HELP                100 command list (without the commands NoOver, NoXover and NoNewnews make unknown)
//	AUTHINFO USER u     381 (281/481 at once when Pass is empty); AUTHINFO PASS p: 281 when User and Pass match, else 481 (482 without USER); 502 when no auth is configured or already done; other variants 501
//	LIST [ACTIVE [wm]]  215 "name high low y|n" (y when Posting and not ReaderOnly); wm is a wildmat (*, ?, comma list, ! negation)
//	LIST NEWSGROUPS [wm] 215 "name<TAB>description"; LIST OVERVIEW.FMT 215; other keywords 501
//	GROUP g             211 "count low high g" (current article = low, none when empty); 411
//	LISTGROUP [g [rng]] 211 + numbers; 411; 412 without g and without a selected group
//	ARTICLE/HEAD/BODY/STAT [n|<id>]  220/221/222/223 "n <id>"; 412, 420, 423, 430; n selects the current article, <id> does not (n is 0 when the article is not in the selected group)
//	NEXT, LAST          223 "n <id>"; 412, 420, 421/422
//	OVER/XOVER [rng|<id>] 224; 412, 420, 423 (empty range or m<n), 430; number 0 for <id>
//	NEWGROUPS d t [GMT] 231 groups whose Created is at or after d t (yymmdd or yyyymmdd, hhmmss; always UTC); 501
//	NEWNEWS wm d t [GMT] 230 message-ids of the articles in groups matching wm whose Arrived is at or after d t (date and time as NEWGROUPS); 501; 503 when a matching generated group spans more numbers than its limit
//	DATE                111 yyyymmddhhmmss (UTC)
//	POST                440 when ReaderOnly; else 340, then 240 "<id>" or 441 (Newsgroups, Subject and From required; a Message-ID that is duplicate or not "<...>" without spaces)
//	COMPRESS alg        only with Compress or CompressOutputOnly (else 500): 206, then both directions compressed (RFC 8054); 501 for a missing or invalid algorithm name; 502 when active; 503 for an algorithm other than DEFLATE
//	QUIT                205, then the connection is closed
//
// With StoreOptions.User or Pass set, every command but CAPABILITIES, MODE, HELP, AUTHINFO, DATE
// and QUIT answers 480 until AUTHINFO succeeds. An unknown command gets 500, a known one with a
// wrong argument count or syntax 501. Ranges are "n", "n-" and "n-m".
//
// COMPRESS (RFC 8054) switches the connection to compression when the status line written
// for it, after the Faults, is a 206 and compression is off; so Faults.Override "COMPRESS"
// {"403 Unable to activate compression"} is a server that cannot compress, and {} one that
// never answers. Once active, every reply and the Faults go through the compressor (each
// write is sync-flushed) and Lines, Counts and Articles record the inflated client stream.
// The handler has no STARTTLS (TLS is Options.TLS, from the first byte; crypto/tls has no
// TLS-level compression), so the RFC's STARTTLS rules do not arise; MODE READER is still
// answered after COMPRESS.
//
// A served article has the stored Headers plus the defaults of Article.WithDefaults (Message-ID,
// Date, and Newsgroups = every group it is filed in) when missing; head and body lines are
// dot-stuffed. An overview line holds the Subject, From, Date, Message-ID and References header
// values (tabs, CR, LF and NUL replaced by spaces), :bytes (head and body with CRLF line ends plus the
// separating blank line), :lines (body lines) and "Xref: " with the stored Xref header, else
// "fakeserver group:number ..." over every location. OverviewFmt only changes the LIST
// OVERVIEW.FMT answer, not the OVER lines.
//
// NEWNEWS lists each message-id once, in group name order and ascending numbers within a
// group (a crossposted article where it is first found). Arrived is the time AddArticle filed
// the article (or the one the test set); a generated article arrived at its Date. A generated
// group is examined whole, so a matching one that spans more numbers than its Generator.Limit
// gets the 503 of OVER, XOVER and LISTGROUP before anything is listed. Without GMT the time is
// UTC too (the fake server's local time is UTC), where INN would take its local time.
//
// POST un-stuffs the article, splits head and body at the first blank line, generates a
// missing Message-ID as <posted-N@fakeserver.invalid> (N counts per handler, skipping ids
// already stored), records it with st.AddPosted and files it with st.AddArticle into each
// group of its Newsgroups header (created when missing). Posting flags are not checked. The
// handler's connections post one at a time (from the duplicate check through the filing), so of
// two concurrent POSTs with the same Message-ID one gets 240 and the other 441.
func StoreHandler(st *Store, o StoreOptions) func(*Conn) {
	seq := new(atomic.Int64)  // generated POST Message-IDs, shared by the handler's connections
	postMu := new(sync.Mutex) // serialises filing POSTs across the handler's connections
	return func(c *Conn) {
		h := &storeConn{st: st, o: o, c: c, seq: seq, postMu: postMu, authed: o.User == "" && o.Pass == ""}
		h.serve()
	}
}

// storeConn is the state of one connection of the store handler.
type storeConn struct {
	st     *Store
	o      StoreOptions
	c      *Conn
	seq    *atomic.Int64
	postMu *sync.Mutex

	group    string // selected group; "" = none
	cur      int64  // current article number; 0 = none
	authed   bool
	user     string // AUTHINFO USER argument
	userSeen bool
}

// action tells serve what to do after dispatch.
type action int

const (
	actReply action = iota // write the reply
	actQuit                // write the reply, then close
	actPost                // run the POST exchange
)

func (h *storeConn) serve() {
	for {
		line, err := h.c.ReadLine()
		if err != nil {
			return
		}
		cmd := command(line)
		args := strings.Fields(line)
		if len(args) > 0 {
			args = args[1:]
		}
		reply, act := h.dispatch(cmd, args)
		switch act {
		case actPost:
			if !h.post(line) {
				return
			}
		default:
			if !h.c.writeReply(line, reply...) || act == actQuit {
				return
			}
			if !h.c.compressAfter(cmd, h.c.last, h.o.CompressOutputOnly) {
				return
			}
		}
		h.after480()
		if h.c.closeDue() {
			return
		}
	}
}

// after480 ends the login when the last reply was a 480, also one from Faults.Override: a
// server that asks for authentication has dropped the login (a login that timed out), so it
// takes AUTHINFO again instead of answering 502. Only for a connection that had logged in,
// with User or Pass configured.
func (h *storeConn) after480() {
	if h.authed && strings.HasPrefix(h.c.last, "480") && (h.o.User != "" || h.o.Pass != "") {
		h.authed, h.user, h.userSeen = false, "", false
	}
}

// authFree are the commands allowed before authentication.
var authFree = map[string]bool{"CAPABILITIES": true, "MODE": true, "HELP": true, "AUTHINFO": true, "DATE": true, "QUIT": true}

// argCount is the allowed argument count [min, max] of every known command.
var argCount = map[string][2]int{
	"CAPABILITIES": {0, 1}, "MODE": {1, 1}, "HELP": {0, 0}, "AUTHINFO": {2, -1}, "LIST": {0, 2},
	"GROUP": {1, 1}, "LISTGROUP": {0, 2}, "ARTICLE": {0, 1}, "HEAD": {0, 1}, "BODY": {0, 1}, "STAT": {0, 1},
	"NEXT": {0, 0}, "LAST": {0, 0}, "OVER": {0, 1}, "XOVER": {0, 1}, "NEWGROUPS": {2, 3}, "NEWNEWS": {3, 4}, "DATE": {0, 0},
	"POST": {0, 0}, "QUIT": {0, 0}, "COMPRESS": {1, 1},
}

// dispatch answers one command; POST and QUIT are flagged for serve.
func (h *storeConn) dispatch(cmd string, args []string) ([]string, action) {
	n, known := argCount[cmd]
	if !known || h.disabled(cmd) {
		return []string{"500 Unknown command"}, actReply
	}
	if !h.authed && !authFree[cmd] {
		return []string{"480 Authentication required"}, actReply
	}
	if len(args) < n[0] || (n[1] >= 0 && len(args) > n[1]) {
		return []string{"501 Syntax error"}, actReply
	}
	switch cmd {
	case "CAPABILITIES":
		return h.capabilities(), actReply
	case "MODE":
		if !strings.EqualFold(args[0], "READER") {
			return []string{"501 Unknown MODE variant"}, actReply
		}
		if h.o.ReaderOnly {
			return []string{"201 Reader mode, posting prohibited"}, actReply
		}
		return []string{"200 Reader mode, posting permitted"}, actReply
	case "HELP":
		return h.help(), actReply
	case "AUTHINFO":
		return h.authinfo(args), actReply
	case "LIST":
		return h.list(args), actReply
	case "GROUP":
		return h.groupCmd(args[0]), actReply
	case "LISTGROUP":
		return h.listgroup(args), actReply
	case "ARTICLE", "HEAD", "BODY", "STAT":
		return h.article(cmd, args), actReply
	case "NEXT", "LAST":
		return h.nextLast(cmd), actReply
	case "OVER", "XOVER":
		return h.over(args), actReply
	case "NEWGROUPS":
		return h.newgroups(args), actReply
	case "NEWNEWS":
		return h.newnews(args), actReply
	case "DATE":
		return []string{"111 " + time.Now().UTC().Format("20060102150405")}, actReply
	case "COMPRESS":
		return []string{compressReply(args, h.c.Compressed())}, actReply
	case "POST":
		if h.o.ReaderOnly {
			return []string{"440 Posting not permitted"}, actReply
		}
		return nil, actPost
	default: // QUIT
		return []string{"205 Connection closing"}, actQuit
	}
}

// disabled reports whether a known command is made unknown by NoOver, NoXover or NoNewnews,
// or is COMPRESS without Compress or CompressOutputOnly.
func (h *storeConn) disabled(cmd string) bool {
	return (cmd == "OVER" && h.o.NoOver) || (cmd == "XOVER" && h.o.NoXover) || (cmd == "NEWNEWS" && h.o.NoNewnews) ||
		(cmd == "COMPRESS" && !h.o.Compress && !h.o.CompressOutputOnly)
}

func (h *storeConn) capabilities() []string {
	out := []string{"101 Capability list:", "VERSION 2", "READER", "LIST ACTIVE NEWSGROUPS OVERVIEW.FMT"}
	if !h.o.NoNewnews {
		out = append(out, "NEWNEWS")
	}
	if !h.o.NoOver {
		out = append(out, "OVER MSGID")
	}
	if !h.o.ReaderOnly {
		out = append(out, "POST")
	}
	if !h.authed {
		out = append(out, "AUTHINFO USER")
	}
	if h.authed && !h.disabled("COMPRESS") && !h.c.Compressed() {
		out = append(out, "COMPRESS DEFLATE")
	}
	return append(out, "IMPLEMENTATION fakeserver", ".")
}

func (h *storeConn) help() []string {
	out := []string{"100 Help text follows"}
	for cmd := range argCount {
		if !h.disabled(cmd) {
			out = append(out, cmd)
		}
	}
	slices.Sort(out[1:])
	return append(out, ".")
}

func (h *storeConn) authinfo(args []string) []string {
	if h.authed {
		return []string{"502 Command unavailable"}
	}
	val := strings.Join(args[1:], " ")
	switch strings.ToUpper(args[0]) {
	case "USER":
		if h.o.Pass == "" { // username-only authentication (RFC 4643 2.3.2)
			if val == h.o.User {
				h.authed = true
				return []string{"281 Authentication accepted"}
			}
			return []string{"481 Authentication failed"}
		}
		h.user, h.userSeen = val, true
		return []string{"381 Password required"}
	case "PASS":
		if !h.userSeen {
			return []string{"482 Authentication commands issued out of sequence"}
		}
		h.userSeen = false
		if h.user == h.o.User && val == h.o.Pass {
			h.authed = true
			return []string{"281 Authentication accepted"}
		}
		return []string{"481 Authentication failed"}
	default:
		return []string{"501 Unknown AUTHINFO variant"}
	}
}

func (h *storeConn) postingFlag(g Group) string {
	if g.Posting && !h.o.ReaderOnly {
		return "y"
	}
	return "n"
}

func (h *storeConn) activeLine(g Group) string {
	g = h.reported(g)
	return fmt.Sprintf("%s %d %d %s", g.Name, g.High, g.Low, h.postingFlag(g))
}

// reported returns g with the numbers this connection reports (StoreOptions.GroupNumbers).
func (h *storeConn) reported(g Group) Group {
	if h.o.GroupNumbers != nil {
		return h.o.GroupNumbers(h.c.ID(), g)
	}
	return g
}

func (h *storeConn) list(args []string) []string {
	kw, pattern := "ACTIVE", ""
	if len(args) > 0 {
		kw = strings.ToUpper(args[0])
	}
	if len(args) > 1 {
		pattern = args[1]
	}
	switch kw {
	case "ACTIVE":
		out := []string{"215 List of newsgroups follows"}
		for _, g := range h.st.Groups() {
			if pattern == "" || wildmat(pattern, g.Name) {
				out = append(out, h.activeLine(g))
			}
		}
		return append(out, ".")
	case "NEWSGROUPS":
		out := []string{"215 List of newsgroup descriptions follows"}
		for _, g := range h.st.Groups() {
			if pattern == "" || wildmat(pattern, g.Name) {
				out = append(out, g.Name+"\t"+g.Description)
			}
		}
		return append(out, ".")
	case "OVERVIEW.FMT":
		if pattern != "" {
			return []string{"501 Syntax error"}
		}
		fields := h.o.OverviewFmt
		if len(fields) == 0 {
			fields = defaultOverviewFmt
		}
		out := append([]string{"215 Order of fields in overview database"}, fields...)
		return append(out, ".")
	default:
		return []string{"501 Unknown LIST keyword"}
	}
}

// selectGroup makes name the selected group (current article = low, none when empty).
func (h *storeConn) selectGroup(name string) (Group, bool) {
	g, ok := h.st.Group(name)
	if !ok {
		return Group{}, false
	}
	h.group, h.cur = name, 0
	if g.Count > 0 {
		h.cur = g.Low
	}
	return g, true
}

func groupLine(g Group) string {
	return fmt.Sprintf("211 %d %d %d %s", g.Count, g.Low, g.High, g.Name)
}

func (h *storeConn) groupCmd(name string) []string {
	g, ok := h.selectGroup(name)
	if !ok {
		return []string{"411 No such newsgroup"}
	}
	return []string{groupLine(h.reported(g))}
}

func (h *storeConn) listgroup(args []string) []string {
	name := h.group
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		return []string{"412 No newsgroup selected"}
	}
	r := numRange{from: 1, open: true}
	if len(args) > 1 && h.o.ListgroupRange == "reject" {
		return []string{"501 Syntax error"}
	}
	if len(args) > 1 && h.o.ListgroupRange != "ignore" {
		var ok bool
		if r, ok = parseRange(args[1]); !ok {
			return []string{"501 Syntax error in range"}
		}
	}
	g, ok := h.selectGroup(name)
	if !ok {
		return []string{"411 No such newsgroup"}
	}
	from, to := max(r.from, g.Low), g.High
	if !r.open {
		to = min(r.to, g.High)
	}
	if lim := h.st.GenLimit(name); lim > 0 && to-from+1 > lim {
		return []string{genTooLarge(to-from+1, lim)}
	}
	out := []string{groupLine(h.reported(g)) + " list follows"}
	if from <= to {
		for _, n := range h.st.NumbersIn(name, from, to) {
			out = append(out, strconv.FormatInt(n, 10))
		}
	}
	return append(out, ".")
}

// article answers ARTICLE, HEAD, BODY and STAT.
func (h *storeConn) article(cmd string, args []string) []string {
	var a Article
	var num int64
	var group string
	switch {
	case len(args) == 1 && strings.HasPrefix(args[0], "<"):
		if !strings.HasSuffix(args[0], ">") {
			return []string{"501 Syntax error in message-id"}
		}
		var locs []Location
		var ok bool
		if a, locs, ok = h.st.ByMessageID(args[0]); !ok {
			return []string{"430 No article with that message-id"}
		}
		group = locs[0].Group
		for _, l := range locs {
			if h.group != "" && l.Group == h.group {
				num = l.Number // the number in the selected group; else 0
				break
			}
		}
	default:
		if h.group == "" {
			return []string{"412 No newsgroup selected"}
		}
		n := h.cur
		if len(args) == 1 {
			var ok bool
			if n, ok = parseNum(args[0]); !ok {
				return []string{"501 Syntax error in article number"}
			}
		} else if n == 0 {
			return []string{"420 Current article number is invalid"}
		}
		var ok bool
		if a, ok = h.st.Article(h.group, n); !ok {
			if len(args) == 0 {
				return []string{"420 Current article number is invalid"}
			}
			return []string{"423 No article with that number"}
		}
		h.cur, num, group = n, n, h.group
	}
	a.Number = num
	a, _ = h.served(a, group)
	id := a.MessageID
	switch cmd {
	case "STAT":
		return []string{fmt.Sprintf("223 %d %s Article exists", num, id)}
	case "HEAD":
		out := []string{fmt.Sprintf("221 %d %s Headers follow", num, id)}
		out = appendStuffed(out, a.Headers)
		return append(out, ".")
	case "BODY":
		out := []string{fmt.Sprintf("222 %d %s Body follows", num, id)}
		out = appendStuffed(out, a.Body)
		return append(out, ".")
	default:
		out := []string{fmt.Sprintf("220 %d %s Article follows", num, id)}
		out = appendStuffed(out, a.Headers)
		out = append(out, "")
		out = appendStuffed(out, a.Body)
		return append(out, ".")
	}
}

func (h *storeConn) nextLast(cmd string) []string {
	if h.group == "" {
		return []string{"412 No newsgroup selected"}
	}
	if h.cur == 0 {
		return []string{"420 Current article number is invalid"}
	}
	var n int64
	var ok bool
	if cmd == "NEXT" {
		if n, ok = h.st.Next(h.group, h.cur); !ok {
			return []string{"421 No next article in this group"}
		}
	} else if n, ok = h.st.Last(h.group, h.cur); !ok {
		return []string{"422 No previous article in this group"}
	}
	a, ok := h.st.Article(h.group, n)
	if !ok {
		return []string{"420 Current article number is invalid"}
	}
	h.cur = n
	a, _ = h.served(a, h.group)
	return []string{fmt.Sprintf("223 %d %s Article found", n, a.MessageID)}
}

// over answers OVER and XOVER.
func (h *storeConn) over(args []string) []string {
	const head = "224 Overview information follows"
	if len(args) == 1 && strings.HasPrefix(args[0], "<") {
		a, locs, ok := h.st.ByMessageID(args[0])
		if !ok {
			return []string{"430 No article with that message-id"}
		}
		return []string{head, h.overviewLine(a, 0, locs[0].Group), "."}
	}
	if h.group == "" {
		return []string{"412 No newsgroup selected"}
	}
	var arts []Article
	if len(args) == 0 {
		a, ok := h.st.Article(h.group, h.cur)
		if h.cur == 0 || !ok {
			return []string{"420 Current article number is invalid"}
		}
		arts = []Article{a}
	} else {
		r, ok := parseRange(args[0])
		if !ok {
			return []string{"501 Syntax error in range"}
		}
		if r.open {
			g, _ := h.st.Group(h.group)
			r.to, r.open = max(g.High, 0), false
		}
		if lim := h.o.MaxOverRange; lim > 0 && r.to >= r.from && r.to-r.from >= lim {
			if h.o.OverRangeTooLarge != nil {
				return slices.Clone(h.o.OverRangeTooLarge)
			}
			r.to = r.from + lim - 1
		}
		if lim := h.st.GenLimit(h.group); lim > 0 {
			g, _ := h.st.Group(h.group)
			if span := min(r.to, g.High) - max(r.from, g.Low) + 1; span > lim {
				return []string{genTooLarge(span, lim)}
			}
		}
		if r.to >= r.from && r.to > 0 {
			arts = h.st.Overview(h.group, r.from, r.to)
		}
		if len(arts) == 0 {
			if h.o.EmptyOverReply != nil {
				return slices.Clone(h.o.EmptyOverReply)
			}
			return []string{"423 No articles in that range"}
		}
	}
	out := []string{head}
	for _, a := range arts {
		out = append(out, h.overviewLine(a, a.Number, h.group))
	}
	return append(out, ".")
}

// overviewLine formats one OVER line of a with article number num.
func (h *storeConn) overviewLine(a Article, num int64, group string) string {
	a, xref := h.served(a, group)
	var size int
	for _, l := range a.Headers {
		size += len(l) + 2
	}
	size += 2 // blank line
	for _, l := range a.Body {
		size += len(l) + 2
	}
	clean := strings.NewReplacer("\t", " ", "\r", " ", "\n", " ", "\x00", " ")
	f := []string{strconv.FormatInt(num, 10)}
	for _, name := range []string{"Subject", "From", "Date", "Message-ID", "References"} {
		f = append(f, clean.Replace(a.Header(name)))
	}
	f = append(f, strconv.Itoa(size), strconv.Itoa(len(a.Body)), "Xref: "+clean.Replace(xref))
	if h.o.OverviewFields != nil {
		f = h.o.OverviewFields(h.c.ID(), group, f)
	}
	return strings.Join(f, "\t")
}

// genTooLarge is the reply to an OVER, XOVER or LISTGROUP that spans more numbers of a
// generated group than its Generator.Limit.
func genTooLarge(span, lim int64) string {
	return fmt.Sprintf("503 fakeserver: request spans %d numbers of a generated group, over its limit of %d", span, lim)
}

// served returns a with the default headers it is served with (Newsgroups = every group it is
// filed in, group when it is not indexed) and its Xref value (the stored Xref header, else
// generated over every location).
func (h *storeConn) served(a Article, group string) (Article, string) {
	var groups, xref []string
	if _, locs, ok := h.st.ByMessageID(a.MessageID); ok {
		for _, l := range locs {
			if !slices.Contains(groups, l.Group) {
				groups = append(groups, l.Group)
			}
			xref = append(xref, fmt.Sprintf("%s:%d", l.Group, l.Number))
		}
	}
	if len(groups) == 0 {
		groups = []string{group}
		xref = []string{fmt.Sprintf("%s:%d", group, a.Number)}
	}
	a = a.WithDefaults(strings.Join(groups, ","))
	if x := a.Header("Xref"); x != "" {
		return a, x
	}
	return a, xrefHost + " " + strings.Join(xref, " ")
}

func (h *storeConn) newgroups(args []string) []string {
	if len(args) == 3 && !strings.EqualFold(args[2], "GMT") {
		return []string{"501 Syntax error"}
	}
	since, ok := parseNewgroupsTime(args[0], args[1], time.Now().UTC())
	if !ok {
		return []string{"501 Syntax error in date or time"}
	}
	out := []string{"231 List of new newsgroups follows"}
	for _, g := range h.st.Groups() {
		if !g.Created.Before(since) {
			out = append(out, h.activeLine(g))
		}
	}
	return append(out, ".")
}

func (h *storeConn) newnews(args []string) []string {
	if len(args) == 4 && !strings.EqualFold(args[3], "GMT") {
		return []string{"501 Syntax error"}
	}
	since, ok := parseNewgroupsTime(args[1], args[2], time.Now().UTC())
	if !ok {
		return []string{"501 Syntax error in date or time"}
	}
	out := []string{"230 List of new articles follows"}
	seen := make(map[string]bool)
	for _, g := range h.st.Groups() {
		if !wildmat(args[0], g.Name) {
			continue
		}
		if lim := h.st.GenLimit(g.Name); lim > 0 && g.High-g.Low+1 > lim {
			return []string{genTooLarge(g.High-g.Low+1, lim)}
		}
		for _, a := range h.st.Overview(g.Name, g.Low, g.High) {
			if !a.Arrived.Before(since) && !seen[a.MessageID] {
				seen[a.MessageID] = true
				out = append(out, a.MessageID)
			}
		}
	}
	return append(out, ".")
}

// post runs the POST exchange after a 340-able POST: it writes the first reply through the
// Faults, reads and files the article and writes 240 or 441. False ends the connection.
func (h *storeConn) post(line string) bool {
	if !h.c.writeReply(line, "340 Send article to be posted") {
		return false
	}
	if ov, ok := h.c.override(line); ok && (len(ov) == 0 || !strings.HasPrefix(ov[0], "340")) {
		return true // the overridden first reply was final: no article follows
	}
	wire, err := h.c.ReadDotBlock()
	if err != nil {
		return false
	}
	return h.c.WriteLines(h.file(wire)) == nil
}

// file stores a posted article (raw wire lines) and returns the final POST reply.
func (h *storeConn) file(wire []string) string {
	var a Article
	inBody := false
	for _, l := range wire {
		if strings.HasPrefix(l, ".") {
			l = l[1:]
		}
		switch {
		case inBody:
			a.Body = append(a.Body, l)
		case l == "":
			inBody = true
		default:
			a.Headers = append(a.Headers, l)
		}
	}
	for _, name := range []string{"Newsgroups", "Subject", "From"} {
		if a.Header(name) == "" {
			return "441 Posting failed: missing " + name + " header"
		}
	}
	var groups []string
	for _, g := range strings.Split(a.Header("Newsgroups"), ",") {
		if g = strings.TrimSpace(g); g != "" && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return "441 Posting failed: no newsgroup"
	}
	h.postMu.Lock()
	defer h.postMu.Unlock()
	switch id := a.Header("Message-ID"); {
	case a.hasHeader("Message-ID") && !validMessageID(id):
		return "441 Posting failed: bad Message-ID header"
	case id != "":
		if _, _, dup := h.st.ByMessageID(id); dup {
			return "441 Posting failed: duplicate " + id
		}
		a.MessageID = id
	default:
		for {
			a.MessageID = fmt.Sprintf("<posted-%d@fakeserver.invalid>", h.seq.Add(1))
			if _, _, taken := h.st.ByMessageID(a.MessageID); !taken {
				break
			}
		}
		a.Headers = append(a.Headers, "Message-ID: "+a.MessageID)
	}
	if d := a.Header("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			a.Date = t
		}
	}
	a.Arrived = time.Now()
	if a.Date.IsZero() {
		a.Date = a.Arrived
	}
	h.st.AddPosted(a)
	for _, g := range groups {
		h.st.AddArticle(g, a)
	}
	return "240 Article received " + a.MessageID
}

// validMessageID reports whether id has the form "<...>" without white space.
func validMessageID(id string) bool {
	return len(id) > 2 && strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") && !strings.ContainsAny(id, " \t\r\n")
}

// appendStuffed appends lines to out, dot-stuffing those that start with ".".
func appendStuffed(out, lines []string) []string {
	for _, l := range lines {
		if strings.HasPrefix(l, ".") {
			l = "." + l
		}
		out = append(out, l)
	}
	return out
}

// numRange is an article range: from..to, or from.. when open.
type numRange struct {
	from, to int64
	open     bool
}

func (r numRange) contains(n int64) bool { return n >= r.from && (r.open || n <= r.to) }

// parseRange parses "n", "n-" and "n-m".
func parseRange(s string) (numRange, bool) {
	lo, hi, dash := strings.Cut(s, "-")
	from, ok := parseNum(lo)
	if !ok {
		return numRange{}, false
	}
	if !dash {
		return numRange{from: from, to: from}, true
	}
	if hi == "" {
		return numRange{from: from, open: true}, true
	}
	to, ok := parseNum(hi)
	if !ok {
		return numRange{}, false
	}
	return numRange{from: from, to: to}, true
}

// parseNum parses an article number: 1 to 16 ASCII digits.
func parseNum(s string) (int64, bool) {
	if s == "" || len(s) > 16 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// parseNewgroupsTime parses the NEWGROUPS and NEWNEWS date (yymmdd or yyyymmdd) and time
// (hhmmss) as UTC. A two-digit year is in the century of now when it is not after now's year,
// else in the one before (RFC 3977 7.3.2).
func parseNewgroupsTime(date, clock string, now time.Time) (time.Time, bool) {
	if _, ok := parseNum(date); !ok || (len(date) != 6 && len(date) != 8) {
		return time.Time{}, false
	}
	if _, ok := parseNum(clock); !ok || len(clock) != 6 {
		return time.Time{}, false
	}
	if len(date) == 6 {
		yy, _ := strconv.Atoi(date[:2])
		century := now.Year() / 100 * 100
		if yy > now.Year()%100 {
			century -= 100
		}
		date = strconv.Itoa(century+yy) + date[2:]
	}
	t, err := time.ParseInLocation("20060102150405", date+clock, time.UTC)
	return t, err == nil
}

// wildmat reports whether name matches the RFC 3977 wildmat pattern: comma-separated patterns
// with "*" and "?", a leading "!" negates; the last matching pattern decides. Character classes
// are not supported ("[" matches itself).
func wildmat(pattern, name string) bool {
	match := false
	for _, p := range strings.Split(pattern, ",") {
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		if glob(p, name) {
			match = !neg
		}
	}
	return match
}

// glob matches name against p with "*" (any run) and "?" (one byte).
func glob(p, name string) bool {
	px, nx := 0, 0
	star, mark := -1, 0
	for nx < len(name) {
		switch {
		case px < len(p) && (p[px] == '?' || p[px] == name[nx]):
			px++
			nx++
		case px < len(p) && p[px] == '*':
			star, mark = px, nx
			px++
		case star >= 0:
			px = star + 1
			mark++
			nx = mark
		default:
			return false
		}
	}
	for px < len(p) && p[px] == '*' {
		px++
	}
	return px == len(p)
}
