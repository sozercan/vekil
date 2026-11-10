//go:build !windows

package macosruntime

import (
	"errors"

	"golang.org/x/sys/unix"
)

func parentProcessAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
