package fakeserver

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultGenLimit is Generator.Limit when it is 0: the most article numbers one OVER, XOVER or
// LISTGROUP of a generated group may span.
const DefaultGenLimit = 100000

// genEpoch is the Date of generated article 0; article n is n seconds later.
var genEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Generator describes a generated group (Store.AddGenerated): a group of any size whose
// articles are made on demand and never stored, like a provider's group of 10^7 or 10^9
// articles. Every number from Low to High exists unless Article reports a hole.
type Generator struct {
	Low, High int64

	// Article returns article n of group; false makes n a hole. The store sets Number to n,
	// and a zero Date (2020-01-01 plus n seconds) and Arrived (= Date). nil: GeneratedArticle.
	Article func(group string, n int64) (Article, bool)

	// Lookup returns the number of the article with message-id id, for ARTICLE <id>, OVER
	// <id> and ByMessageID. nil: ids of the form "<n.group@fakeserver.invalid>" (those of
	// GeneratedArticle).
	Lookup func(id string) (int64, bool)

	// Limit is the most article numbers one OVER, XOVER or LISTGROUP may span (holes count);
	// a larger request gets "503 fakeserver: ..." instead of a reply the test cannot hold in
	// memory. 0: DefaultGenLimit; < 0: no limit.
	Limit int64
}

// GeneratedArticle is the default article n of a generated group: From poster@generated.invalid,
// Subject "generated article n in group", Message-ID "<n.group@fakeserver.invalid>", Date
// 2020-01-01T00:00:00Z plus n seconds, and one body line "body of generated article n". The
// same n always gives the same article.
func GeneratedArticle(group string, n int64) (Article, bool) {
	id := generatedID(n, group)
	return Article{
		Number:    n,
		MessageID: id,
		Headers: []string{
			"From: poster@generated.invalid",
			fmt.Sprintf("Subject: generated article %d in %s", n, group),
			"Message-ID: " + id,
		},
		Body: []string{fmt.Sprintf("body of generated article %d", n)},
		Date: genEpoch.Add(time.Duration(n) * time.Second),
	}, true
}

