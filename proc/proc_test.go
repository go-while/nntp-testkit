package proc

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// echoServer is built once per test binary into a directory that TestMain removes.
var (
	echoDir  string
	echoOnce sync.Once
	echoBin  string
	echoErr  error
	echoSkip string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "proc-echo-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "proc test: %v\n", err)
		os.Exit(1)
	}
	echoDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// buildEcho returns the path of the echo-server fixture, building it on first use. It skips tb
// when the go tool is not on PATH.
func buildEcho(tb testing.TB) string {
	tb.Helper()
	echoOnce.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			echoSkip = "go tool not on PATH: " + err.Error()
			return
		}
		bin := filepath.Join(echoDir, "echo-server")
		cmd := exec.Command(goTool, "build", "-o", bin, "./testdata/echo-server")
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			echoErr = fmt.Errorf("go build echo-server: %v\n%s", err, out)
			return
		}
		echoBin = bin
	})
	if echoSkip != "" {
		tb.Skip(echoSkip)
	}
	if echoErr != nil {
		tb.Fatal(echoErr)
	}
	return echoBin
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		tb.Fatal(err)
	}
	return addr
}

// exchange writes cmd and returns the reply line without CRLF.
func exchange(tb testing.TB, c net.Conn, br *bufio.Reader, cmd string) string {
	tb.Helper()
	if _, err := c.Write([]byte(cmd + "\r\n")); err != nil {
		tb.Fatalf("write %q: %v", cmd, err)
	}
	return readLine(tb, br)
}

func readLine(tb testing.TB, br *bufio.Reader) string {
	tb.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		tb.Fatalf("read: %v (got %q)", err, line)
	}
	return strings.TrimRight(line, "\r\n")
}

func TestStdio(t *testing.T) {
	bin := buildEcho(t)
	dir := t.TempDir()
	p := StartStdio(t, Options{Path: bin, Args: []string{"-log", "cmds.log"}, Dir: dir})
	if p.Addr() != "" {
		t.Errorf("Addr() = %q in stdio mode", p.Addr())
	}
	if p.Pid() <= 0 {
		t.Errorf("Pid() = %d", p.Pid())
	}
	c := p.Conn()
	if c == nil {
		t.Fatal("Conn() = nil in stdio mode")
	}
	if c.LocalAddr().Network() != "pipe" || c.RemoteAddr().Network() != "pipe" {
		t.Errorf("addr networks %q/%q, want pipe", c.LocalAddr().Network(), c.RemoteAddr().Network())
	}
	br := bufio.NewReader(c)
	if got := readLine(t, br); got != "200 proc-echo ready" {
		t.Fatalf("greeting %q", got)
	}
	if got := exchange(t, c, br, "HELP"); got != "500 echo HELP" {
		t.Fatalf("reply %q", got)
	}

	// The server is silent now: a short read deadline must give a timeout error.
	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := br.ReadString('\n')
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read after deadline: %v, want os.ErrDeadlineExceeded", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read after deadline: %T %v is not a net.Error timeout", err, err)
	}
	// Clearing the deadline makes the conn usable again.
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := exchange(t, c, br, "DATE"); got != "500 echo DATE" {
		t.Fatalf("reply after timeout %q", got)
	}

	p.Stop()
	if err := p.Wait(); err != nil {
		t.Errorf("Wait after Stop: %v, want nil (exit 0 on SIGTERM)", err)
	}
	if !strings.Contains(p.Stderr(), "started") {
		t.Errorf("Stderr() = %q, want it to contain started", p.Stderr())
	}
	data, err := os.ReadFile(filepath.Join(dir, "cmds.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "HELP\nDATE\n" {
		t.Errorf("cmds.log = %q", data)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read after Stop: %v, want net.ErrClosed", err)
	}
}

func TestStdioCloseIsEOF(t *testing.T) {
	bin := buildEcho(t)
	p := StartStdio(t, Options{Path: bin})
	br := bufio.NewReader(p.Conn())
	if got := readLine(t, br); got != "200 proc-echo ready" {
		t.Fatalf("greeting %q", got)
	}
	if err := p.Conn().Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Errorf("Wait after stdin EOF: %v, want nil", err)
	}
	p.Stop() // no-op on an exited process
}

