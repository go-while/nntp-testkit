package yenc

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/fakeserver"
)

func TestCorpusShape(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Corpus() {
		if c.Name == "" || strings.ContainsAny(c.Name, " \t/") || seen[c.Name] {
			t.Errorf("bad or duplicate name %q", c.Name)
		}
		seen[c.Name] = true
		if c.Why == "" || len(c.Lines) == 0 {
			t.Errorf("%s: empty Why or Lines", c.Name)
		}
		switch c.Want {
		case fakeserver.WantError:
			if c.Data != nil {
				t.Errorf("%s: WantError with Data", c.Name)
			}
		case fakeserver.WantAccept, fakeserver.WantEither:
			if c.Data == nil {
				t.Errorf("%s: Want %q without Data", c.Name, c.Want)
			}
		default:
			t.Errorf("%s: Want %q", c.Name, c.Want)
		}
		for _, l := range c.Lines {
			if strings.ContainsAny(l, "\r\n") {
				t.Errorf("%s: line %q holds CR or LF", c.Name, l)
			}
		}
	}
	if _, ok := CaseByName("stray-yend-after-block"); !ok {
		t.Error("no stray-yend-after-block")
	}
	if _, ok := CaseByName("no-such-case"); ok {
		t.Error("CaseByName found a case that does not exist")
	}
}

// The accepted cases decode to Data up to their first =yend (a minimal decoder for the test).
func TestCorpusData(t *testing.T) {
	for _, c := range Corpus() {
		if c.Data == nil {
			continue
		}
		if got := decodeUntilYend(c.Lines); !bytes.Equal(got, c.Data) {
			t.Errorf("%s: decodes to %d bytes, want the %d of Data", c.Name, len(got), len(c.Data))
		}
	}
	c, _ := CaseByName("stray-yend-after-block")
	if n := len(c.Lines); c.Lines[n-1] != "=yend size=0" || !strings.HasPrefix(c.Lines[n-2], "=yend size=300 ") {
		t.Errorf("stray-yend-after-block ends %q", c.Lines[len(c.Lines)-2:])
	}
}

// The broken cases differ from the good block where they claim to; the stray =yend follows a
// block whose size and CRC match Data.
func TestCorpusTrailers(t *testing.T) {
	c, _ := CaseByName("stray-yend-after-block")
	want := fmt.Sprintf("=yend size=%d crc32=%08x", len(c.Data), crc32.ChecksumIEEE(c.Data))
	if got := c.Lines[len(c.Lines)-2]; got != want {
		t.Errorf("stray-yend-after-block block ends %q, want %q", got, want)
	}
	last := func(name string) string { c, _ := CaseByName(name); return c.Lines[len(c.Lines)-1] }
	if l := last("bad-crc32"); l == want || !strings.HasPrefix(l, fmt.Sprintf("=yend size=%d ", len(c.Data))) {
		t.Errorf("bad-crc32 ends %q", l)
	}
	if l := last("bad-size"); !strings.HasPrefix(l, fmt.Sprintf("=yend size=%d crc32=%08x", len(c.Data)+1, crc32.ChecksumIEEE(c.Data))) {
		t.Errorf("bad-size ends %q", l)
	}
	if l := last("no-yend"); strings.HasPrefix(l, "=yend") {
		t.Errorf("no-yend ends %q", l)
	}
}

func TestCorpusFresh(t *testing.T) {
	a := Corpus()
	// no two cases of one call share Data
	for i := range a {
		for j := i + 1; j < len(a); j++ {
			if len(a[i].Data) > 0 && len(a[j].Data) > 0 && &a[i].Data[0] == &a[j].Data[0] {
				t.Errorf("%s and %s share Data", a[i].Name, a[j].Name)
			}
		}
	}
	a[0].Lines[0] = "changed"
	a[0].Data[0]++
	b := Corpus()
	if b[0].Lines[0] == "changed" || bytes.Equal(a[0].Data, b[0].Data) {
		t.Fatal("Corpus shares state between calls")
	}
}

func decodeUntilYend(lines []string) []byte {
	var out []byte
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "=ybegin "), strings.HasPrefix(l, "=ypart "):
			continue
		case strings.HasPrefix(l, "=yend"):
			return out
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if c == '=' && i+1 < len(l) {
				i++
				c = l[i] - 64
			}
			out = append(out, c-42)
		}
	}
	return out
}
