package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/launch"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

type launchAgentOptions struct {
	model                    string
	binary                   string
	port                     string
	tokenDir                 string
	providersConfigPath      string
	logLevel                 string
	proxyLogPath             string
	startupTimeout           time.Duration
	streamingUpstreamTimeout time.Duration
	dryRun                   bool
	noSummary                bool
	noMetrics                bool
	forwardedArgs            []string
}

type launchClaudeOptions = launchAgentOptions

type launchTargetSpec struct {
	name        string
	displayName string
	installHelp string
	adapter     launch.Adapter
}

func launchTarget(name string) (launchTargetSpec, bool) {
	switch name {
	case "claude":
		return launchTargetSpec{
			name:        "claude",
			displayName: "Claude Code",
			installHelp: "install Claude Code",
			adapter:     launch.ClaudeAdapter{},
		}, true
	case "codex":
		return launchTargetSpec{
			name:        "codex",
			displayName: "Codex CLI",
			installHelp: "install Codex CLI",
			adapter:     launch.CodexAdapter{},
		}, true
	case "copilot":
		return launchTargetSpec{
			name:        "copilot",
			displayName: "GitHub Copilot CLI",
			installHelp: "install GitHub Copilot CLI",
			adapter:     launch.CopilotAdapter{},
		}, true
	default:
		return launchTargetSpec{}, false
	}
}

func runLaunch(args []string) {
	code := runLaunchCommand(args, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

func runLaunchCommand(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		printLaunchUsage(stderr)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printLaunchUsage(stderr)
		return 0
	}
	target, ok := launchTarget(args[0])
	if !ok {
		_, _ = fmt.Fprintf(stderr, "error: unsupported launch target %q\n", args[0])
		printLaunchUsage(stderr)
		return 2
	}
	return runLaunchAgent(target, args[1:], stderr)
}

func printLaunchUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintln(w, "  vekil launch claude --model MODEL [options] -- [claude args...]")
	_, _ = fmt.Fprintln(w, "  vekil launch codex --model MODEL [options] -- [codex args...]")
	_, _ = fmt.Fprintln(w, "  vekil launch copilot --model MODEL [options] -- [copilot args...]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Supported launch targets:")
	_, _ = fmt.Fprintln(w, "  claude     Start an ephemeral local Vekil proxy and run Claude Code")
	_, _ = fmt.Fprintln(w, "  codex      Start an ephemeral local Vekil proxy and run Codex CLI")
	_, _ = fmt.Fprintln(w, "  copilot    Start an ephemeral local Vekil proxy and run GitHub Copilot CLI")
}

func parseLaunchAgentOptions(target launchTargetSpec, args []string, stderr io.Writer) (launchAgentOptions, error) {
	var opts launchAgentOptions
	fs := flag.NewFlagSet("launch "+target.name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: vekil launch %s --model MODEL [options] -- [%s args...]\n", target.name, target.name)
		fs.PrintDefaults()
	}

	model := fs.String("model", "", "Public Vekil model ID to use")
	binary := fs.String("binary", "", "Path or command name for "+target.displayName)
	port := fs.String("port", "0", "Ephemeral proxy listen port (0 lets the OS choose)")
	tokenDir := fs.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)")
	providersConfig := fs.String("providers-config", getEnv("PROVIDERS_CONFIG", ""), "Path to JSON or YAML provider configuration")
	logLevel := fs.String("log-level", getEnv("LOG_LEVEL", "info"), "Proxy log level")
	proxyLog := fs.String("proxy-log", "", fmt.Sprintf("Proxy JSON log path (default: ~/.config/vekil/logs/launch-%s-*.jsonl)", target.name))
	startupTimeout := fs.Duration("startup-timeout", getEnvDuration("LAUNCH_STARTUP_TIMEOUT", 2*time.Minute), "Maximum time to authenticate and become ready")
	streamingTimeout := fs.Duration("streaming-upstream-timeout", getEnvDuration("STREAMING_UPSTREAM_TIMEOUT", proxy.DefaultStreamingUpstreamTimeout()), "Timeout for streaming upstream inference requests")
	dryRun := fs.Bool("dry-run", false, "Print the child-process plan without starting a proxy; dynamic model metadata remains unresolved")
	noSummary := fs.Bool("no-summary", false, "Do not print an end-of-session usage summary")
	noMetrics := fs.Bool("no-metrics", getEnvBool("NO_METRICS", false), "Disable the Prometheus /metrics endpoint")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if strings.TrimSpace(*model) == "" {
		return opts, fmt.Errorf("--model is required")
	}
	portNumber, err := strconv.Atoi(strings.TrimSpace(*port))
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return opts, fmt.Errorf("--port must be an integer between 0 and 65535")
	}

	return launchAgentOptions{
		model:                    strings.TrimSpace(*model),
		binary:                   strings.TrimSpace(*binary),
		port:                     strconv.Itoa(portNumber),
		tokenDir:                 strings.TrimSpace(*tokenDir),
		providersConfigPath:      strings.TrimSpace(*providersConfig),
		logLevel:                 strings.TrimSpace(*logLevel),
		proxyLogPath:             strings.TrimSpace(*proxyLog),
		startupTimeout:           *startupTimeout,
		streamingUpstreamTimeout: *streamingTimeout,
		dryRun:                   *dryRun,
		noSummary:                *noSummary,
		noMetrics:                *noMetrics,
		forwardedArgs:            append([]string(nil), fs.Args()...),
	}, nil
}

