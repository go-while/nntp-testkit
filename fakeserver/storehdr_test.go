package fakeserver

import (
	"strconv"
	"strings"
	"testing"
)

// TestStoreHdr: HDR (225) and XHDR (221) over testStore (alt.test 1-3, comp.lang.go 1): by
// range, current article and message-id; absent header, metadata, Xref; the error codes.
func TestStoreHdr(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{})
	date := storeDate.Format(DateLayout)

	wantCode(t, c, "XHDR Subject", 412)
	wantCode(t, c, "HDR Subject 1-3", 412)
	_, got := wantBlock(t, c, "XHDR Subject <a2@x>", 221) // by id: XHDR gives the id, HDR 0
	wantEqual(t, "XHDR by id", got, []string{"<a2@x> two"})
	_, got = wantBlock(t, c, "HDR Subject <a2@x>", 225)
	wantEqual(t, "HDR by id", got, []string{"0 two"})
	wantCode(t, c, "HDR Subject <nope@x>", 430)
	wantCode(t, c, "XHDR", 501)
	wantCode(t, c, "HDR Subject 1-3 x", 501)

	wantCode(t, c, "GROUP alt.test", 211)
	_, got = wantBlock(t, c, "XHDR Subject", 221) // current article
	wantEqual(t, "XHDR current", got, []string{"1 one"})
	_, got = wantBlock(t, c, "XHDR Message-ID 1-3", 221)
	wantEqual(t, "XHDR Message-ID 1-3", got, []string{"1 <a1@x>", "2 <a2@x>", "3 <x3@x>"})
	_, got = wantBlock(t, c, "HDR message-id 2-", 225) // field name is case-insensitive, open range
	wantEqual(t, "HDR 2-", got, []string{"2 <a2@x>", "3 <x3@x>"})
	_, got = wantBlock(t, c, "XHDR References 1-2", 221) // absent header: (none) for XHDR
	wantEqual(t, "XHDR References", got, []string{"1 (none)", "2 <a1@x>"})
	_, got = wantBlock(t, c, "HDR References 1-2", 225) // empty for HDR
	wantEqual(t, "HDR References", got, []string{"1 ", "2 <a1@x>"})
	_, got = wantBlock(t, c, "XHDR Date 3", 221)
	wantEqual(t, "XHDR Date", got, []string{"3 " + date})
	_, got = wantBlock(t, c, "HDR Subject 3", 225) // a tab in the value becomes a space
	wantEqual(t, "HDR tab", got, []string{"3 tab here"})
	_, got = wantBlock(t, c, "HDR :lines 1-3", 225)
	wantEqual(t, "HDR :lines", got, []string{"1 3", "2 1", "3 1"})
	_, got = wantBlock(t, c, "XHDR Xref 3", 221)
	wantEqual(t, "XHDR Xref", got, []string{"3 fakeserver alt.test:3 comp.lang.go:1"})
	_, got = wantBlock(t, c, "HDR :bytes 2", 225)
	head2 := []string{"Subject: two", "From: b@x", "Message-ID: <a2@x>", "References: <a1@x>", "Newsgroups: alt.test", "Date: " + date}
	n := 2
	for _, l := range append(head2, "second") {
		n += len(l) + 2
	}
	wantEqual(t, "HDR :bytes", got, []string{"2 " + strconv.Itoa(n)})
	wantCode(t, c, "XHDR Subject 3-1", 423)
	wantCode(t, c, "XHDR Subject 7-9", 423)
	wantCode(t, c, "HDR Subject x-y", 501)

	_, got = wantBlock(t, c, "LIST HEADERS", 215)
	wantEqual(t, "LIST HEADERS", got, []string{":", ":bytes", ":lines"})
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	if !contains(caps, "HDR") {
		t.Fatalf("CAPABILITIES without HDR: %q", caps)
	}
	_, help := wantBlock(t, c, "HELP", 100)
	if !contains(help, "HDR") || !contains(help, "XHDR") {
		t.Fatalf("HELP without HDR/XHDR: %q", help)
	}
}

// TestStoreHdrOptions: NoHdr/NoXhdr make the command unknown; HdrLines rewrites the lines
// (header data out of step with the articles, as a provider whose XHDR index lags).
func TestStoreHdrOptions(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{NoHdr: true}, Faults{})
	wantCode(t, c, "HDR Subject <a2@x>", 500)
	wantCode(t, c, "LIST HEADERS", 501)
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	if contains(caps, "HDR") {
		t.Fatalf("NoHdr but CAPABILITIES lists HDR: %q", caps)
	}
	_, got := wantBlock(t, c, "XHDR Subject <a2@x>", 221)
	wantEqual(t, "XHDR with NoHdr", got, []string{"<a2@x> two"})

	_, c = storeServer(t, testStore(), StoreOptions{NoXhdr: true}, Faults{})
	wantCode(t, c, "XHDR Subject <a2@x>", 500)
	wantBlock(t, c, "HDR Subject <a2@x>", 225)
	wantBlock(t, c, "HELP", 100)

	_, c = storeServer(t, testStore(), StoreOptions{HdrLines: func(conn int, group, field string, lines []string) []string {
		if field != "Message-ID" || group != "alt.test" {
			return lines
		}
		// the index claims a different id for article 2 and knows nothing of article 3
		out := []string{lines[0], "2 <other@x>"}
		return out
	}}, Faults{})
	wantCode(t, c, "GROUP alt.test", 211)
	_, got = wantBlock(t, c, "XHDR Message-ID 1-3", 221)
	wantEqual(t, "HdrLines", got, []string{"1 <a1@x>", "2 <other@x>"})
	_, got = wantBlock(t, c, "XHDR Subject 1-3", 221)
	wantEqual(t, "HdrLines other field", got, []string{"1 one", "2 two", "3 tab here"})
}

func contains(lines []string, s string) bool {
	for _, l := range lines {
		if strings.EqualFold(strings.TrimSpace(l), s) {
			return true
		}
	}
	return false
}
