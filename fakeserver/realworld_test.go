package fakeserver

import (
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// Behaviours of real servers: removal with a kept high-water mark, numbers per connection, old
// LISTGROUP, OVER/XOVER variants, connection limits, slow and changing replies.

func TestStoreRemove(t *testing.T) {
	st := NewStore()
	for i := 1; i <= 5; i++ {
		st.AddArticle("g", art("<r"+string(rune('0'+i))+"@x>"))
	}
	st.AddArticle("h", art("<r3@x>")) // crossposted: g:3, h:1

	if !st.RemoveArticle("g", 5) || st.RemoveArticle("g", 5) || st.RemoveArticle("nope", 1) {
		t.Fatal("RemoveArticle results")
	}
	g, _ := st.Group("g")
	if g.Low != 1 || g.High != 5 || g.Count != 4 {
		t.Errorf("after removing the newest: %+v, want low 1 high 5 (kept) count 4", g)
	}
	if n := st.AddArticle("g", art("<r6@x>")); n != 6 {
		t.Errorf("next number %d, want 6 (5 is not reused)", n)
	}
	if n := st.AddArticle("g", Article{Number: 5, MessageID: "<r5b@x>"}); n != 5 {
		t.Errorf("explicit refill %d", n)
	}

	if n := st.Expire("g", 3); n != 2 {
		t.Errorf("Expire removed %d, want 2", n)
	}
	g, _ = st.Group("g")
	if g.Low != 3 || g.Count != 4 {
		t.Errorf("after Expire: %+v", g)
	}
	if _, ok := st.Article("g", 1); ok {
		t.Error("article 1 still there")
	}
	if _, _, ok := st.ByMessageID("<r1@x>"); ok {
		t.Error("<r1@x> still indexed")
	}

	if n := st.Cancel("<r3@x>"); n != 2 {
		t.Errorf("Cancel removed %d locations, want 2", n)
	}
	if _, _, ok := st.ByMessageID("<r3@x>"); ok {
		t.Error("<r3@x> still indexed")
	}
	h, _ := st.Group("h")
	if h.Count != 0 || h.Low != 2 || h.High != 1 {
		t.Errorf("emptied group %+v, want count 0 low 2 high 1", h)
	}
	if n := st.Cancel("<none@x>"); n != 0 {
		t.Errorf("Cancel of an unknown id: %d", n)
	}
	wantEqual(t, "numbers", st.Numbers("g"), []int64{4, 5, 6})
}

func TestStoreRemoveServed(t *testing.T) {
	st := testStore()
	st.RemoveArticle("alt.test", 3) // the crosspost stays in comp.lang.go
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	wantEqual(t, "GROUP", wantCode(t, c, "GROUP alt.test", 211), "211 2 1 3 alt.test")
	wantCode(t, c, "ARTICLE 3", 423)
	_, ov := wantBlock(t, c, "XOVER <x3@x>", 224)
	if len(ov) != 1 || !strings.HasSuffix(ov[0], "Xref: fakeserver comp.lang.go:1") {
		t.Errorf("Xref after removal: %q", ov)
	}
	st.Expire("alt.test", 10)
	wantEqual(t, "GROUP empty", wantCode(t, c, "GROUP alt.test", 211), "211 0 4 3 alt.test")
	wantCode(t, c, "XOVER 1-3", 423)
}

func TestStoreGroupNumbers(t *testing.T) {
	st := testStore()
	st.RemoveArticle("alt.test", 3)
	// connection 1 sees the store; later ones lag one article behind
	lag := func(conn int, g Group) Group {
		if conn > 1 && g.Name == "alt.test" {
			g.High--
			g.Count--
		}
		return g
	}
	s := New(t, Options{Store: st, StoreOpts: StoreOptions{GroupNumbers: lag}})
	c1 := client.Dial(t, s.Addr())
	c2 := client.Dial(t, s.Addr())
	wantEqual(t, "conn 1", wantCode(t, c1, "GROUP alt.test", 211), "211 2 1 3 alt.test")
	wantEqual(t, "conn 2", wantCode(t, c2, "GROUP alt.test", 211), "211 1 1 2 alt.test")
	line, nums := wantBlock(t, c2, "LISTGROUP alt.test", 211)
	wantEqual(t, "LISTGROUP status", line, "211 1 1 2 alt.test list follows")
	wantEqual(t, "LISTGROUP numbers", nums, []string{"1", "2"}) // the articles stay the stored ones
	_, act := wantBlock(t, c2, "LIST ACTIVE alt.test", 215)
	wantEqual(t, "LIST ACTIVE", act, []string{"alt.test 2 1 y"})

	// HighestExisting: the last number goes down with the newest article
	s2 := New(t, Options{Store: st, StoreOpts: StoreOptions{GroupNumbers: st.HighestExisting}})
	c3 := client.Dial(t, s2.Addr())
	wantEqual(t, "highest existing", wantCode(t, c3, "GROUP alt.test", 211), "211 2 1 2 alt.test")
	wantEqual(t, "highest existing, empty", wantCode(t, c3, "GROUP alt.empty", 211), "211 0 1 0 alt.empty")

	// "211 0 0 0" for an empty group
	zero := func(_ int, g Group) Group {
		if g.Count == 0 {
			g.Low, g.High = 0, 0
		}
		return g
	}
	s3 := New(t, Options{Store: st, StoreOpts: StoreOptions{GroupNumbers: zero}})
	c4 := client.Dial(t, s3.Addr())
	wantEqual(t, "zero", wantCode(t, c4, "GROUP alt.empty", 211), "211 0 0 0 alt.empty")
}

func TestStoreListgroupRange(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{ListgroupRange: "reject"}, Faults{})
	wantCode(t, c, "LISTGROUP alt.test 2-3", 501)
	_, nums := wantBlock(t, c, "LISTGROUP alt.test", 211)
	wantEqual(t, "reject: no range", nums, []string{"1", "2", "3"})

	_, c = storeServer(t, testStore(), StoreOptions{ListgroupRange: "ignore"}, Faults{})
	_, nums = wantBlock(t, c, "LISTGROUP alt.test 2-3", 211)
	wantEqual(t, "ignore", nums, []string{"1", "2", "3"})
	_, nums = wantBlock(t, c, "LISTGROUP alt.test garbage", 211)
	wantEqual(t, "ignore: any argument", nums, []string{"1", "2", "3"})
}

