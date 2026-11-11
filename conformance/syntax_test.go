package conformance

import (
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/fakeserver"
)

// syntaxServer is a hand-written reader server (LineHandler) with one syntax mistake chosen by
// split: it answers CAPABILITIES, MODE READER, LIST ACTIVE, GROUP, STAT, ARTICLE and QUIT for
// the group syn.test with its one article <syn-1@conformance.invalid>. split cuts a command
// line into the keyword as the server compares it and the arguments; ok false is a 501.
func syntaxServer(t *testing.T, split func(line string) (kw string, args []string, ok bool)) *fakeserver.Server {
	t.Helper()
	const id = "<syn-1@conformance.invalid>"
	return fakeserver.New(t, fakeserver.Options{Handler: fakeserver.LineHandler(func(c *fakeserver.Conn, line string) ([]string, bool) {
		kw, args, ok := split(line)
		if !ok {
			return []string{"501 Syntax error"}, false
		}
		switch {
		case kw == "CAPABILITIES":
			return []string{"101 Capability list follows", "VERSION 2", "READER", "LIST ACTIVE", "."}, false
		case kw == "MODE" && len(args) == 1 && strings.EqualFold(args[0], "READER"):
			return []string{"200 Reader mode"}, false
		case kw == "LIST" && len(args) >= 1 && strings.EqualFold(args[0], "ACTIVE"):
			return []string{"215 Active groups follow", "syn.test 1 1 y", "."}, false
		case kw == "GROUP" && len(args) == 1 && args[0] == "syn.test":
			return []string{"211 1 1 1 syn.test"}, false
		case kw == "GROUP" && len(args) == 1:
			return []string{"411 No such group"}, false
		case kw == "STAT" && len(args) == 0:
			return []string{"223 1 " + id}, false
		case kw == "STAT" && len(args) == 1 && args[0] == id:
			return []string{"223 0 " + id}, false
		case kw == "ARTICLE" && len(args) == 1 && args[0] == id:
			return []string{"220 0 " + id + " Article follows", "Subject: syn", "", "body", "."}, false
		case kw == "QUIT":
			return []string{"205 bye"}, true
		}
		return []string{"500 Unknown command"}, false
	})})
}

func runSyntax(t *testing.T, o Options) Result {
	t.Helper()
	var c check
	for _, rc := range readerChecks() {
		if rc.Name == "command-syntax" {
			c = rc
		}
	}
	if c.Run == nil {
		t.Fatal("no check command-syntax")
	}
	return runReader(t, o, []check{c})[0]
}

func TestCommandSyntaxPassesAgainstStore(t *testing.T) {
	t.Parallel()
	for _, so := range []fakeserver.StoreOptions{{}, {ReaderOnly: true}} {
		srv := fakeserver.New(t, fakeserver.Options{Store: testStore(), StoreOpts: so})
		if r := runSyntax(t, Options{Addr: srv.Addr()}); r.Status != Pass {
			t.Errorf("%+v: %s %s, want PASS", so, r.Status, r.Detail)
		}
	}
}

func TestCommandSyntaxPassesAgainstTolerantHandWritten(t *testing.T) {
	t.Parallel()
	srv := syntaxServer(t, func(line string) (string, []string, bool) {
		f := strings.Fields(line)
		if len(f) == 0 {
			return "", nil, false
		}
		return strings.ToUpper(f[0]), f[1:], true
	})
	if r := runSyntax(t, Options{Addr: srv.Addr()}); r.Status != Pass {
		t.Errorf("%s %s, want PASS", r.Status, r.Detail)
	}
}

// TestCommandSyntaxSingleSpace: a server that splits on one SP only answers 500 or 501 to the
// variants with more white space or a TAB, and the Detail shows the bytes and both replies.
func TestCommandSyntaxSingleSpace(t *testing.T) {
	t.Parallel()
	srv := syntaxServer(t, func(line string) (string, []string, bool) {
		f := strings.Split(line, " ")
		for _, a := range f {
			if a == "" {
				return "", nil, false // an empty word: a doubled space
			}
		}
		return strings.ToUpper(f[0]), f[1:], true
	})
	r := runSyntax(t, Options{Addr: srv.Addr()})
	if r.Status != Fail {
		t.Fatalf("%s %s, want FAIL", r.Status, r.Detail)
	}
	for _, want := range []string{
		`"STAT  <syn-1@conformance.invalid>" got "501 Syntax error", but "STAT <syn-1@conformance.invalid>" got "223 0 <syn-1@conformance.invalid>"`,
		`"STAT<TAB><syn-1@conformance.invalid>" got "500 Unknown command"`,
		`"STAT<TAB><TAB><syn-1@conformance.invalid>"`,
		`"STAT <TAB> <syn-1@conformance.invalid>"`,
		`"GROUP<TAB><TAB>syn.test" got "500 Unknown command", but "GROUP syn.test" got "211 1 1 1 syn.test"`,
		`"LIST<TAB>ACTIVE<TAB>syn.test"`,
		`"LIST  ACTIVE  syn.test" got "501 Syntax error", but "LIST ACTIVE syn.test" got "215 Active groups follow"`,
		`"ARTICLE  <syn-1@conformance.invalid>"`,
		"RFC 3977 §3.1",
	} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("Detail lacks %s:\n%s", want, r.Detail)
		}
	}
	if strings.Contains(r.Detail, `"stat <`) || strings.Contains(r.Detail, `"StAt <`) {
		t.Errorf("case variants fail on a server that ignores case:\n%s", r.Detail)
	}
}

// TestCommandSyntaxUpperCaseOnly: a server that compares the keyword as sent fails exactly the
// lower-case and mixed-case variants.
func TestCommandSyntaxUpperCaseOnly(t *testing.T) {
	t.Parallel()
	srv := syntaxServer(t, func(line string) (string, []string, bool) {
		f := strings.Fields(line)
		if len(f) == 0 {
			return "", nil, false
		}
		return f[0], f[1:], true
	})
	r := runSyntax(t, Options{Addr: srv.Addr()})
	if r.Status != Fail {
		t.Fatalf("%s %s, want FAIL", r.Status, r.Detail)
	}
	for _, want := range []string{
		`"stat <syn-1@conformance.invalid>" got "500 Unknown command", but "STAT <syn-1@conformance.invalid>" got "223 0`,
		`"StAt <syn-1@conformance.invalid>"`,
		`"stat  <syn-1@conformance.invalid>"`,
		`"article<TAB><syn-1@conformance.invalid>"`,
		`"group syn.test"`,
		`"list active syn.test"`,
	} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("Detail lacks %s:\n%s", want, r.Detail)
		}
	}
	for _, bad := range []string{`"STAT  <`, `"STAT<TAB>`, `"GROUP  syn`, `"LIST  ACTIVE`} {
		if strings.Contains(r.Detail, bad) {
			t.Errorf("Detail names %s, which an upper-case keyword server accepts:\n%s", bad, r.Detail)
		}
	}
}

func TestCommandSyntaxSkipsWithoutArticle(t *testing.T) {
	t.Parallel()
	st := fakeserver.NewStore()
	st.AddGroup("conf.test.empty", "a group without articles")
	srv := fakeserver.New(t, fakeserver.Options{Store: st})
	r := runSyntax(t, Options{Addr: srv.Addr()})
	if r.Status != Skip || !strings.Contains(r.Detail, "no group with articles") {
		t.Errorf("%s %q, want SKIP no group with articles", r.Status, r.Detail)
	}
}
