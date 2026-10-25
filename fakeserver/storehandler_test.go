package fakeserver

import (
	"bytes"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

var storeDate = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testStore returns a store with alt.test (articles 1-3, 3 crossposted to comp.lang.go),
// alt.empty and comp.lang.go.
func testStore() *Store {
	st := NewStore()
	st.AddGroup("alt.test", "Testing group")
	st.AddGroup("alt.empty", "Nothing here")
	st.AddGroup("comp.lang.go", "Go")
	st.AddArticle("alt.test", Article{Headers: []string{"Subject: one", "From: a@x", "Message-ID: <a1@x>"},
		Body: []string{"line 1", ".dot line", ".."}, Date: storeDate})
	st.AddArticle("alt.test", Article{Headers: []string{"Subject: two", "From: b@x", "Message-ID: <a2@x>", "References: <a1@x>"},
		Body: []string{"second"}, Date: storeDate})
	x3 := Article{Headers: []string{"Subject: tab\there", "From: c@x", "Message-ID: <x3@x>"}, Body: []string{"cross"}, Date: storeDate}
	st.AddArticle("alt.test", x3)
	st.AddArticle("comp.lang.go", x3)
	return st
}

// storeServer starts a Store-mode server and dials it with a strict client.
func storeServer(t *testing.T, st *Store, so StoreOptions, f Faults) (*Server, *client.Conn) {
	t.Helper()
	s := New(t, Options{Store: st, StoreOpts: so, Faults: f})
	return s, client.Dial(t, s.Addr())
}

// wantBlock runs cmd, checks the status code and returns the un-stuffed dot block.
func wantBlock(t *testing.T, c *client.Conn, cmd string, code int) (string, []string) {
	t.Helper()
	line := wantCode(t, c, cmd, code)
	return line, c.ReadDotLines()
}

func wantEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestStoreCapabilities(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{})
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	wantEqual(t, "caps", caps, []string{"VERSION 2", "READER", "LIST ACTIVE NEWSGROUPS OVERVIEW.FMT", "NEWNEWS", "OVER MSGID", "HDR", "POST", "IMPLEMENTATION fakeserver"})
	wantCode(t, c, "MODE READER", 200)
	wantCode(t, c, "MODE STREAM", 501)
	wantCode(t, c, "MODE", 501)
	_, help := wantBlock(t, c, "HELP", 100)
	if len(help) == 0 || help[0] != "ARTICLE" {
		t.Errorf("HELP %q", help)
	}
	wantCode(t, c, "FOO bar", 500)
	wantCode(t, c, "GROUP", 501)
	wantCode(t, c, "GROUP a b", 501)
	wantCode(t, c, "DATE now", 501)
	wantCode(t, c, "NEXT 1", 501)
	wantCode(t, c, "AUTHINFO USER u", 502) // no auth configured
	if err := c.ExpectNoPending(20 * time.Millisecond); err != nil {
		t.Errorf("extra bytes: %v", err)
	}
	if line := wantCode(t, c, "QUIT", 205); !strings.HasPrefix(line, "205 ") {
		t.Errorf("QUIT %q", line)
	}
	wantEOF(t, c)

	_, ro := storeServer(t, testStore(), StoreOptions{ReaderOnly: true}, Faults{})
	_, caps = wantBlock(t, ro, "CAPABILITIES", 101)
	wantEqual(t, "reader-only caps", caps, []string{"VERSION 2", "READER", "LIST ACTIVE NEWSGROUPS OVERVIEW.FMT", "NEWNEWS", "OVER MSGID", "HDR", "IMPLEMENTATION fakeserver"})
	wantCode(t, ro, "MODE READER", 201)
	wantCode(t, ro, "POST", 440)
	_, active := wantBlock(t, ro, "LIST", 215)
	for _, l := range active {
		if !strings.HasSuffix(l, " n") {
			t.Errorf("reader-only LIST ACTIVE line %q", l)
		}
	}
}

func TestStoreAuth(t *testing.T) {
	s, c := storeServer(t, testStore(), StoreOptions{User: "u", Pass: "p"}, Faults{})
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	if !strings.Contains(strings.Join(caps, "|"), "|AUTHINFO USER|") {
		t.Errorf("caps before auth %q", caps)
	}
	wantCode(t, c, "GROUP alt.test", 480)
	wantCode(t, c, "LIST", 480)
	wantCode(t, c, "ARTICLE <a1@x>", 480)
	wantCode(t, c, "POST", 480)
	wantCode(t, c, "DATE", 111)
	wantCode(t, c, "MODE READER", 200)
	wantCode(t, c, "AUTHINFO PASS p", 482)
	wantCode(t, c, "AUTHINFO FOO x", 501)
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS wrong", 481)
	wantCode(t, c, "AUTHINFO USER other", 381)
	wantCode(t, c, "AUTHINFO PASS p", 481)
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	wantCode(t, c, "GROUP alt.test", 211)
	wantBlock(t, c, "LIST", 215)
	wantCode(t, c, "AUTHINFO USER u", 502)
	_, caps = wantBlock(t, c, "CAPABILITIES", 101)
	if strings.Contains(strings.Join(caps, "|"), "AUTHINFO") {
		t.Errorf("caps after auth %q", caps)
	}
	if got := s.Count("AUTHINFO"); got != 9 {
		t.Errorf("Count(AUTHINFO) = %d", got)
	}
	if got := s.Count("POST"); got != 1 {
		t.Errorf("Count(POST) = %d", got)
	}
	if n := len(s.Articles()); n != 0 {
		t.Errorf("POST before auth read %d articles", n)
	}

	// authentication is per connection
	c2 := client.Dial(t, s.Addr())
	wantCode(t, c2, "GROUP alt.test", 480)
}

