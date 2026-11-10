package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// DefaultMaxOutput is the cap of RunOptions.MaxOutput when it is 0.
const DefaultMaxOutput = 1 << 20

// DefaultWaitDelay is RunOptions.WaitDelay when it is 0.
const DefaultWaitDelay = 3 * time.Second

// ErrTimeout is wrapped by the error of TryRun (and so shown by Run) when the command did not
// finish within RunOptions.Timeout; test with errors.Is.
var ErrTimeout = errors.New("timeout")

// RunOptions configures a one-shot command for Run and TryRun.
type RunOptions struct {
	Path string   // binary to run (required); a relative path with a separator is resolved against the caller's directory, a bare name through PATH
	Args []string // arguments after the binary name
	Env  []string // appended to os.Environ(), as in Options (later entries win)
	Dir  string   // working directory; "" = the caller's (unlike Options, which uses tb.TempDir())

	Stdin io.Reader // standard input; nil = /dev/null. Use strings.NewReader for a string

	// Timeout bounds the whole command; 0 = testkit.Deadline(). When it passes, the command's
	// whole process group is killed.
	Timeout time.Duration

	// MaxOutput caps the combined output kept, in bytes: the first and the last half are
	// kept with a "[... N bytes cut ...]" marker between them, so a tool stuck in a loop
	// cannot flood the log or memory. 0 = DefaultMaxOutput; negative = no cap.
	MaxOutput int

	// WaitDelay is how long to keep waiting for the output pipe after the command itself
	// has exited: a command that starts a daemon leaves it holding the pipe, and must not
	// hang the test. 0 = DefaultWaitDelay. The command's exit code still counts.
	WaitDelay time.Duration
}

// Run runs the command, waits for it and returns its combined stdout and stderr (capped, see
// MaxOutput) and its exit code. It fails tb (tb.Fatalf with the output) when the command cannot
// start, does not finish within Timeout (see ErrTimeout) or is killed by a signal; a non-zero
// exit is not a failure here, the caller checks code. See TryRun for the process-group and
// Pdeathsig behaviour.
func Run(tb testing.TB, o RunOptions) (out string, code int) {
	tb.Helper()
	out, code, err := TryRun(o)
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return out, code
}

// TryRun is Run that returns the error instead of failing a test, and needs no tb.
//
// code is the exit status of the command, or -1 when it did not exit normally (it never started,
// timed out or was killed by a signal; err says which, and names the signal). err is nil for any
// exit status, so a non-zero code is the caller's to judge.
//
// The command runs in its own process group. When Timeout passes the whole group is sent SIGKILL,
// so a shell and the sleeps it started are gone together, and the error wraps ErrTimeout and
// holds the command line, the timeout and the output so far. On Linux the command also gets
// Pdeathsig SIGKILL, so a test binary that is killed does not leave it running. When the command
// exits by itself its group is not touched: a daemon it started keeps running (the output pipe
// it holds is given up after WaitDelay and exec.ErrWaitDelay is not reported).
func TryRun(o RunOptions) (out string, code int, err error) {
	if o.Path == "" {
		return "", -1, errors.New("proc: RunOptions.Path is empty")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = testkit.Deadline()
	}
	waitDelay := o.WaitDelay
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	path := o.Path
	if !filepath.IsAbs(path) && strings.ContainsRune(path, filepath.Separator) {
		abs, aerr := filepath.Abs(path)
		if aerr != nil {
			return "", -1, fmt.Errorf("proc: resolve %s: %w", path, aerr)
		}
		path = abs
	}
	name := filepath.Base(path) + shellJoin(o.Args)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, o.Args...)
	cmd.Dir = o.Dir
	cmd.Env = append(os.Environ(), o.Env...)
	cmd.Stdin = o.Stdin
	buf := newCapWriter(o.MaxOutput)
	if o.MaxOutput == 0 {
		buf = newCapWriter(DefaultMaxOutput)
	}
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.WaitDelay = waitDelay
	ownGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid, syscall.SIGKILL) }

	res := make(chan error, 1)
	if serr := lockedStart(cmd, func(werr error) { res <- werr }); serr != nil {
		return "", -1, fmt.Errorf("proc: start %s: %w", name, serr)
	}
	werr := <-res
	unregisterGroup(cmd.Process.Pid)
	out = buf.String()
	if werr != nil && ctx.Err() != nil {
		// The group was killed by Cancel (or the exit raced the deadline and failed).
		return out, -1, fmt.Errorf("proc: %s did not finish within %v, killed with its process group: %w\n--- output so far ---\n%s",
			name, timeout, ErrTimeout, out)
	}
	switch {
	case werr == nil, errors.Is(werr, exec.ErrWaitDelay):
		return out, 0, nil
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return out, -1, fmt.Errorf("proc: %s killed by signal %v (%d)", name, ws.Signal(), int(ws.Signal()))
		}
		return out, ee.ExitCode(), nil
	}
	return out, -1, fmt.Errorf("proc: %s: %w", name, werr)
}

// shellJoin renders args for an error message, each one after a space.
func shellJoin(args []string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteByte(' ')
		if a == "" || strings.ContainsAny(a, " \t\n'\"") {
			fmt.Fprintf(&b, "%q", a)
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}
