//go:build darwin && cgo && sparkle

package main

/*
#cgo darwin CFLAGS: -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework Sparkle
#include <stdlib.h>
#include "updater_bridge.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

func updaterSupported() bool {
	return true
}

func startUpdater() error {
	return updaterCall(C.copilot_proxy_updater_start())
}

func checkForUpdates() error {
	return updaterCall(C.copilot_proxy_updater_check())
}

func updaterCall(message *C.char) error {
	if message == nil {
		return nil
	}

	defer C.free(unsafe.Pointer(message))
	return errors.New(C.GoString(message))
}
