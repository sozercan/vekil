//go:build linux || darwin

package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
	"golang.org/x/sys/unix"
)

func openDurableStateDatabase(path string, allowCreate bool) (*bolt.DB, *os.File, bool, func() error, error) {
	return openDurableStateDatabaseWithFilesystemCheck(path, allowCreate, checkDurableStateFilesystem)
}

// Only the filesystem primitive is substituted in tests; descriptor opening,
// identity checks, bbolt validation/locking and cleanup remain real.
func openDurableStateDatabaseWithFilesystemCheck(path string, allowCreate bool, checkFilesystem func(int) error) (*bolt.DB, *os.File, bool, func() error, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, false, nil, errDurableStatePath
	}
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, nil, false, nil, errDurableStatePath
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, false, nil, errDurableStatePath
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return nil, nil, false, nil, errDurableStatePath
	}
	defer func() { _ = directory.Close() }()
	openedInfo, err := directory.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.IsDir() || openedInfo.Mode().Perm() != 0o700 {
		return nil, nil, false, nil, errDurableStatePath
	}
	var stat unix.Stat_t
	if unix.Fstat(int(directory.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) {
		return nil, nil, false, nil, errDurableStatePath
	}
	if err := checkDurableStatePermissions(int(directory.Fd())); err != nil {
		return nil, nil, false, nil, err
	}
	if err := checkFilesystem(int(directory.Fd())); err != nil {
		return nil, nil, false, nil, err
	}
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	created := errors.Is(err, os.ErrNotExist)
	if created && !allowCreate {
		return nil, nil, false, nil, errDurableStatePath
	}
	if err != nil && !created {
		return nil, nil, false, nil, errDurableStatePath
	}
	if !created && (!before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() < 4*int64(os.Getpagesize())) {
		return nil, nil, false, nil, errDurableStatePath
	}
	var databaseFile *os.File
	openFile := func(_ string, flags int, mode os.FileMode) (*os.File, error) {
		if created {
			flags |= os.O_EXCL
		} else {
			flags &^= os.O_CREATE
		}
		file, err := root.OpenFile(name, flags|unix.O_NOFOLLOW, mode)
		if err != nil {
			return nil, errDurableStatePath
		}
		var stat unix.Stat_t
		if unix.Fstat(int(file.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Mode&0o777 != 0o600 || stat.Mode&unix.S_IFMT != unix.S_IFREG {
			_ = file.Close()
			return nil, errDurableStatePath
		}
		if err := checkDurableStatePermissions(int(file.Fd())); err != nil {
			_ = file.Close()
			return nil, err
		}
		// A regular file can be bind-mounted from a different filesystem than
		// its containing directory. Validate each actual descriptor before bbolt
		// can read pages or acquire a writable handle on unsupported storage.
		if err := checkFilesystem(int(file.Fd())); err != nil {
			_ = file.Close()
			return nil, err
		}
		if !created {
			after, err := file.Stat()
			if err != nil || !os.SameFile(before, after) || after.Size() < 4*int64(os.Getpagesize()) {
				_ = file.Close()
				return nil, errDurableStatePath
			}
		}
		databaseFile = file
		return file, nil
	}
	if !created {
		// Writable bbolt Open preloads (and for some formats writes) freelist
		// state. Validate existing pages read-only first, with no freelist
		// preload; Check contains malformed-page panics in its checking goroutine.
		probe, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond, OpenFile: openFile})
		if err != nil {
			return nil, nil, false, nil, durableDatabaseOpenError(err)
		}
		candidate := &durableStateBindings{db: probe, maxEntries: int(^uint(0) >> 1)}
		checkErr := candidate.initialize(false)
		closeErr := probe.Close()
		if checkErr != nil {
			return nil, nil, false, nil, checkErr
		}
		if closeErr != nil {
			return nil, nil, false, nil, errDurableStateIO
		}
		// The final exclusive open verifies the same inode again. Another
		// legitimate writer can only change it while we have no lock; full
		// record/integrity validation runs again under the exclusive lock.
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 100 * time.Millisecond, OpenFile: openFile})
	if err != nil {
		return nil, nil, false, nil, durableDatabaseOpenError(err)
	}
	// The directory fd must remain open through metadata initialization. Dup it
	// without reopening a potentially replaced path, and close after the barrier.
	fd, err := unix.FcntlInt(directory.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		_ = db.Close()
		return nil, nil, false, nil, errDurableStateIO
	}
	syncDirectory := func() error {
		syncErr := syncDurableStateDirectory(fd)
		return errors.Join(syncErr, unix.Close(fd))
	}
	// The final successful OpenFile belongs to the writable DB. bbolt remains
	// its sole owner; callers must not close or duplicate this descriptor.
	return db, databaseFile, created, syncDirectory, nil
}

func durableDatabaseOpenError(err error) error {
	switch {
	case errors.Is(err, bolterrors.ErrTimeout):
		return errDurableStateLocked
	case errors.Is(err, errDurableStatePath), errors.Is(err, os.ErrPermission):
		return errDurableStatePath
	case errors.Is(err, errDurableStatePlatform):
		return errDurableStatePlatform
	case errors.Is(err, bolterrors.ErrInvalid), errors.Is(err, bolterrors.ErrVersionMismatch), errors.Is(err, bolterrors.ErrChecksum):
		return errDurableStateCorrupt
	default:
		return errDurableStateIO
	}
}
