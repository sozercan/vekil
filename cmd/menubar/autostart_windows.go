package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

var autostartRegistryKey = `Software\Microsoft\Windows\CurrentVersion\Run`

const autostartRegistryName = "Vekil"

func isLaunchAgentInstalled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()

	_, _, err = k.GetStringValue(autostartRegistryName)
	return err == nil
}

func installLaunchAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	return k.SetStringValue(autostartRegistryName, exe)
}

func removeLaunchAgent() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryKey, registry.SET_VALUE)
	if err != nil {
		// If the key doesn't exist, nothing to remove.
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	defer k.Close()

	err = k.DeleteValue(autostartRegistryName)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}
