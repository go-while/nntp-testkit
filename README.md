# nntp-testkit

Shared NNTP test tools in Go: a fake NNTP server (scripted, handler-driven or backed by a stateful
store), a strict test client that reports wire-level mistakes, and a runner for external server
binaries. Standard library only, Go 1.26 or later; CI tests the previous Go release and the
latest on Linux and macOS (`proc` uses Unix signals, so Windows is not tested).

It is written for the test suites of go-pugleaf, nntp-server and nntp-proxy (Go tests import the
packages), and for NewsCache's end-to-end tests (the C++ server runs under `proc`, the strict client
and the fake upstream talk to it). The kit never contacts a host a test did not name.

## Install

```bash
go get github.com/go-while/nntp-testkit
```

Releases are tagged (`@latest`, or a version such as `@v0.3.0`). For local work on the kit itself,
a `replace` directive to a checkout works, e.g. `replace github.com/go-while/nntp-testkit => ../nntp-testkit`.

## Hang guard

Every blocking step (dial, read, write, waits) is bounded by `testkit.Deadline()`: the Go duration in
`NNTP_TEST_DEADLINE` (`testkit.EnvDeadline`), default 60s (`testkit.DefaultDeadline`). It only stops a
hung test; a passing test never waits on it. `fakeserver.Options.Deadline`, `client.Conn.Deadline`
(or the `deadline` argument of `DialWith`/`TryDial`) and `proc.Options.Deadline` override it per use.

## Packages

| package | what |
|---|---|
| `testkit` (root) | `Deadline()`, `DefaultDeadline`, `EnvDeadline` |
| `client` | strict NNTP test client |
| `fakeserver` | fake NNTP server: Script, Handler or Store mode; faults and counters; TLS; streaming peer |
| `proc` | runs an external server binary (stdio or TCP), stops it, dumps its logs on failure |
| `conformance` | stub: planned black-box RFC 3977/4643/4644 checks against any address |

The `doc.go` of each package holds the full contract; `go doc -all ./<package>` prints it.

### client

| API | what |
|---|---|
| `type Conn struct { Conn net.Conn; R *bufio.Reader; Greeting string; Deadline time.Duration; Strict bool }` | one client connection |
| `Dial(tb, addr) *Conn` / `DialWith(tb, addr, deadline) *Conn` | dial, read the greeting, `tb.Cleanup(Close)`; fail tb on error |
| `TryDial(addr, deadline) (*Conn, error)` | the same without `testing`; the caller closes |
| `New(tb, net.Conn) *Conn` | wrap an open conn (net.Pipe, TLS); no greeting read |
| `(*Conn) WithTB(tb) *Conn` | attach a tb to a `TryDial` Conn |
| `ReadGreeting`, `Send(raw)`, `ReadLine`, `ReadStatus` | raw I/O; `Send` appends nothing (pipelining) |
| `TryCmd(format, args...)` / `Cmd(...)` | send one line + CRLF, read the status: `(code, line[, err])` |
| `TryReadDotLines()` / `ReadDotLines()` | dot block, un-stuffed, line ends stripped |
| `RawUntilDot() []byte` | exact bytes up to and including the terminator |
| `ExpectNoPending(d) error` | nothing may arrive within `d` |
| `Close()` | idempotent, nil-safe |
| `ParseCode`, `ExpectCode`, `ExpectLines`, `Unstuff` | text helpers |
| `type StrictError struct { Kind Kind; Line string; Bytes []byte; Err error }`, `type Kind int` | strict-mode error |

`Cmd`, `ReadDotLines` and `RawUntilDot` fail tb; on a `TryDial` Conn without `WithTB` they panic, so
use the `Try` forms there.

Strict mode (on for `Dial`, `DialWith`, `TryDial` and `New`) returns a `*StrictError` whose `Kind` is:

