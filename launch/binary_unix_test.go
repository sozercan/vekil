//go:build !windows

package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExecutableUsesExecutableFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write fallback: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	got, err := resolveExecutable("", "missing-claude", []string{path})
	if err != nil {
		t.Fatalf("resolveExecutable() error = %v", err)
	}
	if got.path != path {
		t.Fatalf("resolved path = %q, want %q", got.path, path)
	}
}
