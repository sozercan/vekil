package main

import (
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	launchdServiceLabel = "com.sertacozercan.vekil"
	// The tray app's Launch at Login agent; see cmd/menubar/launchagent_darwin.go.
	launchdTrayLabel = "com.vekil.menubar"
)

type launchdServiceBackend struct{}

func (launchdServiceBackend) tool() string { return "launchctl" }

func (launchdServiceBackend) registerFlags(*flag.FlagSet) {}

func (launchdServiceBackend) preflight(deps serviceDeps) error {
	if deps.euid == 0 {
		return errors.New("run vekil service install without sudo; it installs a launch agent for your own account")
	}
	return nil
}

func (launchdServiceBackend) defaultLogFile(deps serviceDeps) (string, error) {
	dir, err := launchdLogDir(deps)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vekil.log"), nil
}

func launchdLogDir(deps serviceDeps) (string, error) {
	home, err := deps.userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "vekil"), nil
}

func launchdAgentPath(deps serviceDeps, label string) (string, error) {
	home, err := deps.userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func (launchdServiceBackend) location(deps serviceDeps) (string, error) {
	return launchdAgentPath(deps, launchdServiceLabel)
}

func (b launchdServiceBackend) installed(deps serviceDeps) (bool, error) {
	path, err := b.location(deps)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (b launchdServiceBackend) register(deps serviceDeps, spec serviceSpec) error {
	path, err := b.location(deps)
	if err != nil {
		return err
	}
	logDir, err := launchdLogDir(deps)
	if err != nil {
		return err
	}
	plist, err := renderLaunchdPlist(spec.command, spec.path, filepath.Join(logDir, "launchd.log"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", logDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if trayPath, err := launchdAgentPath(deps, launchdTrayLabel); err == nil {
		if _, err := os.Stat(trayPath); err == nil {
			_, _ = fmt.Fprintln(deps.stderr, "warning: the Vekil tray app also launches at login; turn off its Launch at Login so it does not compete with this service for the port and state file")
		}
	}
	return nil
}

func (b launchdServiceBackend) restart(deps serviceDeps) error {
	path, err := b.location(deps)
	if err != nil {
		return err
	}
	// Unloading fails when the agent is not loaded yet, which is fine.
	_ = deps.run(io.Discard, io.Discard, "launchctl", "unload", path)
	if err := deps.run(deps.stderr, deps.stderr, "launchctl", "load", "-w", path); err != nil {
		return fmt.Errorf("launchctl load -w %s failed: %w", path, err)
	}
	return nil
}

func (b launchdServiceBackend) remove(deps serviceDeps) error {
	path, err := b.location(deps)
	if err != nil {
		return err
	}
	if err := deps.run(deps.stderr, deps.stderr, "launchctl", "unload", "-w", path); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "warning: launchctl unload -w %s failed: %v\n", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func (launchdServiceBackend) status(deps serviceDeps) error {
	return deps.run(deps.stdout, deps.stderr, "launchctl", "list", launchdServiceLabel)
}

func (launchdServiceBackend) logsHint(logFile string) string {
	return "tail -f " + shellQuoteIfNeeded(logFile)
}

// renderLaunchdPlist keeps the agent alive after failures, like systemd's
// Restart=on-failure. Vekil writes its own rotating log; launchdLog only
// captures output written before that file opens, such as crashes.
func renderLaunchdPlist(command []string, path, launchdLog string) (string, error) {
	values := append(append([]string{}, command...), path, launchdLog)
	for _, value := range values {
		if err := validateServiceValue(value); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + launchdServiceLabel + `</string>
    <key>ProgramArguments</key>
    <array>
`)
	for _, arg := range command {
		b.WriteString("        <string>" + xmlEscape(arg) + "</string>\n")
	}
	b.WriteString("    </array>\n")
	if path != "" {
		b.WriteString("    <key>EnvironmentVariables</key>\n    <dict>\n        <key>PATH</key>\n        <string>" + xmlEscape(path) + "</string>\n    </dict>\n")
	}
	b.WriteString(`    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>` + xmlEscape(launchdLog) + `</string>
    <key>StandardErrorPath</key>
    <string>` + xmlEscape(launchdLog) + `</string>
</dict>
</plist>
`)
	return b.String(), nil
}

func xmlEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func shellQuoteIfNeeded(value string) string {
	if !strings.ContainsAny(value, " \t'\"\\$`") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
