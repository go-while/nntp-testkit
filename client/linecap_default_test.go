package client

import (
	"strings"
	"testing"
)

// TestDefaultLineCapRefuses: with no option set, a finite 1 MiB line from the server (16 times
// the default cap) is refused with an error instead of returned whole. The test uses only API
// that is older than the cap, so it also fails on the code before it.
func TestDefaultLineCapRefuses(t *testing.T) {
	p := newPeer(t, scripted("200 hi\r\n"+strings.Repeat("y", 1<<20)+"\r\n"))
	c := Dial(t, p.addr())
	if line, err := c.ReadLine(); err == nil {
		t.Fatalf("a %d-byte line was accepted", len(line))
	}
}
