package proc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envParent makes the test binary act as a parent that starts a server and then waits to be killed.
const envParent = "PROC_TEST_PARENT"

// bareTB is the part of testing.TB the Start functions use when nothing is tested.
type bareTB struct{ testing.TB }

func (bareTB) Helper()         {}
func (bareTB) Cleanup(func())  {}
func (bareTB) TempDir() string { return os.TempDir() }
func (bareTB) Fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}

func init() {
	if os.Getenv(envParent) == "" {
		return
	}
	// Both ways of starting a process: the leader of a server and a one-shot command.
	p := StartStdio(bareTB{}, Options{Path: "sleep", Args: []string{"60"}})
	go TryRun(RunOptions{Path: "sleep", Args: []string{"61"}})
	fmt.Println(p.Pid())
	time.Sleep(time.Hour)
}

func TestPdeathsigKillsServerWhenParentDies(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envParent+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parent said %q", line)
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if pidGone(pid) {
		t.Fatal("the child is not running while its parent lives")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitPidGone(t, pid, "the server of the killed parent")
}
