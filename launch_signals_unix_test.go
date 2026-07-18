//go:build darwin || linux

package main

import (
	"syscall"
	"testing"
)

func TestManagedLaunchSignalsIncludesSIGHUP(t *testing.T) {
	for _, signal := range managedLaunchSignals() {
		if signal == syscall.SIGHUP {
			return
		}
	}
	t.Fatal("managed launch signals do not include SIGHUP")
}