func parseLaunchClaudeOptions(args []string, stderr io.Writer) (launchClaudeOptions, error) {
	target, _ := launchTarget("claude")
	return parseLaunchAgentOptions(target, args, stderr)
}

func runLaunchClaude(args []string, stderr io.Writer) int {
	target, _ := launchTarget("claude")
	return runLaunchAgent(target, args, stderr)
}

func runLaunchCodex(args []string, stderr io.Writer) int {
	target, _ := launchTarget("codex")
	return runLaunchAgent(target, args, stderr)
}

func runLaunchCopilot(args []string, stderr io.Writer) int {
	target, _ := launchTarget("copilot")
	return runLaunchAgent(target, args, stderr)
}

func runLaunchAgent(target launchTargetSpec, args []string, stderr io.Writer) int {
	opts, err := parseLaunchAgentOptions(target, args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(opts.providersConfigPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: failed to load providers config: %v\n", err)
		return 1
	}
	localToken, err := newLaunchToken()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: failed to generate local proxy token: %v\n", err)
		return 1
	}

	launchOpts := launch.Options{
		Model:          opts.model,
		Binary:         opts.binary,
		LocalToken:     localToken,
		ForwardedArgs:  opts.forwardedArgs,
		StartupTimeout: opts.startupTimeout,
		SensitiveEnv:   launchSensitiveEnvironment(providersCfg),
		Stderr:         stderr,
		DryRunBaseURL:  launchLoopbackBaseURL(opts.port),
		DryRun:         opts.dryRun,
		NoSummary:      opts.noSummary,
	}
	if opts.dryRun {
		configuredModel, found, resolveErr := proxy.ResolveStaticProviderModel(providersCfg, opts.model)
		if resolveErr != nil {
			_, _ = fmt.Fprintf(stderr, "error: resolve dry-run model metadata: %v\n", resolveErr)
			return 1
		}
		if found {
			model := launchModelInfoFromProviderConfig(configuredModel)
			launchOpts.DryRunModel = &model
		}
		result, runErr := launch.Run(context.Background(), nil, target.adapter, launchOpts)
		if runErr != nil {
			if errors.Is(runErr, launch.ErrBinaryNotFound) {
				_, _ = fmt.Fprintf(stderr, "error: %v; %s or pass --binary\n", runErr, target.installHelp)
				return 127
			}
			_, _ = fmt.Fprintf(stderr, "error: %v\n", runErr)
			return 1
		}
		return result.ExitCode
	}

	authenticator, err := auth.NewAuthenticator(opts.tokenDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: failed to initialize authenticator: %v\n", err)
		return 1
	}
	logFile, logPath, err := openLaunchLog(target.name, opts.proxyLogPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: failed to open proxy log: %v\n", err)
		return 1
	}
	defer func() { _ = logFile.Close() }()
	log := logger.NewWithWriter(logger.ParseLevel(opts.logLevel), logFile)

	srv, err := server.New(
		authenticator,
		log,
		"127.0.0.1",
		opts.port,
		server.WithInboundAuthToken(localToken),
		server.WithStreamingUpstreamTimeout(opts.streamingUpstreamTimeout),
		server.WithCopilotHeaderConfig(copilotHeaderConfigFromEnv()),
		server.WithProxyOptions(launchProxyOptions(providersCfg, opts)...),
	)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: failed to initialize launch proxy: %v\n", err)
		return 1
	}

	proxyRuntime := &agentLaunchProxy{
		srv:           srv,
		authenticator: authenticator,
		usesCopilot:   srv.ModelUsesCopilot(opts.model),
		log:           log,
	}
	launchOpts.LogPath = logPath
	managedSignals := managedLaunchSignals()
	signals := make(chan os.Signal, len(managedSignals))
	signal.Notify(signals, managedSignals...)
	defer signal.Stop(signals)
	launchOpts.Signals = signals
	result, err := launch.Run(context.Background(), proxyRuntime, target.adapter, launchOpts)
	if err != nil {
		if errors.Is(err, launch.ErrBinaryNotFound) {
			_, _ = fmt.Fprintf(stderr, "error: %v; %s or pass --binary\n", err, target.installHelp)
			_, _ = fmt.Fprintf(stderr, "proxy log: %s\n", logPath)
			return 127
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		_, _ = fmt.Fprintf(stderr, "proxy log: %s\n", logPath)
		return 1
	}
	return result.ExitCode
}

func launchProxyOptions(providersCfg proxy.ProvidersConfig, opts launchAgentOptions) []proxy.Option {
	return launchProxyOptionsWithBuildInfo(providersCfg, opts, version, commit, goVersion)
}