| Kind | when |
|---|---|
| `BareLF` | a line ends in LF without CR |
| `BareCR` | a CR inside a line |
| `NoCRLF` | EOF or deadline after partial data without any line end |
| `BadDotStuffing` | a dot-block line starts with "." but is neither ".." nor the terminator |
| `NoTerminator` | a dot block ends (EOF or deadline) without `.\r\n` |
| `Pending` | `ExpectNoPending` saw bytes (they stay in `R`; `Bytes` holds them) |

`Err` holds the read error and `Unwrap` returns it, so `errors.Is(err, io.EOF)` works.
`ExpectNoPending(d)` returns nil on timeout or EOF, never consumes data and restores the deadline;
call it after a reply to catch a stray byte or a second status line (e.g. a NUL after
`400 too many users`). With `Strict: false` a bare LF is a line end, like a lenient client.

### fakeserver

| API | what |
|---|---|
| `New(tb, Options) *Server` / `Start(Options) (*Server, error)` | listen (default `127.0.0.1:0`) and serve; `New` registers `tb.Cleanup(Close)` |
| `type Options struct { Addr, Greeting string; NoGreeting, RawStall bool; TLS *tls.Config; Script []Step; Handler func(*Conn); Store *Store; StoreOpts StoreOptions; Faults Faults; Deadline time.Duration }` | server config |
| `type Step struct { Expect string; ReadDot bool; Delay time.Duration; Reply []string; Hang, CloseAfter bool }` | one scripted exchange; `Expect` is an anchored regexp, "" matches any line |
| `(*Server) Addr`, `Host`, `Port` | listen address |
| `Lines`, `Articles`, `Mismatches` | client lines, dot blocks read (raw), lines no step matched |
| `Accepted`, `Open`, `WaitOpen(tb, n, d)`, `WaitLines(tb, want)` | connection counts and polling waits |
| `Count(cmd) int`, `Counts() map[string]int` | commands seen |
| `Close()` | close listener and connections, wait for the goroutines |
| `type Conn`: `ReadLine`, `WriteLines(lines...)`, `ReadDotBlock`, `Raw`, `Server` | server side of one connection, for a Handler |
| `TLSConfig(tb) (*tls.Config, *x509.CertPool)` | self-signed cert for 127.0.0.1 and a pool that trusts it |
| `PeerHandler(*PeerOptions) func(*Conn)`, `type PeerOptions` | streaming peer (RFC 4644): MODE STREAM, CHECK, TAKETHIS, AUTHINFO |
| `NewStore() *Store` (the zero value works too) | stateful store; safe for concurrent use; returns copies |
| `(*Store) AddGroup`, `AddGroupAt`, `SetPosting`, `AddArticle(group, Article) int64` | setup |
| `Article`, `ByMessageID`, `Group`, `Groups`, `Numbers`, `Overview`, `Next`, `Last` | lookups |
| `AddPosted`, `Posted` | articles accepted by POST |
| `type Article`, `Group`, `Location`; `(Article) Header`, `WithDefaults` | data model |
| `StoreHandler(*Store, StoreOptions) func(*Conn)`; `type StoreOptions struct { User, Pass string; ReaderOnly bool; OverviewFmt []string }` | RFC 3977 reader over a Store |
| `DefaultGreeting`, `DefaultGreetingNoPost`, `DateLayout` | constants |

Three modes, chosen per server:

- **Script**: `Options.Script` steps run in order on every connection after the greeting. A line
  that does not match the current step gets `500 fakeserver: unexpected <line>` and is recorded in
  `Mismatches()`; after the last step the server keeps reading and recording until EOF.
- **Handler**: `Options.Handler` gets each connection (`*fakeserver.Conn`); the conn is closed when
  it returns. `StoreHandler` and `PeerHandler` are ready-made handlers.
- **Store**: `Options.Store` serves a `Store` with the built-in RFC 3977 handler: CAPABILITIES, MODE
  READER, HELP, LIST (ACTIVE, NEWSGROUPS, OVERVIEW.FMT), GROUP, LISTGROUP, ARTICLE/HEAD/BODY/STAT,
  OVER/XOVER, NEXT/LAST, NEWGROUPS, DATE, POST, AUTHINFO USER/PASS, QUIT. `StoreOpts.User`/`Pass`
  require authentication (480 before it); `ReaderOnly` greets 201 (unless `Greeting` is set) and
  answers POST with 440.

