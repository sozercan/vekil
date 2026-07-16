//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package proxy

import "syscall"

func optimizerTestProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
