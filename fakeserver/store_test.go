package fakeserver

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func art(id string, body ...string) Article {
	return Article{MessageID: id, Headers: []string{"Subject: " + id}, Body: body}
}

func overviewNumbers(as []Article) []int64 {
	out := []int64{}
	for _, a := range as {
		out = append(out, a.Number)
	}
	return out
}

func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic, want one containing %q", want)
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, want) {
			t.Fatalf("panic %q, want it to contain %q", msg, want)
		}
	}()
	f()
}

func TestStoreNumbering(t *testing.T) {
	st := NewStore()
	if n := st.AddArticle("a.b", art("<1@x>")); n != 1 {
		t.Fatalf("first auto number %d, want 1", n)
	}
	if n := st.AddArticle("a.b", art("<2@x>")); n != 2 {
		t.Fatalf("second auto number %d, want 2", n)
	}
	if n := st.AddArticle("a.b", Article{Number: 10, MessageID: "<10@x>"}); n != 10 {
		t.Fatalf("explicit number %d, want 10", n)
	}
	if n := st.AddArticle("a.b", Article{Number: 5, MessageID: "<5@x>"}); n != 5 {
		t.Fatalf("explicit hole number %d, want 5", n)
	}
	if n := st.AddArticle("a.b", art("<11@x>")); n != 11 {
		t.Fatalf("auto after explicit %d, want 11 (High+1)", n)
	}
	a, ok := st.Article("a.b", 10)
	if !ok || a.Number != 10 || a.MessageID != "<10@x>" {
		t.Fatalf("Article(10) = %+v, %v", a, ok)
	}
	if a.Date.IsZero() {
		t.Fatalf("zero Date not defaulted")
	}
	date := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	st.AddArticle("a.b", Article{Number: 20, MessageID: "<20@x>", Date: date})
	if a, _ := st.Article("a.b", 20); !a.Date.Equal(date) {
		t.Fatalf("explicit Date changed: %v", a.Date)
	}
	if _, ok := st.Article("a.b", 3); ok {
		t.Fatalf("Article(3) found in a hole")
	}
	if _, ok := st.Article("no.such", 1); ok {
		t.Fatalf("Article in unknown group found")
	}

	mustPanic(t, "article number 10 is already taken", func() {
		st.AddArticle("a.b", Article{Number: 10, MessageID: "<dup@x>"})
	})
	mustPanic(t, "empty group name", func() { st.AddArticle("", art("<e@x>")) })
	mustPanic(t, "negative article number", func() { st.AddArticle("a.b", Article{Number: -1}) })
	// the failed calls changed nothing
	if g, _ := st.Group("a.b"); g.Count != 6 {
		t.Fatalf("Count after panics %d, want 6", g.Count)
	}
	if _, _, ok := st.ByMessageID("<dup@x>"); ok {
		t.Fatalf("panicking AddArticle indexed its message-id")
	}
}

func TestStoreMessageIDFromHeader(t *testing.T) {
	st := NewStore()
	st.AddArticle("g", Article{Headers: []string{"message-id:  <h@x> "}})
	a, locs, ok := st.ByMessageID("<h@x>")
	if !ok || a.MessageID != "<h@x>" || len(locs) != 1 {
		t.Fatalf("ByMessageID(<h@x>) = %+v, %v, %v", a, locs, ok)
	}
	// an article without any id gets a generated one that ByMessageID resolves; Headers stay as given
	n := st.AddArticle("g", Article{Headers: []string{"Subject: s"}, Body: []string{"x"}})
	stored, ok := st.Article("g", n)
	if !ok || stored.MessageID != "<2.g@fakeserver.invalid>" {
		t.Fatalf("article without id = %+v, %v; want MessageID <2.g@fakeserver.invalid>", stored, ok)
	}
	if !reflect.DeepEqual(stored.Headers, []string{"Subject: s"}) {
		t.Fatalf("AddArticle changed Headers: %q", stored.Headers)
	}
	if _, locs, ok := st.ByMessageID("<2.g@fakeserver.invalid>"); !ok || !reflect.DeepEqual(locs, []Location{{"g", 2}}) {
		t.Fatalf("generated id not indexed: %v, %v", locs, ok)
	}
	if got := stored.WithDefaults("g").Header("Message-ID"); got != stored.MessageID {
		t.Fatalf("WithDefaults serves Message-ID %q, want the indexed %q", got, stored.MessageID)
	}
	if _, _, ok := st.ByMessageID(""); ok {
		t.Fatalf("empty message-id indexed")
	}

	// field and header must agree
	st.AddArticle("g", Article{MessageID: "<same@x>", Headers: []string{"Message-ID: <same@x>"}})
	mustPanic(t, "differs from Message-ID header", func() {
		st.AddArticle("g", Article{MessageID: "<field@x>", Headers: []string{"Message-ID: <header@x>"}})
	})
	if _, _, ok := st.ByMessageID("<field@x>"); ok {
		t.Fatalf("mismatching article indexed")
	}
	if g, _ := st.Group("g"); g.Count != 3 {
		t.Fatalf("Count %d after mismatch panic, want 3", g.Count)
	}
}