// generatedNumber parses "<n.group@fakeserver.invalid>" for group.
func generatedNumber(group, id string) (int64, bool) {
	s, ok := strings.CutPrefix(id, "<")
	if !ok {
		return 0, false
	}
	if s, ok = strings.CutSuffix(s, "."+group+"@fakeserver.invalid>"); !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// limit returns the span limit of g (0 = none).
func (g *Generator) limit() int64 {
	switch {
	case g.Limit < 0:
		return 0
	case g.Limit == 0:
		return DefaultGenLimit
	}
	return g.Limit
}

// article returns article n of the generated group, or false for a hole or a number out of range.
func (g *Generator) article(group string, low, n int64) (Article, bool) {
	if n < low || n > g.High {
		return Article{}, false
	}
	gen := g.Article
	if gen == nil {
		gen = GeneratedArticle
	}
	a, ok := gen(group, n)
	if !ok {
		return Article{}, false
	}
	a = a.clone()
	a.Number = n
	if a.MessageID == "" {
		a.MessageID = a.Header("Message-ID")
	}
	if a.MessageID == "" {
		a.MessageID = generatedID(n, group)
	}
	if a.Date.IsZero() {
		a.Date = genEpoch.Add(time.Duration(n) * time.Second)
	}
	if a.Arrived.IsZero() {
		a.Arrived = a.Date
	}
	return a, true
}

// lookup returns the number of id in the generated group.
func (g *Generator) lookup(group, id string) (int64, bool) {
	if g.Lookup != nil {
		return g.Lookup(id)
	}
	return generatedNumber(group, id)
}

// AddGenerated creates group (or turns an existing group without articles into one) as a
// generated group: its articles Low..High come from gen and are never stored. Count is
// High-Low+1 (holes included, like the estimate of a real server). Store methods work on it:
// Article, ByMessageID, Overview, Next, Last, Numbers (only for spans up to the limit, else it
// panics), Expire (raises Low) and SetGeneratedHigh (new articles arrive); AddArticle,
// RemoveArticle and Cancel do not (AddArticle panics, the others find nothing). An empty
// group name, an existing group with stored articles, Low < 1 or High < Low-1 panic.
func (st *Store) AddGenerated(group, description string, gen Generator) {
	if group == "" {
		panic("fakeserver: Store.AddGenerated with empty group name")
	}
	if gen.Low < 1 || gen.High < gen.Low-1 {
		panic(fmt.Sprintf("fakeserver: Store.AddGenerated(%q): bad range %d-%d", group, gen.Low, gen.High))
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	g := st.groupLocked(group)
	if len(g.nums) > 0 {
		panic(fmt.Sprintf("fakeserver: Store.AddGenerated(%q): group has stored articles", group))
	}
	if description != "" {
		g.meta.Description = description
	}
	gc := gen
	g.gen = &gc
	g.meta.Low, g.meta.High = gen.Low, gen.High
	g.meta.Count = gen.High - gen.Low + 1
}

// SetGeneratedHigh moves the High of a generated group to high (new articles arrive; a lower
// high removes the newest ones). It returns false when group is not a generated group or high
// is below Low-1.
func (st *Store) SetGeneratedHigh(group string, high int64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[group]
	if !ok || g.gen == nil || high < g.meta.Low-1 {
		return false
	}
	g.gen.High = high
	g.meta.High = high
	g.meta.Count = high - g.meta.Low + 1
	return true
}

// GenLimit returns the span limit of a generated group (0 for none, and for a stored group).
func (st *Store) GenLimit(group string) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	if g, ok := st.groups[group]; ok && g.gen != nil {
		return g.gen.limit()
	}
	return 0
}

// numbersLocked returns the existing numbers of the generated group g within [from, to].
// Callers hold st.mu.
func (g *storeGroup) numbersLocked(group string, from, to int64) []int64 {
	out := []int64{}
	for n := max(from, g.meta.Low); n <= min(to, g.meta.High); n++ {
		if _, ok := g.gen.article(group, g.meta.Low, n); ok {
			out = append(out, n)
		}
	}
	return out
}

// generatedByIDLocked looks id up in the generated groups (in name order): the article and its
// one location. Callers hold st.mu.
func (st *Store) generatedByIDLocked(id string) (Article, []Location, bool) {
	var names []string
	for name, g := range st.groups {
		if g.gen != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		g := st.groups[name]
		n, ok := g.gen.lookup(name, id)
		if !ok {
			continue
		}
		if a, ok := g.gen.article(name, g.meta.Low, n); ok && a.MessageID == id {
			return a, []Location{{Group: name, Number: n}}, true
		}
	}
	return Article{}, nil, false
}

// NumbersIn returns the existing article numbers of group within [from, to], ascending (to <= 0
// means High). Unlike Numbers it works on a generated group of any size as long as the range is
// within its limit (a larger one panics).
func (st *Store) NumbersIn(group string, from, to int64) []int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	g, ok := st.groups[group]
	if !ok {
		return []int64{}
	}
	if to <= 0 {
		to = g.meta.High
	}
	if g.gen != nil {
		from, to = max(from, g.meta.Low), min(to, g.meta.High)
		if lim := g.gen.limit(); lim > 0 && to-from+1 > lim {
			panic(fmt.Sprintf("fakeserver: Store.NumbersIn(%q, %d, %d): span over the limit of %d", group, from, to, lim))
		}
		return g.numbersLocked(group, from, to)
	}
	out := []int64{}
	i := sort.Search(len(g.nums), func(i int) bool { return g.nums[i] >= from })
	for ; i < len(g.nums) && g.nums[i] <= to; i++ {
		out = append(out, g.nums[i])
	}
	return out
}
