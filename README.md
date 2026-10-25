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
| `client` | strict NNTP test client; COMPRESS DEFLATE (RFC 8054) |
| `fakeserver` | fake NNTP server: Script, Handler or Store mode; faults and counters; TLS; COMPRESS DEFLATE; transit peer (streaming and IHAVE) |
| `proc` | runs an external server binary (stdio or TCP), stops it, dumps its logs on failure |
| `conformance` | black-box RFC 3977/4643/4644 checks against any NNTP address, with a strict client |
| `cmd/nntp-conformance` | command-line front end of `conformance`: prints the report, exit 1 on any Fail |

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
| `TryCompress() (int, string, error)` / `Compress()` | send `COMPRESS DEFLATE`; on 206 compress both directions (`StartCompress`), else the code, the line and an error (403, 500, 501, 502, 503), still plain |
| `StartCompress() error`, `Compressed() bool` | compress from now on without sending a command (the test sent COMPRESS and read the 206) |
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

**COMPRESS DEFLATE** (RFC 8054): after `StartCompress`, `c.Conn` deflates every write (raw DEFLATE,
sync-flushed per `Send`/`Cmd` and sent in one socket write, so a server that answers a command and
closes, as on QUIT, cannot fail the write of that command) and `c.R` reads the inflated server
stream, starting with the bytes buffered after the 206 line. Strict mode, `ReadDotLines`,
`RawUntilDot` and `ExpectNoPending` work on the inflated stream as before (an empty flush block
inflates to nothing, so `ExpectNoPending` still waits and returns nil). A goroutine inflates the
socket and passes on each byte as soon as it can be decoded, whatever flush the server uses: also
zlib's `Z_PARTIAL_FLUSH`, which INN's nnrpd sends and which Go's `compress/flate` reader holds back
until more data arrives. Read deadlines (the hang guard, `ExpectNoPending`) apply to the inflated
stream, so a timeout loses nothing. A closed connection reads as `io.EOF`, corrupt data as an error
wrapping `flate.CorruptInputError`. Keep `c.Conn` from before `Compress` to write bytes past the
compressor. A second `COMPRESS`, `STARTTLS` or `AUTHINFO` can still be sent to check the server's
502.
`ExpectNoPending(d)` returns nil on timeout or EOF, never consumes data and restores the deadline;
call it after a reply to catch a stray byte or a second status line (e.g. a NUL after
`400 too many users`). With `Strict: false` a bare LF is a line end, like a lenient client.

### fakeserver

| API | what |
|---|---|
| `New(tb, Options) *Server` / `Start(Options) (*Server, error)` | listen (default `127.0.0.1:0`) and serve; `New` registers `tb.Cleanup(Close)` |
| `type Options struct { Addr, Greeting string; NoGreeting, RawStall bool; TLS *tls.Config; Script []Step; Handler func(*Conn); Store *Store; StoreOpts StoreOptions; Faults Faults; Deadline time.Duration; MaxConns int; MaxConnsGreeting string }` | server config; `MaxConns` > 0: one more simultaneous connection gets `MaxConnsGreeting` (default `502 Too many connections`) and is closed |
| `type Step struct { Expect string; ReadDot bool; Delay time.Duration; Reply []string; Hang, CloseAfter, Compress bool }` | one scripted exchange; `Expect` is an anchored regexp, "" matches any line; `Compress`: COMPRESS DEFLATE after the reply |
| `(*Server) Addr`, `Host`, `Port` | listen address |
| `Lines`, `Articles`, `Mismatches` | client lines, dot blocks read (raw), lines no step matched |
| `Accepted`, `Open`, `WaitOpen(tb, n, d)`, `WaitLines(tb, want)` | connection counts and polling waits |
| `Count(cmd) int`, `Counts() map[string]int` | commands seen |
| `Close()` | close listener and connections, wait for the goroutines |
| `type Conn`: `ReadLine`, `WriteLines(lines...)`, `ReadDotBlock`, `Raw`, `Server`, `ID` | server side of one connection, for a Handler; `ID` is the accept order from 1 |
| `(*Conn) StartCompress() error`, `StartCompressOutputOnly() error`, `Compressed() bool` | COMPRESS DEFLATE from now on (call it after writing the 206); `OutputOnly` is a broken server that compresses its output but reads the client's bytes uncompressed |
| `TLSConfig(tb) (*tls.Config, *x509.CertPool)` | self-signed cert for 127.0.0.1 and a pool that trusts it |
| `PeerHandler(*PeerOptions) func(*Conn)`, `type PeerOptions` | transit peer: MODE STREAM, CHECK, TAKETHIS (RFC 4644), IHAVE (RFC 3977 6.3.2), AUTHINFO; options below |
| `NewStore() *Store` (the zero value works too) | stateful store; safe for concurrent use; returns copies |
| `(*Store) AddGroup`, `AddGroupAt`, `SetPosting`, `AddArticle(group, Article) int64` | setup |
| `Article`, `ByMessageID`, `Group`, `Groups`, `Numbers`, `Overview`, `Next`, `Last` | lookups |
| `RemoveArticle(group, n)`, `Expire(group, below)`, `Cancel(id)` | expiry and cancels: the high-water mark `High` is kept (numbers are not reused), `Low` and `Count` follow; an emptied group has `Low` = `High`+1 |
| `AddGenerated(group, desc, Generator)`, `SetGeneratedHigh(group, high)`, `NumbersIn(group, from, to)`, `GenLimit(group)` | generated groups, see below |
| `HighestExisting(conn, Group) Group` | for `StoreOptions.GroupNumbers`: report the highest existing number as `High`, like servers whose last number goes down |
| `AddPosted`, `Posted` | articles accepted by POST |
| `type Article`, `Group`, `Location`; `(Article) Header`, `WithDefaults` | data model; `Article.Arrived` is the arrival time NEWNEWS uses (`AddArticle` sets a zero one to now) |
| `StoreHandler(*Store, StoreOptions) func(*Conn)`; `type StoreOptions` | RFC 3977 reader over a Store; options below |
| `DefaultGreeting`, `DefaultGreetingNoPost`, `DateLayout` | constants |

