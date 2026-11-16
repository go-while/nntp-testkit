package fakeserver

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDefaultLineCapCloses: with no option set, a finite 1 MiB client line (16 times the
// default cap) ends the connection without a reply. Without a cap the server read the whole
// line, answered it and kept the connection open. The test uses only API that is older than
// the cap, so it also fails on the code before it.
func TestDefaultLineCapCloses(t *testing.T) {
	s := New(t, Options{})
	nc, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 512)
	if _, err := nc.Read(buf); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	go func() { _, _ = nc.Write([]byte(strings.Repeat("z", 1<<20) + "\r\n")) }()
	got, err := io.ReadAll(nc)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the connection stayed open after a 1 MiB line (read %q)", got)
	}
	if len(got) != 0 {
		t.Errorf("the server answered a line over the cap: %.200q", got)
	}
}
