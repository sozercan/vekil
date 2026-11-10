//go:build !windows

package macosruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type privateDirectory struct {
	path string
	fd   int
}

func openPrivateDirectory(path string) (*privateDirectory, error) {
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if name == "." || name == ".." || filepath.Dir(path) == path {
		return nil, fmt.Errorf("private directory %q must name a child directory", path)
	}

	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create private directory parent %q: %w", parent, err)
	}
	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open private directory parent %q: %w", parent, err)
	}
	defer func() { _ = unix.Close(parentFD) }()

	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("create private directory %q: %w", path, err)
	}
	directoryFD, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("open private directory %q: symlinks are not allowed", path)
		}
		return nil, fmt.Errorf("open private directory %q: %w", path, err)
	}

	if err := unix.Fchmod(directoryFD, 0o700); err != nil {
		_ = unix.Close(directoryFD)
		return nil, fmt.Errorf("protect private directory %q: %w", path, err)
	}
	return &privateDirectory{path: path, fd: directoryFD}, nil
}

func ensurePrivateDirectory(path string) error {
	directory, err := openPrivateDirectory(path)
	if err != nil {
		return err
	}
	return directory.close()
}

func (d *privateDirectory) createExclusive(name string) (*os.File, error) {
	fd, err := unix.Openat(
		d.fd,
		name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("invalid file descriptor")
	}
	return file, nil
}

func (d *privateDirectory) rename(oldName, newName string) error {
	return unix.Renameat(d.fd, oldName, d.fd, newName)
}

func (d *privateDirectory) remove(name string) error {
	return unix.Unlinkat(d.fd, name, 0)
}

func (d *privateDirectory) sync() error {
	if err := unix.Fsync(d.fd); err != nil && !errors.Is(err, unix.EINVAL) {
		return err
	}
	return nil
}

func (d *privateDirectory) close() error {
	return unix.Close(d.fd)
}
