//go:build !windows

package aikit

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process on this host.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
