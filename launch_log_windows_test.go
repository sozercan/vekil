//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

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
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read DACL: %v", err)
	}
	aceCount := uint16(0)
	if dacl != nil {
		aceCount = dacl.AceCount
	}
	if aceCount != 1 {
		t.Fatalf("DACL ACE count = %d, want 1; DACL=%q", aceCount, descriptor.String())
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatalf("GetAce() error = %v", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Fatalf("ACE type = %d, want allow", ace.Header.AceType)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !user.User.Sid.Equals(aceSID) {
		t.Fatalf("DACL grants %s, want current user %s", aceSID.String(), user.User.Sid.String())
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
