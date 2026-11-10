# nntp-testkit

Shared NNTP test tools in Go: a fake NNTP server (scripted, handler-driven or backed by a stateful
store), a strict test client that reports wire-level mistakes, a runner for external server
binaries, and yEnc-encoded test posts. Standard library only, Go 1.26 or later; CI tests the previous Go release and the
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
| `testkit` (root) | `Deadline()`, `DefaultDeadline`, `EnvDeadline`; `WaitFor`, `FreePort`, `Listening` |
| `client` | strict NNTP test client; COMPRESS DEFLATE (RFC 8054); STARTTLS (RFC 4642) |
| `fakeserver` | fake NNTP server: Script, Handler or Store mode; faults and counters; TLS and STARTTLS; COMPRESS DEFLATE; transit peer (streaming and IHAVE) |
| `tlscert` | test CA and leaf certificates (expired, not yet valid, wrong name), as `tls.Certificate`, PEM or files with a 0600 key |
| `proc` | runs an external server binary (stdio or TCP), stops it, dumps its logs on failure |
| `conformance` | black-box RFC 3977/4643/4644 checks against any NNTP address, with a strict client |
| `cmd/nntp-conformance` | command-line front end of `conformance`: prints the report, exit 1 on any Fail |
| `yenc` | yEnc-encoded test posts: single and multi-part, broken ones, data profiles, a generated group |
| `article` | news articles as raw bytes: unique Message-IDs, minimal valid articles, deliberately broken ones, header edit and lookup, parse, header diff of a sent and a served article, a corpus of tricky articles, control messages |
| `canlock` | Cancel-Lock and Cancel-Key values (RFC 8315): compute them, check a key against a lock |
| `mailrec`, `cmd/fakesendmail` | a stand-in sendmail that records each mail, for servers that mail moderators or report by mail |

The `doc.go` of each package holds the full contract; `go doc -all ./<package>` prints it.

### testkit (root)

