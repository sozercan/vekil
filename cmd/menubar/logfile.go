package main

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/sozercan/vekil/logger"
)

// menubarLogMaxBytes bounds the log file. A full file moves to one ".1"
// backup, so the menubar keeps at most two files.
const menubarLogMaxBytes = logger.DefaultRotatingFileMaxBytes

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
