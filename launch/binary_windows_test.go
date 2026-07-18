//go:build windows

package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveExecutableUsesPATHEXTAndUnwrapsNPMShim(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "claude")
	nodePath := filepath.Join(dir, "node.exe")
	scriptPath := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o700); err != nil {
		t.Fatalf("mkdir script: %v", err)
	}
	if err := os.WriteFile(nodePath, []byte("test"), 0o600); err != nil {
		t.Fatalf("write node: %v", err)
	}
	if err := os.WriteFile(scriptPath, []byte("test"), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	shim := `@ECHO off
"%~dp0\node.exe" "%~dp0\node_modules\@anthropic-ai\claude-code\cli.js" %*
`
	if err := os.WriteFile(base+".cmd", []byte(shim), 0o600); err != nil {
		t.Fatalf("write fallback: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".CMD;.EXE")
	got, err := resolveExecutable("", "missing-claude", []string{base})
	if err != nil {
		t.Fatalf("resolveExecutable() error = %v", err)
	}
	if !strings.EqualFold(got.path, nodePath) {
		t.Fatalf("resolved path = %q, want %q", got.path, nodePath)
	}
	if len(got.prefixArgs) != 1 || !strings.EqualFold(got.prefixArgs[0], scriptPath) {
		t.Fatalf("prefix args = %#v, want script %q", got.prefixArgs, scriptPath)
	}
}