func TestStoreAuthUserOnly(t *testing.T) {
	s, c := storeServer(t, testStore(), StoreOptions{User: "u"}, Faults{})
	wantCode(t, c, "GROUP alt.test", 480)
	wantCode(t, c, "AUTHINFO USER other", 481)
	wantCode(t, c, "GROUP alt.test", 480)
	wantCode(t, c, "AUTHINFO USER u", 281)
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "AUTHINFO USER u", 502)
	if got := s.Count("AUTHINFO"); got != 3 {
		t.Errorf("Count(AUTHINFO) = %d", got)
	}
}

func TestStoreList(t *testing.T) {
	st := testStore()
	if !st.SetPosting("comp.lang.go", false) {
		t.Fatal("SetPosting")
	}
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	all := []string{"alt.empty 0 1 y", "alt.test 3 1 y", "comp.lang.go 1 1 n"}
	_, got := wantBlock(t, c, "LIST", 215)
	wantEqual(t, "LIST", got, all)
	_, got = wantBlock(t, c, "list active", 215)
	wantEqual(t, "LIST ACTIVE", got, all)
	for pattern, want := range map[string][]string{
		"alt.*":            all[:2],
		"*.go":             all[2:],
		"alt.?es?":         all[1:2],
		"*":                all,
		"alt.*,!alt.empty": all[1:2],
		"!alt.*":           nil,
		"nomatch":          nil,
	} {
		_, got = wantBlock(t, c, "LIST ACTIVE "+pattern, 215)
		wantEqual(t, "LIST ACTIVE "+pattern, got, want)
	}
	_, got = wantBlock(t, c, "LIST NEWSGROUPS", 215)
	wantEqual(t, "LIST NEWSGROUPS", got, []string{"alt.empty\tNothing here", "alt.test\tTesting group", "comp.lang.go\tGo"})
	_, got = wantBlock(t, c, "LIST NEWSGROUPS comp.*", 215)
	wantEqual(t, "LIST NEWSGROUPS comp.*", got, []string{"comp.lang.go\tGo"})
	_, got = wantBlock(t, c, "LIST OVERVIEW.FMT", 215)
	wantEqual(t, "LIST OVERVIEW.FMT", got, defaultOverviewFmt)
	wantCode(t, c, "LIST ACTIVE.TIMES", 501)
	wantCode(t, c, "LIST ACTIVE a b", 501)

	_, c2 := storeServer(t, st, StoreOptions{OverviewFmt: []string{"Subject:", ":bytes"}}, Faults{})
	_, got = wantBlock(t, c2, "LIST OVERVIEW.FMT", 215)
	wantEqual(t, "custom OVERVIEW.FMT", got, []string{"Subject:", ":bytes"})
}