Three modes, chosen per server:

- **Script**: `Options.Script` steps run in order on every connection after the greeting. A line
  that does not match the current step gets `500 fakeserver: unexpected <line>` and is recorded in
  `Mismatches()`; after the last step the server keeps reading and recording until EOF.
- **Handler**: `Options.Handler` gets each connection (`*fakeserver.Conn`); the conn is closed when
  it returns. `StoreHandler` and `PeerHandler` are ready-made handlers.
- **Store**: `Options.Store` serves a `Store` with the built-in RFC 3977 handler: CAPABILITIES, MODE
  READER, HELP, LIST (ACTIVE, NEWSGROUPS, OVERVIEW.FMT), GROUP, LISTGROUP, ARTICLE/HEAD/BODY/STAT,
  OVER/XOVER, HDR/XHDR (+ LIST HEADERS), NEXT/LAST, NEWGROUPS, NEWNEWS, DATE, POST, AUTHINFO USER/PASS, COMPRESS (with
  `StoreOpts.Compress`), QUIT. `StoreOpts.User`/`Pass`
  require authentication (480 before it); `ReaderOnly` greets 201 (unless `Greeting` is set) and
  answers POST with 440.

**NEWNEWS** (`NEWNEWS wildmat yyyymmdd hhmmss [GMT]`, RFC 3977 7.4) answers 230 and the
Message-IDs, each once, of the articles in matching groups whose `Article.Arrived` is at or
after the time (date and time as NEWGROUPS, always UTC), in group name order and by number.
`AddArticle` sets a zero `Arrived` to now (set it to date an article for a test; a POSTed
article arrives when it is posted, whatever its Date header says); a generated article arrives
at its Date.

`StoreOptions` make the Store server behave like particular real servers:

