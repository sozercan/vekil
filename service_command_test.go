package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/proxy"
)

const (
	testUserSID  = "S-1-5-21-1-1001"
	testTaskName = scheduledTaskNamePrefix + testUserSID
)

type fakeServiceEnv struct {
	deps      serviceDeps
	stdout    *bytes.Buffer
	stderr    *bytes.Buffer
	commands  []string
	failOn    map[string]bool
	configDir string
	cacheDir  string
	home      string
	gotSource string
	status    auth.AuthStatus
	taskXML   string
	setVars   map[string]string
}

func newFakeServiceEnv(t *testing.T, goos string) *fakeServiceEnv {
	t.Helper()
	root := t.TempDir()
	env := &fakeServiceEnv{
		stdout:    &bytes.Buffer{},
		stderr:    &bytes.Buffer{},
		failOn:    map[string]bool{},
		setVars:   map[string]string{},
		configDir: filepath.Join(root, "xdg"),
		cacheDir:  filepath.Join(root, "cache"),
		home:      filepath.Join(root, "home"),
		status:    auth.AuthStatus{HasVekilAccessToken: true},
	}
	env.deps = serviceDeps{
		stdout:        env.stdout,
		stderr:        env.stderr,
		goos:          goos,
		euid:          1000,
		getenv:        func(key string) string { return map[string]string{"PATH": "/usr/bin:/opt/50%"}[key] },
		executable:    func() (string, error) { return "/opt/vekil dir/vekil", nil },
		userConfigDir: func() (string, error) { return env.configDir, nil },
		userCacheDir:  func() (string, error) { return env.cacheDir, nil },
		userHomeDir:   func() (string, error) { return env.home, nil },
		username:      func() (string, error) { return `CORP\ada`, nil },
		userID:        func() (string, error) { return testUserSID, nil },
		setenv: func(key, value string) error {
			env.setVars[key] = value
			return nil
		},
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(_, _ io.Writer, name string, args ...string) error {
			command := strings.Join(append([]string{name}, args...), " ")
			env.commands = append(env.commands, command)
			if name == "schtasks" && args[0] == "/Create" {
				env.taskXML = readUTF16File(t, args[4])
			}
			if env.failOn[command] {
				return errors.New("boom")
			}
			return nil
		},
		loadProvidersConfig: func(source string) (proxy.ProvidersConfig, error) {
			env.gotSource = source
			return proxy.ProvidersConfig{}, nil
		},
		authStatus: func(string) (auth.AuthStatus, error) { return env.status, nil },
	}
	return env
}

func (e *fakeServiceEnv) unitPath() string {
	return filepath.Join(e.configDir, "systemd", "user", systemdUnitName)
}

func (e *fakeServiceEnv) plistPath() string {
	return filepath.Join(e.home, "Library", "LaunchAgents", launchdServiceLabel+".plist")
}

func (e *fakeServiceEnv) envFilePath() string {
	return filepath.Join(e.home, ".config", "vekil", serviceEnvFileName)
}

