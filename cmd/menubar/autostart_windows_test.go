//go:build windows

package main

import "testing"

func TestWindowsRunCommandQuotesExecutable(t *testing.T) {
	got := windowsRunCommand(`C:\Program Files\Vekil\vekil-tray.exe`)
	want := `"C:\Program Files\Vekil\vekil-tray.exe"`
	if got != want {
		t.Fatalf("windowsRunCommand() = %q, want %q", got, want)
	}
}