| field | effect |
|---|---|
| `User, Pass string` | require AUTHINFO USER/PASS (480 before it); a 480 written to a logged-in connection, also one from `Faults.Override`, ends the login, as a login that timed out |
| `ReaderOnly bool` | greet 201, POST answers 440 |
| `OverviewFmt []string` | the LIST OVERVIEW.FMT answer |
| `GroupNumbers func(conn int, g Group) Group` | the numbers GROUP, LISTGROUP and LIST ACTIVE report, per connection: a lagging frontend of a server farm (lower `High` on some connections), `211 0 0 0` for an empty group, or `st.HighestExisting`; the articles served stay the stored ones |
| `ListgroupRange string` | `"reject"`: 501 for `LISTGROUP group range` (RFC 977/2980 servers); `"ignore"`: the whole group whatever the range |
| `NoOver`, `NoXover bool` | that command is unknown (500, not listed by HELP); `NoOver` also drops `OVER MSGID` from CAPABILITIES |
| `NoHdr`, `NoXhdr bool` | HDR or XHDR is unknown (500); `NoHdr` also drops `HDR` from CAPABILITIES and makes LIST HEADERS a 501 |
| `NoNewnews bool` | NEWNEWS is unknown (500) and not in CAPABILITIES or HELP, as on the many servers without it; INN's `502 NEWNEWS command disabled by administrator` is `Faults.Override` `"NEWNEWS"` |
| `EmptyOverReply []string` | instead of 423 for a range without articles, e.g. `{"420 No article(s) selected"}` or `{"224 Overview follows", "."}` |
| `MaxOverRange int64`, `OverRangeTooLarge []string` | cap one OVER/XOVER range (an open range ends at `High`): a larger range gets `OverRangeTooLarge`, or when that is nil the records of its first `MaxOverRange` numbers only |
| `OverviewFields func(conn int, group string, fields []string) []string` | change the fields of every OVER/XOVER line (`fields[0]` is the number, then Subject, From, Date, Message-ID, References, :bytes, :lines, `Xref: ...`): overview data out of step with the articles, e.g. a Message-ID that is not the one in the article's header |
| `HdrLines func(conn int, group, field string, lines []string) []string` | change the `number value` lines of a HDR/XHDR reply before they are sent (XHDR by `<id>` gives the id instead of the number, an absent header `(none)`; HDR gives 0 and an empty value): header data out of step with the articles |
| `Compress bool` | offer COMPRESS DEFLATE (RFC 8054): listed in CAPABILITIES while off (after authentication when required); COMPRESS answers 206 and compresses both directions; 501 missing or invalid algorithm name (case-sensitive), 502 once active (any valid algorithm), 503 another algorithm, 480 before required auth; AUTHINFO after it 502 |
| `CompressOutputOnly bool` | offer COMPRESS, but after the 206 compress only the server's output and read the client's commands uncompressed: a half implementation, for client tests |

**Generated groups.** `st.AddGenerated(group, desc, fakeserver.Generator{Low: 1, High: 10_000_000})`
makes a group of any size whose articles are built on demand and never stored, like a provider's
group of 10^7 articles. `Generator.Article(group, n)` makes article `n` (false = a hole; nil =
`GeneratedArticle`: Message-ID `<n.group@fakeserver.invalid>`, Date 2020-01-01 plus `n`
seconds); `Generator.Lookup(id)` maps a Message-ID back to its number (nil = that form).
`Count` is `High-Low+1`. GROUP, LIST, ARTICLE by number or Message-ID, NEXT/LAST and ranged
OVER/XOVER/LISTGROUP work as for stored articles; one OVER, XOVER or LISTGROUP that spans more
than `Generator.Limit` numbers (default `DefaultGenLimit`, 100000; < 0 = none) gets
`503 fakeserver: ...` instead of a reply the test cannot hold, so a client that asks for a whole
huge group shows up as an error. NEWNEWS examines a matching generated group whole, so it gets
that 503 too when the group spans more than the limit, whatever the time. `Expire` raises `Low`, `SetGeneratedHigh` lets new articles
arrive; `AddArticle` panics on a generated group, `RemoveArticle` and `Cancel` find nothing.

**PeerHandler** is a transit peer for feeders such as innfeed and innxmit. It
advertises `VERSION 2`, `IHAVE`, `READER` and `STREAMING` (not with `NoStream`), keeps its state across the
connections of one server and answers as innd does:

| command | replies |
|---|---|
| `MODE STREAM` | 203; 501 `Unknown MODE variant` with `NoStream` |
| `CHECK <id>` | 238; 438 when held; 431 once for `Defer431Once`; 480 before auth; 500 with `NoStream` |
| `TAKETHIS <id>` + article | 239 (added to `Have`, `OnArticle` runs); 439 when held or in `Reject`; 480 before auth (after the article); 500 with `NoStream` (no article read) |
| `IHAVE <id>` | 335, then after the article 235 (added to `Have`, `OnArticle` runs) or 437 (in `Reject`, or held meanwhile); at once: 435 when held or the id is not `<...>`, 436 once for `Defer436Once`, 480 before auth |

