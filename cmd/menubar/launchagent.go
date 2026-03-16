package main

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	launchAgentLabel = "com.copilot-proxy.menubar"
	// Keep this in sync with APP_BUNDLE_ID in the Makefile.
	appBundleID = launchAgentLabel
)

const plistHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + launchAgentLabel + `</string>
    <key>ProgramArguments</key>
    <array>
`

const plistFooter = `    </array>
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
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

func isLaunchAgentInstalled() bool {
	p, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func launchAgentProgramArguments(executable string) []string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}

	if isAppBundleExecutable(executable) {
		// Launch the bundle via Launch Services so login items do not pin a
		// transient App Translocation path or bypass app-bundle semantics.
		return []string{"/usr/bin/open", "-b", appBundleID}
	}

	return []string{executable}
}

func isAppBundleExecutable(executable string) bool {
	dir := filepath.Clean(filepath.Dir(executable))
	for {
		if strings.EqualFold(filepath.Ext(dir), ".app") {
			return true
		}

		next := filepath.Dir(dir)
		if next == dir {
			return false
		}
		dir = next
	}
}

func launchAgentPlist(programArguments []string) (string, error) {
	var builder strings.Builder
	builder.WriteString(plistHeader)

	for _, arg := range programArguments {
		builder.WriteString("        <string>")
		if err := xml.EscapeText(&builder, []byte(arg)); err != nil {
			return "", err
		}
		builder.WriteString("</string>\n")
	}

	builder.WriteString(plistFooter)
	return builder.String(), nil
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

	plist, err := launchAgentPlist(launchAgentProgramArguments(exe))
	if err != nil {
		return err
	}

	return os.WriteFile(p, []byte(plist), 0o644)
}

func removeLaunchAgent() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
