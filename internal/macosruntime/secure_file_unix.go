//go:build !windows

package macosruntime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type fileIdentity struct {
	Device uint64
	Inode  uint64
	Size   int64
}

func readSecureFile(path string, maxBytes int64) ([]byte, fileIdentity, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fileIdentity{}, secureOpenError(path, err)
	}
	return readSecureFileDescriptor(fd, path, maxBytes)
}

// readOwnedFile resolves only the verified private directory by path, then
// opens the helper-owned basename relative to that descriptor. Replacing the
// directory path after validation cannot redirect the file read.
func readOwnedFile(path string, maxBytes int64) ([]byte, fileIdentity, error) {
	directory, err := openPrivateDirectory(filepath.Dir(path))
	if err != nil {
		return nil, fileIdentity{}, err
	}
	defer func() { _ = directory.close() }()

	fd, err := unix.Openat(
		directory.fd,
		filepath.Base(path),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fileIdentity{}, secureOpenError(path, err)
	}
	return readSecureFileDescriptor(fd, path, maxBytes)
}

func secureOpenError(path string, err error) error {
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("open %q: symlinks are not allowed", path)
	}
	return fmt.Errorf("open %q: %w", path, err)
}

func readSecureFileDescriptor(fd int, path string, maxBytes int64) ([]byte, fileIdentity, error) {
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fileIdentity{}, fmt.Errorf("open %q: invalid file descriptor", path)
	}
	defer func() { _ = file.Close() }()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fileIdentity{}, fmt.Errorf("stat %q: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fileIdentity{}, fmt.Errorf("read %q: not a regular file", path)
	}
	if stat.Size < 0 || stat.Size > maxBytes {
		return nil, fileIdentity{}, fmt.Errorf("read %q: file exceeds %d bytes", path, maxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fileIdentity{}, fmt.Errorf("read %q: %w", path, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fileIdentity{}, fmt.Errorf("read %q: file exceeds %d bytes", path, maxBytes)
	}
	return body, fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: stat.Size}, nil
}
