package yenc

import (
	"bytes"
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

func TestCorpusFresh(t *testing.T) {
	a := Corpus()
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
