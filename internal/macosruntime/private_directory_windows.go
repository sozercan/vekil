//go:build windows

package macosruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func ensurePrivateDirectory(path string) error {
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if name == "." || name == ".." || filepath.Dir(path) == path {
		return fmt.Errorf("private directory %q must name a child directory", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create private directory parent %q: %w", filepath.Dir(path), err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create private directory %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat private directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("open private directory %q: symlinks are not allowed", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("open private directory %q: not a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect private directory %q: %w", path, err)
	}
	return nil
}