func TestStoreArticles(t *testing.T) {
	st := testStore()
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	date := "Date: " + storeDate.Format(DateLayout)
	head1 := []string{"Subject: one", "From: a@x", "Message-ID: <a1@x>", "Newsgroups: alt.test", date}
	body1 := []string{"line 1", ".dot line", ".."}

	wantCode(t, c, "ARTICLE", 412)
	wantCode(t, c, "ARTICLE 1", 412)
	wantCode(t, c, "NEXT", 412)
	wantCode(t, c, "LAST", 412)
	wantCode(t, c, "LISTGROUP", 412)
	// by message-id without a selected group: number 0
	line, got := wantBlock(t, c, "ARTICLE <a1@x>", 220)
	if line != "220 0 <a1@x> Article follows" {
		t.Errorf("ARTICLE <id> status %q", line)
	}
	wantEqual(t, "ARTICLE <a1@x>", got, append(append(append([]string{}, head1...), ""), body1...))
	wantCode(t, c, "STAT <missing@x>", 430)
	wantCode(t, c, "STAT <bad", 501)

	wantCode(t, c, "GROUP nope", 411)
	if line := wantCode(t, c, "GROUP alt.test", 211); line != "211 3 1 3 alt.test" {
		t.Errorf("GROUP %q", line)
	}
	_, got = wantBlock(t, c, "ARTICLE", 220) // current = low
	wantEqual(t, "ARTICLE current", got, append(append(append([]string{}, head1...), ""), body1...))
	line, got = wantBlock(t, c, "HEAD 2", 221)
	if line != "221 2 <a2@x> Headers follow" {
		t.Errorf("HEAD 2 status %q", line)
	}
	wantEqual(t, "HEAD 2", got, []string{"Subject: two", "From: b@x", "Message-ID: <a2@x>", "References: <a1@x>", "Newsgroups: alt.test", date})
	_, got = wantBlock(t, c, "BODY", 222) // current = 2 after HEAD 2
	wantEqual(t, "BODY current", got, []string{"second"})
	wantCode(t, c, "STAT 9", 423)
	wantCode(t, c, "STAT x9", 501)
	// a message-id select does not move the current article; the number is the selected group's
	if line := wantCode(t, c, "STAT <x3@x>", 223); line != "223 3 <x3@x> Article exists" {
		t.Errorf("STAT <x3@x> %q", line)
	}
	if line := wantCode(t, c, "STAT", 223); !strings.HasPrefix(line, "223 2 <a2@x>") {
		t.Errorf("STAT current %q", line)
	}
	if line := wantCode(t, c, "NEXT", 223); line != "223 3 <x3@x> Article found" {
		t.Errorf("NEXT %q", line)
	}
	wantCode(t, c, "NEXT", 421)
	wantCode(t, c, "LAST", 223)
	if line := wantCode(t, c, "LAST", 223); !strings.HasPrefix(line, "223 1 <a1@x>") {
		t.Errorf("LAST %q", line)
	}
	wantCode(t, c, "LAST", 422)

	// the crossposted article: Newsgroups lists both groups, the number is per group
	_, got = wantBlock(t, c, "HEAD <x3@x>", 221)
	wantEqual(t, "HEAD <x3@x>", got, []string{"Subject: tab\there", "From: c@x", "Message-ID: <x3@x>", "Newsgroups: alt.test,comp.lang.go", date})
	wantCode(t, c, "GROUP comp.lang.go", 211)
	if line := wantCode(t, c, "STAT <x3@x>", 223); !strings.HasPrefix(line, "223 1 <x3@x>") {
		t.Errorf("STAT <x3@x> in comp.lang.go %q", line)
	}
	if line := wantCode(t, c, "STAT <a1@x>", 223); !strings.HasPrefix(line, "223 0 <a1@x>") {
		t.Errorf("STAT <a1@x> outside its group %q", line)
	}

	if line := wantCode(t, c, "GROUP alt.empty", 211); line != "211 0 1 0 alt.empty" {
		t.Errorf("GROUP empty %q", line)
	}
	wantCode(t, c, "ARTICLE", 420)
	wantCode(t, c, "NEXT", 420)
	wantCode(t, c, "ARTICLE 1", 423)

	// LISTGROUP selects the group
	line, got = wantBlock(t, c, "LISTGROUP alt.test", 211)
	if line != "211 3 1 3 alt.test list follows" {
		t.Errorf("LISTGROUP status %q", line)
	}
	wantEqual(t, "LISTGROUP", got, []string{"1", "2", "3"})
	wantCode(t, c, "STAT", 223)
	_, got = wantBlock(t, c, "LISTGROUP alt.test 2-", 211)
	wantEqual(t, "LISTGROUP 2-", got, []string{"2", "3"})
	_, got = wantBlock(t, c, "LISTGROUP alt.test 2", 211)
	wantEqual(t, "LISTGROUP 2", got, []string{"2"})
	_, got = wantBlock(t, c, "LISTGROUP alt.test 3-1", 211)
	wantEqual(t, "LISTGROUP 3-1", got, []string(nil))
	_, got = wantBlock(t, c, "LISTGROUP", 211)
	wantEqual(t, "LISTGROUP current", got, []string{"1", "2", "3"})
	wantCode(t, c, "LISTGROUP alt.test x-", 501)
	wantCode(t, c, "LISTGROUP nope", 411)

	// dot-stuffing on the wire
	wantCode(t, c, "BODY 1", 222)
	if raw := c.RawUntilDot(); !bytes.Equal(raw, []byte("line 1\r\n..dot line\r\n...\r\n.\r\n")) {
		t.Errorf("BODY 1 wire %q", raw)
	}
	if err := c.ExpectNoPending(20 * time.Millisecond); err != nil {
		t.Errorf("extra bytes: %v", err)
	}
}

