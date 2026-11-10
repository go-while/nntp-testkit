//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// ownGroup makes cmd the leader of a new process group (pgid = its pid), so that signals to the
// group reach every descendant that did not leave it, and adds the platform's parent-death
// protection (pdeathsig, Linux only).
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	pdeathsig(cmd.SysProcAttr)
}

// killGroup sends sig to the process group led by pid; ESRCH (no such group) is not an error.
func killGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return errors.New("proc: no process group")
	}
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// groupAlive reports whether the process group led by pid has a member (a zombie that nobody
// has reaped yet counts).
func groupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// registry holds the leaders of the process groups the kit started and has not stopped yet. The
// first registration installs a handler for SIGINT, SIGTERM and SIGHUP that kills them all
// before the test binary dies of that signal, so Ctrl-C of "go test" (which reaches the binary
// but, with the servers in groups of their own, no longer the servers) leaves no orphans.
var registry struct {
	mu     sync.Mutex
	groups map[int]struct{}
	once   sync.Once
}

// registerGroup records the group led by pid and, the first time, installs the handler.
func registerGroup(pid int) {
	registry.once.Do(installSignalHandler)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.groups == nil {
		registry.groups = map[int]struct{}{}
	}
	registry.groups[pid] = struct{}{}
}

// unregisterGroup forgets the group led by pid.
func unregisterGroup(pid int) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.groups, pid)
}

// killRegistered sends SIGKILL to every registered group.
func killRegistered() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for pid := range registry.groups {
		_ = killGroup(pid, syscall.SIGKILL)
	}
}

// installSignalHandler catches SIGINT, SIGTERM and SIGHUP (those the process does not ignore:
// a binary started with nohup keeps ignoring them), kills the registered groups, removes the
// handler and raises the signal again, so the test binary ends as it would have without it
// (unless the program has a handler of its own, which then runs). It cannot help when the
// binary is killed with SIGKILL, or dies of a panic such as the one "go test -timeout" raises
// with SIGQUIT.
func installSignalHandler() {
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	go func() {
		sig := <-ch
		killRegistered()
		signal.Stop(ch)
		if s, ok := sig.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), s)
		}
	}()
}
