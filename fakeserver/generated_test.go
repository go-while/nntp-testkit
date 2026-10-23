package fakeserver

import (
	"fmt"
	"strings"
	"testing"
)

// Generated groups (huge groups made on demand) and OverviewFields.

const genGroup = "alt.binaries.huge"

func TestGeneratedStore(t *testing.T) {
	st := NewStore()
	st.AddGenerated(genGroup, "ten million", Generator{Low: 1, High: 10_000_000,
		Article: func(group string, n int64) (Article, bool) {
			if n%1000 == 0 {
				return Article{}, false // a hole every 1000
			}
			return GeneratedArticle(group, n)
		}})
	g, _ := st.Group(genGroup)
	if g.Low != 1 || g.High != 10_000_000 || g.Count != 10_000_000 || g.Description != "ten million" {
		t.Errorf("group %+v", g)
	}
	a, ok := st.Article(genGroup, 9_999_999)
	if !ok || a.Number != 9_999_999 || a.MessageID != "<9999999.alt.binaries.huge@fakeserver.invalid>" {
		t.Errorf("article: %+v %v", a, ok)
	}
	if _, ok := st.Article(genGroup, 9_999_000); ok {
		t.Error("hole served")
	}
	if _, ok := st.Article(genGroup, 10_000_001); ok {
		t.Error("article above High")
	}
	b, locs, ok := st.ByMessageID("<12345.alt.binaries.huge@fakeserver.invalid>")
	if !ok || b.Number != 12345 || len(locs) != 1 || locs[0] != (Location{genGroup, 12345}) {
		t.Errorf("ByMessageID: %+v %v %v", b, locs, ok)
	}
	for _, id := range []string{"<1000.alt.binaries.huge@fakeserver.invalid>", "<20000000.alt.binaries.huge@fakeserver.invalid>", "<5.other@fakeserver.invalid>"} {
		if _, _, ok := st.ByMessageID(id); ok {
			t.Errorf("ByMessageID(%s) found", id)
		}
	}
	ov := st.Overview(genGroup, 9_999_995, 0)
	if len(ov) != 5 || ov[0].Number != 9_999_995 || ov[4].Number != 9_999_999 {
		t.Errorf("overview to High: %d articles", len(ov))
	}
	wantEqual(t, "NumbersIn", st.NumbersIn(genGroup, 1998, 2001), []int64{1998, 1999, 2001})
	if n, ok := st.Next(genGroup, 999); !ok || n != 1001 {
		t.Errorf("Next over a hole: %d %v", n, ok)
	}
	if n, ok := st.Last(genGroup, 1001); !ok || n != 999 {
		t.Errorf("Last over a hole: %d %v", n, ok)
	}
	if hg := st.HighestExisting(0, g); hg.High != 9_999_999 {
		t.Errorf("HighestExisting %d, want 9999999 (10^7 is a hole)", hg.High)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Numbers of a group over the limit did not panic")
			}
		}()
		st.Numbers(genGroup)
	}()

	if n := st.Expire(genGroup, 5_000_001); n != 5_000_000 {
		t.Errorf("Expire %d", n)
	}
	if !st.SetGeneratedHigh(genGroup, 10_000_010) || st.SetGeneratedHigh("nope", 5) {
		t.Error("SetGeneratedHigh results")
	}
	g, _ = st.Group(genGroup)
	if g.Low != 5_000_001 || g.High != 10_000_010 || g.Count != 5_000_010 {
		t.Errorf("after Expire and growth: %+v", g)
	}
	if _, ok := st.Article(genGroup, 10); ok {
		t.Error("expired article served")
	}
	if st.RemoveArticle(genGroup, 6_000_001) || st.Cancel("<6000001.alt.binaries.huge@fakeserver.invalid>") != 0 {
		t.Error("RemoveArticle/Cancel on a generated group")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("AddArticle to a generated group did not panic")
			}
		}()
		st.AddArticle(genGroup, art("<x@y>"))
	}()
}

