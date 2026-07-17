//go:build windows

package main

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openPrivateLaunchLog(path string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := descriptor.SetDACL(acl, true, false); err != nil {
		return nil, err
	}
	if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	securityAttributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_WRITE|windows.WRITE_DAC,
		windows.FILE_SHARE_READ,
		securityAttributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		err = windows.SetSecurityInfo(
			handle,
			windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil,
			nil,
			acl,
			nil,
		)
	}
	runtime.KeepAlive(user)
	runtime.KeepAlive(acl)
	runtime.KeepAlive(descriptor)
	if err != nil {
		if handle != 0 && handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}
	return file, nil
}
