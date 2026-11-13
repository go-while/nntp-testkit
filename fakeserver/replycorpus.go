package fakeserver

import "strings"

// Reply corpus: a named list of wrong and malformed server replies for the test suite of an
// NNTP CLIENT. Every client project needs the same table (GROUP answered 215, a 211 without
// numbers, a greeting of "hello there", an overview row with a letter for the article number),
// so it lives here once. A case says which command it breaks, the reply lines to write
// instead, and what is wrong with them; Apply puts it into the Options of a fake server. A
// suite loops over the corpus and checks its client against each case:
//
//	for _, rc := range fakeserver.ReplyCorpus() {
//		t.Run(rc.Name, func(t *testing.T) {
//			o := fakeserver.Options{Store: fakeserver.NewStore()}
//			rc.Apply(&o)
//			srv := fakeserver.New(t, o)
//			// connect a client to srv.Addr(); for a greeting case connecting is the test,
//			// else send rc.Line (or any line of command rc.Cmd) and look at the outcome
//		})
//	}
//
// The corpus states the cases, not the verdicts of one client: rc.Want is a generic class
// (WantError: the call must fail cleanly; WantEither: a strict client fails and a lenient one
// may accept, but neither may panic, hang or desynchronise; WantAccept: odd but legal, the
// client must accept it), and the details (error types,
// whether the connection is reusable afterwards) are for each client suite to agree on.
//
// The cases are facts about NNTP replies (RFC 3977 and friends), written from scratch; the
// families are:
//
//	FamilyCode      a well-formed reply with the wrong status code or the wrong shape (a
//	                block where one line is due, or the other way round) for the command
//	FamilyStatus    a malformed status line: no code, a code of 2 or 4 digits, missing,
//	                non-numeric, negative or overflowing fields in 211 and 223, a bad DATE,
//	                8-bit or NUL text, an overlong line
//	FamilyGreeting  a bad greeting: 400, 502, no code, empty, a stray second line
//	FamilyBlock     a multi-line reply whose rows are bad (OVER, HDR, LIST ACTIVE, LISTGROUP,
//	                NEWNEWS, CAPABILITIES, article and body lines), or whose terminator
//	                is wrong
//
// (An empty greeting line is not a case: an empty Options.Greeting means the default.)
// A missing "." terminator is Faults.Endless, an overlong block line Faults.LongLine, a reply
// cut short Faults.DropInMultiline: not repeated here. A reply that depends on an earlier
// answer of the same exchange (POST answered 340 and then, after the article, 340 again) needs
// a Script or a Handler, not an Override, and is not in the corpus. A case with Final set
// replaces the reply that follows the article of POST or IHAVE (240/441, 235/437) instead: the
// first reply is the normal 340 or 335, and the test sends an article before it looks.

// Family groups the cases of the reply corpus.
type Family string

const (
	FamilyCode     Family = "code"
	FamilyStatus   Family = "status"
	FamilyGreeting Family = "greeting"
	FamilyBlock    Family = "block"
)

// Outcome is the generic verdict a client suite may expect of a case.
type Outcome string

const (
	// WantError: the client call fails with an error. It must not panic, hang (beyond its own
	// timeout when NeedsTimeout) or hand the data back as if it were good. Afterwards the
	// connection is either in sync (the next command works) or closed.
	WantError Outcome = "error"
	// WantEither: the reply is wrong or odd but a client may reasonably tolerate it. A strict
	// client reports an error, a lenient one accepts and may keep the odd bytes; neither may
	// panic, hang, or leave the connection out of sync without closing it.
	WantEither Outcome = "either"
	// WantAccept: the reply is odd but legal; a client must accept it and hand the data back
	// (high below low for an empty group, extra overview fields, 281 to AUTHINFO USER). It
	// guards against clients that are stricter than the RFC.
	WantAccept Outcome = "accept"
)

