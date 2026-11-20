//go:build !darwin

package main

import "fyne.io/systray"

func renderTrayStatus(status trayStatus) error {
	if status == trayRunning {
		systray.SetIcon(iconOn)
	} else {
		systray.SetIcon(iconOff)
	}
	return nil
}
