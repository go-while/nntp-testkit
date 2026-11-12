package fakeserver

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// rcServe starts a Store server with the case applied (apply picks Apply or ApplyLine) and
// returns the client connection after the greeting (none read for a greeting case).
func rcServe(t *testing.T, rc ReplyCase, apply func(ReplyCase, *Options)) net.Conn {
	t.Helper()
	o := Options{Store: NewStore()}
	apply(rc, &o)
	srv := New(t, o)
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func rcRead(t *testing.T, conn net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v (got %q)", n, err, buf)
	}
	return string(buf)
}

// rcExpect runs the case against a server and returns what the client got for it.
func rcExpect(t *testing.T, rc ReplyCase, apply func(ReplyCase, *Options)) string {
	t.Helper()
	conn := rcServe(t, rc, apply)
	if rc.IsGreeting() {
		return rcRead(t, conn, len(rc.Bytes()))
	}
	if g := rcRead(t, conn, len(DefaultGreeting)+2); g != DefaultGreeting+"\r\n" {
		t.Fatalf("greeting %q, want the default one", g)
	}
	if _, err := io.WriteString(conn, rc.Line+"\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	return rcRead(t, conn, len(rc.Bytes()))
}

func TestReplyCorpusShape(t *testing.T) {
	cases := ReplyCorpus()
	if len(cases) < 40 {
		t.Fatalf("%d cases, want at least 40", len(cases))
	}
	seen := map[string]bool{}
	for _, rc := range cases {
		if rc.Name == "" || strings.ContainsAny(rc.Name, " \t/") {
			t.Errorf("bad name %q", rc.Name)
		}
		if seen[rc.Name] {
			t.Errorf("duplicate name %q", rc.Name)
		}
		seen[rc.Name] = true
		if rc.Why == "" {
			t.Errorf("%s: empty Why", rc.Name)
		}
		switch rc.Family {
		case FamilyGreeting:
			if rc.Cmd != "" || rc.Line != "" {
				t.Errorf("%s: a greeting case has Cmd %q Line %q", rc.Name, rc.Cmd, rc.Line)
			}
			if !rc.IsGreeting() {
				t.Errorf("%s: IsGreeting false", rc.Name)
			}
		case FamilyCode, FamilyStatus, FamilyBlock:
			if rc.Cmd == "" || rc.Line == "" {
				t.Errorf("%s: Cmd %q or Line %q empty", rc.Name, rc.Cmd, rc.Line)
			}
			if rc.Cmd != strings.ToUpper(rc.Cmd) || rc.Cmd != strings.ToUpper(strings.Fields(rc.Line)[0]) {
				t.Errorf("%s: Cmd %q does not match Line %q", rc.Name, rc.Cmd, rc.Line)
			}
		default:
			t.Errorf("%s: unknown family %q", rc.Name, rc.Family)
		}
		if len(rc.Lines) == 0 {
			t.Errorf("%s: no Lines", rc.Name)
		}
		if rc.Want != WantError && rc.Want != WantEither {
			t.Errorf("%s: Want %q", rc.Name, rc.Want)
		}
		if rc.NeedsTimeout && rc.Want != WantError {
			t.Errorf("%s: NeedsTimeout with Want %q", rc.Name, rc.Want)
		}
		// a multi-line block ends with the terminator unless the case is about that
		if len(rc.Lines) > 1 && !rc.NeedsTimeout && rc.Lines[len(rc.Lines)-1] != "." && rc.Family != FamilyGreeting {
			t.Errorf("%s: block without terminator", rc.Name)
		}
	}
	for _, f := range []Family{FamilyCode, FamilyStatus, FamilyGreeting, FamilyBlock} {
		n := 0
		for _, rc := range cases {
			if rc.Family == f {
				n++
			}
		}
		if n == 0 {
			t.Errorf("family %s is empty", f)
		}
		t.Logf("family %-8s %d cases", f, n)
	}
	t.Logf("%d cases", len(cases))
}

func TestReplyCorpusFresh(t *testing.T) {
	a := ReplyCorpus()
	a[0].Lines[0] = "changed"
	a[0].Name = "changed"
	if b := ReplyCorpus(); b[0].Name == "changed" || b[0].Lines[0] == "changed" {
		t.Fatal("ReplyCorpus shares state between calls")
	}
}

func TestReplyCaseByName(t *testing.T) {
	for _, rc := range ReplyCorpus() {
		got, ok := ReplyCaseByName(rc.Name)
		if !ok || got.Name != rc.Name || string(got.Bytes()) != string(rc.Bytes()) {
			t.Errorf("ReplyCaseByName(%q) = %+v, %v", rc.Name, got, ok)
		}
	}
	if _, ok := ReplyCaseByName("no-such-case"); ok {
		t.Error("ReplyCaseByName found a case that does not exist")
	}
}

func TestReplyCaseBytes(t *testing.T) {
	rc, _ := ReplyCaseByName("group-215-block")
	if got, want := string(rc.Bytes()), "215 list follows\r\ng 2 1 y\r\n.\r\n"; got != want {
		t.Fatalf("Bytes = %q, want %q", got, want)
	}
	rc, _ = ReplyCaseByName("status-empty-line")
	if got := string(rc.Bytes()); got != "\r\n" {
		t.Fatalf("Bytes = %q, want a bare CRLF", got)
	}
}

// Every case reaches the client as exactly its bytes: Apply (the whole command).
func TestReplyCorpusApply(t *testing.T) {
	for _, rc := range ReplyCorpus() {
		t.Run(rc.Name, func(t *testing.T) {
			t.Parallel()
			if got := rcExpect(t, rc, ReplyCase.Apply); got != string(rc.Bytes()) {
				t.Fatalf("client got %q, want %q", got, rc.Bytes())
			}
		})
	}
}

// ApplyLine answers the sample line only, and the same bytes arrive.
func TestReplyCorpusApplyLine(t *testing.T) {
	for _, rc := range ReplyCorpus() {
		t.Run(rc.Name, func(t *testing.T) {
			t.Parallel()
			if got := rcExpect(t, rc, ReplyCase.ApplyLine); got != string(rc.Bytes()) {
				t.Fatalf("client got %q, want %q", got, rc.Bytes())
			}
		})
	}
}

// Nothing but the case's bytes is written: a sample of cases, then a probe after them.
func TestReplyCorpusNoExtraBytes(t *testing.T) {
	for _, name := range []string{"group-211-negative", "article-222-block", "over-nonnumeric-row", "status-bare-lf", "greeting-400"} {
		rc, ok := ReplyCaseByName(name)
		if !ok {
			t.Fatalf("no case %q", name)
		}
		t.Run(name, func(t *testing.T) {
			conn := rcServe(t, rc, ReplyCase.Apply)
			if !rc.IsGreeting() {
				rcRead(t, conn, len(DefaultGreeting)+2)
				_, _ = io.WriteString(conn, rc.Line+"\r\n")
			}
			rcRead(t, conn, len(rc.Bytes()))
			_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			var b [64]byte
			if n, err := conn.Read(b[:]); n != 0 {
				t.Fatalf("extra bytes after the case: %q (%v)", b[:n], err)
			}
		})
	}
}

// Only the sample line is answered wrongly by ApplyLine; Apply keeps other faults in o.
func TestReplyCaseApplyKeepsOptions(t *testing.T) {
	rc, _ := ReplyCaseByName("group-200")
	o := Options{Faults: Faults{Override: map[string][]string{"DATE": {"111 20200101000000"}}, CloseAfter: 7}}
	rc.Apply(&o)
	if o.Faults.CloseAfter != 7 || o.Faults.Override["DATE"] == nil || o.Faults.Override["GROUP"][0] != "200 ok" {
		t.Fatalf("Faults after Apply: %+v", o.Faults)
	}
	o.Faults.Override["GROUP"][0] = "changed"
	if rc.Lines[0] != "200 ok" {
		t.Fatal("Apply shares the Lines slice with the case")
	}
	rc.ApplyLine(&o)
	if got := o.Faults.OverrideLine["GROUP g"]; len(got) != 1 || got[0] != "200 ok" {
		t.Fatalf("OverrideLine = %v", o.Faults.OverrideLine)
	}

	g, _ := ReplyCaseByName("greeting-two-lines")
	var og Options
	g.Apply(&og)
	if og.Greeting != "200 hi\r\nstray line" || og.Faults.Override != nil {
		t.Fatalf("greeting Apply: %+v", og)
	}
}

// ApplyLine leaves another line of the same command alone.
func TestReplyCaseApplyLineOnlyThatLine(t *testing.T) {
	st := NewStore()
	st.AddArticle("g", Article{MessageID: "<1@x>", Body: []string{"b"}})
	rc, _ := ReplyCaseByName("group-200")
	o := Options{Store: st}
	rc.ApplyLine(&o)
	srv := New(t, o)
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	rcRead(t, conn, len(DefaultGreeting)+2)
	_, _ = io.WriteString(conn, "GROUP other\r\n")
	if got := rcRead(t, conn, 3); got != "411" {
		t.Fatalf("other GROUP line got %q, want its normal 411", got)
	}
}
