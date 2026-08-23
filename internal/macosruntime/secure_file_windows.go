//go:build windows

package macosruntime

import (
	"fmt"
	"io"
	"os"
)

type fileIdentity struct {
	Size int64
}

func readSecureFile(path string, maxBytes int64) ([]byte, fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fileIdentity{}, fmt.Errorf("stat %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fileIdentity{}, fmt.Errorf("read %q: not a regular file", path)
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return nil, fileIdentity{}, fmt.Errorf("read %q: file exceeds %d bytes", path, maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fileIdentity{}, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fileIdentity{}, fmt.Errorf("read %q: %w", path, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fileIdentity{}, fmt.Errorf("read %q: file exceeds %d bytes", path, maxBytes)
	}
	return body, fileIdentity{Size: info.Size()}, nil
}

func readOwnedFile(path string, maxBytes int64) ([]byte, fileIdentity, error) {
	return readSecureFile(path, maxBytes)
}
