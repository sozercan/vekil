//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const (
	autostartRegistryKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartRegistryName = "Vekil"
)

// Testable function vars for registry operations.
var (
	registryOpenKey  = registry.OpenKey
	registryCreateKey = registryCreateKeyFunc
)

func registryCreateKeyFunc(k registry.Key, path string, access uint32) (registry.Key, bool, error) {
	return registry.CreateKey(k, path, access)
}

func isLaunchAgentInstalled() bool {
	key, err := registryOpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()

	_, _, err = key.GetStringValue(autostartRegistryName)
	return err == nil
}

func installLaunchAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	key, _, err := registryCreateKey(registry.CURRENT_USER, autostartRegistryKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()

	// Quote the executable path if it contains spaces.
	value := exe
	if containsSpace(value) {
		value = `"` + value + `"`
	}

	return key.SetStringValue(autostartRegistryName, value)
}

func removeLaunchAgent() error {
	key, err := registryOpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer key.Close()

	err = key.DeleteValue(autostartRegistryName)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func containsSpace(s string) bool {
	for _, c := range s {
		if c == ' ' {
			return true
		}
	}
	return false
}
