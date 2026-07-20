//go:build windows

package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const managedProvidersConfigLockRetryInterval = 10 * time.Millisecond

type managedProvidersConfigFileLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireManagedProvidersConfigFileLock(ctx context.Context, path string) (*managedProvidersConfigFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open managed providers config lock %q: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat managed providers config lock %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("managed providers config lock %q is not a regular file", path)
	}
	lock := &managedProvidersConfigFileLock{file: file}
	for {
		err = windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&lock.overlapped,
		)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("lock managed providers config lock %q: %w", path, err)
		}
		timer := time.NewTimer(managedProvidersConfigLockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *managedProvidersConfigFileLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}

func hardenManagedProvidersConfigDirectory(string) error {
	// Files under UserConfigDir inherit the current user's directory ACL.
	return nil
}

func hardenManagedProvidersConfigFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("file is nil")
	}
	// CreateTemp creates the file under the user-owned destination directory;
	// the inherited DACL remains the effective Windows restriction.
	return nil
}

func replaceManagedProvidersConfigFile(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func syncManagedProvidersConfigDirectory(string) error {
	// MOVEFILE_WRITE_THROUGH supplies the durable replacement barrier available
	// through the Windows API; opening directories for fsync is not portable.
	return nil
}
