//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenLaunchLogUsesRestrictedWindowsACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	file, _, err := openLaunchLog("claude", path)
	if err != nil {
		t.Fatalf("openLaunchLog() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo() error = %v", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() error = %v", err)
	}
	sddl := descriptor.String()
	if !strings.Contains(sddl, user.User.Sid.String()) {
		t.Fatalf("DACL %q does not grant the current user", sddl)
	}
	for _, broad := range []string{";;;WD", ";;;BU", ";;;AU"} {
		if strings.Contains(sddl, broad) {
			t.Fatalf("DACL %q contains broad trustee %q", sddl, broad)
		}
	}
}

func TestOpenLaunchLogRejectsExistingWindowsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed existing log: %v", err)
	}
	if _, _, err := openLaunchLog("claude", path); err == nil {
		t.Fatal("openLaunchLog() replaced an existing path")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "keep" {
		t.Fatalf("existing log changed: body=%q err=%v", body, err)
	}
}
