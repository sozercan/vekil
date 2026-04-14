//go:build !darwin || !cgo || !sparkle

package main

import "errors"

var errUpdaterDisabled = errors.New("Sparkle updater is not available in this build")

func updaterSupported() bool {
	return false
}

func startUpdater() error {
	return errUpdaterDisabled
}

func checkForUpdates() error {
	return errUpdaterDisabled
}
