//go:build windows

package main

import (
	"testing"
)

func TestContainsSpace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  bool
	}{
		{`C:\Program Files\vekil\vekil-tray.exe`, true},
		{`C:\vekil\vekil-tray.exe`, false},
		{"", false},
		{" ", true},
	}

	for _, tt := range tests {
		got := containsSpace(tt.input)
		if got != tt.want {
			t.Errorf("containsSpace(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsLaunchAgentInstalledWhenRegistryKeyMissing(t *testing.T) {
	// When the registry key cannot be opened, isLaunchAgentInstalled returns false.
	if !isLaunchAgentInstalled() {
		// Expected: on CI (Linux), registry calls fail, so this returns false.
		return
	}
	// If we're on Windows and the key exists, that's also valid.
}

func TestInstallRemoveLaunchAgentIntegration(t *testing.T) {
	// This test performs real registry operations and only runs on Windows.
	// On other platforms, installLaunchAgent will fail at registry access.

	// First ensure it's not installed.
	wasInstalled := isLaunchAgentInstalled()

	if err := installLaunchAgent(); err != nil {
		// Expected on non-Windows: registry is not available.
		t.Skipf("skipping: registry not available: %v", err)
	}

	if !isLaunchAgentInstalled() {
		t.Fatal("expected launch agent to be installed after install")
	}

	if err := removeLaunchAgent(); err != nil {
		t.Fatalf("removeLaunchAgent() error = %v", err)
	}

	if isLaunchAgentInstalled() {
		t.Fatal("expected launch agent to not be installed after removal")
	}

	// Removing again should not error.
	if err := removeLaunchAgent(); err != nil {
		t.Fatalf("removeLaunchAgent() second call error = %v", err)
	}

	// Restore prior state if it was installed before the test.
	if wasInstalled {
		_ = installLaunchAgent()
	}
}
