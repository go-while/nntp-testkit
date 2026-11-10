package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pidGone reports whether pid no longer runs; a zombie nobody reaped yet counts as gone.
func pidGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := strings.LastIndexByte(string(st), ')'); i >= 0 && strings.HasPrefix(string(st[i+1:]), " Z") {
			return true
		}
	}
	return false
}

// waitGone waits up to 10s for pid to disappear (init reaps orphans asynchronously).
func waitPidGone(t *testing.T, pid int, what string) {
	t.Helper()
	limit := time.Now().Add(10 * time.Second)
	for !pidGone(pid) {
		if time.Now().After(limit) {
			t.Fatalf("%s (pid %d) still running", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func firstPid(t *testing.T, out string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	if err != nil || pid <= 1 {
		t.Fatalf("no pid on the first line of %q", out)
	}
	return pid
}

func TestRunExitCodes(t *testing.T) {
	out, code := Run(t, RunOptions{Path: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 0"}})
	if code != 0 || out != "out\nerr\n" {
		t.Errorf("code %d out %q, want 0 and both streams combined", code, out)
	}
	out, code = Run(t, RunOptions{Path: "sh", Args: []string{"-c", "echo bad; exit 7"}})
	if code != 7 || out != "bad\n" {
		t.Errorf("code %d out %q, want 7", code, out)
	}
	out, code, err := TryRun(RunOptions{Path: "sh", Args: []string{"-c", "exit 3"}})
	if code != 3 || err != nil {
		t.Errorf("TryRun: out %q code %d err %v, want 3 and no error", out, code, err)
	}
}

func TestRunStartError(t *testing.T) {
	_, code, err := TryRun(RunOptions{Path: "/nonexistent/binary"})
	if err == nil || code != -1 {
		t.Errorf("code %d err %v, want -1 and an error", code, err)
	}
	if _, code, err := TryRun(RunOptions{}); err == nil || code != -1 {
		t.Errorf("empty path: code %d err %v", code, err)
	}
}

func TestRunSignal(t *testing.T) {
	out, code, err := TryRun(RunOptions{Path: "sh", Args: []string{"-c", "echo before; kill -KILL $$"}})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "killed by signal") || !strings.Contains(err.Error(), "killed") {
		t.Errorf("code %d err %v, want -1 and the signal named", code, err)
	}
	if errors.Is(err, ErrTimeout) || out != "before\n" {
		t.Errorf("out %q err %v", out, err)
	}
}

func TestRunEnvDirStdin(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, code := Run(t, RunOptions{Path: "sh", Args: []string{"-c", `echo "$NNTP_RUN_TEST:$(pwd -P)"`}, Env: []string{"NNTP_RUN_TEST=v1"}, Dir: dir})
	if code != 0 || out != "v1:"+want+"\n" {
		t.Errorf("out %q code %d, want v1:%s", out, code, want)
	}
	// Env appends to the inherited environment.
	t.Setenv("NNTP_RUN_INHERIT", "yes")
	out, _ = Run(t, RunOptions{Path: "sh", Args: []string{"-c", `echo "$NNTP_RUN_INHERIT"`}})
	if out != "yes\n" {
		t.Errorf("inherited env: %q", out)
	}
	out, code = Run(t, RunOptions{Path: "cat", Stdin: strings.NewReader("line1\nline2\n")})
	if code != 0 || out != "line1\nline2\n" {
		t.Errorf("stdin: out %q code %d", out, code)
	}
	// No Stdin is /dev/null: cat ends at once.
	out, code = Run(t, RunOptions{Path: "cat"})
	if code != 0 || out != "" {
		t.Errorf("no stdin: out %q code %d", out, code)
	}
}

func TestRunOutputCap(t *testing.T) {
	script := "i=0; while [ $i -lt 5000 ]; do echo line$i; i=$((i+1)); done"
	out, code := Run(t, RunOptions{Path: "sh", Args: []string{"-c", script}, MaxOutput: 1000})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.HasPrefix(out, "line0\nline1\n") || !strings.HasSuffix(out, "line4999\n") {
		t.Errorf("head or tail missing: %q ... %q", out[:20], out[len(out)-20:])
	}
	if !strings.Contains(out, "bytes cut ...]") || len(out) > 1000+60 {
		t.Errorf("len %d, want the cap plus a cut marker:\n%s", len(out), out)
	}
	// The count in the marker is exact: kept bytes plus cut bytes are what was written.
	var cut, total int
	total = 0
	for i := range 5000 {
		total += len("line" + strconv.Itoa(i) + "\n")
	}
	marker := out[strings.Index(out, "[... "):]
	if _, err := fmtSscan(marker, &cut); err != nil {
		t.Fatal(err)
	}
	kept := len(out) - len(cutMarker(int64(cut)))
	if kept+cut != total {
		t.Errorf("kept %d + cut %d != written %d", kept, cut, total)
	}
	// Under the cap nothing is cut; negative means no cap.
	out, _ = Run(t, RunOptions{Path: "sh", Args: []string{"-c", script}, MaxOutput: -1})
	if strings.Contains(out, "cut ...]") || !strings.HasSuffix(out, "line4999\n") || len(out) != total {
		t.Errorf("uncapped output has %d bytes, want %d", len(out), total)
	}
}

func fmtSscan(marker string, n *int) (int, error) {
	f := strings.Fields(marker)
	v, err := strconv.Atoi(f[1])
	*n = v
	return 1, err
}

func TestRunTimeoutKillsGroup(t *testing.T) {
	// sh starts a sleeping child and waits for it; neither ends by itself.
	start := time.Now()
	out, code, err := TryRun(RunOptions{
		Path:    "sh",
		Args:    []string{"-c", "sleep 60 & echo $!; wait"},
		Timeout: 500 * time.Millisecond,
	})
	if el := time.Since(start); el > 20*time.Second {
		t.Errorf("TryRun took %v", el)
	}
	if !errors.Is(err, ErrTimeout) || code != -1 {
		t.Fatalf("code %d err %v, want -1 and ErrTimeout", code, err)
	}
	if !strings.Contains(err.Error(), "did not finish within 500ms") || !strings.Contains(err.Error(), "output so far") {
		t.Errorf("error does not name the timeout and the output: %v", err)
	}
	pid := firstPid(t, out)
	if !strings.Contains(err.Error(), strconv.Itoa(pid)) {
		t.Errorf("error lacks the output so far (%d): %v", pid, err)
	}
	waitPidGone(t, pid, "the sleeping grandchild")
}

func TestRunTimeoutKillsDaemonisedGroupMember(t *testing.T) {
	// The grandchild keeps the pipe open and outlives the shell's wait: the group kill, not
	// the leader's death, closes the pipe.
	out, _, err := TryRun(RunOptions{
		Path:      "sh",
		Args:      []string{"-c", "sleep 60 & echo $!; sleep 60"},
		Timeout:   400 * time.Millisecond,
		WaitDelay: 30 * time.Second,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v", err)
	}
	waitPidGone(t, firstPid(t, out), "the background sleep")
}

func TestRunFailsTBOnTimeout(t *testing.T) {
	ftb := &fakeTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ftb, RunOptions{Path: "sleep", Args: []string{"60"}, Timeout: 200 * time.Millisecond})
	}()
	<-done
	if !ftb.Failed() || len(ftb.fatals) != 1 || !strings.Contains(ftb.fatals[0], "did not finish within 200ms") {
		t.Errorf("fatals %q", ftb.fatals)
	}
}

func TestRunDaemonisingChildHoldsPipe(t *testing.T) {
	// The shell exits at once, its background sleep holds stdout/stderr for 30s.
	start := time.Now()
	out, code, err := TryRun(RunOptions{
		Path:      "sh",
		Args:      []string{"-c", "sleep 30 & echo $!; exit 4"},
		WaitDelay: 300 * time.Millisecond,
	})
	pid := firstPid(t, out)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("returned after %v, want about WaitDelay", el)
	}
	if err != nil || code != 4 {
		t.Errorf("code %d err %v, want the shell's exit code 4 and no error", code, err)
	}
	// With a zero exit the wait ends with exec.ErrWaitDelay, which is no failure either.
	start = time.Now()
	out, code, err = TryRun(RunOptions{Path: "sh", Args: []string{"-c", "sleep 30 & echo $!"}, WaitDelay: 300 * time.Millisecond})
	pid2 := firstPid(t, out)
	t.Cleanup(func() { _ = syscall.Kill(pid2, syscall.SIGKILL) })
	if err != nil || code != 0 || time.Since(start) > 10*time.Second {
		t.Errorf("code %d err %v after %v", code, err, time.Since(start))
	}
	// The daemon survives: Run does not kill the group of a command that exited by itself.
	if pidGone(pid2) {
		t.Errorf("the background process %d was killed", pid2)
	}
}
