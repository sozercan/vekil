package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/proxy"
)

const serviceEnvFileName = "service.env"

type serviceDeps struct {
	stdout              io.Writer
	stderr              io.Writer
	goos                string
	euid                int
	getenv              func(string) string
	executable          func() (string, error)
	userConfigDir       func() (string, error)
	userCacheDir        func() (string, error)
	userHomeDir         func() (string, error)
	username            func() (string, error)
	userID              func() (string, error)
	setenv              func(key, value string) error
	lookPath            func(string) (string, error)
	run                 func(stdout, stderr io.Writer, name string, args ...string) error
	loadProvidersConfig func(string) (proxy.ProvidersConfig, error)
	authStatus          func(tokenDir string) (auth.AuthStatus, error)
}

func defaultServiceDeps() serviceDeps {
	return serviceDeps{
		stdout:        os.Stdout,
		stderr:        os.Stderr,
		goos:          runtime.GOOS,
		euid:          os.Geteuid(),
		getenv:        os.Getenv,
		executable:    os.Executable,
		userConfigDir: os.UserConfigDir,
		userCacheDir:  os.UserCacheDir,
		userHomeDir:   os.UserHomeDir,
		username: func() (string, error) {
			current, err := user.Current()
			if err != nil {
				return "", err
			}
			return current.Username, nil
		},
		// On Windows, Uid is the account SID.
		userID: func() (string, error) {
			current, err := user.Current()
			if err != nil {
				return "", err
			}
			return current.Uid, nil
		},
		setenv:   os.Setenv,
		lookPath: exec.LookPath,
		run: func(stdout, stderr io.Writer, name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			return cmd.Run()
		},
		loadProvidersConfig: func(source string) (proxy.ProvidersConfig, error) {
			cfg, err := proxy.LoadProvidersConfigFile(source)
			if err != nil {
				return cfg, err
			}
			if strings.TrimSpace(source) != "" {
				if err := proxy.ValidateProvidersConfig(cfg); err != nil {
					return cfg, fmt.Errorf("validate providers config %q: %w", proxy.ProvidersConfigSourceDisplay(source), err)
				}
			}
			return cfg, nil
		},
		authStatus: func(tokenDir string) (auth.AuthStatus, error) {
			authenticator, err := auth.NewAuthenticator(tokenDir)
			if err != nil {
				return auth.AuthStatus{}, err
			}
			return authenticator.Status(), nil
		},
	}
}

// serviceBackend manages the per-user OS integration that starts Vekil in the
// background: a systemd user unit, a launchd agent, or a logon scheduled task.
type serviceBackend interface {
	tool() string
	// registerFlags adds platform-only install flags.
	registerFlags(fs *flag.FlagSet)
	preflight(deps serviceDeps) error
	// defaultLogFile returns "" when the platform captures stderr itself.
	defaultLogFile(deps serviceDeps) (string, error)
	location(deps serviceDeps) (string, error)
	installed(deps serviceDeps) (bool, error)
	// register writes and registers the definition without restarting it.
	register(deps serviceDeps, spec serviceSpec) error
	restart(deps serviceDeps) error
	remove(deps serviceDeps) error
	status(deps serviceDeps) error
	logsHint(logFile string) string
}

type serviceSpec struct {
	command []string
	path    string
}

func serviceBackendFor(goos string) serviceBackend {
	switch goos {
	case "linux":
		return &systemdServiceBackend{}
	case "darwin":
		return launchdServiceBackend{}
	case "windows":
		return scheduledTaskServiceBackend{}
	default:
		return nil
	}
}

func runService(args []string) {
	if code := runServiceWithDeps(args, defaultServiceDeps()); code != 0 {
		os.Exit(code)
	}
}

func runServiceWithDeps(args []string, deps serviceDeps) int {
	if len(args) == 0 {
		writeServiceUsage(deps.stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		writeServiceUsage(deps.stderr)
		return 0
	case "install", "uninstall", "restart", "status":
	default:
		_, _ = fmt.Fprintf(deps.stderr, "error: unknown service command %q\n", args[0])
		writeServiceUsage(deps.stderr)
		return 2
	}

	backend := serviceBackendFor(deps.goos)
	if backend == nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: vekil service supports Linux (systemd), macOS (launchd), and Windows (Task Scheduler), not %s\n", deps.goos)
		return 1
	}
	if _, err := deps.lookPath(backend.tool()); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %s was not found; vekil service needs it on %s\n", backend.tool(), deps.goos)
		return 1
	}

	if args[0] == "install" {
		return runServiceInstall(args[1:], deps, backend)
	}
	if len(args) > 1 {
		_, _ = fmt.Fprintf(deps.stderr, "error: unexpected argument %q\n", args[1])
		return 2
	}
	installed, err := backend.installed(deps)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	if !installed {
		_, _ = fmt.Fprintln(deps.stdout, "Vekil service is not installed.")
		if args[0] == "uninstall" {
			return 0
		}
		return 1
	}

	switch args[0] {
	case "uninstall":
		if err := backend.remove(deps); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
			return 1
		}
		location, _ := backend.location(deps)
		_, _ = fmt.Fprintf(deps.stdout, "Removed %s. Kept the env file and logs.\n", location)
		return 0
	case "restart":
		if err := backend.restart(deps); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(deps.stdout, "Restarted Vekil service.")
		return 0
	default:
		err := backend.status(deps)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
}

func writeServiceUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: vekil service {install|uninstall|restart|status}")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Run Vekil in the background for the current user: a systemd user service on Linux,")
	_, _ = fmt.Fprintln(w, "a launchd agent on macOS, or a logon scheduled task on Windows.")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	_, _ = fmt.Fprintln(w, "  install [server flags]  Write, register, and (re)start the service with the given server flags")
	_, _ = fmt.Fprintln(w, "  uninstall               Stop and remove the service")
	_, _ = fmt.Fprintln(w, "  restart                 Restart the service, for example after editing its env file")
	_, _ = fmt.Fprintln(w, "  status                  Show the service status")
}

func runServiceInstall(args []string, deps serviceDeps, backend serviceBackend) int {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	flags.SetOutput(deps.stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(deps.stderr, "Usage: vekil service install [server flags]")
		_, _ = fmt.Fprintln(deps.stderr)
		_, _ = fmt.Fprintln(deps.stderr, "Server flags are validated and saved in the service definition. Put environment variables and secrets in the env file.")
		_, _ = fmt.Fprintln(deps.stderr)
		flags.PrintDefaults()
	}
	serve := registerServeFlags(flags)
	backend.registerFlags(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(deps.stderr, "error: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if _, err := serve.parsedPolicyRoutingMode(); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: invalid policy routing mode: %v\n", err)
		return 2
	}
	if _, err := serve.parsedStateBindingsConfig(); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: invalid state bindings configuration: %v\n", err)
		return 2
	}

	serverArgs, explicit, err := serviceServerArgs(flags)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 2
	}

	exe, err := deps.executable()
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: resolve vekil executable: %v\n", err)
		return 1
	}
	if strings.Contains(filepath.ToSlash(exe), "/go-build") {
		_, _ = fmt.Fprintf(deps.stderr, "error: %s is a temporary go run binary; build vekil and run install from the built binary\n", exe)
		return 1
	}
	if err := backend.preflight(deps); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	// Capture PATH before applying the env file; the service applies the file
	// itself at startup, so its PATH override still wins there.
	path := deps.getenv("PATH")
	envFile, envCreated, envVars, err := prepareServiceEnvFile(deps, explicit[serveEnvFileFlag])
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	// Validate with the environment the service will see, so provider
	// secrets that live only in the env file count.
	for _, kv := range envVars {
		if err := deps.setenv(kv[0], kv[1]); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: env file %s: set %s: %v\n", envFile, kv[0], err)
			return 1
		}
	}

	providersCfg, err := deps.loadProvidersConfig(explicit["providers-config"])
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: providers config validation failed: %v\n", err)
		return 1
	}

	if explicit[serveEnvFileFlag] == "" {
		serverArgs = append(serverArgs, "--"+serveEnvFileFlag+"="+envFile)
	}
	logFile := explicit[serveLogFileFlag]
	if logFile == "" {
		if logFile, err = backend.defaultLogFile(deps); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
			return 1
		}
		if logFile != "" {
			serverArgs = append(serverArgs, "--"+serveLogFileFlag+"="+logFile)
		}
	}

	updating, err := backend.installed(deps)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	spec := serviceSpec{command: append([]string{exe}, serverArgs...), path: path}
	if err := backend.register(deps, spec); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}
	if err := backend.restart(deps); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	if providersCfg.UsesCopilot() {
		warnIfServiceLacksCopilotLogin(deps, explicit["token-dir"], envFile, envVars)
	}

	location, _ := backend.location(deps)
	verb := "Installed"
	if updating {
		verb = "Updated"
	}
	_, _ = fmt.Fprintf(deps.stdout, "%s and started %s\n", verb, location)
	if envCreated {
		_, _ = fmt.Fprintf(deps.stdout, "Created %s for environment variables and secrets\n", envFile)
	} else {
		_, _ = fmt.Fprintf(deps.stdout, "Using environment file %s\n", envFile)
	}
	_, _ = fmt.Fprintln(deps.stdout, "Status: vekil service status")
	_, _ = fmt.Fprintf(deps.stdout, "Logs:   %s\n", backend.logsHint(logFile))
	return 0
}

