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
//	func Dial(tb testing.TB, addr string) *Conn            // dial within the deadline, read the greeting, tb.Cleanup(Close); Strict
//	func TryDial(addr string, deadline time.Duration) (*Conn, error) // no testing dependency; Strict; reads the greeting
//	func New(tb testing.TB, c net.Conn) *Conn               // wrap an open conn; no greeting read; tb.Cleanup(Close); Strict
//	func (c *Conn) ReadGreeting() (string, error)          // reads one line into c.Greeting
//	func (c *Conn) Send(raw string) error                  // raw bytes, nothing appended (pipelining)
//	func (c *Conn) ReadLine() (string, error)              // one line, CRLF stripped (LF in non-strict mode)
//	func (c *Conn) ReadStatus() (int, string, error)       // next status line: code and the full line
//	func (c *Conn) TryCmd(format string, args ...any) (int, string, error) // Send(format+CRLF) then ReadStatus
//	func (c *Conn) Cmd(format string, args ...any) (int, string)           // TryCmd; fails tb on error
//	func (c *Conn) TryReadDotLines() ([]string, error)     // dot-terminated block, un-stuffed, CRLF stripped
//	func (c *Conn) ReadDotLines() []string                 // TryReadDotLines; fails tb on error
//	func (c *Conn) RawUntilDot() []byte                    // exact bytes up to and including "\r\n.\r\n" (or ".\r\n" when empty); fails tb on error
//	func (c *Conn) ExpectNoPending(d time.Duration) error  // read for d; any byte → *StrictError{Kind: Pending, Bytes: ...}; nil on timeout
//	func (c *Conn) Close()
//
//	func ParseCode(line string) (int, error)               // 3-digit code at the start, followed by space or end
//	func ExpectCode(code int, line string, want ...int) error
//	func ExpectLines(got, want []string) error
//	func Unstuff(wire []string) []string                   // ".." prefix → "."
//
// Strict mode: ReadLine reads raw bytes and returns a *StrictError when a line ends in a bare LF
// (Kind BareLF), contains a bare CR (BareCR) or arrives without any line end before EOF
// (NoCRLF); TryReadDotLines returns BadDotStuffing for a line that starts with "." but is neither
// ".." stuffed nor the terminator, and NoTerminator when the block ends (EOF or deadline) without
// ".\r\n". StrictError has Kind, Line (the offending line, raw) and Bytes (for Pending) and a
// descriptive Error(). The tb-failing methods (Cmd, ReadDotLines, RawUntilDot) fail with the
// error text. In non-strict mode a bare LF is accepted as a line end, like a lenient client.
//
// The kit never contacts any host a test did not name; the deadline only stops a hung test.
package client
