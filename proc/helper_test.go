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

// envParent makes the test binary act as a parent that starts a server and then waits to be
// killed or signalled: the value is the mode.
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
	switch os.Getenv(envParent) {
	case "":
		return
	case "pdeathsig":
		// Both ways of starting a process: the leader of a server and a one-shot command.
		p := StartStdio(bareTB{}, Options{Path: "sleep", Args: []string{"60"}})
		go TryRun(RunOptions{Path: "sleep", Args: []string{"61"}})
		fmt.Println(p.Pid())
	case "sigint":
		// A server whose shell leaves a sleeping child behind: the leader's Pdeathsig does not
		// reach the child, only the signal handler of the kit does.
		p := StartStdio(bareTB{}, Options{Path: "/bin/sh", Args: []string{"-c", "sleep 60 & echo $!; wait"}})
		child, _ := bufio.NewReader(p.Conn()).ReadString('\n')
		fmt.Println(p.Pid())
		fmt.Println(strings.TrimSpace(child))
	}
	time.Sleep(time.Hour)
	os.Exit(3)
}

func TestSignalKillsRegisteredGroups(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), envParent+"=sigint")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			r := bufio.NewReader(out)
			var pids [2]int
			for i := range pids {
				line, err := r.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if pids[i], err = strconv.Atoi(strings.TrimSpace(line)); err != nil {
					t.Fatalf("parent said %q", line)
				}
			}
			leader, child := pids[0], pids[1]
			defer func() { _ = syscall.Kill(child, syscall.SIGKILL); _ = syscall.Kill(leader, syscall.SIGKILL) }()
			if pidGone(leader) || pidGone(child) {
				t.Fatal("the server is not running while its parent lives")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			// The test binary ends as it would without the handler: killed by the signal.
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("parent: %v, want it to die of %v", err, sig)
			}
			if ws, _ := ee.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != sig {
				t.Errorf("parent: %v, want killed by %v", err, sig)
			}
			waitPidGone(t, leader, "the server")
			waitPidGone(t, child, "the server's sleeping child")
		})
	}
}
