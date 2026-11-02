package logger

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// DefaultRotatingFileMaxBytes bounds each rotating log file.
const DefaultRotatingFileMaxBytes = 16 << 20

// RotatingFile appends to a private log file and moves it to a ".1" backup
// before a write would grow it past maxBytes, so at most two files are kept.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

// OpenRotatingFile opens path for appending, creating private parent
// directories and the file as needed.
func OpenRotatingFile(path string, maxBytes int64) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &RotatingFile{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingFile) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	// The create mode does not apply to an existing file; keep it private.
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
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

func (w *RotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	written := len(p)
	if int64(len(p)) > w.maxBytes {
		// Keep the start of an entry larger than the cap, so the file never
		// exceeds it. Callers see the whole entry written.
		p = append(p[:w.maxBytes-1:w.maxBytes-1], '\n')
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
	if err != nil {
		return n, err
	}
	return written, nil
}
