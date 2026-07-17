//go:build windows

package launch

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWritePrivateTempFileUsesRestrictedWindowsACL(t *testing.T) {
	path, cleanup, err := writePrivateTempFile("vekil-private-acl-", []byte("secret"))
	if err != nil {
		t.Fatalf("writePrivateTempFile() error = %v", err)
	}
	defer func() { _ = cleanup() }()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo() error = %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read security descriptor control: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL is not protected: control=%#x", control)
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
