//go:build !windows

package macosruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const helperStateLockRetryInterval = 20 * time.Millisecond

// HelperStateLock gives one helper process exclusive ownership of the
// helper-managed configuration and runtime state directory.
type HelperStateLock struct {
	directory *privateDirectory
	file      *os.File
}

// AcquireHelperStateLock waits until no earlier helper owns the state files.
// The lock is opened relative to the verified private-directory descriptor and
// must be retained until controller shutdown and state persistence complete.
func AcquireHelperStateLock(ctx context.Context, paths Paths) (*HelperStateLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolved, err := resolvePaths(paths)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(filepath.Dir(resolved.Lock)) != filepath.Clean(resolved.Directory) {
		return nil, errors.New("helper state lock must be inside the state directory")
	}

	directory, err := openPrivateDirectory(resolved.Directory)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(resolved.Lock)
	fd, err := unix.Openat(
		directory.fd,
		name,
		unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		_ = directory.close()
		return nil, fmt.Errorf("open helper state lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), resolved.Lock)
	if file == nil {
		_ = unix.Close(fd)
		_ = directory.close()
		return nil, errors.New("open helper state lock: invalid file descriptor")
	}
	closeLock := func() {
		_ = file.Close()
		_ = directory.close()
	}

	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		closeLock()
		return nil, fmt.Errorf("stat helper state lock: %w", err)
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG {
		closeLock()
		return nil, errors.New("helper state lock is not a regular file")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		closeLock()
		return nil, fmt.Errorf("protect helper state lock: %w", err)
	}

	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var current unix.Stat_t
			if statErr := unix.Fstatat(directory.fd, name, &current, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				closeLock()
				return nil, fmt.Errorf("recheck helper state lock: %w", statErr)
			}
			if current.Mode&unix.S_IFMT != unix.S_IFREG || current.Dev != opened.Dev || current.Ino != opened.Ino {
				_ = unix.Flock(fd, unix.LOCK_UN)
				closeLock()
				return nil, errors.New("helper state lock changed while acquiring it")
			}
			return &HelperStateLock{directory: directory, file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			closeLock()
			return nil, fmt.Errorf("lock helper state: %w", err)
		}

		timer := time.NewTimer(helperStateLockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			closeLock()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// Close releases helper state ownership.
func (l *HelperStateLock) Close() error {
	if l == nil {
		return nil
	}
	var unlockErr, closeErr, directoryErr error
	if l.file != nil {
		unlockErr = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		closeErr = l.file.Close()
		l.file = nil
	}
	if l.directory != nil {
		directoryErr = l.directory.close()
		l.directory = nil
	}
	return errors.Join(unlockErr, closeErr, directoryErr)
}
