// Package proc starts an external NNTP server binary for a test, either with stdin/stdout as the
// client connection (inetd style) or on a TCP address with readiness detected by dialling until a
// greeting arrives, and stops it afterwards (SIGTERM, then SIGKILL after a timeout). Its stderr
// and named log files are written to the test output when the test fails. It also runs one-shot
// helper commands (Run, TryRun) and builds Go servers (GoBuild). Stop relies on signals and
// process groups, so the package needs a Unix system.
//
// Process groups: the server and every one-shot command start as the leader of a new process
// group. Stop signals the whole group (SIGTERM, then SIGKILL), so a server that forks workers,
// or a daemon that its leader started and left running, leaves no orphan; a timed-out Run kills
// the group too. A process that calls setsid or setpgid leaves the group and is not reached.
// On Linux the direct child also gets Pdeathsig SIGKILL, so a test binary that is killed (CI
// cancel, kill -9, go test timeout) takes its servers with it; the kernel ties that signal to the
// OS thread that started the child, so the Start functions and Run start and wait for it on a
// goroutine locked to one thread. A descendant that forked away does not have Pdeathsig, and
// macOS has none at all: there a killed test binary can leave servers behind (a normal exit,
// failure or timeout of the test does not). A server that daemonises (the parent exits with
// status 0 and the daemon keeps running in the group) works: StartTCP keeps dialling until the
// Deadline instead of failing at the parent's exit, and Stop stops the daemon. Wait is bounded by the Deadline (it
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
//		LogBytes    int           // cap of each part of the failure dump; 0 = DefaultLogBytes (64 KiB)
//		Ulimit      []string      // "-n 16", "-f 64", ...: limits set by /bin/sh before it execs Path
//	}
//	type Process struct { ... }
//	func StartStdio(tb testing.TB, o Options) *Process // stdin/stdout piped; Conn() is the client side
//	func StartTCP(tb testing.TB, o Options, addr string) *Process // waits until a dial to addr yields a greeting line, within Deadline; fails tb otherwise
//	func (p *Process) Conn() net.Conn                  // stdio mode: a net.Conn over the pipes (deadlines honoured); nil in TCP mode
//	func (p *Process) Addr() string                    // TCP mode: addr; "" in stdio mode
//	func (p *Process) Pid() int
//	func (p *Process) Signal(sig os.Signal) error      // the leader only
//	func (p *Process) SignalGroup(sig syscall.Signal) error // every process of the group; nil when the group is gone
//	func (p *Process) Stop()                            // SIGTERM to the group, SIGKILL after min(StopTimeout, Deadline) while any member is left; idempotent; also run by tb.Cleanup
//	func (p *Process) Wait() error                      // the exit error, once
//	func (p *Process) Done() <-chan struct{}            // closed once the process has exited; never stops it
//	func (p *Process) Stderr() string                   // captured so far (stdout too in TCP mode)
//	const DefaultStopTimeout = 5 * time.Second
//
// Both Start functions register a tb.Cleanup that stops the process and, when tb.Failed(), logs
// Stderr() and the Logs files with tb.Log. Each part of that dump is capped at LogBytes: a log
// file keeps its tail, the captured output its head and tail, with a "[... N bytes cut ...]"
// marker.
//
// One-shot commands:
//
//	type RunOptions struct {
//		Path      string        // binary (required)
//		Args      []string
//		Env       []string      // appended to os.Environ(), as in Options
//		Dir       string        // "" = the caller's directory (Options uses tb.TempDir())
//		Stdin     io.Reader     // nil = /dev/null
//		Timeout   time.Duration // 0 = testkit.Deadline(); then the process group is killed
//		MaxOutput int           // cap of the combined output, head and tail kept; 0 = DefaultMaxOutput (1 MiB), <0 = none
//		WaitDelay time.Duration // wait for the output pipe after the command exited; 0 = DefaultWaitDelay (3s)
//	}
//	func Run(tb testing.TB, o RunOptions) (out string, code int) // fails tb on start error, timeout, signal
//	func TryRun(o RunOptions) (out string, code int, err error)
//	var ErrTimeout error // wrapped by the timeout error
//
// out is stdout and stderr combined. code is the exit status, -1 when there was none (start
// error, timeout, signal). err is nil for every exit status, a non-zero one included, so the
// caller decides what counts as failure; it is set for a start error, a timeout ("did not
// finish within <d>, killed with its process group", wrapping ErrTimeout, with the output so far)
// and a death by signal (the signal is named). A command that exits while a child it started
// keeps the output pipe open (a daemon) is given up on after WaitDelay and its exit code counts
// (exec.ErrWaitDelay is not an error); the daemon is left running.
//
// Building Go servers:
//
//	func GoBuild(tb testing.TB, out, pkg string, ldflags ...string) string // binary path
//	func CleanBuilds()                                                    // for TestMain
//
// GoBuild runs "go build" under TryRun with a timeout of at least GoBuildFloor (5 minutes), and
// caches the result per process by (out, pkg, ldflags), so many tests share one build.
package proc