Precedence: `RawStall` wins over everything (accept TCP, never write). Otherwise the greeting is
written unless `NoGreeting`; then `Handler` when set, else `Store` when set, else `Script`. With
`NoGreeting` and no Handler neither Store nor Script runs: lines are only recorded.

`Options.Faults` (keys are upper-cased command names, e.g. `"ARTICLE"`) apply in Store and Script
mode (in Script mode to a matched step's reply), not to an external Handler; New and Start copy the
maps.

| field | effect |
|---|---|
| `Delay map[string]time.Duration` | sleep before the reply to that command (capped at the Deadline) |
| `CloseAfter int` | close the connection after the reply to the N-th client line |
| `Override map[string][]string` | write these reply lines instead of the normal reply |
| `DropInMultiline map[string]int` | close after N lines of that command's reply (status line counted; 0 = before anything) |
| `OverrideLine map[string][]string` | `Override` for one client line, e.g. `"ARTICLE 2"`: one 430 in a pipeline; the key is the command upper-cased and the arguments joined by single spaces; wins over `Override` |
| `Drip map[string]time.Duration` | write the reply one byte at a time with this pause between bytes: a slow server that a per-read timeout never sees as stalled |
| `Endless map[string]bool` | write the reply without its `.` terminator, then `<n> endless` lines until the client closes, the server is closed or the Deadline passes; then close |
| `LongLine map[string]int` | insert one line of N `x` bytes after the status line (in a multi-line reply: as its first block line) |

The faults apply in this order: `Delay`; `OverrideLine`, else `Override`; `LongLine`;
`DropInMultiline`; then the write, through `Drip` when set; then `Endless`. `Drip` and `Endless`
are bounded by the Deadline like every server-side wait.

`Count(cmd)` and `Counts()` count the upper-cased first word of every client line read through
`Conn.ReadLine`, in all modes and on all connections (dot-block payload lines are not counted).

### proc

| API | what |
|---|---|
| `type Options struct { Path string; Args, Env []string; Dir string; StopTimeout, Deadline time.Duration; Logs []string }` | `Dir` "" = `tb.TempDir()`; `Env` appended to `os.Environ()` |
| `StartStdio(tb, Options) *Process` | stdin/stdout are the client connection (inetd style): `Conn()` |
| `StartTCP(tb, Options, addr) *Process` | ready once a dial to `addr` yields a greeting line (one probe connection, then QUIT) |
| `(*Process) Conn`, `Addr`, `Pid`, `Signal(sig)`, `Wait`, `Stderr` | `Conn` is nil in TCP mode, `Addr` "" in stdio mode |
| `Stop()` | SIGTERM, SIGKILL after `StopTimeout` (`DefaultStopTimeout` = 5s); idempotent; run by `tb.Cleanup` |

Log dump: the cleanup of both Start functions stops the process and, when the test failed, writes
`Stderr()` (stdout too in TCP mode) and every file in `Options.Logs` to the test output with
`tb.Log`. `Stop` needs a Unix system.

## Example

A fake upstream in Store mode and the strict client. This is `Example` from
[`example_test.go`](example_test.go), copied verbatim so `go test` checks it; edit both together.
The file also holds `Example_scriptCloseAfter` (Script mode, `Faults.CloseAfter`, the client sees
EOF) and `Example_dropInMultiline` (a cut reply gives a `NoTerminator` error).

```go
import (
	"fmt"
	"strings"
	"time"

	"github.com/go-while/nntp-testkit/client"
	"github.com/go-while/nntp-testkit/fakeserver"
)

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
```

## Checks

```bash
gofmt -l . && go vet ./... && go test -race -count=1 ./...
```

Which Go versions CI covers, and what to change when Go 1.28 is out: [NOTES.md](NOTES.md).

## License

GPLv2, see [LICENSE](LICENSE).