// ReplyCase is one wrong or malformed reply.
type ReplyCase struct {
	// Name is a unique short identifier fit for t.Run, e.g. "group-211-nofields".
	Name   string
	Family Family
	// Cmd is the Faults key (upper-cased command, "GROUP") whose reply is replaced. Empty
	// for a greeting case.
	Cmd string
	// Line is a sample client line that the case suits ("GROUP g", "ARTICLE 1"). Apply
	// replaces the reply of every line of command Cmd; ApplyLine only that of Line. Empty
	// for a greeting case.
	Line string
	// Lines are written instead of the normal reply, each followed by CRLF, so a block
	// carries its own "." terminator line. A line may hold bytes that are no valid NNTP
	// (a NUL, 8-bit, a bare LF): they are written as they are. For a greeting case they
	// are the greeting.
	Lines []string
	// Why says what is wrong, with the RFC where there is one.
	Why string
	// Want is the generic expected client outcome.
	Want Outcome
	// NeedsTimeout is set when the client waits for more data than the reply holds (a
	// terminator that never comes): the test needs a read timeout in the client, and the
	// call ends with a timeout error.
	NeedsTimeout bool
	// Final is set when Lines replace the reply that follows the article of POST or IHAVE
	// (240/441, 235/437; Faults.OverrideFinal), not the first reply: the client gets its
	// normal 340 or 335, sends an article and then reads the case. ApplyLine treats it like
	// Apply, since OverrideFinal is keyed by the command only.
	Final bool
}

// IsGreeting reports whether the case is a bad greeting (the reply to connecting, not to a
// command).
func (rc ReplyCase) IsGreeting() bool { return rc.Family == FamilyGreeting }

// Bytes returns exactly what the server writes for the case: every line followed by CRLF.
func (rc ReplyCase) Bytes() []byte { return joinCRLF(rc.Lines) }

// Apply makes the server of o write the case: Faults.Override[Cmd] for a command case (every
// line of that command gets the reply), Faults.OverrideFinal[Cmd] for a Final case,
// Options.Greeting for a greeting case. It adds to the
// maps already in o and keeps the other faults. The server still needs a mode that applies
// Faults: a Store (Options.Store) for a command case; a greeting case works in any mode that
// writes Options.Greeting.
func (rc ReplyCase) Apply(o *Options) {
	if rc.IsGreeting() {
		o.Greeting = strings.Join(rc.Lines, "\r\n")
		return
	}
	if rc.Final {
		if o.Faults.OverrideFinal == nil {
			o.Faults.OverrideFinal = make(map[string][]string)
		}
		o.Faults.OverrideFinal[rc.Cmd] = append([]string(nil), rc.Lines...)
		return
	}
	if o.Faults.Override == nil {
		o.Faults.Override = make(map[string][]string)
	}
	o.Faults.Override[rc.Cmd] = append([]string(nil), rc.Lines...)
}

// ApplyLine is Apply for the sample client line only (Faults.OverrideLine[Line]): other lines
// of the same command get their normal reply.
func (rc ReplyCase) ApplyLine(o *Options) {
	if rc.IsGreeting() || rc.Final {
		rc.Apply(o)
		return
	}
	if o.Faults.OverrideLine == nil {
		o.Faults.OverrideLine = make(map[string][]string)
	}
	o.Faults.OverrideLine[lineKey(rc.Line)] = append([]string(nil), rc.Lines...)
}

// ReplyCaseByName returns the case of that name.
func ReplyCaseByName(name string) (ReplyCase, bool) {
	for _, rc := range ReplyCorpus() {
		if rc.Name == name {
			return rc, true
		}
	}
	return ReplyCase{}, false
}

// rcBlock is a multi-line reply: the status line, the rows and the "." terminator.
func rcBlock(status string, rows ...string) []string {
	return append(append([]string{status}, rows...), ".")
}

func rcCmd(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return strings.ToUpper(f[0])
}

func rcCase(fam Family, name, line string, want Outcome, why string, lines ...string) ReplyCase {
	return ReplyCase{Name: name, Family: fam, Cmd: rcCmd(line), Line: line, Lines: lines, Why: why, Want: want}
}

