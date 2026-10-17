package testkit_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
)

// Example runs a fake upstream in Store mode and talks to it with the strict client. An
// Example has no testing.TB, so it uses fakeserver.Start and client.TryDial with the Try forms
// of the client methods and panics on errors; a test would use fakeserver.New(t, ...) and
// client.Dial(t, ...) with Cmd and ReadDotLines, which fail t. README.md shows this function
// verbatim: change both together.
func Example() {
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	st := fakeserver.NewStore()
	st.AddGroup("test.group", "a group for the example")
	for i, subj := range []string{"first", "second"} {
		st.AddArticle("test.group", fakeserver.Article{
			MessageID: fmt.Sprintf("<%d@example.invalid>", i+1),
			Headers:   []string{"From: alice@example.invalid", "Subject: " + subj},
			Body:      []string{"line one", "line two", ".a line that gets dot-stuffed"},
		})
	}
	s, err := fakeserver.Start(fakeserver.Options{Store: st})
	check(err)
	defer s.Close()

	c, err := client.TryDial(s.Addr(), 0) // 0 = testkit.Deadline() (NNTP_TEST_DEADLINE)
	check(err)
	defer c.Close()
	fmt.Println(c.Greeting)

	// block sends a command and reads its multi-line reply in strict mode (CRLF line ends,
	// dot-stuffing, ".\r\n" terminator); the client un-stuffs the lines.
	block := func(format string, args ...any) (string, []string) {
		_, status, err := c.TryCmd(format, args...)
		check(err)
		lines, err := c.TryReadDotLines()
		check(err)
		return status, lines
	}

	_, caps := block("CAPABILITIES")
	overMsgID := false
	for _, l := range caps {
		overMsgID = overMsgID || l == "OVER MSGID"
	}
	fmt.Println("OVER MSGID listed:", overMsgID)

	_, status, err := c.TryCmd("GROUP test.group")
	check(err)
	fmt.Println(status)

	_, art := block("ARTICLE 1") // head, blank line, body
	subject, bodyLines := "", 0
	for i, l := range art {
		if v, ok := strings.CutPrefix(l, "Subject: "); ok {
			subject = v
		}
		if l == "" {
			bodyLines = len(art) - i - 1
			break
		}
	}
	fmt.Println("subject:", subject, "body lines:", bodyLines, "last:", art[len(art)-1])

	_, over := block("OVER 1-2")
	fmt.Println("overview lines:", len(over))

	// POST: 340, then the article (dot-stuffed) and ".", then 240.
	_, status, err = c.TryCmd("POST")
	check(err)
	fmt.Println(status)
	check(c.Send("Newsgroups: test.group\r\nFrom: bob@example.invalid\r\nSubject: third\r\n" +
		"Message-ID: <3@example.invalid>\r\n\r\nposted body\r\n.\r\n"))
	_, status, err = c.ReadStatus()
	check(err)
	fmt.Println(status)

	status, _ = block("ARTICLE <3@example.invalid>")
	fmt.Println(status)

	fmt.Println("ARTICLE:", s.Count("ARTICLE"), "POST:", s.Count("POST"), "posted:", len(st.Posted()))
	// Nothing may follow a complete reply: no stray bytes, no second status line.
	fmt.Println("pending:", c.ExpectNoPending(50*time.Millisecond))

	// Output:
	// 200 fakeserver ready
	// OVER MSGID listed: true
	// 211 2 1 2 test.group
	// subject: first body lines: 3 last: .a line that gets dot-stuffed
	// overview lines: 2
	// 340 Send article to be posted
	// 240 Article received <3@example.invalid>
	// 220 3 <3@example.invalid> Article follows
	// ARTICLE: 2 POST: 1 posted: 1
	// pending: <nil>
}

// Example_scriptCloseAfter runs a fake server in Script mode with the CloseAfter fault: the
// connection is closed after the reply to the first command, so the client's next read sees
// EOF.
func Example_scriptCloseAfter() {
	s, err := fakeserver.Start(fakeserver.Options{
		Script: []fakeserver.Step{
			{Expect: "MODE READER", Reply: []string{"200 reader mode"}},
			{Expect: "GROUP .*", Reply: []string{"211 0 1 0 never.reached"}},
		},
		Faults: fakeserver.Faults{CloseAfter: 1},
	})
	if err != nil {
		fmt.Println("start:", err)
		return
	}
	defer s.Close()

	c, err := client.TryDial(s.Addr(), 0)
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer c.Close()

	_, line, err := c.TryCmd("MODE READER")
	fmt.Println(line, err)
	_, err = c.ReadLine()
	fmt.Println("next read EOF:", errors.Is(err, io.EOF))
	fmt.Println("MODE:", s.Count("MODE"), "mismatches:", len(s.Mismatches()))

	// Output:
	// 200 reader mode <nil>
	// next read EOF: true
	// MODE: 1 mismatches: 0
}

// Example_dropInMultiline cuts a multi-line reply after its status line and one more line: the
// strict client reports a NoTerminator error that wraps io.EOF.
func Example_dropInMultiline() {
	s, err := fakeserver.Start(fakeserver.Options{
		Script: []fakeserver.Step{{
			Expect: "ARTICLE 1",
			Reply:  []string{"220 1 <1@example.invalid>", "Subject: cut", "", "body", "."},
		}},
		Faults: fakeserver.Faults{DropInMultiline: map[string]int{"ARTICLE": 2}},
	})
	if err != nil {
		fmt.Println("start:", err)
		return
	}
	defer s.Close()

	c, err := client.TryDial(s.Addr(), 0)
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer c.Close()

	code, _, err := c.TryCmd("ARTICLE 1")
	fmt.Println(code, err)
	lines, err := c.TryReadDotLines()
	var serr *client.StrictError
	if errors.As(err, &serr) {
		fmt.Println("kind:", serr.Kind, "EOF:", errors.Is(err, io.EOF), "lines:", lines)
	}

	// Output:
	// 220 <nil>
	// kind: NoTerminator EOF: true lines: [Subject: cut]
}
