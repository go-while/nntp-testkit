//go:build unix

package proc

import (
	"errors"
	"os/exec"
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
