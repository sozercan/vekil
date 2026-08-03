//go:build !windows

package main

import "syscall"

func configureProcessGroup() {
	_ = syscall.Setpgid(0, 0)
}
