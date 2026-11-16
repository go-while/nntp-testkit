package fakeserver

import "testing"

// lineKey upper-cases the command and a keyword first argument, and keeps the case of group
// names, message-ids and user names.
func TestLineKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"article  2", "ARTICLE 2"},
		{"list overview.fmt", "LIST OVERVIEW.FMT"},
		{"mode\treader", "MODE READER"},
		{"LIST active alt.*", "LIST ACTIVE alt.*"},
		{"list headers msgid", "LIST HEADERS MSGID"},
		{"list newsgroups Alt.*", "LIST NEWSGROUPS Alt.*"},
		{"authinfo user Bob", "AUTHINFO USER Bob"},
		{"compress deflate", "COMPRESS DEFLATE"},
		{"capabilities autoupdate", "CAPABILITIES AUTOUPDATE"},
		{"hdr subject 1-10", "HDR SUBJECT 1-10"},
		{"xhdr Message-ID <A@b>", "XHDR MESSAGE-ID <A@b>"},
		{"group Alt.Test", "GROUP Alt.Test"},
		{"article <A@b>", "ARTICLE <A@b>"},
		{"list", "LIST"},
	} {
		if got := lineKey(tc.in); got != tc.want {
			t.Errorf("lineKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// OverrideLine keys written in lower case match the client's upper-case keywords (NewsCache
// used "list overview.fmt" and "mode reader" and its faults never fired), while a group name
// keeps its case.
func TestFaultOverrideLineKeywordCase(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{
		OverrideLine: map[string][]string{
			"mode reader":       {"502 reader override"},
			"list overview.fmt": {"503 fmt override"},
			"group Alt.Test":    {"411 group override"},
		},
	})
	wantCode(t, c, "MODE READER", 502)
	wantCode(t, c, "LIST OVERVIEW.FMT", 503)
	wantCode(t, c, "GROUP alt.test", 211)
}
