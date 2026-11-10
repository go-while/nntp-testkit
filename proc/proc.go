package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// DefaultStopTimeout is the wait between SIGTERM and SIGKILL when Options.StopTimeout is 0.
const DefaultStopTimeout = 5 * time.Second

// DefaultLogBytes is the cap of each part of the failure dump (the captured output, each Logs
// file) when Options.LogBytes is 0. A log file keeps its tail, the captured output its head and
// tail; a "[... N bytes cut ...]" marker shows where bytes were left out.
const DefaultLogBytes = 64 << 10

// dialRetry is the pause between readiness dials in StartTCP.
const dialRetry = 20 * time.Millisecond

// Options configures the external process.
type Options struct {
	Path        string        // binary to run (required)
	Args        []string      // arguments after the binary name
	Env         []string      // appended to os.Environ()
	Dir         string        // working directory; "" = tb.TempDir()
	StopTimeout time.Duration // SIGTERM → SIGKILL; 0 = DefaultStopTimeout
	Deadline    time.Duration // readiness wait and pipe deadlines; 0 = testkit.Deadline()
	Logs        []string      // files (relative to Dir unless absolute) to dump on failure
	LogBytes    int           // cap per dumped log file and for the captured output; 0 = DefaultLogBytes

	// Ulimit sets resource limits of the process, each one "-<letter> <value>" as the shell's
	// ulimit takes it: "-n 16" (open files), "-f 64" (file size, in 512-byte blocks for sh),
	// "-v 200000" ... The process is started through /bin/sh, which sets them and execs Path
	// (so the PID and the signals stay the binary's). An entry of another form fails tb.
	Ulimit []string
}

// ulimitArg is the form of an Options.Ulimit entry.
var ulimitArg = regexp.MustCompile(`^-[A-Za-z] ([0-9]+|unlimited)$`)

// Process is a started external process. Stop it with Stop (also run by tb.Cleanup).
type Process struct {
	cmd         *exec.Cmd
	dir         string
	logs        []string
	addr        string
	stopTimeout time.Duration
	deadline    time.Duration
	logBytes    int
	conn        *pipeConn // stdio mode only

	out *lockedBuffer // captured stderr (and stdout in TCP mode)

	done    chan struct{} // closed when cmd.Wait returned
	waitErr error         // valid after done is closed

	stopOnce sync.Once
}

// lockedBuffer is a bytes.Buffer safe for concurrent Write and String.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newProcess fills the defaults of o and prepares the command; it does not start it.
func newProcess(tb testing.TB, o Options) *Process {
	tb.Helper()
	p := &Process{
		dir:         o.Dir,
		logs:        o.Logs,
		stopTimeout: o.StopTimeout,
		deadline:    o.Deadline,
		logBytes:    o.LogBytes,
		out:         &lockedBuffer{},
		done:        make(chan struct{}),
	}
	if p.dir == "" {
		p.dir = tb.TempDir()
	}
	if p.stopTimeout <= 0 {
		p.stopTimeout = DefaultStopTimeout
	}
	if p.deadline <= 0 {
		p.deadline = testkit.Deadline()
	}
	if p.logBytes <= 0 {
		p.logBytes = DefaultLogBytes
	}
	path := o.Path
	if !filepath.IsAbs(path) && strings.ContainsRune(path, filepath.Separator) {
		// os/exec resolves a relative path with a separator against cmd.Dir; the caller
		// means the test's working directory. A bare name keeps the PATH lookup.
		abs, err := filepath.Abs(path)
		if err != nil {
			tb.Fatalf("proc: resolve %s: %v", path, err)
		}
		path = abs
	}
	args := o.Args
	if len(o.Ulimit) > 0 {
		script := ""
		for _, u := range o.Ulimit {
			if !ulimitArg.MatchString(u) {
				tb.Fatalf("proc: Ulimit entry %q is not \"-<letter> <number|unlimited>\"", u)
			}
			script += "ulimit " + u + " || exit 126; "
		}
		args = append([]string{"-c", script + `exec "$0" "$@"`, path}, args...)
		path = "/bin/sh"
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), o.Env...)
	cmd.Stderr = p.out
	// Bounds Wait when a grandchild keeps the output pipe open after the process exited.
	cmd.WaitDelay = p.stopTimeout
	ownGroup(cmd)
	p.cmd = cmd
	return p
}