func TestTCP(t *testing.T) {
	bin := buildEcho(t)
	addr := freeAddr(t)
	p := StartTCP(t, Options{Path: bin, Args: []string{"-listen", addr}}, addr)
	if p.Addr() != addr {
		t.Errorf("Addr() = %q, want %q", p.Addr(), addr)
	}
	if p.Conn() != nil {
		t.Errorf("Conn() != nil in TCP mode")
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	if got := readLine(t, br); got != "200 proc-echo ready" {
		t.Fatalf("greeting %q", got)
	}
	if got := exchange(t, c, br, "LIST"); got != "500 echo LIST" {
		t.Fatalf("reply %q", got)
	}

	// echo-server handles SIGHUP; if it killed the process, Wait would report the signal.
	if err := p.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if got := exchange(t, c, br, "MODE READER"); got != "500 echo MODE READER" {
		t.Fatalf("reply after SIGHUP %q", got)
	}
	if got := exchange(t, c, br, "QUIT"); got != "205 bye" {
		t.Fatalf("QUIT reply %q", got)
	}

	p.Stop()
	if err := p.Wait(); err != nil {
		t.Errorf("Wait after Stop: %v, want nil (SIGHUP ignored, exit 0 on SIGTERM)", err)
	}
	p.Stop() // second Stop is a no-op
	if err := p.Wait(); err != nil {
		t.Errorf("second Wait: %v", err)
	}
	if !strings.Contains(p.Stderr(), "signal hangup") {
		t.Errorf("Stderr() = %q, want the SIGHUP to be seen", p.Stderr())
	}
}

func TestDone(t *testing.T) {
	bin := buildEcho(t)
	addr := freeAddr(t)
	p := StartTCP(t, Options{Path: bin, Args: []string{"-listen", addr}}, addr)
	select {
	case <-p.Done():
		t.Fatal("Done closed while the process runs")
	default:
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done not closed 10s after SIGTERM")
	}
	if err := p.Wait(); err != nil {
		t.Errorf("Wait after Done: %v, want nil (exit 0 on SIGTERM)", err)
	}
}

func TestStopTimeoutKills(t *testing.T) {
	bin := buildEcho(t)
	p := StartStdio(t, Options{Path: bin, Args: []string{"-ignore-term"}, StopTimeout: 200 * time.Millisecond})
	// The greeting comes after the signal handler is installed, so SIGTERM is ignored from here.
	if got := readLine(t, bufio.NewReader(p.Conn())); got != "200 proc-echo ready" {
		t.Fatalf("greeting %q", got)
	}
	p.Stop()
	err := p.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Wait: %v, want *exec.ExitError", err)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("Wait: %v, want killed by SIGKILL", err)
	}
}

// fakeTB records Log output, failures and cleanups; everything else goes to the real test.
type fakeTB struct {
	testing.TB
	mu       sync.Mutex
	logs     []string
	fatals   []string
	failed   bool
	cleanups []func()
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Log(args ...any) { f.record(&f.logs, fmt.Sprint(args...)) }

func (f *fakeTB) Logf(format string, args ...any) { f.record(&f.logs, fmt.Sprintf(format, args...)) }

func (f *fakeTB) Errorf(format string, args ...any) {
	f.record(&f.fatals, fmt.Sprintf(format, args...))
	f.setFailed()
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}

func (f *fakeTB) Fatal(args ...any) {
	f.record(&f.fatals, fmt.Sprint(args...))
	f.setFailed()
	runtime.Goexit()
}

func (f *fakeTB) Failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

func (f *fakeTB) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeTB) setFailed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = true
}

func (f *fakeTB) record(dst *[]string, s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*dst = append(*dst, s)
}

func (f *fakeTB) runCleanups() {
	f.mu.Lock()
	fns := f.cleanups
	f.cleanups = nil
	f.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

func (f *fakeTB) allLogs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.logs, "\n")
}

func TestFailureLogging(t *testing.T) {
	bin := buildEcho(t)
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%v", failed), func(t *testing.T) {
			dir := t.TempDir()
			extra := filepath.Join(t.TempDir(), "extra.log")
			if err := os.WriteFile(extra, []byte("extra-content\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ftb := &fakeTB{TB: t}
			t.Cleanup(ftb.runCleanups) // a real-t failure must not leak the child
			p := StartStdio(ftb, Options{
				Path: bin,
				Args: []string{"-log", "server.log"},
				Dir:  dir,
				Logs: []string{"server.log", extra, "missing.log"},
			})
			br := bufio.NewReader(p.Conn())
			if got := readLine(t, br); got != "200 proc-echo ready" {
				t.Fatalf("greeting %q", got)
			}
			if got := exchange(t, p.Conn(), br, "HELLO-LOGGED"); got != "500 echo HELLO-LOGGED" {
				t.Fatalf("reply %q", got)
			}
			if failed {
				ftb.setFailed()
			}
			ftb.runCleanups()
			if err := p.Wait(); err != nil {
				t.Errorf("Wait after cleanup: %v", err)
			}
			logs := ftb.allLogs()
			if !failed {
				if logs != "" {
					t.Errorf("passing test logged:\n%s", logs)
				}
				return
			}
			for _, want := range []string{
				"started",
				"HELLO-LOGGED",
				"extra-content",
				filepath.Join(dir, "missing.log") + ": not readable",
			} {
				if !strings.Contains(logs, want) {
					t.Errorf("logs lack %q:\n%s", want, logs)
				}
			}
		})
	}
}

