//go:build unix && !linux

package proc

import "syscall"

// pdeathsig does nothing: only Linux has PR_SET_PDEATHSIG.
func pdeathsig(*syscall.SysProcAttr) {}
