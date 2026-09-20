//go:build linux || darwin

package proxy

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Only the automatic application-data path creates directories. Explicit file
// paths retain the requirement for a pre-existing private directory.
func createPrivateStateDirectory(file string) error {
	if !filepath.IsAbs(file) || filepath.Clean(file) != file {
		return errDurableStatePath
	}
	return ensureStateDirectory(filepath.Dir(file), true)
}

func ensureStateDirectory(dir string, private bool) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if !info.IsDir() || (private && info.Mode().Perm() != 0o700) {
			return errDurableStatePath
		}
		file, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return errDurableStatePath
		}
		defer func() { _ = file.Close() }()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return errDurableStatePath
		}
		if private {
			var stat unix.Stat_t
			if unix.Fstat(int(file.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) || opened.Mode().Perm() != 0o700 {
				return errDurableStatePath
			}
			if err := checkDurableStatePermissions(int(file.Fd())); err != nil {
				return err
			}
		}
		return checkDurableStateFilesystem(int(file.Fd()))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errDurableStatePath
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return errDurableStatePath
	}
	if err := ensureStateDirectory(parent, false); err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return errDurableStatePath
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(dir)
	if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errDurableStatePath
	}
	// Sync the exact parent used for mkdir before any new database can be exposed.
	parentFile, err := root.Open(".")
	if err != nil {
		return errDurableStateIO
	}
	syncErr := syncDurableStateDirectory(int(parentFile.Fd()))
	closeErr := parentFile.Close()
	if syncErr != nil || closeErr != nil {
		return errDurableStateIO
	}
	return ensureStateDirectory(dir, true)
}
