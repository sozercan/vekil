//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// detachOwnedConsole closes the console window when this process is its only
// user, as when Task Scheduler starts vekil at logon. A console shared with a
// terminal is left attached.
func detachOwnedConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	var pids [2]uint32
	count, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if count == 1 {
		_, _, _ = kernel32.NewProc("FreeConsole").Call()
	}
}
