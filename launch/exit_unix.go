//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package launch

import (
	"os"
	"syscall"
)

func processStateExitCode(state *os.ProcessState) int {
	if state == nil {
		return 1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	code := state.ExitCode()
	if code < 0 {
		return 1
	}
	return code
}

func processSignalExitCode(signalValue os.Signal) int {
	if sig, ok := signalValue.(syscall.Signal); ok {
		return 128 + int(sig)
	}
	if signalValue == os.Interrupt {
		return 130
	}
	return 1
}