func TestStartTCPExitBeforeReady(t *testing.T) {
	bin := buildEcho(t)
	ftb := &fakeTB{TB: t}
	t.Cleanup(ftb.runCleanups)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// An invalid listen address makes echo-server exit 1 at once; 127.0.0.1:1 refuses.
		StartTCP(ftb, Options{Path: bin, Args: []string{"-listen", "bad:addr:x"}}, "127.0.0.1:1")
	}()
	<-done
	ftb.runCleanups()
	if !ftb.Failed() {
		t.Fatal("StartTCP did not fail the TB")
	}
	if len(ftb.fatals) != 1 || !strings.Contains(ftb.fatals[0], "exited before") {
		t.Errorf("fatals = %q, want one 'exited before' message", ftb.fatals)
	}
	if logs := ftb.allLogs(); !strings.Contains(logs, "listen:") {
		t.Errorf("logs lack the server's stderr:\n%s", logs)
	}
}

func TestRelativePath(t *testing.T) {
	bin := buildEcho(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.ContainsRune(rel, filepath.Separator) {
		t.Fatalf("relative path %q has no separator", rel)
	}
	// Dir differs from the working directory: rel must not be resolved against it.
	p := StartStdio(t, Options{Path: rel, Dir: t.TempDir()})
	if got := readLine(t, bufio.NewReader(p.Conn())); got != "200 proc-echo ready" {
		t.Fatalf("greeting %q", got)
	}
	p.Stop()
	if err := p.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestWaitDeadlineStops(t *testing.T) {
	bin := buildEcho(t)
	p := StartStdio(t, Options{
		Path:        bin,
		Args:        []string{"-ignore-term"},
		Deadline:    200 * time.Millisecond,
		StopTimeout: 200 * time.Millisecond,
	})
	c := p.Conn()
	// The greeting read gets the kit's hang guard, not the short Deadline under test.
	if err := c.SetReadDeadline(time.Now().Add(testkit.Deadline())); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, bufio.NewReader(c)); got != "200 proc-echo ready" {
		t.Fatalf("greeting %q", got)
	}
	// stdin stays open and SIGTERM is ignored: Wait hits its deadline, Stop escalates to SIGKILL.
	err := p.Wait()
	if err == nil || !strings.Contains(err.Error(), "not exited within") {
		t.Fatalf("Wait: %v, want a 'not exited within' error", err)
	}
	killed := func(err error) bool {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return false
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
	}
	if !killed(err) {
		t.Errorf("Wait: %v, want the SIGKILL exit joined in", err)
	}
	if err2 := p.Wait(); !killed(err2) || strings.Contains(err2.Error(), "not exited within") {
		t.Errorf("second Wait: %v, want the recorded SIGKILL exit", err2)
	}
}

func TestUlimit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	p := StartStdio(t, Options{
		Path:   "/bin/sh",
		Args:   []string{"-c", `ulimit -n; ulimit -f; echo "$$"; read x`},
		Ulimit: []string{"-n 17", "-f 64"},
	})
	br := bufio.NewReader(p.Conn())
	if got := readLine(t, br); got != "17" {
		t.Errorf("ulimit -n: %q, want 17", got)
	}
	if got := readLine(t, br); got != "64" {
		t.Errorf("ulimit -f: %q, want 64", got)
	}
	if got := readLine(t, br); got != strconv.Itoa(p.Pid()) {
		t.Errorf("pid %q, want %d (the wrapper execs)", got, p.Pid())
	}

	for _, bad := range []string{"-n", "-n 1; rm -rf x", "n 5", "-nn 5"} {
		ftb := &fakeTB{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			StartStdio(ftb, Options{Path: "/bin/sh", Args: []string{"-c", "true"}, Ulimit: []string{bad}})
		}()
		<-done
		ftb.runCleanups()
		if !ftb.Failed() {
			t.Errorf("Ulimit %q did not fail the TB", bad)
		}
	}
}