func TestStoreOverVariants(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{NoOver: true}, Faults{})
	_, caps := wantBlock(t, c, "CAPABILITIES", 101)
	for _, l := range caps {
		if strings.HasPrefix(l, "OVER") {
			t.Errorf("NoOver: CAPABILITIES has %q", l)
		}
	}
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "OVER 1-3", 500)
	wantBlock(t, c, "XOVER 1-3", 224)

	_, c = storeServer(t, testStore(), StoreOptions{NoXover: true}, Faults{})
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "XOVER 1-3", 500)
	wantBlock(t, c, "OVER 1-3", 224)

	_, c = storeServer(t, testStore(), StoreOptions{EmptyOverReply: []string{"420 No article(s) selected"}}, Faults{})
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "XOVER 7-9", 420)
	wantCode(t, c, "XOVER 9-", 420)
	wantBlock(t, c, "XOVER 1-3", 224)

	_, c = storeServer(t, testStore(), StoreOptions{EmptyOverReply: []string{"224 Overview follows", "."}}, Faults{})
	wantCode(t, c, "GROUP alt.test", 211)
	if _, ov := wantBlock(t, c, "OVER 7-9", 224); len(ov) != 0 {
		t.Errorf("empty 224: %q", ov)
	}
}

func TestStoreMaxOverRange(t *testing.T) {
	st := NewStore()
	for i := 0; i < 10; i++ {
		st.AddArticle("g", Article{})
	}
	numbers := func(ov []string) []string {
		var out []string
		for _, l := range ov {
			out = append(out, l[:strings.IndexByte(l, '\t')])
		}
		return out
	}
	_, c := storeServer(t, st, StoreOptions{MaxOverRange: 3}, Faults{})
	wantCode(t, c, "GROUP g", 211)
	_, ov := wantBlock(t, c, "OVER 2-9", 224)
	wantEqual(t, "truncated", numbers(ov), []string{"2", "3", "4"})
	_, ov = wantBlock(t, c, "OVER 8-", 224)
	wantEqual(t, "open range, within the cap", numbers(ov), []string{"8", "9", "10"})
	_, ov = wantBlock(t, c, "OVER 7-", 224)
	wantEqual(t, "open range, truncated", numbers(ov), []string{"7", "8", "9"})

	_, c = storeServer(t, st, StoreOptions{MaxOverRange: 3, OverRangeTooLarge: []string{"501 Range too large"}}, Faults{})
	wantCode(t, c, "GROUP g", 211)
	wantCode(t, c, "OVER 2-9", 501)
	_, ov = wantBlock(t, c, "OVER 2-4", 224)
	wantEqual(t, "within the cap", numbers(ov), []string{"2", "3", "4"})
}