func TestStoreDefaultHeaders(t *testing.T) {
	st := NewStore()
	st.AddArticle("alt.bare", Article{Body: []string{"b"}, Date: storeDate})
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	wantCode(t, c, "GROUP alt.bare", 211)
	line, got := wantBlock(t, c, "HEAD 1", 221)
	if line != "221 1 <1.alt.bare@fakeserver.invalid> Headers follow" {
		t.Errorf("HEAD status %q", line)
	}
	wantEqual(t, "default headers", got, []string{"Message-ID: <1.alt.bare@fakeserver.invalid>", "Newsgroups: alt.bare", "Date: " + storeDate.Format(DateLayout)})
}

func TestStoreOver(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{})
	date := storeDate.Format(DateLayout)
	// bytes: every served header line + CRLF, the blank line, every body line + CRLF
	size := func(head, body []string) int {
		n := 2
		for _, l := range append(append([]string{}, head...), body...) {
			n += len(l) + 2
		}
		return n
	}
	head1 := []string{"Subject: one", "From: a@x", "Message-ID: <a1@x>", "Newsgroups: alt.test", "Date: " + date}
	ov1 := strings.Join([]string{"1", "one", "a@x", date, "<a1@x>", "", strconv.Itoa(size(head1, []string{"line 1", ".dot line", ".."})), "3", "Xref: fakeserver alt.test:1"}, "\t")
	head2 := []string{"Subject: two", "From: b@x", "Message-ID: <a2@x>", "References: <a1@x>", "Newsgroups: alt.test", "Date: " + date}
	ov2 := strings.Join([]string{"2", "two", "b@x", date, "<a2@x>", "<a1@x>", strconv.Itoa(size(head2, []string{"second"})), "1", "Xref: fakeserver alt.test:2"}, "\t")
	head3 := []string{"Subject: tab\there", "From: c@x", "Message-ID: <x3@x>", "Newsgroups: alt.test,comp.lang.go", "Date: " + date}
	ov3 := strings.Join([]string{"3", "tab here", "c@x", date, "<x3@x>", "", strconv.Itoa(size(head3, []string{"cross"})), "1", "Xref: fakeserver alt.test:3 comp.lang.go:1"}, "\t")

	wantCode(t, c, "XOVER", 412)
	wantCode(t, c, "OVER 1-3", 412)
	_, got := wantBlock(t, c, "OVER <a2@x>", 224) // by message-id without a group: number 0
	wantEqual(t, "OVER <a2@x>", got, []string{"0" + strings.TrimPrefix(ov2, "2")})
	wantCode(t, c, "OVER <nope@x>", 430)

	wantCode(t, c, "GROUP alt.test", 211)
	_, got = wantBlock(t, c, "XOVER", 224)
	wantEqual(t, "XOVER current", got, []string{ov1})
	_, got = wantBlock(t, c, "XOVER 1-3", 224)
	wantEqual(t, "XOVER 1-3", got, []string{ov1, ov2, ov3})
	_, got = wantBlock(t, c, "OVER 2-", 224)
	wantEqual(t, "OVER 2-", got, []string{ov2, ov3})
	_, got = wantBlock(t, c, "OVER 3", 224)
	wantEqual(t, "OVER 3", got, []string{ov3})
	_, got = wantBlock(t, c, "OVER 2-99", 224)
	wantEqual(t, "OVER 2-99", got, []string{ov2, ov3})
	wantCode(t, c, "XOVER 3-1", 423)
	wantCode(t, c, "XOVER 7-9", 423)
	wantCode(t, c, "XOVER 7-", 423)
	wantCode(t, c, "XOVER 0-0", 423)
	wantCode(t, c, "XOVER x-y", 501)
	wantCode(t, c, "XOVER 1-x", 501)
	wantCode(t, c, "GROUP alt.empty", 211)
	wantCode(t, c, "XOVER", 420)
	wantCode(t, c, "XOVER 1-", 423)
}

