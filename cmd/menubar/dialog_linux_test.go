package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestShowOsascriptDialogDBusDismissedReturnsCancel(t *testing.T) {
	restoreDialogHooks(t)

	execLookPath = func(string) (string, error) {
		return "", exec.ErrNotFound
	}
	notifyWithActions = func(string, string, []string) (string, error) {
		return "", errNotificationDismissed
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Cancel" {
		t.Fatalf("showOsascriptDialog() = %q, want Cancel", got)
	}
}

func TestShowOsascriptDialogZenityCancelStopsFallback(t *testing.T) {
	restoreDialogHooks(t)

	tmpDir := t.TempDir()
	zenity := writeTestExecutable(t, tmpDir, "zenity", `#!/bin/sh
exit 1
`)

	execLookPath = dialogLookup(map[string]string{
		"zenity": zenity,
	})
	notifyWithActions = func(string, string, []string) (string, error) {
		t.Fatal("notifyWithActions should not be called after a user cancel")
		return "", nil
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Cancel" {
		t.Fatalf("showOsascriptDialog() = %q, want Cancel", got)
	}
}

func TestShowOsascriptDialogZenityErrorFallsBackToDBus(t *testing.T) {
	restoreDialogHooks(t)

	tmpDir := t.TempDir()
	zenity := writeTestExecutable(t, tmpDir, "zenity", `#!/bin/sh
echo "cannot open display" >&2
exit 1
`)

	execLookPath = dialogLookup(map[string]string{
		"zenity": zenity,
	})

	dbusCalled := false
	notifyWithActions = func(string, string, []string) (string, error) {
		dbusCalled = true
		return "default", nil
	}

	got := showOsascriptDialog("Sign in", "message", "Open GitHub", "Cancel")
	if got != "Open GitHub" {
		t.Fatalf("showOsascriptDialog() = %q, want Open GitHub", got)
	}
	if !dbusCalled {
		t.Fatal("expected DBus fallback to be used after zenity failed")
	}
}

func TestShowErrorDialogFallsBackAfterZenityFailure(t *testing.T) {
	restoreDialogHooks(t)

	tmpDir := t.TempDir()
	zenity := writeTestExecutable(t, tmpDir, "zenity", `#!/bin/sh
dir=$(dirname "$0")
printf 'zenity\n' >> "$dir/calls.log"
echo "cannot open display" >&2
exit 1
`)
	kdialog := writeTestExecutable(t, tmpDir, "kdialog", `#!/bin/sh
dir=$(dirname "$0")
printf 'kdialog\n' >> "$dir/calls.log"
exit 0
`)

	execLookPath = dialogLookup(map[string]string{
		"zenity":  zenity,
		"kdialog": kdialog,
	})

	notifyCalled := false
	notify = func(string, string) error {
		notifyCalled = true
		return nil
	}

	showErrorDialog("title", "message")

	data, err := os.ReadFile(filepath.Join(tmpDir, "calls.log"))
	if err != nil {
		t.Fatalf("failed to read dialog call log: %v", err)
	}
	if got := string(data); got != "zenity\nkdialog\n" {
		t.Fatalf("unexpected dialog fallback order %q", got)
	}
	if notifyCalled {
		t.Fatal("expected kdialog success to stop before DBus fallback")
	}
}

func restoreDialogHooks(t *testing.T) {
	t.Helper()

	oldLookPath := execLookPath
	oldCommand := execCommand
	oldNotifyWithActions := notifyWithActions
	oldNotify := notify

	t.Cleanup(func() {
		execLookPath = oldLookPath
		execCommand = oldCommand
		notifyWithActions = oldNotifyWithActions
		notify = oldNotify
	})
}

func dialogLookup(paths map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if path, ok := paths[name]; ok {
			return path, nil
		}
		return "", exec.ErrNotFound
	}
}

func writeTestExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
	return path
}