func TestStoreCreatedAndPosting(t *testing.T) {
	st := NewStore()
	at := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	st.AddGroupAt("old.group", "old", at)
	if g, _ := st.Group("old.group"); !g.Created.Equal(at) || !g.Posting || g.Description != "old" {
		t.Fatalf("AddGroupAt = %+v", g)
	}
	st.AddGroup("old.group", "")
	if g, _ := st.Group("old.group"); !g.Created.Equal(at) {
		t.Fatalf("AddGroup changed Created to %v", g.Created)
	}
	// an auto-created group gets Created now; AddGroupAt can set it afterwards
	st.AddArticle("auto.group", art("<a@x>"))
	if g, _ := st.Group("auto.group"); g.Created.IsZero() {
		t.Fatalf("auto-created group has zero Created")
	}
	later := at.Add(24 * time.Hour)
	st.AddGroupAt("auto.group", "", later)
	if g, _ := st.Group("auto.group"); !g.Created.Equal(later) || g.Count != 1 {
		t.Fatalf("AddGroupAt on existing group = %+v", g)
	}
	// zero created behaves like AddGroup
	st.AddGroupAt("auto.group", "", time.Time{})
	if g, _ := st.Group("auto.group"); !g.Created.Equal(later) {
		t.Fatalf("AddGroupAt(zero) changed Created to %v", g.Created)
	}

	if !st.SetPosting("old.group", false) {
		t.Fatalf("SetPosting on existing group = false")
	}
	if g, _ := st.Group("old.group"); g.Posting {
		t.Fatalf("Posting still true")
	}
	st.AddGroup("old.group", "again")
	if g, _ := st.Group("old.group"); g.Posting {
		t.Fatalf("repeat AddGroup reset Posting")
	}
	if !st.SetPosting("old.group", true) {
		t.Fatalf("SetPosting(true) = false")
	}
	if g, _ := st.Group("old.group"); !g.Posting {
		t.Fatalf("Posting not restored")
	}
	if st.SetPosting("no.such", false) {
		t.Fatalf("SetPosting on unknown group = true")
	}
	if _, ok := st.Group("no.such"); ok {
		t.Fatalf("SetPosting created a group")
	}
	var zero Store
	if zero.SetPosting("g", true) {
		t.Fatalf("SetPosting on zero Store = true")
	}
}

func TestStoreArrived(t *testing.T) {
	st := NewStore()
	before := time.Now()
	st.AddArticle("g", art("<a@x>"))
	at := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	st.AddArticle("g", Article{MessageID: "<b@x>", Arrived: at})
	a, _ := st.Article("g", 1)
	if a.Arrived.Before(before) || !a.Arrived.Equal(a.Date) {
		t.Errorf("zero Arrived: %v (Date %v), want now", a.Arrived, a.Date)
	}
	if b, _ := st.Article("g", 2); !b.Arrived.Equal(at) || b.Date.Before(before) {
		t.Errorf("explicit Arrived: %v, Date %v", b.Arrived, b.Date)
	}
}

func TestStoreMultiGroup(t *testing.T) {
	st := NewStore()
	st.AddArticle("b.two", art("<filler@x>"))
	a := art("<cross@x>", "body")
	n1 := st.AddArticle("c.three", a)
	n2 := st.AddArticle("b.two", a)
	n3 := st.AddArticle("a.one", a)
	if n1 != 1 || n2 != 2 || n3 != 1 {
		t.Fatalf("numbers %d %d %d, want 1 2 1", n1, n2, n3)
	}
	got, locs, ok := st.ByMessageID("<cross@x>")
	if !ok {
		t.Fatalf("ByMessageID not found")
	}
	want := []Location{{"c.three", 1}, {"b.two", 2}, {"a.one", 1}}
	if !reflect.DeepEqual(locs, want) {
		t.Fatalf("locations %v, want %v (insertion order)", locs, want)
	}
	if got.Number != 1 || !reflect.DeepEqual(got.Body, []string{"body"}) {
		t.Fatalf("first copy %+v", got)
	}
	locs[0].Group = "mutated"
	if _, locs2, _ := st.ByMessageID("<cross@x>"); locs2[0].Group != "c.three" {
		t.Fatalf("locations slice shares store memory")
	}
	if _, _, ok := st.ByMessageID("<missing@x>"); ok {
		t.Fatalf("unknown message-id found")
	}
}