func TestStoreNewgroups(t *testing.T) {
	st := NewStore()
	st.AddGroupAt("old.group", "", time.Date(2019, 12, 31, 23, 59, 59, 0, time.UTC))
	st.AddGroupAt("new.group", "", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	st.AddArticle("new.group", Article{Body: []string{"x"}})
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	_, got := wantBlock(t, c, "NEWGROUPS 20260101 000000 GMT", 231)
	wantEqual(t, "since 2026", got, []string{"new.group 1 1 y"})
	_, got = wantBlock(t, c, "NEWGROUPS 20260601 120000", 231) // at the creation second: included
	wantEqual(t, "since creation", got, []string{"new.group 1 1 y"})
	_, got = wantBlock(t, c, "NEWGROUPS 200101 000000 GMT", 231) // yy 20 → 2020
	wantEqual(t, "since 2020", got, []string{"new.group 1 1 y"})
	_, got = wantBlock(t, c, "NEWGROUPS 191231 235959", 231)
	wantEqual(t, "since 2019", got, []string{"new.group 1 1 y", "old.group 0 1 y"})
	_, got = wantBlock(t, c, "NEWGROUPS 20270101 000000", 231)
	wantEqual(t, "since 2027", got, []string(nil))
	for _, bad := range []string{"NEWGROUPS 20261301 000000", "NEWGROUPS 2026 000000", "NEWGROUPS 20260101 0000",
		"NEWGROUPS 20260101 000000 EST", "NEWGROUPS 20260101", "NEWGROUPS x 000000"} {
		wantCode(t, c, bad, 501)
	}
}

func TestStoreNewnews(t *testing.T) {
	st := NewStore()
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }
	st.AddArticle("alt.a", Article{MessageID: "<old@x>", Arrived: time.Date(2019, 12, 31, 23, 59, 59, 0, time.UTC)})
	st.AddArticle("alt.a", Article{MessageID: "<new@x>", Arrived: day(6, 1), Date: day(1, 1)}) // Arrived decides, not Date
	x := Article{MessageID: "<x@x>", Arrived: day(3, 1)}
	st.AddArticle("alt.a", x)
	st.AddArticle("comp.b", x)                                // crossposted: listed once
	st.AddArticle("comp.b", Article{MessageID: "<now@x>"})    // Arrived = now
	st.AddArticle("alt.gone", Article{MessageID: "<gone@x>"}) // cancelled below
	st.Cancel("<gone@x>")
	st.AddGroup("alt.empty", "")
	_, c := storeServer(t, st, StoreOptions{}, Faults{})

	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"NEWNEWS * 20260101 000000 GMT", []string{"<new@x>", "<x@x>", "<now@x>"}},
		{"NEWNEWS * 191231 235959", []string{"<old@x>", "<new@x>", "<x@x>", "<now@x>"}}, // yy 19 → 2019
		{"NEWNEWS alt.* 20260601 120000", []string{"<new@x>"}},                          // at the arrival second: included
		{"NEWNEWS alt.* 20260601 120001", nil},
		{"newnews comp.b 20260101 000000 gmt", []string{"<x@x>", "<now@x>"}},
		{"NEWNEWS *,!alt.* 20260101 000000", []string{"<x@x>", "<now@x>"}},
		{"NEWNEWS !* 20000101 000000", nil},
		{"NEWNEWS no.such 20000101 000000", nil},
		{"NEWNEWS * 20990101 000000 GMT", nil},
	} {
		line, got := wantBlock(t, c, tc.cmd, 230)
		wantEqual(t, tc.cmd, got, tc.want)
		if !strings.HasPrefix(line, "230 ") {
			t.Errorf("%s: %q", tc.cmd, line)
		}
	}
	for _, bad := range []string{"NEWNEWS * 20261301 000000", "NEWNEWS * 2026 000000", "NEWNEWS * 20260101 0000",
		"NEWNEWS * 20260101 000000 EST", "NEWNEWS * 20260101", "NEWNEWS 20260101 000000", "NEWNEWS * x 000000",
		"NEWNEWS * 20260101 000000 GMT x"} {
		wantCode(t, c, bad, 501)
	}

	// a posted article arrives now, whatever its Date header says
	wantCode(t, c, "POST", 340)
	if err := c.Send("Newsgroups: alt.a\r\nFrom: p@x\r\nSubject: s\r\nMessage-ID: <posted@x>\r\n" +
		"Date: Mon, 01 Jan 2001 00:00:00 +0000\r\n\r\nbody\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 240)
	since := time.Now().UTC().Add(-time.Minute).Format("20060102 150405")
	_, got := wantBlock(t, c, "NEWNEWS alt.a "+since+" GMT", 230)
	wantEqual(t, "posted", got, []string{"<posted@x>"})
	if p := st.Posted(); len(p) != 1 || p[0].Arrived.IsZero() || p[0].Date.Year() != 2001 {
		t.Errorf("Posted() = %+v", p)
	}

	// NEWNEWS needs authentication like the other reading commands
	_, ac := storeServer(t, st, StoreOptions{User: "u", Pass: "p"}, Faults{})
	wantCode(t, ac, "NEWNEWS * 20260101 000000", 480)

	// INN with NEWNEWS turned off: Faults.Override
	_, oc := storeServer(t, st, StoreOptions{}, Faults{Override: map[string][]string{"NEWNEWS": {"502 NEWNEWS command disabled by administrator"}}})
	wantCode(t, oc, "NEWNEWS * 20260101 000000", 502)
}

func TestStoreNoNewnews(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{NoNewnews: true, NoOver: true}, Faults{})
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	wantEqual(t, "caps", caps, []string{"VERSION 2", "READER", "LIST ACTIVE NEWSGROUPS OVERVIEW.FMT", "HDR", "POST", "IMPLEMENTATION fakeserver"})
	wantCode(t, c, "NEWNEWS * 20260101 000000", 500)
	wantCode(t, c, "NEWNEWS", 500) // unknown before the argument check
	_, help := wantBlock(t, c, "HELP", 100)
	for _, l := range help {
		if l == "NEWNEWS" || l == "OVER" {
			t.Errorf("HELP lists the unknown command %s", l)
		}
	}
	if !strings.Contains(strings.Join(help, " "), "XOVER") {
		t.Errorf("HELP without XOVER: %q", help)
	}
}

