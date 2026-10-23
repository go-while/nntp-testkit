// Package proc starts an external NNTP server binary for a test, either with stdin/stdout as the
// client connection (inetd style) or on a TCP address with readiness detected by dialling until a
// greeting arrives, and stops it afterwards (SIGTERM, then SIGKILL after a timeout). Its stderr
// and named log files are written to the test output when the test fails. Stop relies on
// SIGTERM/SIGKILL, so the package needs a Unix system. Wait is bounded by the Deadline (it
// stops the process when that passes). In TCP mode the child's stdout is captured with its
// stderr, and one readiness probe connection (greeting, QUIT) reaches the server before the
// test's own; a relative Path with a separator is resolved against the caller's directory,
// a bare name through PATH.
//
// Contract (plan nntp-testkit; implemented in proc.go and stdio.go):
//
//	type Options struct {
//		Path        string        // binary to run (required)
//		Args        []string
//		Env         []string      // appended to os.Environ()
//		Dir         string        // working directory; "" = tb.TempDir()
//		StopTimeout time.Duration // SIGTERM → SIGKILL; 0 = 5s
//		Deadline    time.Duration // readiness wait and pipe deadlines; 0 = testkit.Deadline()
//		Logs        []string      // files (relative to Dir unless absolute) to dump on failure
//		Ulimit      []string      // "-n 16", "-f 64", ...: limits set by /bin/sh before it execs Path
//	}
//	type Process struct { ... }
//	func StartStdio(tb testing.TB, o Options) *Process // stdin/stdout piped; Conn() is the client side
//	func StartTCP(tb testing.TB, o Options, addr string) *Process // waits until a dial to addr yields a greeting line, within Deadline; fails tb otherwise
//	func (p *Process) Conn() net.Conn                  // stdio mode: a net.Conn over the pipes (deadlines honoured); nil in TCP mode
//	func (p *Process) Addr() string                    // TCP mode: addr; "" in stdio mode
//	func (p *Process) Pid() int
//	func (p *Process) Signal(sig os.Signal) error
//	func (p *Process) Stop()                            // SIGTERM, SIGKILL after StopTimeout, then Wait; idempotent; also run by tb.Cleanup
//	func (p *Process) Wait() error                      // the exit error, once
//	func (p *Process) Done() <-chan struct{}            // closed once the process has exited; never stops it
//	func (p *Process) Stderr() string                   // captured so far (stdout too in TCP mode)
//	const DefaultStopTimeout = 5 * time.Second
//
// Both Start functions register a tb.Cleanup that stops the process and, when tb.Failed(), logs
// Stderr() and the Logs files with tb.Log.
package proc
