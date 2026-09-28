package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// menubarLogMaxBytes bounds the log file. A full file moves to one ".1"
// backup, so the menubar keeps at most two files.
const menubarLogMaxBytes = 16 << 20

// menubarLogPath returns where the menubar writes its log, which also holds
// the proxy's own logs: ~/Library/Logs/vekil on macOS, the user cache
// directory's vekil/logs elsewhere.
func menubarLogPath() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Logs", "vekil", "menubar.log"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "vekil", "logs", "menubar.log"), nil
}

// rotatingLogFile appends to a log file and moves it to a ".1" backup before a
// write would grow it past maxBytes.
type rotatingLogFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

func openRotatingLogFile(path string, maxBytes int64) (*rotatingLogFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &rotatingLogFile{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingLogFile) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file, w.size = file, info.Size()
	return nil
}

func (w *rotatingLogFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		_ = w.file.Close()
		w.file = nil
		if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
