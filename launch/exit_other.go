//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package launch

import "os"

func processStateExitCode(state *os.ProcessState) int {
	if state == nil {
		return 1
	}
	code := state.ExitCode()
	if code < 0 {
		return 1
	}
	return code
}

func processSignalExitCode(signalValue os.Signal) int {
	if signalValue == os.Interrupt {
		return 130
	}
	return 1
}
