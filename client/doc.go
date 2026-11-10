// Package client is a test client for NNTP servers: a connection with a hang guard, one-call
// commands, dot-block reading and a strict mode that reports wire-level mistakes of the server.
//
// Contract (plan nntp-testkit; implemented in client.go and strict.go):
//
//	type Conn struct {
//		Conn     net.Conn
//		R        *bufio.Reader
//		Greeting string        // the greeting line read by Dial/ReadGreeting (CRLF stripped)
//		Deadline time.Duration // hang guard per blocking step; 0 = testkit.Deadline()
//		Strict   bool          // report wire mistakes (see below); Dial sets it
//	}
//	func Dial(tb testing.TB, addr string) *Conn            // DialWith(tb, addr, 0)
//	func DialWith(tb testing.TB, addr string, deadline time.Duration) *Conn // dial + greeting within deadline (0 = testkit.Deadline()), sets Conn.Deadline, tb.Cleanup(Close); Strict; fails tb on error
//	func TryDial(addr string, deadline time.Duration) (*Conn, error) // no testing dependency; Strict; reads the greeting; its tb-failing methods panic until WithTB
//	func New(tb testing.TB, c net.Conn) *Conn               // wrap an open conn; no greeting read; tb.Cleanup(Close); Strict
//	func (c *Conn) WithTB(tb testing.TB) *Conn              // attach a tb to a TryDial/raw Conn (registers tb.Cleanup(Close))
//	func (c *Conn) For(tb testing.TB) *Conn                 // a view of c that fails tb instead (shared stream, no cleanup): for subtests sharing a parent's conn
//	func (c *Conn) ReadGreeting() (string, error)          // reads one line into c.Greeting
//	func (c *Conn) Send(raw string) error                  // raw bytes, nothing appended (pipelining)
//	func (c *Conn) ReadLine() (string, error)              // one line, CRLF stripped (LF in non-strict mode)
//	func (c *Conn) ReadStatus() (int, string, error)       // next status line: code and the full line
//	func (c *Conn) TryCmd(format string, args ...any) (int, string, error) // Send(format+CRLF) then ReadStatus
//	func (c *Conn) Cmd(format string, args ...any) (int, string)           // TryCmd; fails tb on error
//	func (c *Conn) ExpectCmd(want int, format string, args ...any) string // Cmd; fails tb unless the code is want; the status line
//	func (c *Conn) ExpectStatus(want int, what string) string             // ReadStatus; fails tb on error or unless the code is want
//	func (c *Conn) MustSend(raw string)                                   // Send; fails tb on error
//	func (c *Conn) Capabilities() (map[string][]string, []string)         // CAPABILITIES: 101 and a block ParseCapabilities takes, else fails tb
//	func (c *Conn) TryReadDotLines() ([]string, error)     // dot-terminated block, un-stuffed, CRLF stripped
//	func (c *Conn) ReadDotLines() []string                 // TryReadDotLines; fails tb on error
//	func (c *Conn) RawUntilDot() []byte                    // exact bytes up to and including "\r\n.\r\n" (or ".\r\n" when empty); fails tb on error
//	func (c *Conn) TryRawUntilDot() ([]byte, error)        // the same, returning the error
//	func (c *Conn) ExpectNoPending(d time.Duration) error  // wait up to d for bytes; any → *StrictError{Kind: Pending, Bytes: ...}; nil on timeout/EOF; never consumes the bytes; restores the deadline
//	func (c *Conn) TryCompress() (int, string, error)     // send "COMPRESS DEFLATE" (RFC 8054); 206 → StartCompress, nil error; else code, line and an error, c stays plain
//	func (c *Conn) Compress()                               // TryCompress; fails tb unless 206
//	func (c *Conn) StartCompress() error                    // compress from now on without sending anything (the test sent COMPRESS and read the 206); error when already on
//	func (c *Conn) Compressed() bool
//	func (c *Conn) Close()
//
//	func ParseCode(line string) (int, error)               // 3-digit code at the start, followed by space or end
//	func ExpectCode(code int, line string, want ...int) error
//	func ExpectLines(got, want []string) error
//	func Unstuff(wire []string) []string                   // ".." prefix → "."
//	func Stuff(lines []string) string                      // lines → dot-stuffed CRLF block with ".\r\n": what a client sends after POST/IHAVE/TAKETHIS
//	func ParseCapabilities(lines []string) (map[string][]string, error) // CAPABILITIES lines → upper-cased label → arguments; error on a blank or doubled line
//
// Requests (requests.go): the article argument is the raw article in wire form (CRLF line ends,
// not dot-stuffed, no terminator; the article package's Bytes returns it). A refusal is a
// result, not a failure: the Try forms return an error only for an I/O, parse or strict-mode
// error, the plain forms fail tb only then. The id is the message-ID as it goes on the wire,
// angle brackets included, sent as given.
//
//	func StuffBytes(article []byte) []byte                 // dot-stuff each line start (after CRLF), add the final CRLF when missing, append ".\r\n"; every other byte (bare CR, NUL, 8-bit) stays
//	func (c *Conn) TryPost(article []byte) (code int, line string, err error)        // POST; on 340 send the article, return the final reply (240, 441 ...), else the first (440, 480 ...)
//	func (c *Conn) TryIhave(id string, article []byte) (int, string, error)          // IHAVE id; on 335 send and return the final (235, 436, 437), else the first (435, 436, 480 ...)
//	func (c *Conn) TryCheck(id string) (int, string, error)                          // CHECK id: 238, 431, 438 ...
//	func (c *Conn) TryTakethis(id string, article []byte) (int, string, error)       // TAKETHIS id and the article in one write, then the reply (239, 439 ...)
//	func (c *Conn) TryModeStream() (int, string, error)                              // MODE STREAM
//	func (c *Conn) TryLogin(user, pass string) (int, string, error)                  // AUTHINFO USER, on 381 AUTHINFO PASS; the final reply (281, 481, 482, 502, 483 ...) or the first when not 381
//	func (c *Conn) Post(article []byte) (int, string)       // the plain forms: TryX, fail tb on an error, return code and line
//	func (c *Conn) Ihave(id string, article []byte) (int, string)
//	func (c *Conn) Check(id string) (int, string)
//	func (c *Conn) Takethis(id string, article []byte) (int, string)
//	func (c *Conn) ModeStream() (int, string)
//	func (c *Conn) Login(user, pass string) (int, string)
//
//	type Reply struct { Cmd string; Code int; Line string; Lines []string } // Lines: the un-stuffed block of a multi-line reply, nil otherwise
//	func MultiLine(cmd string, code int) bool               // RFC 3977/4644/2980 multi-line reply codes (100, 101, 215, 220-222, 224, 225, 230, 231, 282; 211 after LISTGROUP)
//	func (c *Conn) TryPipeline(cmds []string, multi func(cmd string, code int) bool) ([]Reply, error) // write all cmds (CRLF appended unless the entry ends in one) from a goroutine while reading the replies in order; multi nil = MultiLine; on error the replies so far
//	func (c *Conn) Pipeline(cmds []string, multi func(cmd string, code int) bool) []Reply            // TryPipeline; fails tb on error
//
// Dialing (dial.go):
//
//	func TryDialFrom(addr, localIP string, deadline time.Duration) (*Conn, error) // TryDial from a local source address ("127.0.0.2"); the OS must own it
//	func DialFrom(tb testing.TB, addr, localIP string) *Conn                      // + greeting, tb.Cleanup(Close), fails tb
//	func TryDialUntil(addr, prefix string, d time.Duration) (*Conn, error)        // redial every 50 ms until the greeting starts with prefix; d bounds the whole wait (0 = testkit.Deadline()); refused dials and other greetings count as "not yet"
//	func DialUntil(tb testing.TB, addr, prefix string, d time.Duration) *Conn
//
// Raw I/O (raw.go): nothing is interpreted, no strict checks.
//
//	func (c *Conn) TryReadUntilQuiet(quiet time.Duration, limit int) ([]byte, error) // bytes until silent for quiet, or limit bytes (limit <= 0: none); nil error for both; io.EOF itself when the server closed; ends after one hang guard anyway
//	func (c *Conn) ReadUntilQuiet(quiet time.Duration, limit int) (data []byte, eof bool) // fails tb on an error other than EOF
//	func (c *Conn) ExpectClosed(d time.Duration) ([]string, error)  // the peer closes within d (0 = hang guard); the lines it sent before, CRLF stripped (a partial last line included); EOF and ECONNRESET count as closed; error on timeout, more than 16 lines or 64 KiB, or another read error (lines returned too)
//	func (c *Conn) MustClosed(d time.Duration) []string             // ExpectClosed; fails tb on an error
//	func (c *Conn) SendChunks(raw []byte, n int, pause time.Duration) error // n bytes per write (n < 1 = 1), pause between writes: split CRLF or the terminator across writes
//	func (c *Conn) SendDrip(raw []byte, pause time.Duration) error  // SendChunks with 1 byte
//
// Every wait of these helpers is the Conn's hang guard (testkit.Deadline() by default, so
// NNTP_TEST_DEADLINE scales it) or the duration the caller passes; none sleeps a fixed time
// except the caller's pause and DialUntil's 50 ms poll. Pipeline's guard slides: it is moved on
// with every reply read, so a stalled server fails after one deadline and a long batch does not.
//
// Strict mode: ReadLine reads raw bytes and returns a *StrictError when a line ends in a bare LF
// (Kind BareLF), contains a bare CR (BareCR) or arrives without any line end before EOF
// (NoCRLF); TryReadDotLines returns BadDotStuffing for a line that starts with "." but is neither
// ".." stuffed nor the terminator, and NoTerminator when the block ends (EOF or deadline) without
// ".\r\n" (also for a partial last line). StrictError has Kind (an int with String()), Line (the
// offending line, raw), Bytes (for Pending) and Err (the underlying read error, via Unwrap, so
// errors.Is(err, io.EOF) and net.Error timeout checks work), and a descriptive Error(). The tb-failing methods (Cmd, ReadDotLines, RawUntilDot) fail with the
// error text. In non-strict mode a bare LF is accepted as a line end, like a lenient client.
//
// Compression (RFC 8054 COMPRESS DEFLATE): after StartCompress c.Conn deflates every Write (raw
// DEFLATE, no zlib header), sync-flushes it and sends it in one socket write, so every Send and
// Cmd reaches the server whole and a server that answers and closes (QUIT) cannot fail the
// write of the command it answered, and c.R reads the inflated server stream, starting with the
// bytes buffered after the 206 line. Strict mode, ReadDotLines, RawUntilDot and ExpectNoPending
// keep their contracts on the inflated stream: an empty flush block inflates to nothing, so
// ExpectNoPending still waits its duration and returns nil, and bytes it reports stay readable.
// An inflater goroutine reads the socket and passes on every byte as soon as it can be decoded,
// whatever flush the server uses (sync, full, or zlib's partial flush as INN's nnrpd sends);
// the hang guard and other read deadlines apply to the inflated stream, so a timeout loses
// nothing, and writes keep the socket's deadline. A server that closes reads as io.EOF; corrupt
// compressed data is a read error wrapping flate.CorruptInputError (every later read returns
// it). Close ends the goroutine. The client does not stop a test from sending COMPRESS,
// STARTTLS or AUTHINFO after compression, so it can check the server's 502.
//
// The kit never contacts any host a test did not name; the deadline only stops a hung test.
package client
