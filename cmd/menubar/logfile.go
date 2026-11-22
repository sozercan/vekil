package main

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/sozercan/vekil/logger"
)

const menubarLogMaxBytes = logger.DefaultRotatingFileMaxBytes

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
