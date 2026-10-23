// Package yenc writes yEnc-encoded article bodies for tests: single-part and multi-part posts as
// body lines, ready for a fakeserver.Article, and a fakeserver.Generator that serves a group of
// them. Options cover the choices posting tools differ in (line length, which characters to
// escape) and broken posts (no =ybegin or =yend line, a wrong size or CRC). Data makes
// reproducible test data in a few profiles. It is an encoder for tests, not a decoder; only the
// standard library is used.
//
// Contract (plan nntp-testkit). Implemented in yenc.go:
//
//	type Options struct {
//		Line         int    // characters per line, before an escape at its end adds one; 0 = 128
//		RawDots      bool   // leave a '.' at the start of a line unescaped: the server dot-stuffs it
//		RawSpaces    bool   // leave tab and space at the start and end of a line unescaped
//		ExtraEscapes string // more encoded characters to escape everywhere, as some posting tools do
//		NoBegin      bool   // leave out =ybegin (and =ypart)
//		NoEnd        bool   // leave out =yend
//		BadSize      bool   // =yend gives one byte more than the data
//		BadCRC       bool   // =yend gives CRC32s (pcrc32 and crc32) with the lowest bit flipped
//	}
//	// The zero value encodes as yEnc 1.3 recommends and most posting tools do: NUL, LF, CR and
//	// '=' escaped everywhere, tab and space at the start and end of a line, '.' at its start.
//	// An escape that would make a line start with "=y" (the start of a control line) is not
//	// written, whatever ExtraEscapes says.
//
//	func Encode(name string, data []byte, o Options) []string
//	// The body lines of a single-part post: "=ybegin line=L size=N name=NAME", the data lines,
//	// "=yend size=N crc32=XXXXXXXX". Lines carry no CRLF and no dot-stuffing: the server adds
//	// those, as for any fakeserver.Article body. 0 bytes of data give no data line.
//
//	func EncodeParts(name string, data []byte, partSize int, o Options) [][]string
//	// One post per part of partSize bytes (the last may be shorter): "=ybegin part=P total=T
//	// line=L size=N name=NAME", "=ypart begin=B end=E" (1-based, inclusive), the data lines,
//	// "=yend size=S part=P pcrc32=XXXXXXXX", and on the last part " crc32=XXXXXXXX" of the
//	// whole data. It panics unless partSize > 0 and len(data) > 0.
//
//	type Profile string
//	const (
//		Random  Profile = "random"  // uniform bytes, like compressed or encrypted files (most of Usenet): an escape every ~64 bytes
//		Plain   Profile = "plain"   // no byte encodes to NUL, LF, CR, '=', '.', tab or space: no escapes
//		Escapes Profile = "escapes" // every second byte encodes to NUL, LF, CR or '='
//		Edges   Profile = "edges"   // a third of the bytes encode to '.', tab or space, which encoders treat specially at line ends
//	)
//	func Data(p Profile, size int, seed uint64) []byte
//	// size bytes of profile p from a PCG seeded with (seed, seed+1): the same arguments always
//	// give the same bytes. It panics on an unknown profile.
//
//	func Generator(p Profile, low, high int64, size func(n int64) int, o Options) fakeserver.Generator
//	// For Store.AddGenerated: article n (low..high) is a single-part post named "<p>-<n>.bin"
//	// of Data(p, size(n), uint64(n)), with the headers, Message-ID and Date of
//	// fakeserver.GeneratedArticle (so Lookup stays nil) and the Subject posting tools write:
//	// `<group> - "<name>" yEnc (1/1) <size>`. size nil: 750000 bytes for every article.
package yenc
