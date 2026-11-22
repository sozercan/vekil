//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

func detachOwnedConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	var pids [2]uint32
	count, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if count == 1 {
		_, _, _ = kernel32.NewProc("FreeConsole").Call()
	}
}