| `PeerOptions` field | effect |
|---|---|
| `User, Pass string` | require AUTHINFO USER/PASS |
| `Have map[string]bool` | ids the peer already holds; accepted articles are added (read it after `Close`) |
| `Defer431Once`, `Defer436Once map[string]bool` | CHECK (431) or IHAVE (436) of these ids is deferred the first time only; innd's 436 comes before the transfer (paused, or the article is arriving on another connection) |
| `Reject map[string]bool` | refused once the article is read, every time, as a content filter does: CHECK 238, TAKETHIS 439, IHAVE 335 then 437; ids refused by id alone are `Have` |
| `NoStream bool` | a peer without streaming, as innd with streaming off: MODE STREAM 501, CHECK and TAKETHIS unknown (500, the article lines after a TAKETHIS are then read as commands), no `STREAMING` capability; innfeed and innxmit fall back to IHAVE |
| `StallModeStream time.Duration` | sleep before the first MODE STREAM reply (capped at the Deadline) |
| `OnArticle func(msgID string, wire []string)` | runs before the 239/235 with the raw (dot-stuffed) lines |
| `Compress`, `CompressOutputOnly bool` | COMPRESS DEFLATE as in `StoreOptions` (innd itself has none; INN's nnrpd has); AUTHINFO answers 502 once active |

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
| `LineDelay map[string]time.Duration` | write the reply one line at a time with this pause between lines: a slow server on a large OVER |
| `OverrideFrom map[string]int` | `Override`/`OverrideLine` of that command apply only from its N-th use on a connection: e.g. `"OVER": 2` with `Override` `"OVER": {"480 Authentication required"}`, a login that expires in the middle of a session |
| `OverrideTimes map[string]int` | `Override`/`OverrideLine` of that command apply at most N times per connection, then the normal reply: one 480 that the client answers with AUTHINFO before it retries |

The faults apply in this order: `Delay`; `OverrideLine`, else `Override`; `LongLine`;
`DropInMultiline`; then the write, through `Drip` when set and with `LineDelay` between lines;
then `Endless`. `Drip`, `LineDelay` and `Endless` are bounded by the Deadline like every
server-side wait.

`Count(cmd)` and `Counts()` count the upper-cased first word of every client line read through
`Conn.ReadLine`, in all modes and on all connections (dot-block payload lines are not counted).

**COMPRESS DEFLATE** (RFC 8054): compression starts after the CRLF of the 206 in both directions.
Every write (replies, every fault, `Raw().Write`) is deflated, sync-flushed and sent in one socket
write; the client stream is inflated as it arrives, whatever flush the client uses, so `Lines`,
`Counts`, `Articles` and `Mismatches` record the commands as the client wrote them. The Store
handler starts compression when the status it wrote for COMPRESS, after the faults, is a 206:
`Faults.Override` `"COMPRESS"` `{"403 Unable to activate compression"}` is a server that cannot,
`{}` one that never answers. Corrupt compressed input closes the connection. Bytes a client
pipelines after `COMPRESS DEFLATE` are inflated too (INN's nnrpd reads them as plain commands).
There is no STARTTLS (TLS is `Options.TLS`), and crypto/tls has no TLS-level compression.

### proc

| API | what |
|---|---|
| `type Options struct { Path string; Args, Env []string; Dir string; StopTimeout, Deadline time.Duration; Logs, Ulimit []string }` | `Dir` "" = `tb.TempDir()`; `Env` appended to `os.Environ()`; `Ulimit` entries such as `"-n 16"` (open files) or `"-f 64"` (file size) are set by `/bin/sh`, which then execs `Path`: fd exhaustion and write failures (EFBIG) |
| `StartStdio(tb, Options) *Process` | stdin/stdout are the client connection (inetd style): `Conn()` |
| `StartTCP(tb, Options, addr) *Process` | ready once a dial to `addr` yields a greeting line (one probe connection, then QUIT) |
| `(*Process) Conn`, `Addr`, `Pid`, `Signal(sig)`, `Wait`, `Done`, `Stderr` | `Conn` is nil in TCP mode, `Addr` "" in stdio mode; `Done()` is closed once the process exited (a bounded wait that, unlike `Wait`, never stops it) |
| `Stop()` | SIGTERM, SIGKILL after `StopTimeout` (`DefaultStopTimeout` = 5s); idempotent; run by `tb.Cleanup` |

Log dump: the cleanup of both Start functions stops the process and, when the test failed, writes
`Stderr()` (stdout too in TCP mode) and every file in `Options.Logs` to the test output with
`tb.Log`. `Stop` needs a Unix system.

### conformance

| API | what |
|---|---|
| `Run(tb, Options) Report` | run every check; each Fail is a `tb.Errorf`, then one summary line; nothing on tb with `ReportOnly` |
| `Check(Options) (Report, error)` | the same without `testing`; the error only when the first connection cannot be opened (no listener, TLS failure, dial or greeting timeout); a greeting with a wire mistake is the `greeting` check's Fail, not an error |
| `Names() []string` | the check names in report order (the values of `Options.Skip`) |
| `type Options struct { Addr, User, Pass, PostGroup string; TLS *tls.Config; Deadline time.Duration; Skip []string; ReportOnly bool }` | `User` empty: auth checks skipped; `PostGroup` empty: no check posts; `TLS` non-nil: TLS from the first byte (no STARTTLS); `Deadline` 0: `testkit.Deadline()` |
| `type Result struct { Name string; Status Status; Detail, RFC string }`, `Pass`, `Fail`, `Skip` | one check; `Detail` says why it failed or was skipped (on Pass an occasional note) |
| `type Report struct { Results []Result; Capabilities []string }`; `Failed()`, `Counts()`, `String()` | `Capabilities` is the list read right after the greeting, before MODE READER or AUTHINFO |

Every check opens its own connection (four run at a time; the results keep the order of
`Names()`), so one failure cannot poison the next. Before the dial a check is skipped when it is
in `Options.Skip` ("skipped by option"), needs credentials without `User` or posts without
`PostGroup`; after the dial and CAPABILITIES it is skipped when its capability is not listed
(unless the server lists `MODE-READER`: the check then sends MODE READER and tests again). A
broken greeting fails `greeting` once and skips the rest; a panic in a check is its Fail.
The articles that checks post carry a Message-ID under `@conformance.invalid`.

| RFC | checks |
|---|---|
| RFC 3977 | `greeting` `capabilities` `mode-reader` `group` `listgroup` `next-last` `article` `head` `body` `stat` `no-group` `no-current-article` `article-out-of-range` `article-unknown-id` `list-active` `list-newsgroups` `list-overview-fmt` `list-unknown-keyword` `over` `over-errors` `date` `help` `unknown-command` `bad-arguments` `quit-eof` |
| RFC 4643 (with `User`) | `authinfo-user-pass` `authinfo-wrong-pass` `authinfo-pass-without-user` `authinfo-480` `authinfo-after-auth` `authinfo-capabilities` |
| RFC 4644 and RFC 3977 §6.3.2 (STREAMING / IHAVE; posting ones with `PostGroup`) | `mode-stream` `mode-stream-capability` `check-new` `check-pipelined` `takethis` `check-have` `takethis-duplicate` `ihave` `ihave-have` `ihave-malformed` |

```bash
go run github.com/go-while/nntp-testkit/cmd/nntp-conformance -addr 127.0.0.1:1119 [-user u -pass p] [-group g]
```

| flag | effect |
|---|---|
| `-addr host:port` | the server under test (required) |
| `-user`, `-pass` | AUTHINFO USER/PASS; without `-user` the RFC 4643 checks are skipped |
| `-group` | a group the checks may post into (TAKETHIS, IHAVE); without it nothing is posted |
| `-tls`, `-tls-insecure` | TLS from the first byte, `ServerName` = the host of `-addr`; `-tls-insecure` skips the certificate check (and implies `-tls`) |
| `-deadline` | hang guard per read and write, default `10s` |
| `-skip a,b` | check names to leave out (an unknown name is a usage error) |
| `-json` | the report as JSON (`results`, `capabilities`, `pass`/`fail`/`skip` counts) instead of text |

Exit status: 0 no Fail, 1 any Fail, 2 a usage error or a server that cannot be reached.

Self-test (the `conformance` tests): the kit's own Store server passes every check it can be
given (the stream and IHAVE checks skip: it lists neither STREAMING nor IHAVE), the transit peer
(`PeerHandler`) passes the stream checks, and fakes with one deliberate mistake (a bare-LF
greeting, an ARTICLE reply cut before its terminator, a NUL after a 411, 7 OVER fields) fail
exactly the matching check.

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