func launchProxyOptionsWithBuildInfo(providersCfg proxy.ProvidersConfig, opts launchAgentOptions, buildVersion, buildCommit, buildGoVersion string) []proxy.Option {
	return []proxy.Option{
		proxy.WithProvidersConfig(providersCfg),
		proxy.WithAllowedModels(opts.model),
		proxy.WithDeferredDynamicProviderModelValidation(true),
		proxy.WithMetricsEnabled(!opts.noMetrics),
		proxy.WithBuildInfo(buildVersion, buildCommit, buildGoVersion),
	}
}

func launchModelInfoFromProviderConfig(cfg proxy.ProviderModelConfig) launch.ModelInfo {
	model := launch.ModelInfo{
		ID:                 strings.TrimSpace(cfg.PublicID),
		Name:               strings.TrimSpace(cfg.Name),
		SupportedEndpoints: append([]string(nil), cfg.Endpoints...),
		Capabilities: launch.ModelCapabilities{
			Supports: launch.ModelCapabilitySupports{
				ReasoningEffort: append([]string(nil), cfg.ReasoningEffort...),
			},
		},
	}
	if model.Name == "" {
		model.Name = model.ID
	}
	if cfg.ContextWindow != nil {
		model.Capabilities.Limits.MaxContextWindowTokens = *cfg.ContextWindow
	}
	if cfg.ParallelToolCalls != nil {
		model.Capabilities.Supports.ParallelToolCalls = *cfg.ParallelToolCalls
	}
	if cfg.Vision != nil {
		model.Capabilities.Supports.Vision = *cfg.Vision
	}
	return model
}

type agentLaunchProxy struct {
	srv           *server.Server
	authenticator serveStartupAuthenticator
	usesCopilot   bool
	log           *logger.Logger
}

func (p *agentLaunchProxy) Start(ctx context.Context) error {
	if err := startServeServer(ctx, p.srv, p.authenticator, p.usesCopilot, p.log); err != nil {
		return err
	}
	if p.usesCopilot {
		return nil
	}
	if err := p.srv.ValidateDynamicProviderModels(ctx); err != nil {
		validationErr := fmt.Errorf("provider model validation failed: %w", err)
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return errors.Join(validationErr, p.srv.Stop(stopCtx))
	}
	return nil
}

func (p *agentLaunchProxy) Addr() string { return p.srv.Addr() }

func (p *agentLaunchProxy) Done() <-chan error { return p.srv.Done() }

func (p *agentLaunchProxy) Stop(ctx context.Context) error { return p.srv.Stop(ctx) }

func copilotHeaderConfigFromEnv() proxy.CopilotHeaderConfig {
	return proxy.CopilotHeaderConfig{
		EditorVersion:       getEnv("COPILOT_EDITOR_VERSION", ""),
		EditorPluginVersion: getEnv("COPILOT_PLUGIN_VERSION", ""),
		UserAgent:           getEnv("COPILOT_USER_AGENT", ""),
		IntegrationID:       getEnv("COPILOT_INTEGRATION_ID", ""),
		GitHubAPIVersion:    getEnv("COPILOT_GITHUB_API_VERSION", ""),
		OpenAIIntent:        getEnv("COPILOT_OPENAI_INTENT", ""),
	}
}

func newLaunchToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func launchLoopbackBaseURL(port string) string {
	if strings.TrimSpace(port) == "0" {
		return "http://127.0.0.1:<dynamic>"
	}
	return "http://127.0.0.1:" + strings.TrimSpace(port)
}

func launchSensitiveEnvironment(cfg proxy.ProvidersConfig) []string {
	keys := map[string]struct{}{
		"COPILOT_GITHUB_TOKEN": {},
	}
	for _, provider := range cfg.Providers {
		if key := strings.TrimSpace(provider.APIKeyEnv); key != "" {
			keys[key] = struct{}{}
		}
		if strings.TrimSpace(provider.AuthMode) == "azure_identity" {
			for _, key := range []string{
				"AZURE_CLIENT_SECRET",
				"IDENTITY_HEADER",
				"MSI_SECRET",
				"AZURE_CLIENT_CERTIFICATE_PASSWORD",
				"AZURE_CLIENT_CERTIFICATE_PATH",
				"AZURE_USERNAME",
				"AZURE_PASSWORD",
				"AZURE_FEDERATED_TOKEN_FILE",
			} {
				keys[key] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func openLaunchLog(target, configuredPath string) (*os.File, string, error) {
	path := strings.TrimSpace(configuredPath)
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", err
		}
		path = filepath.Join(home, path[2:])
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", err
		}
		logDir := filepath.Join(home, ".config", "vekil", "logs")
		path = filepath.Join(
			logDir,
			fmt.Sprintf("launch-%s-%s-%d.jsonl", target, time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid()),
		)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	file, err := openPrivateLaunchLog(path)
	if err != nil {
		return nil, "", err
	}
	return file, path, nil
}
