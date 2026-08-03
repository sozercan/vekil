//go:build !windows

package macosruntime

import (
	"errors"
	"fmt"
	"io"
	"os"

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
		if errors.Is(err, unix.ELOOP) {
			return nil, fileIdentity{}, fmt.Errorf("open %q: symlinks are not allowed", path)
		}
		return nil, fileIdentity{}, fmt.Errorf("open %q: %w", path, err)
	}
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
