package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopEntry(t *testing.T) {
	t.Parallel()

	entry := desktopEntry("/usr/local/bin/vekil-tray")

	wantFragments := []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=Vekil",
		"Exec=/usr/local/bin/vekil-tray",
		"Terminal=false",
	}

	for _, fragment := range wantFragments {
		if !strings.Contains(entry, fragment) {
			t.Fatalf("desktopEntry() missing fragment %q in:\n%s", fragment, entry)
		}
	}
}

func TestDesktopEntryEscapesExecPath(t *testing.T) {
	t.Parallel()

	entry := desktopEntry(`/tmp/My Apps/vekil%tray`)

	if !strings.Contains(entry, `Exec="/tmp/My Apps/vekil%%tray"`) {
		t.Fatalf("desktopEntry() did not escape Exec path:\n%s", entry)
	}
}

func TestInstallRemoveLaunchAgent(t *testing.T) {
	// Cannot use t.Parallel() because t.Setenv modifies process environment.

	// Use a temporary directory as XDG_CONFIG_HOME so we don't touch the
	// real autostart directory.
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	expectedPath := filepath.Join(tmpDir, "autostart", autostartFilename)

	// Should not be installed initially.
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

	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("failed to read desktop file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "[Desktop Entry]") {
		t.Fatalf("desktop file missing [Desktop Entry] header:\n%s", content)
	}
	if !strings.Contains(content, "Type=Application") {
		t.Fatalf("desktop file missing Type=Application:\n%s", content)
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
