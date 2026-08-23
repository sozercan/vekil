//go:build !windows

package macosruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func ensurePrivateDirectory(path string) error {
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if name == "." || name == ".." || filepath.Dir(path) == path {
		return fmt.Errorf("private directory %q must name a child directory", path)
	}

	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create private directory parent %q: %w", parent, err)
	}
	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open private directory parent %q: %w", parent, err)
	}
	defer func() { _ = unix.Close(parentFD) }()

	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create private directory %q: %w", path, err)
	}
	directoryFD, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return fmt.Errorf("open private directory %q: symlinks are not allowed", path)
		}
		return fmt.Errorf("open private directory %q: %w", path, err)
	}
	defer func() { _ = unix.Close(directoryFD) }()

	if err := unix.Fchmod(directoryFD, 0o700); err != nil {
		return fmt.Errorf("protect private directory %q: %w", path, err)
	}
	return nil
}
