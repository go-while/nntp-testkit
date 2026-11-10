package proc

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
)

// waitForFile returns the trimmed content of a file once it has a line.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	var got string
	testkit.WaitFor(t, 10*time.Second, path, func() bool {
		b, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(b), "\n") {
			return false
		}
		got = strings.TrimSpace(string(b))
		return true
	})
	return got
}

// forkingServer starts the echo server under a shell that first forks a sleeping child (which
// writes its pid to a file); wrapper "exec" keeps the shell's pid for the server, "daemon" runs
// the server in the background and lets the shell exit 0.
func forkingServer(t *testing.T, tb testing.TB, mode, addr string, extra Options) (*Process, int) {
	t.Helper()
	bin := buildEcho(t)
	dir := t.TempDir()
	script := `sleep 60 & echo $! > "$PIDFILE"; `
	switch mode {
	case "exec":
		script += `exec "$0" "$@"`
	case "daemon":
		script += `"$0" "$@" & exit 0`
	}
	o := extra
	o.Path = "/bin/sh"
	o.Args = []string{"-c", script, bin, "-listen", addr}
	o.Env = []string{"PIDFILE=" + filepath.Join(dir, "child.pid")}
	o.Dir = dir
	p := StartTCP(tb, o, addr)
	pid, err := strconv.Atoi(waitForFile(t, filepath.Join(dir, "child.pid")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) }) // if the test fails first
	return p, pid
}

func TestStartTCPCleanupKillsForkedGrandchild(t *testing.T) {
	ftb := &fakeTB{TB: t}
	t.Cleanup(ftb.runCleanups)
	p, child := forkingServer(t, ftb, "exec", freeAddr(t), Options{StopTimeout: 2 * time.Second})
	if pidGone(child) {
		t.Fatal("the forked child is not running before the cleanup")
	}
	ftb.runCleanups()
	waitPidGone(t, child, "the forked grandchild")
	if testkit.Listening(p.Addr()) {
		t.Error("the server still listens after the cleanup")
	}
}

func TestStopSignalsGroupThenKills(t *testing.T) {
	// The leader traps TERM and ignores it; the group gets SIGKILL after StopTimeout, so the
	// child that also ignores TERM goes too.
	dir := t.TempDir()
	p := StartStdio(t, Options{
		Path: "/bin/sh", Dir: dir, StopTimeout: 300 * time.Millisecond,
		Args: []string{"-c", `trap '' TERM; (trap '' TERM; echo $$ > child.pid; exec sleep 60) & echo ready; while :; do sleep 1; done`},
	})
	if got, _ := bufio.NewReader(p.Conn()).ReadString('\n'); got != "ready\n" {
		t.Fatalf("greeting %q", got)
	}
	child, _ := strconv.Atoi(waitForFile(t, filepath.Join(dir, "child.pid")))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	start := time.Now()
	p.Stop()
	if el := time.Since(start); el < 250*time.Millisecond || el > 10*time.Second {
		t.Errorf("Stop took %v, want about StopTimeout", el)
	}
	waitPidGone(t, child, "the TERM-ignoring child")
	if err := p.Wait(); err == nil {
		t.Error("Wait returned nil for a SIGKILLed leader")
	}
}

func TestStartTCPDaemonising(t *testing.T) {
	// The shell exits 0 at once; the server lives on in the group. StartTCP waits for it, and
	// Stop reaches it although the leader is gone.
	ftb := &fakeTB{TB: t}
	t.Cleanup(ftb.runCleanups)
	addr := freeAddr(t)
	p, child := forkingServer(t, ftb, "daemon", addr, Options{StopTimeout: 2 * time.Second, Daemonizes: true})
	if !testkit.Listening(addr) {
		t.Fatal("the daemon does not listen")
	}
	ftb.runCleanups()
	testkit.WaitFor(t, 10*time.Second, "the daemon to stop listening", func() bool { return !testkit.Listening(addr) })
	waitPidGone(t, child, "the forked child")
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Error("Done not closed after Stop")
	}
	if err := p.Wait(); err != nil {
		t.Errorf("Wait: %v, want the leader's exit 0", err)
	}
}

