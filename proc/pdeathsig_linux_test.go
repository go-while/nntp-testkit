package proc

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestPdeathsigKillsServerWhenParentDies(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envParent+"=pdeathsig")
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