func (e *fakeServiceEnv) wantCommands(t *testing.T, want ...string) {
	t.Helper()
	if got := strings.Join(e.commands, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func readUTF16File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE || len(data)%2 != 0 {
		t.Fatalf("task XML is not UTF-16LE with BOM: % x", data[:min(len(data), 4)])
	}
	units := make([]uint16, 0, len(data)/2-1)
	for i := 2; i < len(data); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(data[i:]))
	}
	return string(utf16.Decode(units))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestServiceInstallSystemd(t *testing.T) {
	env := newFakeServiceEnv(t, "linux")
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	code := runServiceWithDeps([]string{"install", "--port", "8080", "--providers-config", "providers.yaml", "--copilot-user-agent", `a"b$c%d\e`}, env.deps)
	if code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, env.stderr)
	}

	wantSource := filepath.Join(cwd, "providers.yaml")
	if env.gotSource != wantSource {
		t.Fatalf("validated providers config %q, want %q", env.gotSource, wantSource)
	}
	unit := readFile(t, env.unitPath())
	for _, want := range []string{
		`ExecStart="/opt/vekil dir/vekil" "--copilot-user-agent=a\"b$$c%%d\\e" "--port=8080" ` + systemdQuoteExecArg("--providers-config="+wantSource) + " " + systemdQuoteExecArg("--env-file="+env.envFilePath()),
		`Environment="PATH=/usr/bin:/opt/50%%"`,
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want+"\n") {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "EnvironmentFile") || strings.Contains(unit, "--log-file") {
		t.Errorf("unit should rely on --env-file and journald:\n%s", unit)
	}
	env.wantCommands(t,
		"loginctl enable-linger",
		"systemctl --user daemon-reload",
		"systemctl --user enable vekil.service",
		"systemctl --user restart vekil.service",
	)
	info, err := os.Stat(env.envFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode = %v, want 0600", info.Mode().Perm())
	}
	for _, want := range []string{"Installed and started", "journalctl --user -u vekil -f"} {
		if !strings.Contains(env.stdout.String(), want) {
			t.Fatalf("stdout missing %q: %s", want, env.stdout)
		}
	}
}

func TestServiceInstallPreservesEnvFileOnUpdate(t *testing.T) {
	env := newFakeServiceEnv(t, "linux")
	if code := runServiceWithDeps([]string{"install"}, env.deps); code != 0 {
		t.Fatalf("first install exit = %d, stderr = %s", code, env.stderr)
	}
	if err := os.WriteFile(env.envFilePath(), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.stdout.Reset()
	env.commands = nil

	if code := runServiceWithDeps([]string{"install", "--no-linger"}, env.deps); code != 0 {
		t.Fatalf("second install exit = %d, stderr = %s", code, env.stderr)
	}
	if got := readFile(t, env.envFilePath()); got != "SECRET=1\n" {
		t.Fatalf("env file overwritten: %q", got)
	}
	unit := readFile(t, env.unitPath())
	if !strings.Contains(unit, `ExecStart="/opt/vekil dir/vekil" `+systemdQuoteExecArg("--env-file="+env.envFilePath())+"\n") {
		t.Fatalf("unexpected ExecStart:\n%s", unit)
	}
	if !strings.Contains(env.stdout.String(), "Updated and started") {
		t.Fatalf("stdout = %q", env.stdout)
	}
	if strings.Contains(strings.Join(env.commands, "\n"), "loginctl") {
		t.Fatalf("--no-linger should skip loginctl: %q", env.commands)
	}
}

func TestServiceInstallExplicitFiles(t *testing.T) {
	env := newFakeServiceEnv(t, "darwin")
	dir := t.TempDir()
	envFile := filepath.Join(dir, "custom.env")
	if err := os.WriteFile(envFile, []byte("LOG_LEVEL=debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "vekil.log")

	if code := runServiceWithDeps([]string{"install", "--env-file", envFile, "--log-file", logFile}, env.deps); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, env.stderr)
	}
	plist := readFile(t, env.plistPath())
	for _, want := range []string{"<string>--env-file=" + envFile + "</string>", "<string>--log-file=" + logFile + "</string>"} {
		if strings.Count(plist, want) != 1 {
			t.Errorf("plist should contain %q once:\n%s", want, plist)
		}
	}
	if _, err := os.Stat(env.envFilePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default env file created despite --env-file: %v", err)
	}
	if !strings.Contains(env.stdout.String(), "tail -f "+shellQuoteIfNeeded(logFile)) {
		t.Fatalf("stdout = %s", env.stdout)
	}
}

func TestServiceInstallLaunchd(t *testing.T) {
	env := newFakeServiceEnv(t, "darwin")
	if code := runServiceWithDeps([]string{"install", "--copilot-user-agent", "a<b&c"}, env.deps); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, env.stderr)
	}
	logDir := filepath.Join(env.home, "Library", "Logs", "vekil")
	plist := readFile(t, env.plistPath())
	for _, want := range []string{
		"<string>" + launchdServiceLabel + "</string>",
		"<string>/opt/vekil dir/vekil</string>",
		"<string>--copilot-user-agent=a&lt;b&amp;c</string>",
		"<string>--env-file=" + env.envFilePath() + "</string>",
		"<string>--log-file=" + filepath.Join(logDir, "vekil.log") + "</string>",
		"<key>PATH</key>\n        <string>/usr/bin:/opt/50%</string>",
		"<key>SuccessfulExit</key>\n        <false/>",
		"<key>StandardErrorPath</key>\n    <string>" + filepath.Join(logDir, "launchd.log") + "</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	env.wantCommands(t,
		"launchctl unload "+env.plistPath(),
		"launchctl load -w "+env.plistPath(),
	)
	if strings.Contains(env.stderr.String(), "tray app") {
		t.Fatalf("unexpected tray warning: %s", env.stderr)
	}

	env.commands = nil
	if code := runServiceWithDeps([]string{"uninstall"}, env.deps); code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, env.stderr)
	}
	env.wantCommands(t, "launchctl unload -w "+env.plistPath())
	if _, err := os.Stat(env.plistPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist still present: %v", err)
	}
}

