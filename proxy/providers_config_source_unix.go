//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const managedProvidersConfigLockRetryInterval = 10 * time.Millisecond

type managedProvidersConfigFileLock struct {
	file *os.File
}

func acquireManagedProvidersConfigFileLock(ctx context.Context, path string) (*managedProvidersConfigFileLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open managed providers config lock %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open managed providers config lock %q: invalid file descriptor", path)
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
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod managed providers config lock %q: %w", path, err)
	}

	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			pathInfo, pathErr := os.Lstat(path)
			fileInfo, fileErr := file.Stat()
			if pathErr != nil || fileErr != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
				switch {
				case pathErr != nil:
					return nil, fmt.Errorf("recheck managed providers config lock %q: %w", path, pathErr)
				case fileErr != nil:
					return nil, fmt.Errorf("recheck opened managed providers config lock %q: %w", path, fileErr)
				default:
					return nil, fmt.Errorf("managed providers config lock %q changed while acquiring it", path)
				}
			}
			return &managedProvidersConfigFileLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}

func hardenManagedProvidersConfigDirectory(path string) error {
	return os.Chmod(path, 0o700)
}

func hardenManagedProvidersConfigFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("file is nil")
	}
	return file.Chmod(0o600)
}

func replaceManagedProvidersConfigFile(from, to string) error {
	return os.Rename(from, to)
}

func syncManagedProvidersConfigDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
