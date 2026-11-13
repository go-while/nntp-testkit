package yenc

import (
	"slices"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// Case is one broken or odd yEnc post body for the test suite of a yEnc DECODER (a client
// library, a downloader, a SIMD decoder). Every decoder needs the same table, so it lives
// here once. A suite loops over Corpus, decodes c.Lines, or serves them as the body of a
// fakeserver.Article and fetches that, and checks the outcome against c.Want:
//
//	for _, c := range yenc.Corpus() {
//		t.Run(c.Name, func(t *testing.T) {
//			got, err := decode(c.Lines)
//			// WantAccept: err == nil and got == c.Data
//			// WantError: err != nil (no panic, no hang)
//			// WantEither: either; with err == nil, got == c.Data
//		})
//	}
//
// Want uses the classes of the reply corpus (fakeserver.WantError, WantEither, WantAccept).
// The cases are facts about yEnc 1.3 posts, written from scratch.
type Case struct {
	// Name is a unique short identifier fit for t.Run, e.g. "stray-yend-after-block".
	Name string
	// Lines are the body lines as Encode writes them: no CRLF, not dot-stuffed.
	Lines []string
	// Data are the decoded bytes a decoder returns when it accepts the case; nil when no
	// decoder may accept it (Want is WantError).
	Data []byte
	// Why says what is odd or broken.
	Why string
	// Want is the generic expected decoder outcome.
	Want fakeserver.Outcome
}

// CaseByName returns the case of that name.
func CaseByName(name string) (Case, bool) {
	for _, c := range Corpus() {
		if c.Name == name {
			return c, true
		}
	}
	return Case{}, false
}

// Corpus returns the yEnc cases, in a fixed order, a new slice each call.
func Corpus() []Case {
	data := Data(Escapes, 300, 1)
	good := Encode("a.bin", data, Options{Line: 64})
	with := func(lines ...string) []string { return append(slices.Clone(good), lines...) }
	bad := func(o Options) []string { o.Line = 64; return Encode("a.bin", data, o) }
	return []Case{
		{Name: "stray-yend-after-block", Lines: with("=yend size=0"), Data: data, Want: fakeserver.WantAccept,
			Why: "a second =yend after a complete block whose size and CRC match: the block ended at the first =yend, the stray line must not be checked against it"},
		{Name: "text-after-yend", Lines: with("", "-- ", "posted with a newsreader"), Data: data, Want: fakeserver.WantAccept,
			Why: "plain text (a signature) after =yend: yEnc data ends at =yend, what follows is not decoded"},
		{Name: "bad-crc32", Lines: bad(Options{BadCRC: true}), Want: fakeserver.WantError,
			Why: "=yend gives a crc32 that does not match the data"},
		{Name: "bad-size", Lines: bad(Options{BadSize: true}), Want: fakeserver.WantError,
			Why: "=yend gives a size one byte more than the data and than =ybegin says"},
		{Name: "no-yend", Lines: bad(Options{NoEnd: true}), Data: data, Want: fakeserver.WantEither,
			Why: "the post ends without =yend: the data may be cut short and nothing can be checked; a lenient decoder returns what it has"},
	}
}
