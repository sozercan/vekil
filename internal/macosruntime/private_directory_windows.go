//go:build windows

package macosruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type privateDirectory struct {
	path string
}

func openPrivateDirectory(path string) (*privateDirectory, error) {
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if name == "." || name == ".." || filepath.Dir(path) == path {
		return nil, fmt.Errorf("private directory %q must name a child directory", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create private directory parent %q: %w", filepath.Dir(path), err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create private directory %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat private directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("open private directory %q: symlinks are not allowed", path)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("open private directory %q: not a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return nil, fmt.Errorf("protect private directory %q: %w", path, err)
	}
	return &privateDirectory{path: path}, nil
}

func ensurePrivateDirectory(path string) error {
	_, err := openPrivateDirectory(path)
	return err
}

func (d *privateDirectory) createExclusive(name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(d.path, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func (d *privateDirectory) rename(oldName, newName string) error {
	return os.Rename(filepath.Join(d.path, oldName), filepath.Join(d.path, newName))
}

func (d *privateDirectory) remove(name string) error {
	return os.Remove(filepath.Join(d.path, name))
}

func (d *privateDirectory) sync() error {
	file, err := os.Open(d.path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := file.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

func (d *privateDirectory) close() error { return nil }