func TestServiceInstallLaunchdChecks(t *testing.T) {
	t.Run("refuses root", func(t *testing.T) {
		env := newFakeServiceEnv(t, "darwin")
		env.deps.euid = 0
		if code := runServiceWithDeps([]string{"install"}, env.deps); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if len(env.commands) != 0 || !strings.Contains(env.stderr.String(), "without sudo") {
			t.Fatalf("commands = %q, stderr = %s", env.commands, env.stderr)
		}
	})
	t.Run("warns about tray launch at login", func(t *testing.T) {
		env := newFakeServiceEnv(t, "darwin")
		trayPlist := filepath.Join(env.home, "Library", "LaunchAgents", launchdTrayLabel+".plist")
		if err := os.MkdirAll(filepath.Dir(trayPlist), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(trayPlist, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if code := runServiceWithDeps([]string{"install"}, env.deps); code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, env.stderr)
		}
		if !strings.Contains(env.stderr.String(), "tray app also launches at login") {
			t.Fatalf("stderr = %s", env.stderr)
		}
	})
}

func TestServiceInstallScheduledTask(t *testing.T) {
	env := newFakeServiceEnv(t, "windows")
	env.failOn["schtasks /Query /TN "+testTaskName] = true
	if code := runServiceWithDeps([]string{"install", "--copilot-user-agent", `say "hi" & go`}, env.deps); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, env.stderr)
	}
	if len(env.commands) != 4 || !strings.HasPrefix(env.commands[1], "schtasks /Create /TN "+testTaskName+" /XML ") || !strings.HasSuffix(env.commands[1], " /F") {
		t.Fatalf("commands = %q", env.commands)
	}
	if env.commands[2] != "schtasks /End /TN "+testTaskName || env.commands[3] != "schtasks /Run /TN "+testTaskName {
		t.Fatalf("commands = %q", env.commands)
	}
	if _, err := os.Stat(strings.Fields(env.commands[1])[4]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary task XML not removed: %v", err)
	}
	logFile := filepath.Join(env.cacheDir, "vekil", "logs", "vekil.log")
	wantArgs := `&#34;--copilot-user-agent=say \&#34;hi\&#34; &amp; go&#34; ` + windowsQuoteArg("--env-file="+env.envFilePath()) + " " + windowsQuoteArg("--log-file="+logFile)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		"<UserId>CORP\\ada</UserId>\n    </LogonTrigger>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<Command>/opt/vekil dir/vekil</Command>",
		"<Arguments>" + wantArgs + "</Arguments>",
	} {
		if !strings.Contains(env.taskXML, want) {
			t.Errorf("task XML missing %q:\n%s", want, env.taskXML)
		}
	}
	if !strings.Contains(env.stdout.String(), "Get-Content -Wait '"+logFile+"'") {
		t.Fatalf("stdout = %s", env.stdout)
	}
}