func rcGreeting(name string, want Outcome, why string, lines ...string) ReplyCase {
	return ReplyCase{Name: name, Family: FamilyGreeting, Lines: lines, Why: why, Want: want}
}

func rcFinal(rc ReplyCase) ReplyCase {
	rc.Final = true
	return rc
}

func rcTimeout(rc ReplyCase) ReplyCase {
	rc.NeedsTimeout = true
	return rc
}

// ReplyCorpus returns the wrong and malformed replies, in a fixed order, a new slice each call.
// A client suite loops (see the example at the top of this file).
func ReplyCorpus() []ReplyCase {
	const (
		c = FamilyCode
		s = FamilyStatus
		b = FamilyBlock
	)
	art := []string{"Subject: a", "", "body"}
	ovHead := "1\tsubj\ta@b\tMon, 01 Jan 2024 00:00:00 +0000\t<1@x>\t\t10\t1"
	long := strings.Repeat("x", 64*1024)
	return []ReplyCase{
		// code: wrong status code or wrong shape for the command
		rcCase(c, "group-200", "GROUP g", WantError, "GROUP answers 211 or 411 (RFC 3977 6.1.1); 200 is the MODE READER code", "200 ok"),
		rcCase(c, "group-215-block", "GROUP g", WantError, "a LIST code and a block where GROUP has one line (6.1.1)", rcBlock("215 list follows", "g 2 1 y")...),
		rcCase(c, "group-100-block", "GROUP g", WantError, "an informational 1xx (help) block as the reply to GROUP", rcBlock("100 help text", "line one")...),
		rcCase(c, "article-211", "ARTICLE 1", WantError, "a GROUP reply to ARTICLE (6.2.1 allows 220, 412, 420, 423, 430)", "211 1 1 1 g"),
		rcCase(c, "article-222-block", "ARTICLE 1", WantError, "222 is the BODY code; ARTICLE answers 220 (6.2.1)", rcBlock("222 1 <a@b>", "x", ".y")...),
		rcCase(c, "article-223", "ARTICLE 1", WantError, "223 is the STAT code, one line; ARTICLE answers 220 and a block (6.2.1)", "223 1 <a@b>"),
		rcCase(c, "article-430-block", "ARTICLE 1", WantEither, "an error code followed by a block that no error reply has (3.2.1: 4xx are one line)", rcBlock("430 no such article", "stray")...),
		rcCase(c, "body-220-block", "BODY 1", WantError, "220 is the ARTICLE code; BODY answers 222 (6.2.3)", rcBlock("220 1 <a@b>", art...)...),
		rcCase(c, "body-221-block", "BODY 1", WantError, "221 is the HEAD code; BODY answers 222 (6.2.3)", rcBlock("221 1 <a@b>", "Subject: a")...),
		rcCase(c, "body-199", "BODY 1", WantError, "a status code (199) outside every defined range", "199 hm"),
		rcCase(c, "body-381", "BODY 1", WantError, "a continuation code (381, password wanted) as the reply to BODY", "381 more"),
		rcCase(c, "head-222-block", "HEAD 1", WantError, "222 is the BODY code; HEAD answers 221 (6.2.2)", rcBlock("222 1 <a@b>", "x", ".y")...),
		rcCase(c, "head-223", "HEAD 1", WantError, "223 is the STAT code, one line; HEAD answers 221 and a block (6.2.2)", "223 1 <a@b>"),
		rcCase(c, "stat-220-block", "STAT 1", WantError, "a 220 article block where STAT answers one 223 line (6.2.4)", rcBlock("220 1 <a@b>", art...)...),
		rcCase(c, "hdr-221-block", "HDR Subject 1", WantError, "221 is the HEAD code; HDR answers 225 (8.5)", rcBlock("221 hdr", "1 a")...),
		rcCase(c, "xhdr-225-block", "XHDR Subject 1", WantEither, "XHDR (RFC 2980) answers 221; 225 is the HDR code of RFC 3977 8.5, which some servers use for both", rcBlock("225 hdr", "1 a")...),
		rcCase(c, "over-225-block", "OVER 1-2", WantError, "225 is the HDR code; OVER answers 224 (8.3)", rcBlock("225 over", ovHead)...),
		rcCase(c, "over-224-no-block", "OVER 1-2", WantError, "224 without the block of rows it announces (8.3)", "224 overview follows"),
		rcCase(c, "list-231-block", "LIST ACTIVE", WantError, "231 is the NEWGROUPS code; LIST answers 215 (7.6.1)", rcBlock("231 list", "a 1 1 y")...),
		rcCase(c, "list-200", "LIST ACTIVE", WantError, "200 and one line where LIST answers 215 and a block (7.6.1)", "200 ok"),
		rcCase(c, "list-282-block", "LIST ACTIVE", WantError, "282 is the old XGTITLE code, not a LIST code", rcBlock("282 list", "a 1 1 y")...),
		rcCase(c, "listgroup-215-block", "LISTGROUP g", WantError, "215 is the LIST code; LISTGROUP answers 211 and a block (6.1.2)", rcBlock("215 list", "1")...),
		rcTimeout(rcCase(c, "listgroup-211-no-block", "LISTGROUP g", WantError, "211 without the block of article numbers it announces (6.1.2): the client waits for the rows", "211 2 1 2 g")),
		rcCase(c, "capabilities-215-block", "CAPABILITIES", WantError, "215 is the LIST code; CAPABILITIES answers 101 (5.2)", rcBlock("215 caps", "VERSION 2")...),
		rcCase(c, "help-101-block", "HELP", WantError, "101 is the CAPABILITIES code; HELP answers 100 (7.2)", rcBlock("101 help", "VERSION 2")...),
		rcCase(c, "newnews-231-block", "NEWNEWS * 20200101 000000", WantError, "231 is the NEWGROUPS code; NEWNEWS answers 230 (7.4)", rcBlock("231 ids", "<a@b>")...),
		rcCase(c, "date-211", "DATE", WantError, "a GROUP reply to DATE, which answers 111 (7.1)", "211 1 1 1 g"),
		rcCase(c, "mode-reader-202", "MODE READER", WantError, "202 is no MODE READER code (5.3 has 200, 201, 502)", "202 hm"),
		rcCase(c, "compress-200", "COMPRESS DEFLATE", WantError, "COMPRESS answers 206 or an error (RFC 8054 2.2); 200 is not it", "200 hm"),
		rcCase(c, "xfeature-200", "XFEATURE COMPRESS GZIP", WantError, "XFEATURE COMPRESS GZIP answers 290; 200 is not it", "200 hm"),
		rcCase(c, "post-240-first", "POST", WantError, "POST first answers 340 (send) or 440; 240 is the final code that follows the article (6.3.1)", "240 ok"),
		rcCase(c, "post-335-first", "POST", WantError, "335 is the IHAVE send code; POST answers 340 (6.3.1)", "335 ok"),
		rcCase(c, "ihave-340-first", "IHAVE <i@x>", WantError, "340 is the POST send code; IHAVE answers 335 (6.3.2)", "340 ok"),
		rcCase(c, "quit-200", "QUIT", WantError, "QUIT answers 205 (5.4)", "200 hm"),
		rcCase(c, "authinfo-user-250", "AUTHINFO USER u", WantError, "AUTHINFO USER answers 381, 281 or 481 (RFC 4643 2.3)", "250 hm"),
		rcCase(c, "authinfo-user-215-block", "AUTHINFO USER u", WantError, "a 215 block where AUTHINFO USER answers one line (RFC 4643 2.3)", rcBlock("215 what", "111 19990101000000")...),
		rcFinal(rcCase(c, "post-240-address-in-text", "POST", WantAccept, "the 240 text is free (6.3.1) and holds no message-id; an address in angle brackets there is not the Message-ID of the article", "240 posted ok; report abuse to <abuse@example.net>")),
		rcFinal(rcCase(c, "post-240-empty-brackets", "POST", WantAccept, "the 240 text is free (6.3.1); '<>' in it is no message-id (RFC 5536 3.1.3 needs content between the brackets)", "240 <> posted")),
		rcCase(c, "authinfo-user-281", "AUTHINFO USER u", WantAccept, "281 to USER with no PASS asked: legal when the server needs no password (RFC 4643 2.3.1); a client must not send PASS", "281 ok"),

		// status: a malformed status line
		rcCase(s, "status-code-600", "BODY 1", WantError, "a first digit of 6 is no reply class (3.2)", "600 what"),
		rcCase(s, "status-code-000", "DATE", WantError, "a code of 000 is no reply class (3.2)", "000 zero"),
		rcCase(s, "status-code-2-digits", "STAT 1", WantError, "the code has 3 digits (3.1)", "22 1 <a@b>"),
		rcCase(s, "status-code-4-digits", "DATE", WantError, "the code has 3 digits (3.1)", "1111 20200101000000"),
		rcCase(s, "status-code-nondigit", "STAT 1", WantError, "a letter inside the 3-digit code (3.1)", "2x3 1 <a@b>"),
		rcCase(s, "status-no-code", "DATE", WantError, "no numeric code at all (3.1)", "OK 20200101000000"),
		rcCase(s, "status-code-only", "BODY 1", WantEither, "a code without any text after it; the block that follows is well formed", rcBlock("222", "x")...),
		rcCase(s, "status-leading-space", "DATE", WantError, "a space before the code (3.1)", " 111 20200101000000"),
		rcCase(s, "status-no-space", "DATE", WantError, "no space between the code and the text: the code reads as 4 or more digits (3.1)", "11120200101000000"),
		rcCase(s, "status-tab-separator", "DATE", WantEither, "a TAB where the single space after the code belongs (3.1)", "111\t20200101000000"),
		rcCase(s, "status-empty-line", "STAT 1", WantError, "an empty line instead of a status line (3.1)", ""),
		rcCase(s, "status-bare-lf", "DATE", WantEither, "the line ends in a bare LF; an empty CRLF line follows (3.1 wants CRLF)", "111 20200101000000\n"),
		rcCase(s, "status-embedded-cr", "DATE", WantEither, "a CR inside the status text, not before the LF (3.1)", "111 20200101\r000000"),
		rcCase(s, "status-text-nul", "MODE READER", WantEither, "a NUL byte in the status text (3.1: no NUL in a line)", "200 ok\x00 hi"),
		rcCase(s, "status-text-8bit", "MODE READER", WantEither, "8-bit bytes that are not UTF-8 in the status text (3.1 allows UTF-8 only)", "200 caf\xe9 \xff\xfe"),
		rcCase(s, "status-over-512", "MODE READER", WantEither, "a status line of about 600 bytes (3.1: at most 512 octets with the CRLF)", "200 "+strings.Repeat("x", 600)),
		rcCase(s, "status-long-64k", "MODE READER", WantEither, "a status line of 64 KiB: a client with a line limit must fail cleanly, not buffer without end", "200 "+long),
		rcCase(s, "group-211-nofields", "GROUP g", WantError, "211 without count, low, high and group (6.1.1)", "211"),
		rcCase(s, "group-211-two-fields", "GROUP g", WantError, "211 with count and low only (6.1.1)", "211 5 1"),
		rcCase(s, "group-211-no-name", "GROUP g", WantEither, "211 with the three numbers but no group name (6.1.1)", "211 5 1 5"),
		rcCase(s, "group-211-nonnumeric-count", "GROUP g", WantError, "a letter for the article count (6.1.1)", "211 x 1 2 g"),
		rcCase(s, "group-211-nonnumeric-high", "GROUP g", WantError, "a letter for the high water mark (6.1.1)", "211 1 1 y g"),
		rcCase(s, "group-211-float", "GROUP g", WantError, "a decimal fraction for the count (6.1.1)", "211 1.5 1 2 g"),
		rcCase(s, "group-211-negative", "GROUP g", WantError, "negative numbers (6.1.1: numbers are non-negative)", "211 -5 -3 -9 g"),
		rcCase(s, "group-211-overflow", "GROUP g", WantError, "numbers over 64 bits", "211 99999999999999999999999999 1 99999999999999999999999999 g"),
		rcCase(s, "group-211-high-below-low", "GROUP g", WantEither, "high below low with articles reported (6.1.1 allows it only for an empty group)", "211 3 9 1 g"),
		rcCase(s, "group-211-count-over-range", "GROUP g", WantAccept, "a count larger than low..high holds (6.1.1 calls the count an estimate)", "211 100 1 2 g"),
		rcCase(s, "group-211-extra-fields", "GROUP g", WantEither, "text after the group name (6.1.1)", "211 2 1 2 g extra stuff"),
		rcCase(s, "stat-223-no-msgid", "STAT 1", WantError, "223 with the article number but no message-id (6.2.4)", "223 1"),
		rcCase(s, "stat-223-nonnumeric", "STAT 1", WantError, "a word for the article number (6.2.4)", "223 abc <a@b>"),
		rcCase(s, "stat-223-msgid-no-brackets", "STAT 1", WantError, "a message-id without angle brackets (6.2.4, RFC 5536 3.1.3)", "223 1 a@b"),
		rcCase(s, "stat-223-msgid-unclosed", "STAT 1", WantError, "a message-id without the closing bracket (6.2.4)", "223 1 <a@b"),
		rcCase(s, "stat-223-msgid-no-at", "STAT <abc>", WantEither, "a message-id without an at sign (RFC 5536 3.1.3)", "223 0 <abc>"),
		rcCase(s, "date-garbage", "DATE", WantError, "111 with text that is no timestamp (7.1)", "111 garbage"),
		rcCase(s, "date-too-short", "DATE", WantError, "a timestamp of 4 digits where yyyymmddhhmmss has 14 (7.1)", "111 2020"),
		rcCase(s, "date-too-long", "DATE", WantError, "a timestamp of 15 digits (7.1)", "111 202001010000001"),
		rcCase(s, "date-nonnumeric", "DATE", WantError, "a letter inside the 14 digits (7.1)", "111 2020010100000x"),

		// greeting
		rcGreeting("greeting-400", WantError, "400: service temporarily unavailable, a connect that must fail (5.1)", "400 too busy"),
		rcGreeting("greeting-502", WantError, "502: service permanently unavailable, a connect that must fail (5.1)", "502 go away"),
		rcGreeting("greeting-garbage", WantError, "no status code in the greeting (5.1)", "hello there"),
		rcGreeting("greeting-code-only", WantEither, "200 with no text (5.1)", "200"),
		rcGreeting("greeting-211", WantError, "a GROUP reply as the greeting (5.1 allows 200, 201, 400, 502)", "211 1 1 1 g"),
		rcGreeting("greeting-205", WantError, "205 (closing) as the greeting (5.1)", "205 bye"),
		rcGreeting("greeting-bare-lf", WantEither, "the greeting ends in a bare LF; an empty CRLF line follows (3.1)", "200 hi\n"),
		rcGreeting("greeting-nul", WantEither, "a NUL byte in the greeting text (3.1)", "200 hi\x00there"),
		rcGreeting("greeting-8bit", WantEither, "8-bit bytes that are not UTF-8 in the greeting text (3.1)", "200 caf\xe9 \xff"),
		rcGreeting("greeting-two-lines", WantEither, "a stray second line after the greeting: the first command would read it as its reply (desync)", "200 hi", "stray line"),

		// block: a multi-line reply with bad rows
		rcCase(b, "over-nonnumeric-row", "OVER 1-2", WantError, "a word for the article number of an overview row (8.3.2)", rcBlock("224 overview follows", "abc\tx\ta@b\td\t<1@x>\t\t1\t1")...),
		rcCase(b, "over-negative-number", "OVER 1-2", WantError, "a negative article number in an overview row (8.3.2)", rcBlock("224 overview follows", "-1\tsubj\ta@b\td\t<1@x>\t\t1\t1")...),
		rcCase(b, "over-too-few-fields", "OVER 1-2", WantError, "an overview row of 2 fields where the format has 8 (8.3.2)", rcBlock("224 overview follows", "1\tsubj")...),
		rcCase(b, "over-no-tabs", "OVER 1-2", WantError, "an overview row separated by spaces, not TABs (8.3.2)", rcBlock("224 overview follows", "1 subj a@b d <1@x> 1 1")...),
		rcCase(b, "over-extra-fields", "OVER 1-2", WantAccept, "overview rows may carry more fields than the 8 standard ones (8.3.2, 'Xref: ...')", rcBlock("224 overview follows", ovHead+"\tXref: h g:1\textra\tmore")...),
		rcCase(b, "over-nonnumeric-bytes", "OVER 1-2", WantEither, "a word in the :bytes and :lines fields (8.3.2)", rcBlock("224 overview follows", "1\tsubj\ta@b\td\t<1@x>\t\tbytes\tlines")...),
		rcCase(b, "over-msgid-no-brackets", "OVER 1-2", WantEither, "a message-id field without angle brackets (8.3.2)", rcBlock("224 overview follows", "1\tsubj\ta@b\td\tabc\t\t1\t1")...),
		rcCase(b, "over-empty-row", "OVER 1-2", WantEither, "an empty row inside the block (8.3.2)", rcBlock("224 overview follows", "", ovHead)...),
		rcCase(b, "over-non-utf8-subject", "OVER 1-2", WantEither, "bytes that are not UTF-8 in the Subject field; a client keeps them or reports an error", rcBlock("224 overview follows", "1\tsubj \xff\xfe\ta@b\td\t<1@x>\t\t1\t1")...),
		rcCase(b, "hdr-nonnumeric-row", "HDR Subject 1", WantError, "a word for the article number of an HDR row (8.5)", rcBlock("225 hdr", "abc x")...),
		rcCase(b, "hdr-row-no-space", "HDR Subject 1", WantEither, "an HDR row of the number only, no value and no space (8.5)", rcBlock("225 hdr", "1")...),
		rcCase(b, "hdr-msgid-row-for-range", "HDR Subject 1-2", WantEither, "a message-id where the article number belongs: rows of a range request carry numbers, the 0 form is for a message-id argument only (8.5.2); a client that checks the row syntax but not which request it answers may accept it", rcBlock("225 hdr", "<a@b> x")...),
		rcCase(b, "hdr-msgid-unclosed-row", "HDR Subject <a@b>", WantError, "a row that starts with '<' but has no closing '>'; the row for a message-id argument starts with 0 (8.5.2)", rcBlock("225 hdr", "<broken value")...),
		rcCase(b, "hdr-msgid-empty-row", "HDR Subject <a@b>", WantError, "'<>' where the 0 of a message-id argument belongs (8.5.2)", rcBlock("225 hdr", "<> value")...),
		rcCase(b, "hdr-negative-row", "HDR Subject 1-2", WantError, "a negative article number in an HDR row (8.5.2: numbers are digits only)", rcBlock("225 hdr", "-1 x")...),
		rcCase(b, "hdr-plus-sign-row", "HDR Subject 1-2", WantError, "a '+' before the article number (8.5.2: digits only)", rcBlock("225 hdr", "+1 x")...),
		rcCase(b, "hdr-underscore-row", "HDR Subject 1-2", WantError, "'1_0' for the article number: some number parsers (Python int()) accept the underscore (8.5.2: digits only)", rcBlock("225 hdr", "1_0 x")...),
		rcCase(b, "hdr-non-ascii-digit-row", "HDR Subject 1-2", WantError, "U+0661 (Arabic-Indic digit one) for the article number: Unicode-aware parsers read it as 1 (8.5.2: ASCII digits only)", rcBlock("225 hdr", "\u0661 x")...),
		rcCase(b, "list-overview-fmt-empty-name", "LIST OVERVIEW.FMT", WantError, "a field of ':' alone, with no header name (8.4.2)", rcBlock("215 fmt", "Subject:", "From:", "Date:", "Message-ID:", "References:", ":bytes", ":lines", ":")...),
		rcCase(b, "list-overview-fmt-padded-name", "LIST OVERVIEW.FMT", WantEither, "spaces around a field name (8.4.2: a field is a header name and a colon, or a colon and a metadata name); a lenient client trims them; one that keeps ' :full' as the name of the Xref field no longer finds it", rcBlock("215 fmt", "Subject:", "From:", "Date:", "Message-ID:", "References:", ":bytes", ":lines", " :full", "Xref :full")...),
		rcCase(b, "list-active-too-few-fields", "LIST ACTIVE", WantError, "a LIST ACTIVE row without the status field (7.6.3)", rcBlock("215 list", "alt.test 5 1")...),
		rcCase(b, "list-active-nonnumeric", "LIST ACTIVE", WantError, "letters for high and low (7.6.3)", rcBlock("215 list", "alt.test x y y")...),
		rcCase(b, "list-active-empty-group", "LIST ACTIVE", WantAccept, "high below low is how an empty group reads (7.6.3): not an error", rcBlock("215 list", "alt.test 0 1 y")...),
		rcCase(b, "list-active-bad-flag", "LIST ACTIVE", WantEither, "an unknown status flag 'q' (7.6.3 allows y, n, m and others)", rcBlock("215 list", "alt.test 5 1 q")...),
		rcCase(b, "list-active-empty-row", "LIST ACTIVE", WantEither, "an empty row inside the block (7.6.3)", rcBlock("215 list", "", "alt.test 5 1 y")...),
		rcCase(b, "listgroup-invalid-211-line", "LISTGROUP g", WantEither, "a 211 line with letters for the numbers; the rows are fine (6.1.2)", rcBlock("211 x y z g", "1", "2")...),
		rcCase(b, "listgroup-nonnumeric-row", "LISTGROUP g", WantError, "a letter for an article number (6.1.2)", rcBlock("211 2 1 2 g", "1", "x")...),
		rcCase(b, "listgroup-negative-row", "LISTGROUP g", WantError, "a negative article number (6.1.2)", rcBlock("211 2 1 2 g", "1", "-2")...),
		rcCase(b, "newnews-nonmsgid-row", "NEWNEWS * 20200101 000000", WantEither, "a row that is no message-id (7.4)", rcBlock("230 ids", "not-a-msgid", "<a@b>")...),
		rcCase(b, "capabilities-no-version", "CAPABILITIES", WantEither, "no VERSION line first (5.2 requires it)", rcBlock("101 caps", "READER")...),
		rcCase(b, "article-220-nonnumeric", "ARTICLE 1", WantError, "a word for the article number in 220 (6.2.1)", rcBlock("220 x <a@b>", art...)...),
		rcCase(b, "article-bad-header-line", "ARTICLE 1", WantEither, "a header line without a colon (RFC 5322 2.2)", rcBlock("220 1 <a@b>", "Subject: x", "no colon here", "", "body")...),
		rcCase(b, "article-headers-only", "ARTICLE 1", WantEither, "no blank line and no body after the headers (RFC 5536 3)", rcBlock("220 1 <a@b>", "Subject: x")...),
		rcCase(b, "body-nul", "BODY 1", WantEither, "a NUL byte in a body line (RFC 3977 3.1.1 forbids it)", rcBlock("222 1 <a@b>", "a\x00b")...),
		rcCase(b, "body-bare-lf", "BODY 1", WantEither, "a bare LF inside a body line (3.1.1)", rcBlock("222 1 <a@b>", "one\ntwo", "three")...),
		rcCase(b, "body-unstuffed-dot", "BODY 1", WantEither, "a line starting with one dot that the server did not dot-stuff (3.1.1)", rcBlock("222 1 <a@b>", "a", ".hidden", "b")...),
		rcTimeout(rcCase(b, "block-terminator-dot-space", "BODY 1", WantError, "'. ' (a dot and a space) is no terminator (3.1.1): the client waits for the real one", "222 1 <a@b>", "x", ". ")),
	}
}
