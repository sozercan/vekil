package main

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

const testRegistrySubKey = `Software\Vekil\Test\Autostart`

func TestInstallRemoveLaunchAgent(t *testing.T) {
	// Use a test-specific registry key to avoid modifying the real Run key.
	k, _, err := registry.CreateKey(registry.CURRENT_USER, testRegistrySubKey, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("failed to create test registry key: %v", err)
	}
	k.Close()
	t.Cleanup(func() {
		registry.DeleteKey(registry.CURRENT_USER, testRegistrySubKey)
	})

	// Override the registry key used by autostart functions.
	origKey := autostartRegistryKey
	autostartRegistryKey = testRegistrySubKey
	t.Cleanup(func() { autostartRegistryKey = origKey })

	// Verify initial state: not installed.
	if isLaunchAgentInstalled() {
		t.Fatal("expected launch agent to not be installed initially")
	}

	// Install.
	if err := installLaunchAgent(); err != nil {
		t.Fatalf("installLaunchAgent() error = %v", err)
	}

	if !isLaunchAgentInstalled() {
		t.Fatal("expected launch agent to be installed after install")
	}

	// Remove.
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
}