func TestStoreDate(t *testing.T) {
	_, c := storeServer(t, NewStore(), StoreOptions{}, Faults{})
	line := wantCode(t, c, "DATE", 111)
	if !regexp.MustCompile(`^111 \d{14}$`).MatchString(line) {
		t.Errorf("DATE %q", line)
	}
}

func TestStorePost(t *testing.T) {
	st := testStore()
	s, c := storeServer(t, st, StoreOptions{}, Faults{})
	wantCode(t, c, "POST", 340)
	if err := c.Send("Newsgroups: alt.test, misc.new\r\nSubject: hello\r\nFrom: me@x\r\n\r\nfirst\r\n..leading dot\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	id := "<posted-1@fakeserver.invalid>"
	if line := wantStatus(t, c, 240); line != "240 Article received "+id {
		t.Errorf("POST reply %q", line)
	}
	posted := st.Posted()
	if len(posted) != 1 {
		t.Fatalf("Posted() = %d articles", len(posted))
	}
	p := posted[0]
	wantEqual(t, "posted headers", p.Headers, []string{"Newsgroups: alt.test, misc.new", "Subject: hello", "From: me@x", "Message-ID: " + id})
	wantEqual(t, "posted body", p.Body, []string{"first", ".leading dot"})
	if p.MessageID != id {
		t.Errorf("posted id %q", p.MessageID)
	}
	_, locs, ok := st.ByMessageID(id)
	wantEqual(t, "locations", locs, []Location{{"alt.test", 4}, {"misc.new", 1}})
	if !ok {
		t.Fatal("posted article not filed")
	}
	_, got := wantBlock(t, c, "BODY "+id, 222)
	wantEqual(t, "BODY of the posted article", got, []string{"first", ".leading dot"})
	if line := wantCode(t, c, "GROUP misc.new", 211); line != "211 1 1 1 misc.new" {
		t.Errorf("GROUP misc.new %q", line)
	}
	if line := wantCode(t, c, "ARTICLE "+id, 220); line != "220 1 "+id+" Article follows" {
		t.Errorf("ARTICLE posted %q", line)
	}
	_ = c.ReadDotLines()
	if got := s.Count("POST"); got != 1 {
		t.Errorf("Count(POST) = %d", got)
	}
	if arts := s.Articles(); len(arts) != 1 || arts[0][5] != "..leading dot" {
		t.Errorf("Articles() = %q", arts)
	}

	// an explicit Message-ID is kept; a duplicate or a missing header fails with 441
	wantCode(t, c, "POST", 340)
	if err := c.Send("Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\nMessage-ID: <mine@x>\r\nDate: " + storeDate.Format(DateLayout) + "\r\n\r\nb\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 240)
	if a, _, ok := st.ByMessageID("<mine@x>"); !ok || !a.Date.Equal(storeDate) {
		t.Errorf("explicit id: %v %+v", ok, a)
	}
	for _, art := range []string{
		"Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\nMessage-ID: <mine@x>\r\n\r\nb\r\n.\r\n",
		"Newsgroups: alt.test\r\nFrom: f\r\n\r\nb\r\n.\r\n",
		"Subject: s\r\nFrom: f\r\n\r\nb\r\n.\r\n",
		"Newsgroups: alt.test\r\nSubject: s\r\n\r\nb\r\n.\r\n",
		"Newsgroups: ,\r\nSubject: s\r\nFrom: f\r\n\r\nb\r\n.\r\n",
		"Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\nMessage-ID:\r\n\r\nb\r\n.\r\n",
		"Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\nMessage-ID: no-brackets@x\r\n\r\nb\r\n.\r\n",
		"Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\nMessage-ID: <two words@x>\r\n\r\nb\r\n.\r\n",
		"Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\nMessage-ID: <>\r\n\r\nb\r\n.\r\n",
	} {
		wantCode(t, c, "POST", 340)
		if err := c.Send(art); err != nil {
			t.Fatal(err)
		}
		wantStatus(t, c, 441)
	}
	if n := len(st.Posted()); n != 2 {
		t.Errorf("Posted() = %d after the failures", n)
	}
	wantCode(t, c, "POST x", 501)
}

func TestStorePostConcurrent(t *testing.T) {
	st := NewStore()
	s := New(t, Options{Store: st})
	conns := []*client.Conn{client.Dial(t, s.Addr()), client.Dial(t, s.Addr())}
	for _, c := range conns {
		wantCode(t, c, "POST", 340)
	}
	art := "Newsgroups: alt.race\r\nSubject: s\r\nFrom: f\r\nMessage-ID: <same@x>\r\n\r\nb\r\n.\r\n"
	start := make(chan struct{})
	codes := make(chan int, len(conns))
	for _, c := range conns {
		go func(c *client.Conn) {
			<-start
			if err := c.Send(art); err != nil {
				codes <- 0
				return
			}
			code, _, err := c.ReadStatus()
			if err != nil {
				code = 0
			}
			codes <- code
		}(c)
	}
	close(start)
	got := map[int]int{}
	for range conns {
		got[<-codes]++
	}
	wantEqual(t, "reply codes", got, map[int]int{240: 1, 441: 1})
	if n := len(st.Posted()); n != 1 {
		t.Errorf("Posted() = %d articles", n)
	}
	if g, _ := st.Group("alt.race"); g.Count != 1 {
		t.Errorf("alt.race Count = %d", g.Count)
	}
}

func TestStoreOverClean(t *testing.T) {
	st := NewStore()
	st.AddArticle("alt.nul", Article{Headers: []string{"Subject: a\x00b\tc", "From: f", "Message-ID: <n@x>"}, Date: storeDate})
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	_, got := wantBlock(t, c, "OVER <n@x>", 224)
	if len(got) != 1 || strings.Split(got[0], "\t")[1] != "a b c" {
		t.Errorf("OVER with NUL and tab %q", got)
	}
}

func TestStoreCounts(t *testing.T) {
	s, c := storeServer(t, testStore(), StoreOptions{}, Faults{})
	for i := 0; i < 2; i++ {
		wantBlock(t, c, "ARTICLE <a1@x>", 220)
	}
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "QUIT", 205)
	wantEOF(t, c)
	if got := s.Count("ARTICLE"); got != 2 {
		t.Errorf("Count(ARTICLE) = %d", got)
	}
	wantEqual(t, "Counts()", s.Counts(), map[string]int{"ARTICLE": 2, "GROUP": 1, "QUIT": 1})
}

func TestStoreFaults(t *testing.T) {
	t.Run("Delay", func(t *testing.T) {
		_, c := storeServer(t, testStore(), StoreOptions{}, Faults{Delay: map[string]time.Duration{"GROUP": 20 * time.Millisecond}})
		if line := wantCode(t, c, "GROUP alt.test", 211); line != "211 3 1 3 alt.test" {
			t.Errorf("delayed GROUP %q", line)
		}
	})

	t.Run("Override", func(t *testing.T) {
		_, c := storeServer(t, testStore(), StoreOptions{}, Faults{Override: map[string][]string{"GROUP": {"411 overridden"}}})
		if line := wantCode(t, c, "GROUP alt.test", 411); line != "411 overridden" {
			t.Errorf("override %q", line)
		}
		wantCode(t, c, "LISTGROUP alt.test", 211) // the store still answers other commands
		_ = c.ReadDotLines()
	})

	t.Run("OverridePost", func(t *testing.T) {
		st := testStore()
		s, c := storeServer(t, st, StoreOptions{}, Faults{Override: map[string][]string{"POST": {"440 not today"}}})
		wantCode(t, c, "POST", 440)
		wantCode(t, c, "DATE", 111) // no article is read after a final override
		if len(st.Posted()) != 0 || len(s.Articles()) != 0 {
			t.Error("article read after the overridden POST")
		}
	})

	t.Run("CloseAfter", func(t *testing.T) {
		s, c := storeServer(t, testStore(), StoreOptions{}, Faults{CloseAfter: 2})
		wantCode(t, c, "MODE READER", 200)
		wantCode(t, c, "GROUP alt.test", 211)
		wantEOF(t, c)
		s.WaitOpen(t, 0, 2*time.Second)
	})

	t.Run("DropInMultiline", func(t *testing.T) {
		s, c := storeServer(t, testStore(), StoreOptions{}, Faults{DropInMultiline: map[string]int{"ARTICLE": 3}})
		wantCode(t, c, "GROUP alt.test", 211)
		wantCode(t, c, "ARTICLE 1", 220)
		lines, err := c.TryReadDotLines()
		var se *client.StrictError
		if !errors.As(err, &se) || se.Kind != client.NoTerminator {
			t.Fatalf("cut reply: err %v, want NoTerminator", err)
		}
		wantEqual(t, "lines before the drop", lines, []string{"Subject: one", "From: a@x"})
		s.WaitOpen(t, 0, 2*time.Second)
	})
}

func TestStoreHandlerAsHandler(t *testing.T) {
	st := testStore()
	s := New(t, Options{Handler: StoreHandler(st, StoreOptions{ReaderOnly: true})})
	c := client.Dial(t, s.Addr())
	wantCode(t, c, "MODE READER", 201)
	if line := wantCode(t, c, "GROUP alt.test", 211); line != "211 3 1 3 alt.test" {
		t.Errorf("GROUP %q", line)
	}
	wantBlock(t, c, "ARTICLE 2", 220)
	if got := s.Count("ARTICLE"); got != 1 {
		t.Errorf("Count(ARTICLE) = %d", got)
	}
}

func TestWildmat(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "a.b", true}, {"a.*", "a.b", true}, {"a.*", "b.a", false}, {"?.b", "a.b", true},
		{"?.b", "ab.b", false}, {"a*b*c", "aXbYc", true}, {"a*b*c", "aXbY", false},
		{"*,!a.*", "a.b", false}, {"!a.*,a.b", "a.b", true}, {"x,y", "y", true}, {"", "", true}, {"", "a", false},
	} {
		if got := wildmat(tc.pattern, tc.name); got != tc.want {
			t.Errorf("wildmat(%q, %q) = %v", tc.pattern, tc.name, got)
		}
	}
}

