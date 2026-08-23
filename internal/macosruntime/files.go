package macosruntime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func writeAtomicFile(path string, body []byte) (returnErr error) {
	dir := filepath.Dir(path)
	if err := ensurePrivateDirectory(dir); err != nil {
		return err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("generate temporary filename: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(random[:])+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", path, err)
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary file for %q: %w", path, err)
	}
	if _, err := file.Write(body); err != nil {
		return fmt.Errorf("write temporary file for %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file for %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install %q: %w", path, err)
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return nil
}

func writeExclusiveFile(path string, body []byte) (returnErr error) {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create owned file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect owned file %q: %w", path, err)
	}
	if _, err := file.Write(body); err != nil {
		return fmt.Errorf("write owned file %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync owned file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close owned file %q: %w", path, err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %q for sync: %w", path, err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync directory %q: %w", path, err)
	}
	return nil
}

func writeAll(w io.Writer, body []byte) error {
	for len(body) > 0 {
		n, err := w.Write(body)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		body = body[n:]
	}
	return nil
}