func TestServiceInstallScheduledTaskRejectsPercent(t *testing.T) {
	env := newFakeServiceEnv(t, "windows")
	if code := runServiceWithDeps([]string{"install", "--copilot-user-agent", "50%"}, env.deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(env.stderr.String(), "Task Scheduler would expand") {
		t.Fatalf("stderr = %s", env.stderr)
	}
	for _, command := range env.commands {
		if strings.Contains(command, "/Create") {
			t.Fatalf("task created despite invalid value: %q", env.commands)
		}
	}
}

func TestWindowsQuoteArg(t *testing.T) {
	for arg, want := range map[string]string{
		"":                  `""`,
		`C:\vekil\x.env`:    `C:\vekil\x.env`,
		`C:\Program Files\`: `"C:\Program Files\\"`,
		`a"b`:               `"a\"b"`,
		`a\"b c`:            `"a\\\"b c"`,
		`a\\b c`:            `"a\\b c"`,
	} {
		if got := windowsQuoteArg(arg); got != want {
			t.Errorf("windowsQuoteArg(%q) = %q, want %q", arg, got, want)
		}
	}
}

func TestServiceInstallFailures(t *testing.T) {
	t.Run("unsupported OS", func(t *testing.T) {
		env := newFakeServiceEnv(t, "freebsd")
		if code := runServiceWithDeps([]string{"install"}, env.deps); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if len(env.commands) != 0 || !strings.Contains(env.stderr.String(), "not freebsd") {
			t.Fatalf("commands = %q, stderr = %s", env.commands, env.stderr)
		}
	})
	t.Run("missing service manager", func(t *testing.T) {
		env := newFakeServiceEnv(t, "linux")
		env.deps.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		if code := runServiceWithDeps([]string{"status"}, env.deps); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(env.stderr.String(), "systemctl was not found") {
			t.Fatalf("stderr = %s", env.stderr)
		}
	})
	t.Run("invalid providers config writes nothing", func(t *testing.T) {
		env := newFakeServiceEnv(t, "linux")
		env.deps.loadProvidersConfig = func(string) (proxy.ProvidersConfig, error) {
			return proxy.ProvidersConfig{}, errors.New("bad config")
		}
		if code := runServiceWithDeps([]string{"install", "--providers-config", "/x.yaml"}, env.deps); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if _, err := os.Stat(env.unitPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unit written despite invalid config: %v", err)
		}
	})
	t.Run("invalid env file writes nothing", func(t *testing.T) {
		env := newFakeServiceEnv(t, "linux")
		if err := os.MkdirAll(filepath.Dir(env.envFilePath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(env.envFilePath(), []byte("not an assignment\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := runServiceWithDeps([]string{"install"}, env.deps); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(env.stderr.String(), "line 1: expected KEY=value") {
			t.Fatalf("stderr = %s", env.stderr)
		}
		if _, err := os.Stat(env.unitPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unit written despite invalid env file: %v", err)
		}
	})
	t.Run("systemctl failure", func(t *testing.T) {
		env := newFakeServiceEnv(t, "linux")
		env.failOn["systemctl --user daemon-reload"] = true
		if code := runServiceWithDeps([]string{"install"}, env.deps); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(env.stderr.String(), "hint: systemctl --user needs your user systemd session") {
			t.Fatalf("stderr = %s", env.stderr)
		}
	})
	t.Run("linger failure only warns", func(t *testing.T) {
		env := newFakeServiceEnv(t, "linux")
		env.failOn["loginctl enable-linger"] = true
		if code := runServiceWithDeps([]string{"install"}, env.deps); code != 0 {
			t.Fatalf("exit = %d, want 0; stderr = %s", code, env.stderr)
		}
		if !strings.Contains(env.stderr.String(), "sudo loginctl enable-linger $USER") {
			t.Fatalf("stderr = %s", env.stderr)
		}
	})
	t.Run("no-linger is Linux only", func(t *testing.T) {
		env := newFakeServiceEnv(t, "darwin")
		if code := runServiceWithDeps([]string{"install", "--no-linger"}, env.deps); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
}

func TestServiceInstallWarnsWithoutSavedCopilotLogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  auth.AuthStatus
		envFile string
		warn    bool
	}{
		{name: "saved login", status: auth.AuthStatus{HasVekilAccessToken: true}},
		{name: "github cli opt-in", status: auth.AuthStatus{GitHubCLIAutoSignIn: true}},
		{name: "env file token", envFile: "COPILOT_GITHUB_TOKEN=gho_x\n"},
		{name: "shell env token only", status: auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceEnv}, warn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeServiceEnv(t, "linux")
			env.status = tc.status
			if tc.envFile != "" {
				writeTestEnvFile(t, env.envFilePath(), tc.envFile)
			}
			if code := runServiceWithDeps([]string{"install"}, env.deps); code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, env.stderr)
			}
			if got := strings.Contains(env.stderr.String(), "run `vekil login`"); got != tc.warn {
				t.Fatalf("warned = %v, want %v; stderr = %s", got, tc.warn, env.stderr)
			}
		})
	}
}

func TestServiceLifecycleSystemd(t *testing.T) {
	env := newFakeServiceEnv(t, "linux")
	for _, command := range []string{"uninstall", "restart", "status"} {
		want := 1
		if command == "uninstall" {
			want = 0
		}
		if code := runServiceWithDeps([]string{command}, env.deps); code != want || len(env.commands) != 0 {
			t.Fatalf("%s without unit: exit = %d, commands = %q", command, code, env.commands)
		}
	}

	if code := runServiceWithDeps([]string{"install"}, env.deps); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, env.stderr)
	}
	env.commands = nil
	if code := runServiceWithDeps([]string{"restart"}, env.deps); code != 0 {
		t.Fatalf("restart exit = %d, stderr = %s", code, env.stderr)
	}
	if code := runServiceWithDeps([]string{"status"}, env.deps); code != 0 {
		t.Fatalf("status exit = %d, stderr = %s", code, env.stderr)
	}
	if code := runServiceWithDeps([]string{"uninstall"}, env.deps); code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, env.stderr)
	}
	env.wantCommands(t,
		"systemctl --user restart vekil.service",
		"systemctl --user status --no-pager vekil.service",
		"systemctl --user disable --now vekil.service",
		"systemctl --user daemon-reload",
	)
	if _, err := os.Stat(env.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit still present: %v", err)
	}
	if _, err := os.Stat(env.envFilePath()); err != nil {
		t.Fatalf("env file removed: %v", err)
	}
}

func writeTestEnvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServiceInstallValidatesWithEnvFile(t *testing.T) {
	env := newFakeServiceEnv(t, "linux")
	writeTestEnvFile(t, env.envFilePath(), "PROVIDER_KEY=secret\n")
	env.deps.loadProvidersConfig = func(string) (proxy.ProvidersConfig, error) {
		if env.setVars["PROVIDER_KEY"] != "secret" {
			return proxy.ProvidersConfig{}, errors.New("PROVIDER_KEY is not set")
		}
		return proxy.ProvidersConfig{}, nil
	}
	if code := runServiceWithDeps([]string{"install"}, env.deps); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, env.stderr)
	}
	if unit := readFile(t, env.unitPath()); strings.Contains(unit, "secret") {
		t.Fatalf("env file value leaked into unit:\n%s", unit)
	}
}

func TestScheduledTaskNameIsPerUser(t *testing.T) {
	env := newFakeServiceEnv(t, "windows")
	for _, sid := range []string{"S-1-5-21-1-1001", "S-1-5-21-1-1002"} {
		env.deps.userID = func() (string, error) { return sid, nil }
		if got, err := scheduledTaskName(env.deps); err != nil || got != "Vekil-"+sid {
			t.Fatalf("scheduledTaskName(%q) = %q, %v", sid, got, err)
		}
	}
	env.deps.userID = func() (string, error) { return `..\Vekil`, nil }
	if _, err := scheduledTaskName(env.deps); err == nil {
		t.Fatal("expected an error for a SID with a path separator")
	}
}