// OverrideLine answers one client line, here one article of a pipeline; it wins over Override
// and its keys are normalised.
func TestFaultOverrideLine(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{
		OverrideLine: map[string][]string{"article  2": {"430 No such article"}, "HEAD 1": {"503 head override"}},
		Override:     map[string][]string{"HEAD": {"502 any head"}},
	})
	wantCode(t, c, "GROUP alt.test", 211)
	if err := c.Send("ARTICLE 1\r\nARTICLE 2\r\nARTICLE 3\r\nARTICLE <a2@x>\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{220, 430, 220, 220} {
		line := wantStatus(t, c, want)
		if want == 220 {
			c.ReadDotLines()
		} else if line != "430 No such article" {
			t.Errorf("ARTICLE 2: %q", line)
		}
	}
	wantCode(t, c, "HEAD 1", 503)
	wantCode(t, c, "HEAD 2", 502)
}

// Drip writes the reply a byte at a time; the next reply is normal, and a drip past the
// Deadline closes the connection.
func TestFaultDrip(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{Drip: map[string]time.Duration{"DATE": 10 * time.Millisecond}})
	start := time.Now()
	line := wantCode(t, c, "DATE", 111)
	if took := time.Since(start); took < 150*time.Millisecond || len(line) != 18 {
		t.Errorf("DATE %q in %v, want 18 bytes dripped over 190ms", line, took)
	}
	start = time.Now()
	wantCode(t, c, "MODE READER", 200)
	if took := time.Since(start); took > time.Second {
		t.Errorf("MODE READER after a drip took %v", took)
	}

	s := New(t, Options{Store: testStore(), Deadline: 100 * time.Millisecond,
		Faults: Faults{Drip: map[string]time.Duration{"HELP": 20 * time.Millisecond}}})
	c2 := client.Dial(t, s.Addr())
	c2.Strict = false
	if err := c2.Send("HELP\r\n"); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	for {
		if _, err := c2.ReadLine(); err != nil {
			break
		}
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("drip past the Deadline: closed after %v", took)
	}
}

