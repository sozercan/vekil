package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

const scheduledTaskNamePrefix = "Vekil-"

// scheduledTaskServiceBackend registers a logon task that runs as the current
// user, so Vekil keeps that user's GitHub, Azure CLI, and Codex logins without
// administrator rights or a stored password.
type scheduledTaskServiceBackend struct{}

// scheduledTaskName is per user because Task Scheduler names are machine-wide.
// The account SID stays stable across user renames.
func scheduledTaskName(deps serviceDeps) (string, error) {
	sid, err := deps.userID()
	if err != nil {
		return "", fmt.Errorf("resolve current user SID: %w", err)
	}
	if sid == "" || strings.ContainsAny(sid, `\/:*?"<>|`) || validateServiceValue(sid) != nil {
		return "", fmt.Errorf("current user SID %q cannot be used in a task name", sid)
	}
	return scheduledTaskNamePrefix + sid, nil
}

func (scheduledTaskServiceBackend) schtasks(deps serviceDeps, stdout, stderr io.Writer, verb string, extra ...string) error {
	name, err := scheduledTaskName(deps)
	if err != nil {
		return err
	}
	return deps.run(stdout, stderr, "schtasks", append([]string{verb, "/TN", name}, extra...)...)
}

func (scheduledTaskServiceBackend) tool() string { return "schtasks" }

func (scheduledTaskServiceBackend) registerFlags(*flag.FlagSet) {}

func (scheduledTaskServiceBackend) preflight(serviceDeps) error { return nil }

func (scheduledTaskServiceBackend) defaultLogFile(deps serviceDeps) (string, error) {
	cache, err := deps.userCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(cache, "vekil", "logs", "vekil.log"), nil
}

func (scheduledTaskServiceBackend) location(deps serviceDeps) (string, error) {
	name, err := scheduledTaskName(deps)
	if err != nil {
		return "", err
	}
	return "scheduled task " + name, nil
}

func (b scheduledTaskServiceBackend) installed(deps serviceDeps) (bool, error) {
	if _, err := scheduledTaskName(deps); err != nil {
		return false, err
	}
	return b.schtasks(deps, io.Discard, io.Discard, "/Query") == nil, nil
}

func (b scheduledTaskServiceBackend) register(deps serviceDeps, spec serviceSpec) error {
	username, err := deps.username()
	if err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	}
	definition, err := renderScheduledTaskXML(username, spec.command)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "vekil-task-*.xml")
	if err != nil {
		return fmt.Errorf("create task definition: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(encodeUTF16LE(definition))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write task definition: %w", err)
	}
	if err := b.schtasks(deps, deps.stderr, deps.stderr, "/Create", "/XML", file.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create failed: %w", err)
	}
	return nil
}

func (b scheduledTaskServiceBackend) restart(deps serviceDeps) error {
	// Ending fails when the task is not running, which is fine.
	_ = b.schtasks(deps, io.Discard, io.Discard, "/End")
	if err := b.schtasks(deps, deps.stderr, deps.stderr, "/Run"); err != nil {
		return fmt.Errorf("schtasks /Run failed: %w", err)
	}
	return nil
}

func (b scheduledTaskServiceBackend) remove(deps serviceDeps) error {
	_ = b.schtasks(deps, io.Discard, io.Discard, "/End")
	if err := b.schtasks(deps, deps.stderr, deps.stderr, "/Delete", "/F"); err != nil {
		return fmt.Errorf("schtasks /Delete failed: %w", err)
	}
	return nil
}

func (b scheduledTaskServiceBackend) status(deps serviceDeps) error {
	return b.schtasks(deps, deps.stdout, deps.stderr, "/Query", "/V", "/FO", "LIST")
}

func (scheduledTaskServiceBackend) logsHint(logFile string) string {
	return "Get-Content -Wait '" + strings.ReplaceAll(logFile, "'", "''") + "'"
}

// renderScheduledTaskXML starts Vekil at the user's logon with their
// interactive token. ExecutionTimeLimit PT0S removes Task Scheduler's default
// 72-hour limit, and the battery settings keep laptops from stopping it.
func renderScheduledTaskXML(username string, command []string) (string, error) {
	for _, value := range append([]string{username}, command...) {
		if err := validateServiceValue(value); err != nil {
			return "", err
		}
		// Task Scheduler expands %VAR% in actions with no escape syntax.
		if strings.Contains(value, "%") {
			return "", fmt.Errorf("value %q contains %%, which Task Scheduler would expand", value)
		}
	}
	args := make([]string, 0, len(command)-1)
	for _, arg := range command[1:] {
		args = append(args, windowsQuoteArg(arg))
	}
	user := xmlEscape(username)

	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Vekil proxy. Managed by vekil service install.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + user + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + user + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(command[0]) + `</Command>
      <Arguments>` + xmlEscape(strings.Join(args, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`, nil
}

// windowsQuoteArg quotes an argument for the Windows command-line parsing
// rules Go uses to build os.Args.
func windowsQuoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// encodeUTF16LE matches the encoding declared in the task XML; schtasks
// reliably accepts UTF-16 with a byte-order mark.
func encodeUTF16LE(text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[2+2*i:], unit)
	}
	return out
}
