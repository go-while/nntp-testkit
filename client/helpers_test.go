package client

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/go-while/nntp-testkit/fakeserver"
)

func TestStuff(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ".\r\n"},
		{[]string{""}, "\r\n.\r\n"},
		{[]string{"Subject: x", "", "body", ".", "..two", ".x"},
			"Subject: x\r\n\r\nbody\r\n..\r\n...two\r\n..x\r\n.\r\n"},
	} {
		got := Stuff(tc.in)
		if got != tc.want {
			t.Errorf("Stuff(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Unstuff of the wire lines (without the terminator) gives the input back.
		wire := strings.Split(strings.TrimSuffix(got, ".\r\n"), "\r\n")
		wire = wire[:len(wire)-1] // the empty string after the last CRLF
		if back := Unstuff(wire); len(tc.in) > 0 && !slices.Equal(back, tc.in) {
			t.Errorf("Unstuff(Stuff(%q)) = %q", tc.in, back)
		}
	}
}

func TestParseCapabilities(t *testing.T) {
	caps, err := ParseCapabilities([]string{
		"VERSION 2", "reader", "List ACTIVE newsgroups", "IMPLEMENTATION fake 1.0", "OVER\tMSGID",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"VERSION":        {"2"},
		"READER":         {},
		"LIST":           {"ACTIVE", "newsgroups"},
		"IMPLEMENTATION": {"fake", "1.0"},
		"OVER":           {"MSGID"},
	}
	if !maps.EqualFunc(caps, want, slices.Equal) {
		t.Errorf("caps = %q, want %q", caps, want)
	}
	if caps["READER"] == nil {
		t.Error("READER without arguments maps to nil, want an empty slice")
	}

	caps, err = ParseCapabilities([]string{"VERSION 2", "READER", "", "reader x", "POST"})
	if err == nil || !strings.Contains(err.Error(), "blank") {
		t.Errorf("blank line: err %v, want the blank line named", err)
	}
	if !slices.Equal(caps["READER"], []string{}) || caps["POST"] == nil {
		t.Errorf("caps after errors = %q, want READER (first line) and POST", caps)
	}
	if _, err := ParseCapabilities([]string{"READER", "Reader"}); err == nil ||
		!strings.Contains(err.Error(), "READER is listed twice") {
		t.Errorf("doubled label: err %v", err)
	}
	if caps, err := ParseCapabilities(nil); err != nil || len(caps) != 0 {
		t.Errorf("no lines: %q, %v", caps, err)
	}
}

// storeServer is a Store fakeserver with one group and one article.
func storeServer(t *testing.T) *fakeserver.Server {
	t.Helper()
	st := fakeserver.NewStore()
	st.AddArticle("test.group", fakeserver.Article{MessageID: "<1@test.invalid>",
		Headers: []string{"From: a@test.invalid", "Subject: one"}, Body: []string{"body"}})
	return fakeserver.New(t, fakeserver.Options{Store: st})
}

func TestExpectHelpers(t *testing.T) {
	s := storeServer(t)
	c := Dial(t, s.Addr())
	caps, lines := c.Capabilities()
	if len(lines) == 0 || lines[0] != "VERSION 2" {
		t.Errorf("capability lines %q", lines)
	}
	if _, ok := caps["READER"]; !ok {
		t.Errorf("READER not in %q", caps)
	}
	if line := c.ExpectCmd(211, "GROUP %s", "test.group"); !strings.HasPrefix(line, "211 1 1 1 ") {
		t.Errorf("GROUP: %q", line)
	}
	c.ExpectCmd(340, "POST")
	c.MustSend(Stuff([]string{"Newsgroups: test.group", "From: b@test.invalid", "Subject: two",
		"Message-ID: <2@test.invalid>", "", ".dotted"}))
	c.ExpectStatus(240, "POST article")
	c.ExpectCmd(222, "BODY <2@test.invalid>")
	if body := c.ReadDotLines(); !slices.Equal(body, []string{".dotted"}) {
		t.Errorf("BODY = %q, want the posted, un-stuffed line", body)
	}

	// Pipelined: two commands in one write, two statuses.
	c.MustSend("DATE\r\nGROUP test.group\r\n")
	c.ExpectStatus(111, "DATE")
	c.ExpectStatus(211, "GROUP")

	for name, fn := range map[string]func(c *Conn){
		"ExpectCmd":    func(c *Conn) { c.ExpectCmd(200, "GROUP test.group") },
		"ExpectStatus": func(c *Conn) { c.MustSend("DATE\r\n"); c.ExpectStatus(200, "the DATE reply") },
	} {
		rec := &recordTB{TB: t}
		rec.run(func(tb testing.TB) { fn(c.For(tb)) })
		if !rec.failed || !strings.Contains(rec.msg, "want 200") {
			t.Errorf("%s with the wrong code: failed %v, msg %q", name, rec.failed, rec.msg)
		}
	}

	// A closed connection fails MustSend and ExpectStatus.
	c.Close()
	rec := &recordTB{TB: t}
	rec.run(func(tb testing.TB) { c.For(tb).MustSend("DATE\r\n") })
	if !rec.failed || !strings.Contains(rec.msg, "send 6 bytes") {
		t.Errorf("MustSend on a closed conn: failed %v, msg %q", rec.failed, rec.msg)
	}
}

func TestCapabilitiesBroken(t *testing.T) {
	s := fakeserver.New(t, fakeserver.Options{Script: []fakeserver.Step{
		{Expect: "CAPABILITIES", Reply: []string{"101 list", "VERSION 2", "READER", "READER", "."}},
		{Expect: "CAPABILITIES", Reply: []string{"500 what?"}},
	}})
	c := Dial(t, s.Addr())
	for i, want := range []string{"READER is listed twice", "got \"500 what?\", want 101"} {
		rec := &recordTB{TB: t}
		rec.run(func(tb testing.TB) { c.For(tb).Capabilities() })
		if !rec.failed || !strings.Contains(rec.msg, want) {
			t.Errorf("CAPABILITIES %d: failed %v, msg %q, want %q", i+1, rec.failed, rec.msg, want)
		}
	}
}
