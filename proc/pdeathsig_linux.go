package proc

import "syscall"

// pdeathsig asks the kernel to SIGKILL the child when its parent dies, so a test binary that is
// itself killed (timeout, CI cancel, kill -9) leaves no server behind. The kernel ties this to
// the parent's OS thread, not the process: the child dies when the thread that started it
// exits. The Start functions and Run therefore start and wait for the child on one goroutine
// locked to its thread (see lockedStart), which keeps that thread alive for the child's life.
// Only the direct child has it; a descendant that forked away does not inherit it.
func pdeathsig(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
