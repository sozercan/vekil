//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
	"time"
)

const openAICodexLockRetryInterval = 10 * time.Millisecond

type openAICodexProcessLock struct {
	file *os.File
}

func openOpenAICodexWritableFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("invalid file descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	return file, nil
}

func acquireOpenAICodexProcessLock(ctx context.Context, path string) (*openAICodexProcessLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open OpenAI Codex refresh lock %q without following symlinks: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open OpenAI Codex refresh lock %q: invalid file descriptor", path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat OpenAI Codex refresh lock %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("OpenAI Codex refresh lock %q is not a regular file", path)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod OpenAI Codex refresh lock %q: %w", path, err)
	}

	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			pathInfo, statErr := os.Lstat(path)
			fileInfo, fileStatErr := file.Stat()
			if statErr != nil || fileStatErr != nil || !pathInfo.Mode().IsRegular() || !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
				switch {
				case statErr != nil:
					return nil, fmt.Errorf("recheck OpenAI Codex refresh lock %q: %w", path, statErr)
				case fileStatErr != nil:
					return nil, fmt.Errorf("recheck opened OpenAI Codex refresh lock %q: %w", path, fileStatErr)
				default:
					return nil, fmt.Errorf("OpenAI Codex refresh lock %q changed while acquiring it", path)
				}
			}
			return &openAICodexProcessLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock OpenAI Codex refresh lock %q: %w", path, err)
		}

		timer := time.NewTimer(openAICodexLockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *openAICodexProcessLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil || closeErr != nil {
		return errors.Join(unlockErr, closeErr)
	}
	return nil
}

func alignOpenAICodexFileOwner(file *os.File, owner os.FileInfo) error {
	if file == nil || owner == nil {
		return nil
	}
	ownerStat, ok := owner.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}
	fileStat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok || (fileStat.Uid == ownerStat.Uid && fileStat.Gid == ownerStat.Gid) {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("OpenAI Codex sidecar ownership differs from auth.json")
	}
	return file.Chown(int(ownerStat.Uid), int(ownerStat.Gid))
}

func openAICodexRunningAsRoot() bool {
	return os.Geteuid() == 0
}