func TestSignalGroup(t *testing.T) {
	dir := t.TempDir()
	p := StartStdio(t, Options{
		Path: "/bin/sh", Dir: dir,
		Args: []string{"-c", `sleep 60 & echo $! > child.pid; echo ready; wait`},
	})
	if got, _ := bufio.NewReader(p.Conn()).ReadString('\n'); got != "ready\n" {
		t.Fatalf("greeting %q", got)
	}
	child, _ := strconv.Atoi(waitForFile(t, filepath.Join(dir, "child.pid")))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitPidGone(t, child, "the child")
	_ = p.Wait()
	if err := p.SignalGroup(syscall.SIGTERM); err != nil {
		t.Errorf("SignalGroup on a gone group: %v, want nil", err)
	}
}

func TestLogDumpCap(t *testing.T) {
	ftb := &fakeTB{TB: t}
	dir := t.TempDir()
	big := strings.Repeat("0123456789", 3000) + "THE-END\n" // 30 KB
	if err := os.WriteFile(filepath.Join(dir, "daemon.log"), []byte("HEAD-OF-LOG\n"+big), 0o600); err != nil {
		t.Fatal(err)
	}
	p := StartStdio(ftb, Options{
		Path: "/bin/sh", Dir: dir, LogBytes: 1000, Logs: []string{"daemon.log", "missing.log"},
		Args: []string{"-c", `i=0; while [ $i -lt 400 ]; do echo err$i >&2; i=$((i+1)); done; echo SENTINEL-END >&2`},
	})
	_ = p.Wait()
	ftb.setFailed()
	ftb.runCleanups()
	logs := ftb.allLogs()
	if !strings.Contains(logs, "THE-END") || strings.Contains(logs, "HEAD-OF-LOG") {
		t.Errorf("log file dump should hold its tail only:\n%.300s", logs)
	}
	if !strings.Contains(logs, "bytes cut ...]") || !strings.Contains(logs, "err0\n") || !strings.Contains(logs, "SENTINEL-END") {
		t.Errorf("stderr dump should keep head and tail with a marker:\n%.300s", logs)
	}
	if !strings.Contains(logs, "missing.log: not readable") {
		t.Errorf("missing log not reported:\n%.300s", logs)
	}
	if len(logs) > 4000 {
		t.Errorf("dump has %d bytes, want it capped near 3 x 1000", len(logs))
	}
}

func TestStartTCPExit0WithoutDaemonizesFailsAtOnce(t *testing.T) {
	bin := buildEcho(t)
	ftb := &fakeTB{TB: t}
	t.Cleanup(ftb.runCleanups)
	addr := freeAddr(t)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The shell starts the server in the background and exits 0: a daemon without the option.
		StartTCP(ftb, Options{Path: "/bin/sh", Args: []string{"-c", `(sleep 3; exec "$0" "$@") & exit 0`, bin, "-listen", addr}, StopTimeout: time.Second}, addr)
	}()
	<-done
	if !ftb.Failed() || len(ftb.fatals) != 1 || !strings.Contains(ftb.fatals[0], "exited before") {
		t.Errorf("fatals %q, want one 'exited before' message", ftb.fatals)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Errorf("took %v", el)
	}
	ftb.runCleanups()
	testkit.WaitFor(t, 10*time.Second, "the stray daemon to stop", func() bool { return !testkit.Listening(addr) })
}

func TestStopDoesNotSignalReusedGroup(t *testing.T) {
	// A process that exited and was reaped with nothing left in its group: Stop and
	// SignalGroup must not touch the group id, which may belong to someone else by now.
	p := StartStdio(t, Options{Path: "sh", Args: []string{"-c", "exit 0"}})
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if !p.groupGone() {
		t.Fatal("the group of an exited leader without children is not gone")
	}
	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		t.Errorf("SignalGroup: %v", err)
	}
	p.Stop() // must return at once without waiting or signalling
}

func TestCaptureCap(t *testing.T) {
	w := newCapWriter(maxCapture)
	chunk := []byte(strings.Repeat("x", 1<<20))
	for range 40 {
		_, _ = w.Write(chunk)
	}
	if got := w.String(); len(got) > maxCapture+64 || !strings.Contains(got, "bytes cut ...]") {
		t.Errorf("capture of 40 MiB holds %d bytes", len(got))
	}
}
