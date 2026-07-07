//go:build windows

package main

import (
	"os/exec"
	"testing"
)

func restoreDialogHooks(t *testing.T) {
	t.Helper()

	oldLookPath := execLookPath
	oldCommand := execCommand
	oldMessageBox := messageBox

	t.Cleanup(func() {
		execLookPath = oldLookPath
		execCommand = oldCommand
		messageBox = oldMessageBox
	})
}

func TestShowOsascriptDialogYesReturnsDefault(t *testing.T) {
	restoreDialogHooks(t)

	messageBox = func(title, message string, flags uint32) int {
		return idYes
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Open GitHub" {
		t.Fatalf("showOsascriptDialog() = %q, want Open GitHub", got)
	}
}

func TestShowOsascriptDialogNoReturnsCancel(t *testing.T) {
	restoreDialogHooks(t)

	messageBox = func(title, message string, flags uint32) int {
		return idNo
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Cancel" {
		t.Fatalf("showOsascriptDialog() = %q, want Cancel", got)
	}
}

func TestShowErrorDialogCallsMessageBox(t *testing.T) {
	restoreDialogHooks(t)

	called := false
	messageBox = func(title, message string, flags uint32) int {
		called = true
		if title != "Error Title" {
			t.Fatalf("title = %q, want Error Title", title)
		}
		if message != "Error Message" {
			t.Fatalf("message = %q, want Error Message", message)
		}
		if flags != mbOK|mbIconError {
			t.Fatalf("flags = %d, want %d", flags, mbOK|mbIconError)
		}
		return idOK
	}

	showErrorDialog("Error Title", "Error Message")
	if !called {
		t.Fatal("expected messageBox to be called")
	}
}

func TestChooseProvidersConfigPathCanceled(t *testing.T) {
	restoreDialogHooks(t)

	execCommand = func(name string, args ...string) *exec.Cmd {
		// Simulate PowerShell exiting with code 1 (dialog canceled).
		return exec.Command("cmd", "/c", "exit 1")
	}

	_, err := chooseProvidersConfigPath()
	if err != errDialogCanceled {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want errDialogCanceled", err)
	}
}

func TestEscapeXML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"a & b", "a &amp; b"},
		{"<script>", "&lt;script&gt;"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{"it's", "it&apos;s"},
	}

	for _, tt := range tests {
		got := escapeXML(tt.input)
		if got != tt.want {
			t.Errorf("escapeXML(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEscapePowerShellString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"it's here", "it''s here"},
		{"no'quotes'at'all", "no''quotes''at''all"},
	}

	for _, tt := range tests {
		got := escapePowerShellString(tt.input)
		if got != tt.want {
			t.Errorf("escapePowerShellString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