// Endless never ends its reply: the client reads filler lines until it gives up; the server
// lets go of the connection when the client closes, or at the Deadline.
func TestFaultEndless(t *testing.T) {
	s, c := storeServer(t, testStore(), StoreOptions{}, Faults{Endless: map[string]bool{"OVER": true}})
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "OVER 1-3", 224)
	for i := range 3 {
		if l, err := c.ReadLine(); err != nil || !strings.HasPrefix(l, strconv.Itoa(i+1)+"\t") {
			t.Fatalf("overview line %d: %q, %v", i+1, l, err)
		}
	}
	for i := 1; i <= 20000; i++ {
		l, err := c.ReadLine()
		if err != nil || l != strconv.Itoa(i)+" endless" {
			t.Fatalf("filler %d: %q, %v", i, l, err)
		}
	}
	c.Close()
	s.WaitOpen(t, 0, 5*time.Second)

	s2 := New(t, Options{Store: testStore(), Deadline: 100 * time.Millisecond, Faults: Faults{Endless: map[string]bool{"DATE": true}}})
	c2 := client.Dial(t, s2.Addr())
	if err := c2.Send("DATE\r\n"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n := 0
	for ; ; n++ {
		if _, err := c2.ReadLine(); err != nil {
			break
		}
	}
	if took := time.Since(start); n < 2 || took > 3*time.Second {
		t.Errorf("endless DATE: %d lines, ended after %v", n, took)
	}
}

// LongLine puts one overlong line after the status line, inside the dot block.
func TestFaultLongLine(t *testing.T) {
	const n = 2 << 20
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{LongLine: map[string]int{"ARTICLE": n}})
	wantCode(t, c, "GROUP alt.test", 211)
	_, lines := wantBlock(t, c, "ARTICLE 1", 220)
	if len(lines) < 2 || len(lines[0]) != n || strings.Trim(lines[0], "x") != "" || lines[1] != "Subject: one" {
		t.Errorf("first lines: %d bytes, then %q", len(lines[0]), lines[1:2])
	}
}