// serviceServerArgs returns the explicitly set server flags in a stable order.
// Shell environment defaults are intentionally not captured; the service reads
// environment variables from its env file instead.
func serviceServerArgs(flags *flag.FlagSet) ([]string, map[string]string, error) {
	explicit := map[string]string{}
	var visitErr error
	flags.Visit(func(f *flag.Flag) {
		if visitErr != nil || isServiceOnlyFlag(f.Name) {
			return
		}
		value := f.Value.String()
		switch f.Name {
		case "providers-config":
			value = strings.TrimSpace(value)
			if value != "" && !isRemoteProvidersConfigSource(value) {
				value, visitErr = filepath.Abs(value)
			}
		case "token-dir":
			if value != "" && !strings.HasPrefix(value, "~") {
				value, visitErr = filepath.Abs(value)
			}
		case serveEnvFileFlag, serveLogFileFlag:
			if value != "" {
				value, visitErr = filepath.Abs(value)
			}
		}
		explicit[f.Name] = value
	})
	if visitErr != nil {
		return nil, nil, visitErr
	}

	args := make([]string, 0, len(explicit))
	flags.Visit(func(f *flag.Flag) {
		if value, ok := explicit[f.Name]; ok {
			args = append(args, "--"+f.Name+"="+value)
		}
	})
	return args, explicit, nil
}

func isServiceOnlyFlag(name string) bool {
	return name == systemdNoLingerFlag
}

func isRemoteProvidersConfigSource(source string) bool {
	lower := strings.ToLower(source)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// validateServiceValue rejects values that cannot be written safely into a
// unit, plist, or task definition.
func validateServiceValue(value string) error {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("value %q contains a control character that cannot be written to a service definition", value)
		}
	}
	return nil
}

const serviceEnvFileTemplate = `# Environment for the Vekil background service, one KEY=value per line.
# Created by "vekil service install" and never overwritten. Values here
# override the service's inherited environment, including PATH.
# Apply changes with: vekil service restart
#
# Examples:
# COPILOT_GITHUB_TOKEN=
# AZURE_OPENAI_API_KEY=
# LOG_LEVEL=debug
`

// prepareServiceEnvFile creates the default env file when no explicit one is
// given, then parses the file the service will load.
func prepareServiceEnvFile(deps serviceDeps, explicitPath string) (string, bool, [][2]string, error) {
	path := explicitPath
	created := false
	if path == "" {
		home, err := deps.userHomeDir()
		if err != nil {
			return "", false, nil, fmt.Errorf("resolve home directory: %w", err)
		}
		// Matches the default token directory so all Vekil user state stays together.
		path = filepath.Join(home, ".config", "vekil", serviceEnvFileName)
		if created, err = ensureServiceEnvFile(path); err != nil {
			return "", false, nil, err
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false, nil, fmt.Errorf("open env file: %w", err)
	}
	defer func() { _ = file.Close() }()
	vars, err := parseServeEnvFile(file)
	if err != nil {
		return "", false, nil, fmt.Errorf("env file %s: %w", path, err)
	}
	return path, created, vars, nil
}

func ensureServiceEnvFile(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create %s: %w", path, err)
	}
	_, writeErr := file.WriteString(serviceEnvFileTemplate)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

func warnIfServiceLacksCopilotLogin(deps serviceDeps, tokenDir, envFile string, envVars [][2]string) {
	for _, kv := range envVars {
		if kv[0] == "COPILOT_GITHUB_TOKEN" && strings.TrimSpace(kv[1]) != "" {
			return
		}
	}
	status, err := deps.authStatus(tokenDir)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "warning: could not check GitHub login: %v\n", err)
		return
	}
	// A shell COPILOT_GITHUB_TOKEN does not reach the service, so only the env
	// file's token, saved credentials, and opted-in GitHub CLI reuse count.
	if status.HasVekilAccessToken || (status.GitHubCLIAutoSignIn && !status.SignedOut) {
		return
	}
	_, _ = fmt.Fprintf(deps.stderr, "warning: no saved GitHub login was found, so the service will wait for device-code sign-in in its logs; run `vekil login` (or set COPILOT_GITHUB_TOKEN in %s), then: vekil service restart\n", envFile)
}
