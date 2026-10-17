package fakeserver

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// DateLayout is the RFC 5322 date format used by Article.WithDefaults for the Date header.
const DateLayout = "Mon, 02 Jan 2006 15:04:05 -0700"

// Article is one stored article. Headers holds the header lines without CRLF ("Name: value";
// continuation lines start with a space or tab), Body the body lines without CRLF and without
// dot-stuffing. Number is the article number in the group it was returned from.
type Article struct {
	Number    int64
	MessageID string
	Headers   []string
	Body      []string
	Date      time.Time
}

// Group is the state of one newsgroup. An empty group has Low 1, High 0 and Count 0; otherwise
// Low is the lowest existing number, High the highest number ever assigned and Count the number
// of articles (holes from explicit numbers are not counted).
type Group struct {
	Name, Description string
	Low, High, Count  int64
	Posting           bool
}

// Location is one place an article is filed under: a group and its number there.
type Location struct {
	Group  string
	Number int64
}

// Store holds groups, articles and posted articles for the stateful mode. All methods are safe
// for concurrent use. The zero value is ready to use (&Store{} works like NewStore()). Every
// returned value is a copy: callers cannot change the store through it.
type Store struct {
	mu     sync.Mutex
	groups map[string]*storeGroup
	byID   map[string][]Location // message-id → every location, in insertion order
	posted []Article
}

// storeGroup is one group's metadata plus its articles; nums is kept sorted ascending.
type storeGroup struct {
	meta     Group
	articles map[int64]Article
	nums     []int64
}

// NewStore returns an empty Store.
func NewStore() *Store {
	st := &Store{}
	st.mu.Lock()
	st.initLocked()
	st.mu.Unlock()
	return st
}

// initLocked creates the maps of a zero-value Store. Callers hold st.mu.
func (st *Store) initLocked() {
	if st.groups == nil {
		st.groups = make(map[string]*storeGroup)
	}
	if st.byID == nil {
		st.byID = make(map[string][]Location)
	}
}

// groupLocked returns the group, creating it (Posting true, empty) when it does not exist.
// Callers hold st.mu.
func (st *Store) groupLocked(name string) *storeGroup {
	st.initLocked()
	g, ok := st.groups[name]
	if !ok {
		g = &storeGroup{
			meta:     Group{Name: name, Low: 1, High: 0, Posting: true},
			articles: make(map[int64]Article),
		}
		st.groups[name] = g
	}
	return g
}

// AddGroup creates a group with Posting true. It is idempotent: an existing group keeps its
// articles; a non-empty description replaces the stored one.
func (st *Store) AddGroup(name, description string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	g := st.groupLocked(name)
	if description != "" {
		g.meta.Description = description
	}
}

// AddArticle files a copy of a in group and returns its number there: a.Number when > 0 (a
// number that is already taken panics, it is a test-setup bug), else High+1 (the first article
// of a group gets 1). A zero Date becomes time.Now(); an empty MessageID is taken from the
// Message-ID header when present. A group that does not exist is created. To file one article
// in several groups, call AddArticle once per group; ByMessageID then returns every location.
// An empty group name or a negative number panics as well.
func (st *Store) AddArticle(group string, a Article) int64 {
	if group == "" {
		panic("fakeserver: Store.AddArticle with empty group name")
	}
	if a.Number < 0 {
		panic(fmt.Sprintf("fakeserver: Store.AddArticle(%q): negative article number %d", group, a.Number))
	}
	a = a.clone()
	if a.Date.IsZero() {
		a.Date = time.Now()
	}
	if a.MessageID == "" {
		a.MessageID = a.Header("Message-ID")
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	g := st.groupLocked(group)
	n := a.Number
	if n > 0 {
		if _, taken := g.articles[n]; taken {
			panic(fmt.Sprintf("fakeserver: Store.AddArticle(%q): article number %d is already taken", group, n))
		}
	} else {
		n = g.meta.High + 1
	}
	a.Number = n
	g.articles[n] = a
	i := sort.Search(len(g.nums), func(i int) bool { return g.nums[i] >= n })
	g.nums = slices.Insert(g.nums, i, n)
	g.meta.Count = int64(len(g.nums))
	g.meta.Low = g.nums[0]
	g.meta.High = max(g.meta.High, n)
	if a.MessageID != "" {
		st.byID[a.MessageID] = append(st.byID[a.MessageID], Location{Group: group, Number: n})
	}
	return n
}

// Article returns a copy of article n of group.
func (st *Store) Article(group string, n int64) (Article, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[group]
	if !ok {
		return Article{}, false
	}
	a, ok := g.articles[n]
	if !ok {
		return Article{}, false
	}
	return a.clone(), true
}

// ByMessageID returns a copy of the first article stored under id (its Number is the one in the
// first location's group) and every location it was filed under, in insertion order.
func (st *Store) ByMessageID(id string) (Article, []Location, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	locs := st.byID[id]
	if len(locs) == 0 {
		return Article{}, nil, false
	}
	first := locs[0]
	a := st.groups[first.Group].articles[first.Number]
	return a.clone(), slices.Clone(locs), true
}

// Group returns the state of one group.
func (st *Store) Group(name string) (Group, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[name]
	if !ok {
		return Group{}, false
	}
	return g.meta, true
}

// Groups returns every group, sorted by name.
func (st *Store) Groups() []Group {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Group, 0, len(st.groups))
	for _, g := range st.groups {
		out = append(out, g.meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Numbers returns the existing article numbers of group, ascending (for LISTGROUP). An unknown
// or empty group gives an empty slice.
func (st *Store) Numbers(group string) []int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[group]
	if !ok {
		return []int64{}
	}
	out := make([]int64, len(g.nums))
	copy(out, g.nums)
	return out
}

// Overview returns copies of the articles of group with numbers within [from, to], ascending.
// to <= 0 means the group's High; from > to gives an empty slice.
func (st *Store) Overview(group string, from, to int64) []Article {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []Article{}
	g, ok := st.groups[group]
	if !ok {
		return out
	}
	if to <= 0 {
		to = g.meta.High
	}
	if from > to {
		return out
	}
	i := sort.Search(len(g.nums), func(i int) bool { return g.nums[i] >= from })
	for ; i < len(g.nums) && g.nums[i] <= to; i++ {
		out = append(out, g.articles[g.nums[i]].clone())
	}
	return out
}

// AddPosted records a copy of an article accepted by POST. It does not file the article into any
// group: the handler does that itself with AddArticle for each group of its Newsgroups header.
func (st *Store) AddPosted(a Article) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.posted = append(st.posted, a.clone())
}

// Posted returns copies of the articles recorded by AddPosted, in order.
func (st *Store) Posted() []Article {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Article, len(st.posted))
	for i, a := range st.posted {
		out[i] = a.clone()
	}
	return out
}

