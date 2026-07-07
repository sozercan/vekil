//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const windowsRunValueName = "Vekil"
const windowsRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

func windowsRunCommand(executable string) string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return ""
	}
	return `"` + strings.ReplaceAll(executable, `"`, `\"`) + `"`
}

func isLaunchAgentInstalled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer func() { _ = key.Close() }()
	value, _, err := key.GetStringValue(windowsRunValueName)
	return err == nil && strings.TrimSpace(value) != ""
}

func installLaunchAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	command := windowsRunCommand(exe)
	if command == "" {
		return fmt.Errorf("empty executable path")
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = key.Close() }()
	return key.SetStringValue(windowsRunValueName, command)
}

func removeLaunchAgent() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	defer func() { _ = key.Close() }()
	if err := key.DeleteValue(windowsRunValueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