func TestGeneratedServed(t *testing.T) {
	st := NewStore()
	st.AddGenerated(genGroup, "", Generator{Low: 1, High: 10_000_000, Limit: 1000})
	st.AddGenerated("small.gen", "", Generator{Low: 1, High: 3})
	_, c := storeServer(t, st, StoreOptions{}, Faults{})

	wantEqual(t, "GROUP", wantCode(t, c, "GROUP "+genGroup, 211), "211 10000000 1 10000000 "+genGroup)
	_, ov := wantBlock(t, c, "XOVER 9999998-", 224)
	if len(ov) != 3 || !strings.HasPrefix(ov[0], "9999998\tgenerated article 9999998 in "+genGroup+"\t") ||
		!strings.HasSuffix(ov[2], "\tXref: fakeserver "+genGroup+":10000000") {
		t.Errorf("XOVER near the top: %q", ov)
	}
	_, ov = wantBlock(t, c, "OVER 5000001-5001000", 224)
	if len(ov) != 1000 {
		t.Errorf("OVER of 1000: %d lines", len(ov))
	}
	if line := wantCode(t, c, "XOVER 1-", 503); !strings.Contains(line, "over its limit of 1000") {
		t.Errorf("XOVER of the whole group: %q", line)
	}
	wantCode(t, c, "LISTGROUP "+genGroup, 503)
	_, lg := wantBlock(t, c, "LISTGROUP "+genGroup+" 9999999-", 211)
	wantEqual(t, "LISTGROUP range", strings.Join(lg, " "), "9999999 10000000")
	_, art := wantBlock(t, c, "ARTICLE 7654321", 220)
	if !strings.Contains(strings.Join(art, "\n"), "body of generated article 7654321") {
		t.Errorf("ARTICLE: %q", art)
	}
	wantEqual(t, "STAT <id>", wantCode(t, c, "STAT <42.alt.binaries.huge@fakeserver.invalid>", 223),
		"223 42 <42.alt.binaries.huge@fakeserver.invalid> Article exists")
	wantCode(t, c, "NEXT", 223)
	_, active := wantBlock(t, c, "LIST ACTIVE", 215)
	wantEqual(t, "LIST", strings.Join(active, "|"), genGroup+" 10000000 1 y|small.gen 3 1 y")
	_, lg = wantBlock(t, c, "LISTGROUP small.gen", 211)
	wantEqual(t, "LISTGROUP small", strings.Join(lg, " "), "1 2 3")

	// new articles arrive
	st.SetGeneratedHigh(genGroup, 10_000_005)
	wantEqual(t, "GROUP grown", wantCode(t, c, "GROUP "+genGroup, 211), "211 10000005 1 10000005 "+genGroup)
	_, ov = wantBlock(t, c, "XOVER 10000004-10000010", 224)
	if len(ov) != 2 {
		t.Errorf("XOVER of the new ones: %q", ov)
	}
}

func TestOverviewFields(t *testing.T) {
	st := testStore()
	so := StoreOptions{OverviewFields: func(conn int, group string, f []string) []string {
		if f[0] == "2" {
			f[4] = "<other@x>" // overview out of step with the article
		}
		f[1] = fmt.Sprintf("%s [conn %d %s]", f[1], conn, group)
		return f
	}}
	_, c := storeServer(t, st, so, Faults{})
	wantCode(t, c, "GROUP alt.test", 211)
	_, ov := wantBlock(t, c, "XOVER 1-3", 224)
	if len(ov) != 3 {
		t.Fatalf("XOVER: %q", ov)
	}
	f := strings.Split(ov[1], "\t")
	if f[4] != "<other@x>" || !strings.HasSuffix(f[1], " [conn 1 alt.test]") {
		t.Errorf("rewritten line: %q", f)
	}
	if f := strings.Split(ov[0], "\t"); f[4] == "<other@x>" {
		t.Errorf("line 1 changed: %q", f)
	}
	// the article itself keeps its Message-ID
	_, head := wantBlock(t, c, "HEAD 2", 221)
	if strings.Contains(strings.Join(head, "\n"), "<other@x>") {
		t.Errorf("HEAD 2 has the overview id: %q", head)
	}
	wantCode(t, c, "ARTICLE <other@x>", 430)
}
