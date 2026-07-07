package main

import (
	"os/exec"
	"testing"
)

func TestShowOsascriptDialogPowerShellErrorReturnsDefault(t *testing.T) {
	restoreDialogHooks(t)

	execCommand = func(name string, args ...string) *exec.Cmd {
		// Simulate PowerShell failing to execute.
		return exec.Command("cmd_that_does_not_exist_xyz")
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Open GitHub" {
		t.Fatalf("showOsascriptDialog() = %q, want Open GitHub", got)
	}
}

func TestShowOsascriptDialogYesReturnsDefault(t *testing.T) {
	restoreDialogHooks(t)

	execCommand = func(name string, args ...string) *exec.Cmd {
		// Simulate PowerShell returning "Yes".
		return exec.Command("cmd", "/c", "echo", "Yes")
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Open GitHub" {
		t.Fatalf("showOsascriptDialog() = %q, want Open GitHub", got)
	}
}

func TestShowOsascriptDialogNoReturnsCancel(t *testing.T) {
	restoreDialogHooks(t)

	execCommand = func(name string, args ...string) *exec.Cmd {
		// Simulate PowerShell returning "No".
		return exec.Command("cmd", "/c", "echo", "No")
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Cancel" {
		t.Fatalf("showOsascriptDialog() = %q, want Cancel", got)
	}
}

func TestEscapePowerShellString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"no quotes", "hello world", "hello world"},
		{"single quote", "it's", "it''s"},
		{"multiple quotes", "can't won't", "can''t won''t"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := escapePowerShellString(tc.input)
			if got != tc.want {
				t.Fatalf("escapePowerShellString(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestChooseProvidersConfigPathCanceled(t *testing.T) {
	restoreDialogHooks(t)

	execCommand = func(name string, args ...string) *exec.Cmd {
		// Simulate PowerShell exit code 1 (dialog canceled).
		return exec.Command("cmd", "/c", "exit", "1")
	}

	_, err := chooseProvidersConfigPath()
	if err != errDialogCanceled {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want errDialogCanceled", err)
	}
}

func restoreDialogHooks(t *testing.T) {
	t.Helper()

	oldLookPath := execLookPath
	oldCommand := execCommand

	t.Cleanup(func() {
		execLookPath = oldLookPath
		execCommand = oldCommand
	})
}
