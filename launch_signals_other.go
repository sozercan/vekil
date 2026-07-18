//go:build !darwin && !linux

package main

import (
	"os"
	"syscall"
)

func managedLaunchSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
