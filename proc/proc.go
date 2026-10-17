package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// DefaultStopTimeout is the wait between SIGTERM and SIGKILL when Options.StopTimeout is 0.
const DefaultStopTimeout = 5 * time.Second

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
}

// Process is a started external process. Stop it with Stop (also run by tb.Cleanup).
type Process struct {
	cmd         *exec.Cmd
	dir         string
	logs        []string
	addr        string
	stopTimeout time.Duration
	deadline    time.Duration
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
	cmd := exec.Command(o.Path, o.Args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), o.Env...)
	cmd.Stderr = p.out
	// Bounds Wait when a grandchild keeps the output pipe open after the process exited.
	cmd.WaitDelay = p.stopTimeout
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
	if err := p.cmd.Start(); err != nil {
		return err
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	tb.Cleanup(func() {
		p.Stop()
		if tb.Failed() {
			p.dumpLogs(tb)
		}
	})
	return nil
}

// dumpLogs writes the captured output and every Logs file to the test output.
func (p *Process) dumpLogs(tb testing.TB) {
	tb.Logf("proc: %s (pid %d) stderr:\n%s", p.cmd.Path, p.Pid(), p.Stderr())
	for _, name := range p.logs {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Logf("proc: log %s: not readable: %v", path, err)
			continue
		}
		tb.Logf("proc: log %s:\n%s", path, data)
	}
}

// StartTCP starts the process (stdin from /dev/null; stdout and stderr captured, see Stderr)
// and waits until a dial to addr yields a greeting line, retrying every 20ms within Deadline.
// It fails tb when the deadline passes or the process exits first; the cleanup then stops the
// process and logs the captured output.
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
	for {
		err := probe(addr, time.Until(limit))
		if err == nil {
			return p
		}
		select {
		case <-p.done:
			tb.Fatalf("proc: %s exited before %s was ready: %v (last dial: %v)", o.Path, addr, p.waitErr, err)
			return p
		default:
		}
		if !time.Now().Before(limit) {
			tb.Fatalf("proc: %s not ready on %s within %v: %v", o.Path, addr, p.deadline, err)
			return p
		}
		t := time.NewTimer(dialRetry)
		select {
		case <-p.done:
		case <-t.C:
		}
		t.Stop()
	}
}

// probe dials addr and reads one line within d.
func probe(addr string, d time.Duration) error {
	if d <= 0 {
		return errors.New("deadline passed")
	}
	c, err := net.DialTimeout("tcp", addr, d)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetReadDeadline(time.Now().Add(d)); err != nil {
		return err
	}
	if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
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

// Signal sends sig to the process.
func (p *Process) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return errors.New("proc: process not started")
	}
	return p.cmd.Process.Signal(sig)
}

// Stop sends SIGTERM, then SIGKILL when the process has not exited after StopTimeout, waits
// for it and closes the stdio pipes. It is idempotent and also run by tb.Cleanup.
func (p *Process) Stop() {
	p.stopOnce.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			t := time.NewTimer(p.stopTimeout)
			select {
			case <-p.done:
			case <-t.C:
				_ = p.cmd.Process.Kill()
				<-p.done
			}
			t.Stop()
		}
		if p.conn != nil {
			_ = p.conn.Close()
		}
	})
}

// Wait blocks until the process has exited and returns its exit error (nil on exit status 0).
// Every call returns the same value; it is safe to call before or after Stop. It returns an
// error at once when the process never started.
func (p *Process) Wait() error {
	if p.cmd.Process == nil {
		return errors.New("proc: process not started")
	}
	<-p.done
	return p.waitErr
}

// Stderr returns the output captured so far: stderr, plus stdout in TCP mode. After Wait or
// Stop it holds everything the process wrote.
func (p *Process) Stderr() string { return p.out.String() }
