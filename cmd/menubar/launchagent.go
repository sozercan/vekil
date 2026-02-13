package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.copilot-proxy.menubar</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <false/>
</dict>
</plist>
`

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.copilot-proxy.menubar.plist"), nil
}

func isLaunchAgentInstalled() bool {
	p, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func installLaunchAgent() error {
	p, err := plistPath()
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	return os.WriteFile(p, []byte(fmt.Sprintf(plistTemplate, exe)), 0o644)
}

func removeLaunchAgent() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	return os.Remove(p)
}