| API | what |
|---|---|
| `Deadline() time.Duration`, `DefaultDeadline`, `EnvDeadline` | the hang guard (above) |
| `WaitFor(tb, d, what, cond func() bool)` | poll `cond` every 50 ms until it holds; `tb.Fatalf("timed out after <d> waiting for <what>")` after `d` (0 = `Deadline()`) |
| `FreePort(tb) int` | a TCP port of 127.0.0.1 that was free a moment ago, and not in use on 0.0.0.0 or [::] either, for a server that must be told its port (another process may take it first) |
| `Listening(addr) bool` | a TCP dial to `addr` succeeds within a second (closed at once, nothing written): a server is up, or with `WaitFor` it has stopped |

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
| `TryStartTLS(cfg) (*Conn, error)` / `StartTLS(cfg) *Conn` | send `STARTTLS` (RFC 4642), expect 382, fail when any byte is already buffered after the 382 (plaintext injection, CVE-2011-0411), handshake within the deadline; a new strict `Conn` over the `*tls.Conn` (no greeting read; `cfg` is cloned, `ServerName` defaults to the remote host). A reply other than 382 is a `*StartTLSError{Code, Line}` and `c` stays plain; the injection is a `*StrictError` of Kind `Pending`; verification errors are crypto/tls's (`x509.HostnameError`, `x509.CertificateInvalidError` with `Reason` `Expired`, `x509.UnknownAuthorityError`). `StartTLS` fails tb and registers the cleanup |
| `TLSState() (tls.ConnectionState, bool)`, `IsTLS() bool` | peer certificates, version, server name of a TLS `Conn` |
| `Close()` | idempotent, nil-safe |
| `ExpectCmd(want, format, args...) string` | `Cmd`, and fail tb unless the code is `want`; returns the status line |
| `ExpectStatus(want, what) string` | read the next status line (the 240 after a POSTed article, a pipelined reply); fail tb on error or unless the code is `want` |
| `MustSend(raw)` | `Send` that fails tb |
| `Capabilities() (map[string][]string, []string)` | send CAPABILITIES; fail tb unless 101 and a list `ParseCapabilities` takes; the map and the lines in order |
| `ParseCode`, `ExpectCode`, `ExpectLines`, `Unstuff` | text helpers (`Unstuff` removes the first dot of every line that starts with one, RFC 3977 3.1.1) |
| `Stuff(lines) string` | the lines as a client sends them after POST, IHAVE or TAKETHIS: dot-stuffed, CRLF after each, `.\r\n` at the end |
| `ParseCapabilities(lines) (map[string][]string, error)` | CAPABILITIES lines → label upper-cased (labels are case-insensitive, RFC 3977 §3.3.1) → arguments as sent (an empty slice for none); error on a blank line or a label listed twice, the map holds the rest |
| `StuffBytes(article []byte) []byte` | an article in wire form (CRLF, not stuffed, no terminator) as a client sends it: line starts (after CRLF; a bare LF is not one) dot-stuffed, final CRLF added, `.\r\n` appended; every other byte (bare CR, NUL, 8-bit) untouched |
| `TryPost(article) (code, line, sent, err)`, `TryIhave(id, article)`, `TryCheck(id)`, `TryTakethis(id, article)`, `TryModeStream()`, `TryLogin(user, pass)` | the transfer and login exchanges in one call. POST/IHAVE send the article on 340/335 and return the final reply, else the first one; TAKETHIS sends command and article without waiting; LOGIN sends PASS on 381. A refusal (435, 440, 480, 481 ...) is a result: `err` is for I/O and protocol errors only. `sent` says whether the article went out (a 436 or 437 can come before or after the transfer; TAKETHIS always sends). `ExpectPost(want, article)`, `ExpectIhave(want, id, article)`, `ExpectTakethis(want, id, article)` fail the test unless the article was sent and the final code is `want`. `id` includes the `<>` |
| `SendBytes`, `MustSendBytes` | `Send` and `MustSend` for a `[]byte` (e.g. `StuffBytes(article)`) |
| `Post`, `Ihave`, `Check`, `Takethis`, `ModeStream`, `Login` | the same, `(code, line)`; fail tb only on an I/O or protocol error, never on a refusal |
| `TryPipeline(cmds, multi) ([]Reply, error)` / `Pipeline(...)`, `type Reply struct { Cmd string; Code int; Line string; Lines []string }`, `MultiLine(cmd, code) bool` | write all commands from a goroutine while reading the replies in order (no deadlock when the server stops reading); `multi(cmd, code)` says whether a dot block follows (nil: `MultiLine`; `Reply.Cmd` is the entry's first line); the deadline slides with every line, a dot block also ends within 10 deadlines and 64 MiB |
| `TryDialFrom(addr, localIP, deadline)` / `DialFrom(tb, addr, localIP)` | dial from a chosen source address (127.0.0.2) and read the greeting; skip the test where the OS cannot bind it (macOS without an alias) |
| `TryDialUntil(addr, prefix, d)` / `DialUntil(tb, addr, prefix, d)` | redial every 50 ms until the greeting starts with `prefix` (a server that greets 400 while it starts); refused dials count as not yet |
| `TryReadUntilQuiet(quiet, limit) ([]byte, error)` / `ReadUntilQuiet(...) (data, eof)` | raw bytes until the server is silent for `quiet` or `limit` bytes; `io.EOF` when the server closed |
| `ExpectClosed(d) ([]string, error)` / `MustClosed(d) []string` | the peer closes within `d`; the lines it sent before the close (QUIT 205, 400 then close); EOF and a reset count as closed |
| `SendChunks(raw, n, pause) error`, `SendDrip(raw, pause)` | write `raw` n bytes (1) per write with a pause: split CRLF or the terminator across writes |
| `type StrictError struct { Kind Kind; Line string; Bytes []byte; Err error }`, `type Kind int` | strict-mode error |

`Cmd`, `ReadDotLines`, `RawUntilDot`, `ExpectCmd`, `ExpectStatus`, `MustSend` and `Capabilities` fail
the Conn's tb (`c.For(t)` in a subtest); on a `TryDial` Conn without `WithTB` they panic, so use the
`Try` forms there.

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
| `Accepted`, `Open`, `MaxOpen`, `WaitOpen(tb, n, d)`, `WaitLines(tb, want)` | connection counts (`MaxOpen`: the most open at once, e.g. a client's parallel-connection limit) and polling waits |
| `Count(cmd) int`, `Counts() map[string]int` | commands seen |
| `Close()` | close listener and connections, wait for the goroutines |
| `type Conn`: `ReadLine`, `WriteLines(lines...)`, `ReadDotBlock`, `Raw`, `Server`, `ID` | server side of one connection, for a Handler; `ID` is the accept order from 1 |
| `(*Conn) StartCompress() error`, `StartCompressOutputOnly() error`, `Compressed() bool` | COMPRESS DEFLATE from now on (call it after writing the 206); `OutputOnly` is a broken server that compresses its output but reads the client's bytes uncompressed |
| `TLSConfig(tb) (*tls.Config, *x509.CertPool)` | self-signed cert for 127.0.0.1 and a pool that trusts it (package `tlscert` makes CA-issued ones) |
| `StoreOptions.StartTLS`, `PeerOptions.StartTLS *StartTLSOptions{Config *tls.Config; Refuse string; HideCapability, AuthNeedsTLS bool; CloseAfterHandshake int}` | STARTTLS (RFC 4642), see below; nil: unknown command |
| `(*Conn) StartTLS(cfg) error`, `TLS() bool`, `TLSState()` | for a Handler: TLS server side from now on (call it after writing the 382) |
| `(*Server) TLSHandshakes()`, `TLSFailed()`, `TLSServerNames()` | completed and failed handshakes (implicit TLS and STARTTLS), SNI of every ClientHello |
| `LineHandler(f func(c *Conn, line string) (reply []string, closeConn bool)) func(*Conn)` | answer each client line with what `f` returns, in any order (what `Script` cannot); `f` may `c.ReadDotBlock()` for an article; `Faults` apply |
| `PeerHandler(*PeerOptions) func(*Conn)`, `type PeerOptions` | transit peer: MODE STREAM, CHECK, TAKETHIS (RFC 4644), IHAVE (RFC 3977 6.3.2), AUTHINFO; options below |
| `(*Conn) Wait(ch <-chan struct{}) bool`, `Sleep(d) bool` | for a Handler or `PeerOptions.BeforeReply`: block until `ch` fires (true) or the Deadline passes / the server closes (false); `Sleep` is bounded the same way |
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

`PeerOptions` knobs for peers that hold, slow, cut or answer oddly (all keep `Counts`, `Accepted`,
`MaxOpen`, `Have` and `OnArticle`, are bounded by `Options.Deadline` and end on `Close`):

| field | effect |
|---|---|
| `HoldGreeting <-chan struct{}` | the greeting waits until the channel is closed (or delivers a value, releasing one connection); the connection counts as open meanwhile, so a test can assert `MaxOpen <= K`. Needs `Options.NoGreeting: true` (the peer writes `Options.Greeting` itself; `fakeserver.NewPeer(t, opts, po)` sets it for you); a hold past the Deadline (60 s by default) or `Close` drops the connection |
| `GreetingDelay time.Duration` | wait before the greeting (after `HoldGreeting`), capped at the Deadline; needs `NoGreeting` (set by `NewPeer`/`StartPeer`) |
| `BeforeReply func(c *Conn, cmd, line, status string)` | runs before every reply is written (IHAVE: before the 335 and before the final one) and may block, e.g. `c.Wait(release)`: hold a reply at a known point. A reply held when the server closes is not sent |
| `CapabilitiesReply []string` | the whole answer to CAPABILITIES, e.g. `{"500 What?"}` |
| `ModeStreamReply []string` | the answer to MODE STREAM, e.g. `{"203 StreamOK."}` (Diablo's feeder, with `CapabilitiesReply`) or `{"501 ..."}`; CHECK and TAKETHIS keep working (unlike `NoStream`) |
| `DropAfterChecks int`, `DropReply string` | per connection, the CHECK after the first N gets `DropReply` (default `400 Service temporarily unavailable`) and the connection closes |
| `SilentAfterModeStream bool` | answer MODE STREAM, then read and record lines but never answer, until the client closes, `Close` or the Deadline |

`Options.Faults` apply to `PeerHandler` as to a `LineHandler` (Delay, Override, OverrideLine,
CloseAfter, Drip, ...): `OverrideLine: {"MODE STREAM": {"203 StreamOK."}}` is the same as
`ModeStreamReply`. For IHAVE they apply to the 335 only, as to POST's 340.

Three modes, chosen per server:

- **Script**: `Options.Script` steps run in order on every connection after the greeting. A line
  that does not match the current step gets `500 fakeserver: unexpected <line>` and is recorded in
  `Mismatches()`; after the last step the server keeps reading and recording until EOF.
- **Handler**: `Options.Handler` gets each connection (`*fakeserver.Conn`); the conn is closed when
  it returns. `StoreHandler`, `PeerHandler` and `LineHandler` are ready-made handlers.
- **Store**: `Options.Store` serves a `Store` with the built-in RFC 3977 handler: CAPABILITIES, MODE
  READER, HELP, LIST (ACTIVE, ACTIVE.TIMES, NEWSGROUPS, OVERVIEW.FMT), GROUP, LISTGROUP, ARTICLE/HEAD/BODY/STAT,
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
advertises `VERSION 2`, `IHAVE` and `STREAMING` (not with `NoStream`; no `READER`: MODE READER answers 502 as innd with reader access denied), keeps its state across the
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
mode (in Script mode to a matched step's reply) and to a `LineHandler`'s replies, not to another
Handler; New and Start copy the maps.

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
crypto/tls has no TLS-level compression.

**STARTTLS** (RFC 4642): the Store handler and `PeerHandler` offer it with `StartTLS`
(`StartTLSOptions`; `Options.TLS` stays implicit TLS from the first byte). It is listed in
CAPABILITIES while the connection is plain, not compressed and not authenticated; it answers 382
and the handshake follows the CRLF of that line. Afterwards it is not listed and a second STARTTLS
is 502, as on an implicit TLS connection, after COMPRESS (compress after TLS works) and after a
successful AUTHINFO. The upgrade forgets the session (selected group and article), and plaintext
the client pipelined behind STARTTLS closes the connection. Test knobs: `Refuse` (a reply such as
`502 ...` or `580 ...` instead of 382: a refusing server or a stripping attacker; in Store mode
`Faults.Override` `"STARTTLS"` does the same), `HideCapability`, `AuthNeedsTLS` (with `User`/`Pass` set: AUTHINFO before
TLS is 483 as RFC 4643 describes, and the Store handler lists `AUTHINFO` without `USER`) and
`CloseAfterHandshake` (the first N upgrades are closed right after the handshake). The Server
counts handshakes and records the SNI of every ClientHello. Make the certificates with `tlscert`.

### tlscert

| API | what |
|---|---|
| `NewCA(tb) *CA`; `CA{Cert *x509.Certificate; Key crypto.Signer; CertPEM []byte; Pool *x509.CertPool}` | a test CA (ECDSA P-256) |
| `(*CA) Issue(tb, Options) *Leaf` | a leaf signed by the CA |
| `SelfSigned(tb, Options) *Leaf` | a self-signed leaf that is its own trust anchor |
| `type Options struct { CommonName string; DNSNames []string; IPs []net.IP; NotBefore, NotAfter time.Time; Expired, NotYetValid bool; KeyType KeyType; ClientAuth bool }` | the zero value: localhost, 127.0.0.1 and ::1, valid from a day ago for two days, ECDSA. `Expired`/`NotYetValid` set a window in the past/future, `DNSNames`/`IPs` another name give a hostname mismatch; `KeyType` is `ECDSA`, `RSA` (2048) or `Ed25519` |
| `type Leaf struct { Cert; Key; CertPEM, KeyPEM, CAPEM []byte; Pool *x509.CertPool }` | `KeyPEM` is PKCS#8; `CAPEM` the issuer |
| `(*Leaf) TLSCertificate() tls.Certificate`, `ServerConfig() *tls.Config`, `ClientConfig(serverName) *tls.Config` | for Go servers and fakes (`fakeserver.StartTLSOptions.Config`, `Options.TLS`) and clients (`client.Conn.TryStartTLS`); `CA.ClientConfig` too |
| `(*Leaf) Write(tb, dir) Files{Cert, Key, CA string}`, `WriteKeyMode(tb, dir, mode)` | `cert.pem`, `key.pem` (mode 0600, files replaced atomically) and `ca.pem` in `dir` (`""` = `tb.TempDir()`), for servers that read paths from their config (INN, diablo, NewsCache); a server running as another user needs `WriteKeyMode` 0640 + chgrp, or 0644 |
| `(*CA) WriteCA(tb, dir) string` | `ca.pem` alone; its path |

A leaf made to fail (`Expired`, `NotYetValid`, another name) still carries the issuer's trust, so
the failure is the certificate's alone: `*tls.CertificateVerificationError` wrapping
`x509.CertificateInvalidError` (`Reason` `x509.Expired` for both time windows) or `x509.HostnameError`.

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
| `Run(tb, Options) Report` | run every check; each Fail is a `tb.Errorf`, then one summary line; no Errorf with `ReportOnly` (an unreachable server is logged) |
| `RunVerbose(tb, Options) Report` | `Run` that first logs every result that is not a Skip (`tb.Logf`, one line each), so a passing run shows what passed |
| `Check(Options) (Report, error)` | the same without `testing`; the error only when the first connection cannot be opened (no listener, TLS failure, dial or greeting timeout); a greeting with a wire mistake, or a 400/502 greeting ("service unavailable: <line>"), is the `greeting` check's Fail and every other check is skipped, not an error |
| `Names() []string` | the check names in report order (the values of `Options.Skip`) |
| `type Options struct { Addr, User, Pass, PostGroup string; TLS *tls.Config; Deadline time.Duration; Skip []string; ReportOnly bool; Workers int; StrictOverviewFmt bool }` | `User` empty: auth checks skipped; `PostGroup` empty: no check posts; `TLS` non-nil: TLS from the first byte (no STARTTLS); `Deadline` 0: `testkit.Deadline()`; `Workers` 0: 4 checks at a time; `StrictOverviewFmt`: require preferred overview metadata names |
| `type Result struct { Name string; Status Status; Detail, RFC, Missing string }`, `Pass`, `Fail`, `Skip` | one check; `Detail` says why it failed or was skipped (on Pass an occasional note); on a Skip for a capability the server does not advertise, `Missing` names it ("NEWNEWS", "STREAMING", "LIST ACTIVE.TIMES") and `Detail` reads "<Missing> not advertised": match skips on `Missing` |
| `type Report struct { Results []Result; Capabilities []string }`; `Failed()`, `Counts()`, `String()`, `ByName()` | `Capabilities` is the list read right after the greeting, before MODE READER or AUTHINFO |

Every check opens its own connection (`Options.Workers` at a time, default 4; the results keep
the order of `Names()`), so one failure cannot poison the next. The checks that post run after
all the others, so no article arrives in `PostGroup` while a reader check counts its articles. Before the dial a check is skipped when it is
in `Options.Skip` ("skipped by option"), needs credentials without `User` or posts without
`PostGroup`; after the dial and CAPABILITIES it is skipped when its capability is not listed
(unless the server lists `MODE-READER`: the check then sends MODE READER and tests again). A
broken greeting fails `greeting` once and skips the rest; a panic in a check is its Fail.
The articles that checks post carry a Message-ID under `@conformance.invalid`.

`Options.StrictOverviewFmt` requires the RFC 3977 section 8.4.2 preferred `:bytes` and
`:lines` metadata names in both the format and OVER checks. The zero value remains
compatible with the RFC-permitted legacy `Bytes:`/`Lines:` aliases. This opt-in policy
does not make those legacy spellings RFC violations; field-name case and optional
overview extensions are still handled as before.

| RFC | checks |
|---|---|
| RFC 3977 | `greeting` `capabilities` `mode-reader` `group` `listgroup` `listgroup-range` `next-last` `article` `head` `body` `stat` `no-group` `no-current-article` `article-out-of-range` `article-unknown-id` `list-active` `list-active-times` `list-newsgroups` `list-overview-fmt` `list-headers` `list-unknown-keyword` `over` `over-errors` `hdr` `newgroups` `newnews` `post` (with `PostGroup`) `date` `help` `unknown-command` `bad-arguments` `quit-eof` |
| RFC 4643 (with `User`) | `authinfo-user-pass` `authinfo-wrong-pass` `authinfo-pass-without-user` `authinfo-480` `authinfo-after-auth` `authinfo-capabilities` |
| RFC 4644 and RFC 3977 §6.3.2 (STREAMING / IHAVE; posting ones with `PostGroup`) | `mode-stream` `mode-stream-capability` `check-new` `check-pipelined` `takethis` `check-have` `takethis-duplicate` `ihave` `ihave-have` `ihave-malformed` |

```bash
go run github.com/go-while/nntp-testkit/cmd/nntp-conformance -addr 127.0.0.1:1119 [-user u -pass p] [-group g]
```

| flag | effect |
|---|---|
| `-addr host:port` | the server under test (required) |
| `-user`, `-pass` | AUTHINFO USER/PASS; without `-user` the RFC 4643 checks are skipped |
| `-group` | a group the checks may post into (POST, TAKETHIS, IHAVE); without it nothing is posted |
| `-tls`, `-tls-insecure` | TLS from the first byte, `ServerName` = the host of `-addr`; `-tls-insecure` skips the certificate check (and implies `-tls`) |
| `-deadline` | hang guard per read and write, default `10s` |
| `-workers` | checks at the same time, each on its own connection, default 4; lower it for a server that allows fewer connections (a refused dial is a Fail) |
| `-skip a,b` | check names to leave out (an unknown name is a usage error) |
| `-json` | the report as JSON (`results`, `capabilities`, `pass`/`fail`/`skip` counts) instead of text |
| `-strict-overview-fmt` | require preferred `:bytes`/`:lines` instead of the RFC-permitted legacy aliases; off by default |

Exit status: 0 no Fail, 1 any Fail (also a 400/502 greeting), 2 a usage error or a server that
cannot be reached.

Self-test (the `conformance` tests): the kit's own Store server passes every check it can be
given (the stream and IHAVE checks skip: it lists neither STREAMING nor IHAVE), the transit peer
(`PeerHandler`) passes the stream checks, and fakes with one deliberate mistake (a bare-LF
greeting, an ARTICLE reply cut before its terminator, a NUL after a 411, 7 OVER fields, a
LISTGROUP that ignores its range, a bad LIST ACTIVE.TIMES time, a LIST HEADERS name with its
colon, wrong HDR values, NEWGROUPS and NEWNEWS that ignore the date, a POST that is never filed)
fail exactly the matching check.

### yenc

yEnc-encoded article bodies, for testing decoders and clients that fetch binaries. The lines go
into a `fakeserver.Article` body like any other: no CRLF and no dot-stuffing (the server adds it).

| API | what |
|---|---|
| `type Options struct { Line int; RawDots, RawSpaces bool; ExtraEscapes string; NoBegin, NoEnd, BadSize, BadCRC bool }` | the zero value encodes as yEnc 1.3 recommends: NUL, LF, CR and `=` escaped, tab and space at line ends, `.` at line starts; `Line` 0 = 128 |
| `Encode(name, data, Options) []string` | a single-part post: `=ybegin`, the data lines, `=yend size= crc32=` |
| `EncodeParts(name, data, partSize, Options) [][]string` | one post per part: `=ybegin part= total=`, `=ypart begin= end=`, the data, `=yend size= part= pcrc32=` (and `crc32=` of the whole data on the last part) |
| `Data(Profile, size, seed) []byte`; `Random`, `Plain`, `Escapes`, `Edges` | reproducible data: random bytes (most of Usenet), no escapes, an escape in every second byte, a third of the bytes at `.`, tab and space |
| `Generator(Profile, low, high, size func(n int64) int, Options) fakeserver.Generator` | a generated group: article n is `Data(p, size(n), n)` named `<p>-<n>.bin`, with `GeneratedArticle`'s headers and a yEnc Subject |

`RawDots` and `RawSpaces` model posting tools that leave those characters to the server's
dot-stuffing and to the reader. The broken-post options model corrupt uploads; `Faults.DropInMultiline`
cuts a post off mid-body instead.

### article

Articles as raw bytes, for suites that post and fetch. Nothing is normalised: an `Article` keeps the
order of its header fields, the exact case of their names, duplicates, empty values, folds, lines
without a colon, bare CR or LF, NUL and 8-bit bytes, so a test can send what a normal library would
refuse to write. `Bytes()` is the wire form: CRLF line ends, **not** dot-stuffed, without the
terminating `.`; `client.Stuff` (or the client's send) adds both.

| API | what |
|---|---|
| `MsgID(tb) string` / `MsgIDAt(tb, domain)` / `NewMsgID(name, domain)` | a unique Message-ID `<TestName.time.nonce.seq@testkit.invalid>`: the test name reduced to letters, digits, `-` and `_`, the time, a per-process nonce, a process-wide sequence; `ValidMsgID(id)` checks the form (RFC 5536) |
| `New(tb, Options) *Article` / `Minimal(tb, group)` | a minimal valid article: Path, From, Newsgroups, Subject, Message-ID, Date and a short body; `Options` sets `Newsgroups, Subject, From, PathHost, MessageID, Domain, Date, Body, Approved, Extra` (more raw header lines) |
| `type Article struct { Fields []Field; Sep string; Body []byte }`, `type Field struct { Raw, EOL string }` | the raw form: a field is its exact text (folds included) and its line end (`""` = CRLF, `"\n"`, `NoEOL`); `Sep` is what separates head and body (`NoEOL`: no empty line) |
| `Add(name, value)`, `AddRaw(raw)`, `Insert(i, raw)`, `Set(name, value)`, `SetRaw(name, raw)`, `Del(name)` | edit, chainable; `AddRaw("Path:")` is an empty Path (`SetRaw("Path", "Path:")` empties it in place), `AddRaw("Path: a\r\n !b")` a folded one, `AddRaw("no colon")` a line without colon; `Set` replaces in place keeping the old spelling of the name and drops duplicates |
| `SetBody([]byte)`, `SetBodyLines(lines...)` | the body as bytes, or lines each plus CRLF (a leading `.` is not doubled) |
| `Get(name)`, `Lookup(name)`, `Values(name)`, `Has`, `Count`, `Duplicates()`, `MessageID()` | read, case-insensitive; values are unfolded |
| `Bytes()`, `String()`, `Lines()`, `BodyLines()`, `Clone()`, `Canon()` | the wire form, and CRLF-split lines (what `client.Stuff` takes); `Canon()` is `Parse(Bytes())`: a bare LF inside a field is a line end on the wire, so it splits the field |
| `Parse(raw) *Article` / `ParseDotBlock(raw)` / `ParseLines(lines)` | split at the first empty line (CRLF or LF), keep every header line and the body as they are; `Parse(b).Bytes()` equals `b` for any `b`. `ParseDotBlock` takes the client's `RawUntilDot` bytes (drops the terminator and the stuffing dots, nothing else), the right way to read a served article back; `ParseLines` on `ReadDotLines` output loses bare LF and CR-before-CRLF |
| `Compare(sent, got, DiffOptions) Diff`, `ExpectSame(tb, sent, got, opts)` | the header diff: fields added, lost, changed (unfolded values) or reordered, ignoring what servers touch (`DefaultAllowed()`: Path, Xref, Injection-Info, Injection-Date, NNTP-Posting-Host, NNTP-Posting-Date, Lines, X-Trace, X-Complaints-To, plus `Allow`; `Strict` drops the defaults), and the body byte for byte (`IgnoreTrailingSpace` to relax); both sides are canonicalised, folds count as one space, `CaseSensitiveNames` checks the spelling; a server that normalises bare CR/LF cannot be compared byte for byte. `PathExtends(sent, got)`: the sent Path is the tail of the served one |
| `Corpus() []Case`, `CaseNames()`, `CaseByName(name)`; `Case.Article(tb, group)` | tricky articles by name: `latin1`, `utf8`, `nul`, `bare-cr`, `bare-lf`, `dot-lines`, `long-line`, `max-line`, `long-message-id`, `folded-subject`, `folded-path`, `lf-only`, `no-colon`, `dup-header`, `empty-path`, `empty-subject`, ... (`Case.Invalid` marks the RFC-breaking ones) |
| `Cancel(tb, Options, target)`, `Newgroup(tb, Options, group, moderated)`, `Rmgroup(tb, Options, group)` | control messages: the `Control` and `cmsg` Subject headers and a body, no PGP signature; `Cancel`'s Newsgroups is `Options.Newsgroups`, "test" when empty |

```go
for _, c := range article.Corpus() {
	t.Run(c.Name, func(t *testing.T) {
		sent := c.Article(t, "test")
		cl.ExpectCmd(340, "POST")
		cl.MustSend(client.Stuff(sent.Lines())) // stuffing and the final "." are the client's
		cl.ExpectStatus(240, "post")
		// ... later fetch it back with ARTICLE: the raw block keeps bare CR and LF
		cl.ExpectCmd(220, "ARTICLE %s", sent.MessageID())
		got := article.ParseDotBlock(cl.RawUntilDot())
		article.ExpectSame(t, sent, got, article.DiffOptions{Allow: []string{"Content-Length"}})
	})
}
```

### canlock

Cancel-Lock and Cancel-Key header values (RFC 8315 section 4), for testing servers that check
them: a key is base64(HMAC-H(secret, uid + Message-ID)), its lock base64(H(key)). The results
match INN's `tests/lib/canlock-t.c` vectors.

| API | what |
|---|---|
| `type Hash string`; `SHA1`, `SHA256` | the schemes; another name panics in the functions below |
| `Key(h, secret, uid, msgid) string` / `KeyValue(...)` | the Cancel-Key element `sha256:<b64>` / its bare value; `uid` "" for an administrator's key |
| `Lock(h, key) string` / `LockValue(h, key)` | the Cancel-Lock element / bare value for a key (an element or a bare value) |
| `Elements(v) []string` | the elements of a header value; comments in parentheses dropped |
| `Verify(lock, key) bool` | whether some key element opens a lock element of the same scheme (case-insensitive); unknown schemes ignored |

```go
key := canlock.Key(canlock.SHA256, "secret", "", "<1@example.invalid>")
art.Add("Cancel-Lock", canlock.Lock(canlock.SHA256, key))
// ... the cancel carries "Cancel-Key: "+key
```

### mailrec

A stand-in for sendmail that records mail instead of sending it, for servers that mail a
moderated group's submissions or send reports. `New` builds `cmd/fakesendmail` for one test with
its record directory built in, so it records even when the server clears the environment of the
programs it runs; give `Recorder.Path` to the server as its sendmail or mailer.

| API | what |
|---|---|
| `New(tb) *Recorder`; `Recorder{Path, Dir string}` | build the stand-in into `tb.TempDir()` (needs the `go` command) |
| `(*Recorder) Mails(tb)`, `Wait(tb, n)`, `ExpectNone(tb)` | the mails so far; wait for n of them (bounded by `testkit.Deadline()`); fail if one arrives within 200ms |
| `(*Recorder) SetExit(tb, code)`, `Env()` | make the stand-in exit with code from now on (the mail is still recorded); `NNTP_TEST_MAIL_DIR=<Dir>` for a server's environment |
| `type Mail struct { N int; Argv []string; Msg []byte }` | one mail: `Recipients()`, `From()` (-f), `HeaderMode()` (-t), `Header(name)` (unfolded), `Body()` |
| `Read(dir)`, `Record(dir, argv, msg)`, `Main(...)` | read or write a record directory without a `Recorder`; `Main` is the command |
| `EnvDir` (`NNTP_TEST_MAIL_DIR`), `EnvExit` (`NNTP_TEST_MAIL_EXIT`) | the command's record directory (wins over the built-in one) and its default exit status |

A record directory holds `N/argv` (one argument per line, argv[0] first), `N/msg` (stdin) and
`N/done` (written last; a mail without it is still being written) for N = 1, 2, ...; the file
`exit` in it, if present, is the exit status. Without a directory the command reads the mail and
drops it. For a server whose sendmail path is compiled in, install the built command there on a
test machine only and set `NNTP_TEST_MAIL_DIR` per test; never on a machine that sends real mail.

```go
mr := mailrec.New(t)
srv := proc.StartTCP(t, proc.Options{Path: serverBin, Args: []string{"-mailer", mr.Path}}, addr)
// ... post to a moderated group
m := mr.Wait(t, 1)[0]
if got := m.Recipients(); len(got) != 1 || got[0] != "mod@example.invalid" { t.Errorf("to %q", got) }
```

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

MIT, see [LICENSE](LICENSE). Versions before v0.11.2 were published as GPLv2; the
author, who wrote all of it, allows them to be used under the MIT license as well.