// start starts the command and the reaper goroutine and registers the cleanup. The reaper
// ends when the process exits; Stop (run by the cleanup) makes sure it does.
func (p *Process) start(tb testing.TB) error {
	tb.Helper()
	if p.cmd.Path == "" {
		return errors.New("Options.Path is empty")
	}
	err := lockedStart(p.cmd, func(err error) {
		// A daemon that inherited the output pipe keeps it open after the leader exited
		// successfully; WaitDelay then ends the wait with ErrWaitDelay. That is not a failure.
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}
		p.waitErr = err
		close(p.done)
	})
	if err != nil {
		return err
	}
	tb.Cleanup(func() {
		p.Stop()
		if tb.Failed() {
			p.dumpLogs(tb)
		}
	})
	return nil
}

// lockedStart starts cmd and returns the start error; on success cmd.Wait runs on the same
// goroutine and finished receives its result. The goroutine stays locked to its OS thread until
// the process has been waited for: with Pdeathsig the kernel kills the child when the thread
// that created it exits, so that thread must not be one the Go runtime may retire.
func lockedStart(cmd *exec.Cmd, finished func(error)) error {
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		started <- err
		if err != nil {
			return
		}
		finished(cmd.Wait())
	}()
	return <-started
}

// dumpLogs writes the captured output and the tail of every Logs file to the test output, each
// capped at the LogBytes of the Options.
func (p *Process) dumpLogs(tb testing.TB) {
	tb.Logf("proc: %s (pid %d) stderr:\n%s", p.cmd.Path, p.Pid(), cutMiddle(p.Stderr(), p.logBytes))
	for _, name := range p.logs {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.dir, path)
		}
		data, err := readTail(path, p.logBytes)
		if err != nil {
			tb.Logf("proc: log %s: not readable: %v", path, err)
			continue
		}
		tb.Logf("proc: log %s:\n%s", path, data)
	}
}

// readTail returns the last max bytes of the file, preceded by a cut marker when it is longer.
func readTail(path string, max int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	cut := int64(0)
	if size > int64(max) {
		cut = size - int64(max)
		if _, err := f.Seek(cut, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)))
	if err != nil {
		return "", err
	}
	if cut > 0 {
		return cutMarker(cut) + string(data), nil
	}
	return string(data), nil
}

// StartTCP starts the process (stdin from /dev/null; stdout and stderr captured, see Stderr)
// and waits until a dial to addr yields a greeting line, retrying every 20ms within Deadline.
// It fails tb when the deadline passes or the process exits first; the cleanup then stops the
// process and logs the captured output. Each probe connection reads the greeting, sends
// "QUIT\r\n" and closes, so at least one connection reaches the server before the test's own
// (relevant for connection caps and accept counters).
func StartTCP(tb testing.TB, o Options, addr string) *Process {
	tb.Helper()
	p := newProcess(tb, o)
	p.addr = addr
	p.cmd.Stdout = p.out
	if err := p.start(tb); err != nil {
		tb.Fatalf("proc: start %s: %v", o.Path, err)
		return p
	}
	limit := time.Now().Add(p.deadline)
	exited := false // the leader exited with status 0: a daemon may still come up
	for {
		err := probe(addr, limit)
		if err == nil {
			return p
		}
		if !exited {
			select {
			case <-p.done:
				if p.waitErr != nil {
					tb.Fatalf("proc: %s exited before %s was ready: %v (last dial: %v)", o.Path, addr, p.waitErr, err)
					return p
				}
				// Exit status 0: a server that daemonises (the parent exits, the daemon
				// keeps running in the group) looks like this; keep dialling until Deadline.
				exited = true
			default:
			}
		}
		if !time.Now().Before(limit) {
			if exited {
				tb.Fatalf("proc: %s exited 0 and nothing was ready on %s within %v: %v", o.Path, addr, p.deadline, err)
			} else {
				tb.Fatalf("proc: %s not ready on %s within %v: %v", o.Path, addr, p.deadline, err)
			}
			return p
		}
		t := time.NewTimer(dialRetry)
		if exited {
			<-t.C
		} else {
			select {
			case <-p.done:
			case <-t.C:
			}
		}
		t.Stop()
	}
}