// Next returns the lowest existing number of group strictly above n (for NEXT).
func (st *Store) Next(group string, n int64) (int64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[group]
	if !ok {
		return 0, false
	}
	i := sort.Search(len(g.nums), func(i int) bool { return g.nums[i] > n })
	if i == len(g.nums) {
		return 0, false
	}
	return g.nums[i], true
}

// Last returns the highest existing number of group strictly below n (for LAST).
func (st *Store) Last(group string, n int64) (int64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[group]
	if !ok {
		return 0, false
	}
	i := sort.Search(len(g.nums), func(i int) bool { return g.nums[i] >= n })
	if i == 0 {
		return 0, false
	}
	return g.nums[i-1], true
}

// Header returns the value of the first header line named name (case-insensitive), with
// continuation lines unfolded and surrounding white space trimmed; "" when absent.
func (a Article) Header(name string) string {
	for i, line := range a.Headers {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 || !strings.EqualFold(line[:colon], name) {
			continue
		}
		v := line[colon+1:]
		for _, cont := range a.Headers[i+1:] {
			if cont == "" || (cont[0] != ' ' && cont[0] != '\t') {
				break
			}
			v += cont
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// hasHeader reports whether a header line named name exists (even with an empty value).
func (a Article) hasHeader(name string) bool {
	for _, line := range a.Headers {
		colon := strings.IndexByte(line, ':')
		if colon > 0 && strings.EqualFold(line[:colon], name) {
			return true
		}
	}
	return false
}

// WithDefaults returns a copy of a with the Message-ID, Newsgroups and Date header lines appended
// when missing (existing lines are kept unchanged). Message-ID uses a.MessageID, else the
// existing header, else "<number.group@fakeserver.invalid>"; the copy's MessageID is set to that
// value. Newsgroups is group (not added when group is ""). Date uses a.Date (time.Now() when
// zero, also stored in the copy) in DateLayout.
func (a Article) WithDefaults(group string) Article {
	a = a.clone()
	if a.MessageID == "" {
		a.MessageID = a.Header("Message-ID")
	}
	if a.MessageID == "" {
		a.MessageID = fmt.Sprintf("<%d.%s@fakeserver.invalid>", a.Number, group)
	}
	if !a.hasHeader("Message-ID") {
		a.Headers = append(a.Headers, "Message-ID: "+a.MessageID)
	}
	if group != "" && !a.hasHeader("Newsgroups") {
		a.Headers = append(a.Headers, "Newsgroups: "+group)
	}
	if a.Date.IsZero() {
		a.Date = time.Now()
	}
	if !a.hasHeader("Date") {
		a.Headers = append(a.Headers, "Date: "+a.Date.Format(DateLayout))
	}
	return a
}

// clone returns a with its own copies of Headers and Body.
func (a Article) clone() Article {
	a.Headers = slices.Clone(a.Headers)
	a.Body = slices.Clone(a.Body)
	return a
}
