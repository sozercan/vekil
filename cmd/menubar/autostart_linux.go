package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const autostartFilename = "vekil.desktop"

func autostartDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "autostart"), nil
}

func autostartPath() (string, error) {
	dir, err := autostartDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, autostartFilename), nil
}

func desktopEntry(executable string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Vekil
Comment=Vekil proxy system tray
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, desktopExec(executable))
}

const desktopExecReservedChars = " \t\n\"'\\><~|&;$*?#()`"

func desktopExec(executable string) string {
	executable = strings.ReplaceAll(executable, "%", "%%")
	if !strings.ContainsAny(executable, desktopExecReservedChars) {
		return executable
	}

	var b strings.Builder
	b.Grow(len(executable) + 2)
	b.WriteByte('"')
	for _, r := range executable {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '"':
			b.WriteString(`\\"`)
		case '$':
			b.WriteString(`\\$`)
		case '`':
			b.WriteByte('\\')
			b.WriteByte('\\')
			b.WriteByte('`')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func isLaunchAgentInstalled() bool {
	p, err := autostartPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func installLaunchAgent() error {
	p, err := autostartPath()
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	return os.WriteFile(p, []byte(desktopEntry(exe)), 0o644)
}

func removeLaunchAgent() error {
	p, err := autostartPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