// probe dials addr and reads one line before limit, then sends QUIT so a server with a
// connection cap frees the slot promptly.
func probe(addr string, limit time.Time) error {
	if !time.Now().Before(limit) {
		return errors.New("deadline passed")
	}
	d := net.Dialer{Deadline: limit}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(limit); err != nil {
		return err
	}
	if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	_, _ = c.Write([]byte("QUIT\r\n"))
	return nil
}

// Conn returns the client side of the stdio pipes; nil in TCP mode. Stop closes it.
func (p *Process) Conn() net.Conn {
	if p.conn == nil {
		return nil
	}
	return p.conn
}

// Addr returns the TCP address given to StartTCP; "" in stdio mode.
func (p *Process) Addr() string { return p.addr }

// Pid returns the process id (0 when the process never started).
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Signal sends sig to the process itself (the group leader); SignalGroup reaches its children too.
func (p *Process) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("proc: process not started")
	}
	return p.cmd.Process.Signal(sig)
}

// SignalGroup sends sig to every process of the server's process group (the leader and the
// children and grandchildren that did not leave the group with setsid or setpgid). It succeeds
// when the group no longer exists.
func (p *Process) SignalGroup(sig syscall.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("proc: process not started")
	}
	return killGroup(p.cmd.Process.Pid, sig)
}

// Stop sends SIGTERM to the whole process group of the server, then SIGKILL to the group when
// the leader or any other member is still there after StopTimeout (at most Deadline), waits for
// the leader and closes the stdio pipes. So a server that forks workers leaves no orphan, and
// a daemon that the leader started and left running (the leader exited, its children stay in
// the group) is stopped too. A process that moved to another group or session (setsid) is not
// reached. It is idempotent and also run by tb.Cleanup. It needs a Unix system. A zombie that
// nothing reaps (a container whose PID 1 never calls wait) counts as a member until the grace
// period ends.
func (p *Process) Stop() {
	p.stopOnce.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		pid := p.cmd.Process.Pid
		_ = killGroup(pid, syscall.SIGTERM)
		t := time.NewTimer(min(p.stopTimeout, p.deadline))
		if !p.waitGone(pid, t.C) {
			_ = killGroup(pid, syscall.SIGKILL)
			<-p.done
		}
		t.Stop()
		if p.conn != nil {
			_ = p.conn.Close()
		}
	})
}

// waitGone waits until the leader has exited and its group is empty; false when limit fires
// first.
func (p *Process) waitGone(pid int, limit <-chan time.Time) bool {
	for {
		select {
		case <-p.done:
			if !groupAlive(pid) {
				return true
			}
		default:
		}
		poll := time.NewTimer(10 * time.Millisecond)
		select {
		case <-limit:
			poll.Stop()
			return false
		case <-poll.C:
		}
	}
}

// Wait blocks until the process has exited and returns its exit error (nil on exit status 0,
// also when a daemon it left running still holds the output pipe: exec.ErrWaitDelay is dropped);
// it is safe to call before or after Stop. When the process has not exited within Deadline,
// Wait calls Stop and returns a "not exited within" error joined with the exit error; later
// calls return the recorded exit error. It returns an error at once when the process never
// started.
func (p *Process) Wait() error {
	if p.cmd.Process == nil {
		return errors.New("proc: process not started")
	}
	t := time.NewTimer(p.deadline)
	defer t.Stop()
	select {
	case <-p.done:
		return p.waitErr
	case <-t.C:
	}
	p.Stop()
	return errors.Join(fmt.Errorf("proc: %s not exited within %v, stopped", p.cmd.Path, p.deadline), p.Wait())
}

// Done returns a channel that is closed once the process has exited and its output has been
// collected: a non-blocking check (select with default) or a bounded wait (select with a
// timer) that, unlike Wait, never stops the process.
func (p *Process) Done() <-chan struct{} { return p.done }

// Stderr returns the output captured so far: stderr, plus stdout in TCP mode. After Wait or
// Stop it holds everything the process wrote.
func (p *Process) Stderr() string { return p.out.String() }
