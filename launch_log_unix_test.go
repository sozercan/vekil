//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLaunchLogUsesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	file, gotPath, err := openLaunchLog("claude", path)
	if err != nil {
		t.Fatalf("openLaunchLog() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if gotPath != path {
		t.Fatalf("path = %q, want %q", gotPath, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %#o, want 0600", got)
	}
}

func TestOpenLaunchLogRejectsExistingPathAndSymlink(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.jsonl")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed existing log: %v", err)
	}
	if _, _, err := openLaunchLog("claude", existing); err == nil {
		t.Fatal("openLaunchLog() replaced an existing path")
	}
	body, err := os.ReadFile(existing)
	if err != nil || string(body) != "keep" {
		t.Fatalf("existing log changed: body=%q err=%v", body, err)
	}

	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, _, err := openLaunchLog("claude", link); err == nil {
		t.Fatal("openLaunchLog() followed a symlink")
	}
}
