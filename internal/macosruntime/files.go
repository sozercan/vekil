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
	dirPath := filepath.Dir(path)
	directory, err := openPrivateDirectory(dirPath)
	if err != nil {
		return err
	}
	defer func() { _ = directory.close() }()
	name := filepath.Base(path)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("generate temporary filename: %w", err)
	}
	tmpName := "." + name + "." + hex.EncodeToString(random[:]) + ".tmp"
	file, err := directory.createExclusive(tmpName)
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", path, err)
	}
	defer func() {
		_ = file.Close()
		_ = directory.remove(tmpName)
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary file for %q: %w", path, err)
	}
	if err := writeAll(file, body); err != nil {
		return fmt.Errorf("write temporary file for %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file for %q: %w", path, err)
	}
	if err := directory.rename(tmpName, name); err != nil {
		return fmt.Errorf("install %q: %w", path, err)
	}
	if err := directory.sync(); err != nil {
		return fmt.Errorf("sync directory %q: %w", dirPath, err)
	}
	return nil
}

func writeExclusiveFile(path string, body []byte) error {
	return writeExclusiveFileWithBody(path, func(file *os.File) error {
		if err := writeAll(file, body); err != nil {
			return fmt.Errorf("write owned file %q: %w", path, err)
		}
		return nil
	})
}

func writeExclusiveFileWithBody(path string, writeBody func(*os.File) error) (returnErr error) {
	if writeBody == nil {
		return errors.New("owned file writer is required")
	}
	dirPath := filepath.Dir(path)
	directory, err := openPrivateDirectory(dirPath)
	if err != nil {
		return err
	}
	defer func() { _ = directory.close() }()
	name := filepath.Base(path)
	file, err := directory.createExclusive(name)
	if err != nil {
		return fmt.Errorf("create owned file %q: %w", path, err)
	}
	defer func() {
		_ = file.Close()
		if returnErr == nil {
			return
		}
		if err := directory.remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove incomplete owned file %q: %w", path, err))
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect owned file %q: %w", path, err)
	}
	if err := writeBody(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync owned file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close owned file %q: %w", path, err)
	}
	if err := directory.sync(); err != nil {
		return fmt.Errorf("sync directory %q: %w", dirPath, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := openPrivateDirectory(path)
	if err != nil {
		return fmt.Errorf("open directory %q for sync: %w", path, err)
	}
	defer func() { _ = directory.close() }()
	if err := directory.sync(); err != nil {
		return fmt.Errorf("sync directory %q: %w", path, err)
	}
	return nil
}

func removePrivateFile(path string) error {
	dirPath := filepath.Dir(path)
	directory, err := openPrivateDirectory(dirPath)
	if err != nil {
		return err
	}
	defer func() { _ = directory.close() }()
	if err := directory.remove(filepath.Base(path)); err != nil {
		return err
	}
	if err := directory.sync(); err != nil {
		return fmt.Errorf("sync directory %q: %w", dirPath, err)
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