func TestMaxConns(t *testing.T) {
	s := New(t, Options{Store: testStore(), MaxConns: 2})
	c1 := client.Dial(t, s.Addr())
	c2 := client.Dial(t, s.Addr())
	c3, err := client.TryDial(s.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	if c3.Greeting != "502 Too many connections" {
		t.Errorf("greeting %q", c3.Greeting)
	}
	if _, err := c3.ReadLine(); err == nil {
		t.Error("rejected connection stayed open")
	}
	if s.Open() != 2 || s.Accepted() != 3 {
		t.Errorf("open %d accepted %d, want 2 and 3", s.Open(), s.Accepted())
	}
	wantCode(t, c1, "QUIT", 205)
	s.WaitOpen(t, 1, 2*time.Second)
	c4 := client.Dial(t, s.Addr())
	wantCode(t, c4, "GROUP alt.test", 211)
	wantCode(t, c2, "GROUP alt.test", 211)

	s2 := New(t, Options{Store: testStore(), MaxConns: 1, MaxConnsGreeting: "400 Too many users"})
	client.Dial(t, s2.Addr())
	c5, err := client.TryDial(s2.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c5.Close()
	if c5.Greeting != "400 Too many users" {
		t.Errorf("custom greeting %q", c5.Greeting)
	}
}

func TestConnID(t *testing.T) {
	ids := make(chan int, 3)
	s := New(t, Options{Handler: func(c *Conn) { ids <- c.ID() }})
	for i := 0; i < 3; i++ {
		client.Dial(t, s.Addr())
		if id := <-ids; id != i+1 {
			t.Errorf("conn %d: ID %d", i+1, id)
		}
	}
}

func TestFaultLineDelay(t *testing.T) {
	_, c := storeServer(t, testStore(), StoreOptions{}, Faults{LineDelay: map[string]time.Duration{"XOVER": 60 * time.Millisecond}})
	wantCode(t, c, "GROUP alt.test", 211)
	start := time.Now()
	_, ov := wantBlock(t, c, "XOVER 1-3", 224)
	// status, 3 lines and "." = 5 lines, 4 pauses
	if d := time.Since(start); d < 200*time.Millisecond || len(ov) != 3 {
		t.Errorf("XOVER took %v (%d lines), want >= 240ms in all", d, len(ov))
	}
	start = time.Now()
	wantCode(t, c, "STAT 1", 223)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("STAT delayed %v", d)
	}
}

func TestFaultOverrideFrom(t *testing.T) {
	f := Faults{
		Override:     map[string][]string{"XOVER": {"480 Authentication required"}},
		OverrideLine: map[string][]string{"ARTICLE 1": {"502 Access denied"}},
		OverrideFrom: map[string]int{"XOVER": 3, "ARTICLE": 2},
	}
	s, c := storeServer(t, testStore(), StoreOptions{}, f)
	wantCode(t, c, "GROUP alt.test", 211)
	wantBlock(t, c, "XOVER 1-3", 224)
	wantBlock(t, c, "XOVER 1-3", 224)
	wantCode(t, c, "XOVER 1-3", 480)
	wantCode(t, c, "XOVER 1-3", 480)
	wantBlock(t, c, "ARTICLE 1", 220)
	wantCode(t, c, "ARTICLE 1", 502)
	// counted per connection
	c2 := client.Dial(t, s.Addr())
	wantCode(t, c2, "GROUP alt.test", 211)
	wantBlock(t, c2, "XOVER 1-3", 224)
}

func TestFaultOverrideTimes(t *testing.T) {
	f := Faults{
		Override:      map[string][]string{"XOVER": {"480 Authentication required"}},
		OverrideFrom:  map[string]int{"XOVER": 2},
		OverrideTimes: map[string]int{"XOVER": 1},
	}
	s, c := storeServer(t, testStore(), StoreOptions{}, f)
	wantCode(t, c, "GROUP alt.test", 211)
	wantBlock(t, c, "XOVER 1-3", 224)
	wantCode(t, c, "XOVER 1-3", 480)
	wantBlock(t, c, "XOVER 1-3", 224) // once only
	wantBlock(t, c, "XOVER 1-3", 224)
	c2 := client.Dial(t, s.Addr())
	wantCode(t, c2, "GROUP alt.test", 211)
	wantBlock(t, c2, "XOVER 1-3", 224)
	wantCode(t, c2, "XOVER 1-3", 480) // per connection
}

// TestStoreLoginDropped: a 480 from Faults.Override ends the login of an authenticated
// connection, so AUTHINFO works again (not 502) and then the command is answered normally.
func TestStoreLoginDropped(t *testing.T) {
	f := Faults{
		Override:      map[string][]string{"GROUP": {"480 Authentication required"}},
		OverrideFrom:  map[string]int{"GROUP": 2},
		OverrideTimes: map[string]int{"GROUP": 1},
	}
	_, c := storeServer(t, testStore(), StoreOptions{User: "u", Pass: "p"}, f)
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "GROUP alt.test", 480)
	wantCode(t, c, "LIST ACTIVE", 480) // the login is gone
	wantCode(t, c, "AUTHINFO USER u", 381)
	wantCode(t, c, "AUTHINFO PASS p", 281)
	wantCode(t, c, "GROUP alt.test", 211)
	wantCode(t, c, "AUTHINFO USER u", 502) // logged in again

	// without auth configured a 480 changes nothing
	f2 := Faults{Override: map[string][]string{"GROUP": {"480 Authentication required"}}}
	_, c2 := storeServer(t, testStore(), StoreOptions{}, f2)
	wantCode(t, c2, "GROUP alt.test", 480)
	wantBlock(t, c2, "LIST ACTIVE alt.test", 215)
}
