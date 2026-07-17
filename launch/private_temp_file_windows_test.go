//go:build windows

package launch

import (
	"strings"
	"testing"

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