func TestStoreOverview(t *testing.T) {
	st := NewStore()
	for _, n := range []int64{2, 4, 7, 9} {
		st.AddArticle("g", Article{Number: n, MessageID: fmt.Sprintf("<%d@x>", n)})
	}
	cases := []struct {
		from, to int64
		want     []int64
	}{
		{1, 9, []int64{2, 4, 7, 9}},
		{3, 7, []int64{4, 7}},
		{4, 4, []int64{4}},
		{5, 6, []int64{}},
		{8, 0, []int64{9}},
		{0, -1, []int64{2, 4, 7, 9}},
		{7, 3, []int64{}},
		{10, 0, []int64{}},
		{1, 100, []int64{2, 4, 7, 9}},
	}
	for _, c := range cases {
		if got := overviewNumbers(st.Overview("g", c.from, c.to)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Overview(%d, %d) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
	if got := st.Overview("no.such", 1, 0); got == nil || len(got) != 0 {
		t.Errorf("Overview of unknown group = %#v, want empty non-nil", got)
	}
	st.AddGroup("empty", "")
	if got := st.Overview("empty", 1, 0); len(got) != 0 {
		t.Errorf("Overview of empty group = %v", got)
	}
}

func TestStoreNumbersNextLast(t *testing.T) {
	st := NewStore()
	for _, n := range []int64{5, 3, 8} {
		st.AddArticle("g", Article{Number: n})
	}
	if got := st.Numbers("g"); !reflect.DeepEqual(got, []int64{3, 5, 8}) {
		t.Fatalf("Numbers = %v", got)
	}
	nums := st.Numbers("g")
	nums[0] = 99
	if got := st.Numbers("g"); got[0] != 3 {
		t.Fatalf("Numbers result shares store memory")
	}
	if got := st.Numbers("no.such"); got == nil || len(got) != 0 {
		t.Fatalf("Numbers(unknown) = %#v, want empty non-nil", got)
	}

	next := []struct {
		n    int64
		want int64
		ok   bool
	}{{0, 3, true}, {3, 5, true}, {4, 5, true}, {5, 8, true}, {8, 0, false}, {100, 0, false}}
	for _, c := range next {
		if got, ok := st.Next("g", c.n); got != c.want || ok != c.ok {
			t.Errorf("Next(%d) = %d, %v; want %d, %v", c.n, got, ok, c.want, c.ok)
		}
	}
	last := []struct {
		n    int64
		want int64
		ok   bool
	}{{100, 8, true}, {8, 5, true}, {6, 5, true}, {5, 3, true}, {3, 0, false}, {0, 0, false}}
	for _, c := range last {
		if got, ok := st.Last("g", c.n); got != c.want || ok != c.ok {
			t.Errorf("Last(%d) = %d, %v; want %d, %v", c.n, got, ok, c.want, c.ok)
		}
	}
	if _, ok := st.Next("no.such", 0); ok {
		t.Errorf("Next in unknown group ok")
	}
	if _, ok := st.Last("no.such", 10); ok {
		t.Errorf("Last in unknown group ok")
	}
}

func TestStoreGroups(t *testing.T) {
	st := NewStore()
	st.AddGroup("zeta", "Z group")
	st.AddGroup("alpha", "A group")
	alphaCreated, _ := st.Group("alpha")
	st.AddArticle("mid.auto", art("<m@x>"))
	st.AddArticle("alpha", Article{Number: 4})
	st.AddArticle("alpha", Article{Number: 9})
	st.AddArticle("alpha", Article{})
	st.AddGroup("alpha", "")            // idempotent, keeps articles and description
	st.AddGroup("zeta", "Z group, new") // non-empty description replaces

	got := st.Groups()
	for i := range got {
		if got[i].Created.IsZero() {
			t.Fatalf("group %s has zero Created", got[i].Name)
		}
		got[i].Created = time.Time{}
	}
	want := []Group{
		{Name: "alpha", Description: "A group", Low: 4, High: 10, Count: 3, Posting: true},
		{Name: "mid.auto", Low: 1, High: 1, Count: 1, Posting: true},
		{Name: "zeta", Description: "Z group, new", Low: 1, High: 0, Count: 0, Posting: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Groups =\n%+v\nwant\n%+v", got, want)
	}
	g, ok := st.Group("alpha")
	if !g.Created.Equal(alphaCreated.Created) {
		t.Fatalf("repeat AddGroup changed Created: %v -> %v", alphaCreated.Created, g.Created)
	}
	g.Created = time.Time{}
	if !ok || g != want[0] {
		t.Fatalf("Group(alpha) = %+v, %v", g, ok)
	}
	if _, ok := st.Group("no.such"); ok {
		t.Fatalf("Group(unknown) ok")
	}
}

func TestStoreZeroValue(t *testing.T) {
	var st Store
	if got := st.Groups(); len(got) != 0 {
		t.Fatalf("zero Store Groups = %v", got)
	}
	if _, _, ok := st.ByMessageID("<a@x>"); ok {
		t.Fatalf("zero Store ByMessageID ok")
	}
	if got := st.Posted(); len(got) != 0 {
		t.Fatalf("zero Store Posted = %v", got)
	}
	if n := st.AddArticle("g", art("<a@x>")); n != 1 {
		t.Fatalf("zero Store AddArticle = %d", n)
	}
	if _, locs, ok := st.ByMessageID("<a@x>"); !ok || len(locs) != 1 {
		t.Fatalf("zero Store ByMessageID after add = %v, %v", locs, ok)
	}
	st2 := &Store{}
	st2.AddGroup("h", "desc")
	if g, ok := st2.Group("h"); !ok || !g.Posting || g.Description != "desc" {
		t.Fatalf("zero Store AddGroup = %+v, %v", g, ok)
	}
}

func TestStoreCopiesIsolation(t *testing.T) {
	st := NewStore()
	in := Article{MessageID: "<p@x>", Headers: []string{"Subject: s"}, Body: []string{"line"}}
	st.AddPosted(in)
	in.Headers[0] = "Subject: changed by caller"
	in.Body[0] = "changed"

	p := st.Posted()
	if len(p) != 1 || p[0].Headers[0] != "Subject: s" || p[0].Body[0] != "line" {
		t.Fatalf("Posted after mutating the input = %+v", p)
	}
	p[0].Headers[0] = "Subject: mutated"
	p[0].Body = append(p[0].Body, "extra")
	p[0].MessageID = "<other@x>"
	p = append(p, Article{MessageID: "<more@x>"})
	if again := st.Posted(); len(again) != 1 || again[0].Headers[0] != "Subject: s" ||
		len(again[0].Body) != 1 || again[0].MessageID != "<p@x>" {
		t.Fatalf("Posted after mutating the result = %+v", again)
	}
	_ = p

	add := Article{MessageID: "<s@x>", Headers: []string{"Subject: s"}, Body: []string{"b"}}
	n := st.AddArticle("g", add)
	add.Headers[0] = "Subject: changed"
	a, _ := st.Article("g", n)
	if a.Headers[0] != "Subject: s" {
		t.Fatalf("AddArticle kept the caller's Headers slice")
	}
	a.Headers[0] = "Subject: mutated"
	a.Body[0] = "mutated"
	ov := st.Overview("g", 1, 0)
	ov[0].Headers[0] = "Subject: mutated ov"
	bm, _, _ := st.ByMessageID("<s@x>")
	bm.Body[0] = "mutated bm"
	if a2, _ := st.Article("g", n); a2.Headers[0] != "Subject: s" || a2.Body[0] != "b" {
		t.Fatalf("store changed through returned copies: %+v", a2)
	}
}

func TestArticleHeader(t *testing.T) {
	a := Article{Headers: []string{
		"From: a@b",
		"SUBJECT:  first  ",
		"X-Folded: one",
		"\ttwo",
		" three",
		"Subject: second",
		"Empty:",
		"NoColonLine",
		": no name",
	}}
	cases := map[string]string{
		"subject":  "first",
		"Subject":  "first",
		"from":     "a@b",
		"x-folded": "one\ttwo three",
		"Empty":    "",
		"Missing":  "",
		"From ":    "",
		"":         "",
	}
	for name, want := range cases {
		if got := a.Header(name); got != want {
			t.Errorf("Header(%q) = %q, want %q", name, got, want)
		}
	}
	if got := (Article{}).Header("Subject"); got != "" {
		t.Errorf("Header on empty article = %q", got)
	}
}

func TestArticleWithDefaults(t *testing.T) {
	date := time.Date(2024, 3, 5, 6, 7, 8, 0, time.FixedZone("", 2*3600))
	a := Article{Number: 7, MessageID: "<w@x>", Date: date, Headers: []string{"Subject: s"}}
	got := a.WithDefaults("g.one")
	want := []string{"Subject: s", "Message-ID: <w@x>", "Newsgroups: g.one", "Date: Tue, 05 Mar 2024 06:07:08 +0200"}
	if !reflect.DeepEqual(got.Headers, want) {
		t.Fatalf("WithDefaults headers = %q, want %q", got.Headers, want)
	}
	if !reflect.DeepEqual(a.Headers, []string{"Subject: s"}) {
		t.Fatalf("WithDefaults changed the receiver: %q", a.Headers)
	}

	// present headers (any case) are kept, nothing duplicated
	full := Article{MessageID: "<f@x>", Date: date, Headers: []string{
		"message-id: <f@x>", "NEWSGROUPS: a,b", "date: Mon, 01 Jan 2001 00:00:00 +0000",
	}}
	if got := full.WithDefaults("g.one"); !reflect.DeepEqual(got.Headers, full.Headers) {
		t.Fatalf("WithDefaults on complete article = %q", got.Headers)
	}

	// only the missing one is added; the id comes from the header when MessageID is empty
	part := Article{Headers: []string{"Message-ID: <h@x>", "Date: Mon, 01 Jan 2001 00:00:00 +0000"}}
	got = part.WithDefaults("g.two")
	if !reflect.DeepEqual(got.Headers, append(part.Headers[:2:2], "Newsgroups: g.two")) {
		t.Fatalf("WithDefaults partial = %q", got.Headers)
	}
	if got.MessageID != "<h@x>" {
		t.Fatalf("MessageID from header = %q", got.MessageID)
	}

	// no id anywhere: a generated one; empty group: no Newsgroups
	bare := Article{Number: 3, Date: date}.WithDefaults("")
	if bare.MessageID == "" || bare.Header("Message-ID") != bare.MessageID {
		t.Fatalf("generated id %q, header %q", bare.MessageID, bare.Header("Message-ID"))
	}
	if bare.MessageID != "<3@fakeserver.invalid>" {
		t.Fatalf("generated id for empty group %q, want <3@fakeserver.invalid>", bare.MessageID)
	}
	if bare.Header("Newsgroups") != "" {
		t.Fatalf("Newsgroups added for empty group: %q", bare.Headers)
	}
	gen := Article{Number: 3, Date: date}.WithDefaults("g")
	if gen.MessageID != "<3.g@fakeserver.invalid>" {
		t.Fatalf("generated id %q", gen.MessageID)
	}

	// zero Date gets a parseable Date header
	z := Article{MessageID: "<z@x>"}.WithDefaults("g")
	if z.Date.IsZero() {
		t.Fatalf("zero Date not defaulted")
	}
	if _, err := time.Parse(DateLayout, z.Header("Date")); err != nil {
		t.Fatalf("Date header %q: %v", z.Header("Date"), err)
	}
}

func TestStoreConcurrent(t *testing.T) {
	st := &Store{}
	const workers, per = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				a := art(fmt.Sprintf("<%d.%d@x>", w, i), "b")
				st.AddArticle("conc.one", a)
				st.AddArticle("conc.two", a)
				st.AddPosted(a)
				_ = st.Groups()
				_ = st.Overview("conc.one", 1, 0)
				_, _ = st.Next("conc.two", int64(i))
			}
		}(w)
	}
	wg.Wait()

	const total = workers * per
	for _, name := range []string{"conc.one", "conc.two"} {
		g, ok := st.Group(name)
		if !ok || g.Count != total || g.Low != 1 || g.High != total {
			t.Fatalf("Group(%s) = %+v, want Count/High %d", name, g, total)
		}
		nums := st.Numbers(name)
		for i, n := range nums {
			if n != int64(i+1) {
				t.Fatalf("%s numbers not contiguous at %d: %d", name, i, n)
			}
		}
	}
	if got := len(st.Posted()); got != total {
		t.Fatalf("Posted = %d, want %d", got, total)
	}
	for w := 0; w < workers; w++ {
		for i := 0; i < per; i++ {
			if _, locs, ok := st.ByMessageID(fmt.Sprintf("<%d.%d@x>", w, i)); !ok || len(locs) != 2 {
				t.Fatalf("ByMessageID(<%d.%d@x>) = %v, %v", w, i, locs, ok)
			}
		}
	}
}
