package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/pkg/browser"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

// Version and commit are injected via -ldflags; Go reports its runtime version directly.
var (
	version   = "dev"
	commit    = "unknown"
	goVersion = runtime.Version()
)

type cliCommand int

const (
	cliCommandServe cliCommand = iota
	cliCommandLogin
	cliCommandLogout
	cliCommandLaunch
	cliCommandConfig
	cliCommandVersion
)

func main() {
	// Dispatch subcommands before falling through to the default server mode.
	switch commandFromArgs(os.Args) {
	case cliCommandLogin:
		runLogin(os.Args[2:])
		return
	case cliCommandLogout:
		runLogout(os.Args[2:])
		return
	case cliCommandLaunch:
		runLaunch(os.Args[2:])
		return
	case cliCommandConfig:
		runConfig(os.Args[2:])
		return
	case cliCommandVersion:
		writeVersion(os.Stdout)
		return
	}

	runServe()
}

func commandFromArgs(args []string) cliCommand {
	if len(args) < 2 {
		return cliCommandServe
	}

	switch args[1] {
	case "login":
		return cliCommandLogin
	case "logout":
		return cliCommandLogout
	case "launch":
		return cliCommandLaunch
	case "config":
		return cliCommandConfig
	case "version", "--version", "-version":
		return cliCommandVersion
	default:
		return cliCommandServe
	}
}

func writeVersion(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	_, _ = fmt.Fprintf(w, "vekil %s (commit %s, %s)\n", version, commit, goVersion)
}

type loginOptions struct {
	tokenDir     string
	useGitHubCLI bool
	force        bool
}

var errConflictingLoginFlags = fmt.Errorf("--github-cli/--gh cannot be used with --force")

type loginAuthenticator interface {
	SignInWithGitHubCLI(context.Context) error
	RefreshTokenNonInteractive(context.Context) (string, error)
	RequestDeviceCode(context.Context) (*auth.DeviceCodeResponse, error)
	PollForAuthorization(context.Context, *auth.DeviceCodeResponse) error
}

type loginDeps struct {
	stderr           io.Writer
	newAuthenticator func(string) (loginAuthenticator, error)
	openURL          func(string) error
}

func runLogin(args []string) {
	if code := runLoginWithDeps(args, defaultLoginDeps()); code != 0 {
		os.Exit(code)
	}
}

func defaultLoginDeps() loginDeps {
	return loginDeps{
		stderr: os.Stderr,
		newAuthenticator: func(tokenDir string) (loginAuthenticator, error) {
			return auth.NewAuthenticator(tokenDir)
		},
		openURL: browser.OpenURL,
	}
}

func runLoginWithDeps(args []string, deps loginDeps) int {
	deps = normalizeLoginDeps(deps)

	opts, err := parseLoginOptions(args, deps.stderr)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		if err == errConflictingLoginFlags {
			_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		}
		return 2
	}

	authenticator, err := deps.newAuthenticator(opts.tokenDir)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	ctx := context.Background()
	if opts.useGitHubCLI {
		if err := authenticator.SignInWithGitHubCLI(ctx); err != nil {
			_, _ = fmt.Fprintf(deps.stderr, "error signing in with GitHub CLI: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(deps.stderr, "Login successful.")
		return 0
	}

	if !opts.force {
		if _, err := authenticator.RefreshTokenNonInteractive(ctx); err == nil {
			_, _ = fmt.Fprintln(deps.stderr, "Already logged in.")
			return 0
		} else if !auth.IsInteractiveLoginRequired(err) {
			_, _ = fmt.Fprintf(deps.stderr, "error refreshing existing login: %v\n", err)
			return 1
		}
	}

	dcResp, err := authenticator.RequestDeviceCode(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error requesting device code: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(deps.stderr, "Opening browser to %s\n", dcResp.VerificationURI)
	_, _ = fmt.Fprintf(deps.stderr, "Enter code: %s\n", dcResp.UserCode)

	if err := deps.openURL(dcResp.VerificationURI); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "Could not open browser automatically, please visit the URL above.\n")
	}

	if err := authenticator.PollForAuthorization(ctx, dcResp); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(deps.stderr, "Login successful.")
	return 0
}

func normalizeLoginDeps(deps loginDeps) loginDeps {
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if deps.newAuthenticator == nil {
		deps.newAuthenticator = defaultLoginDeps().newAuthenticator
	}
	if deps.openURL == nil {
		deps.openURL = func(string) error { return nil }
	}
	return deps
}

func parseLoginOptions(args []string, stderr io.Writer) (loginOptions, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	opts := loginOptions{}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tokenDir := fs.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)")
	githubCLI := fs.Bool("github-cli", false, "Sign in using the currently authenticated GitHub CLI account")
	gh := fs.Bool("gh", false, "Alias for --github-cli")
	force := fs.Bool("force", false, "Force the interactive GitHub device-code flow")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	opts.tokenDir = *tokenDir
	opts.useGitHubCLI = *githubCLI || *gh
	opts.force = *force
	if opts.useGitHubCLI && opts.force {
		return opts, errConflictingLoginFlags
	}
	return opts, nil
}

func runLogout(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	tokenDir := fs.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)")
	fs.Parse(args) //nolint:errcheck

	authenticator, err := auth.NewAuthenticator(*tokenDir)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := authenticator.SignOut(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	_, _ = fmt.Fprintln(os.Stderr, "Logged out. Vekil will not use GitHub CLI automatically until you run vekil login --github-cli.")
}

var errProvidersConfigRequired = errors.New("--providers-config PATH is required")

type configValidateOptions struct {
	providersConfigPath string
}

type configValidateDeps struct {
	stdout                      io.Writer
	stderr                      io.Writer
	validateProvidersConfigFile func(string) error
}

func runConfig(args []string) {
	if code := runConfigWithDeps(args, defaultConfigValidateDeps()); code != 0 {
		os.Exit(code)
	}
}

func defaultConfigValidateDeps() configValidateDeps {
	return configValidateDeps{
		stdout:                      os.Stdout,
		stderr:                      os.Stderr,
		validateProvidersConfigFile: proxy.ValidateProvidersConfigFile,
	}
}

func runConfigWithDeps(args []string, deps configValidateDeps) int {
	deps = normalizeConfigValidateDeps(deps)

	if len(args) == 0 {
		_, _ = fmt.Fprintln(deps.stderr, "error: config command is required")
		writeConfigUsage(deps.stderr)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		writeConfigUsage(deps.stderr)
		return 0
	case "validate":
		return runConfigValidateWithDeps(args[1:], deps)
	default:
		_, _ = fmt.Fprintf(deps.stderr, "error: unknown config command %q\n", args[0])
		writeConfigUsage(deps.stderr)
		return 2
	}
}

func runConfigValidateWithDeps(args []string, deps configValidateDeps) int {
	deps = normalizeConfigValidateDeps(deps)

	opts, err := parseConfigValidateOptions(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeConfigValidateUsage(deps.stderr)
			return 0
		}
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		writeConfigValidateUsage(deps.stderr)
		return 2
	}

	if err := deps.validateProvidersConfigFile(opts.providersConfigPath); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: providers config validation failed: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(deps.stdout, "Providers config is valid: %s\n", opts.providersConfigPath)
	return 0
}

func normalizeConfigValidateDeps(deps configValidateDeps) configValidateDeps {
	defaults := defaultConfigValidateDeps()
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if deps.validateProvidersConfigFile == nil {
		deps.validateProvidersConfigFile = defaults.validateProvidersConfigFile
	}
	return deps
}

func parseConfigValidateOptions(args []string) (configValidateOptions, error) {
	var opts configValidateOptions
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	providersConfigPath := fs.String("providers-config", "", "Path to JSON or YAML provider configuration")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *providersConfigPath == "" {
		return opts, errProvidersConfigRequired
	}

	opts.providersConfigPath = *providersConfigPath
	return opts, nil
}

func writeConfigUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: vekil config validate --providers-config PATH")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	_, _ = fmt.Fprintln(w, "  validate    Validate a provider configuration without starting the server")
}

func writeConfigValidateUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: vekil config validate --providers-config PATH")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Validate a provider configuration without starting the server or contacting inference upstreams.")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Options:")
	_, _ = fmt.Fprintln(w, "  --providers-config PATH    Path to a JSON or YAML provider configuration (required)")
}

type serveFlags struct {
	port                            *string
	host                            *string
	tokenDir                        *string
	providersConfigPath             *string
	logLevel                        *string
	streamingUpstreamTimeout        *time.Duration
	copilotEditorVersion            *string
	copilotPluginVersion            *string
	copilotUserAgent                *string
	copilotIntegrationID            *string
	copilotGitHubAPIVersion         *string
	copilotOpenAIIntent             *string
	responsesWSEnabled              *bool
	responsesWSTurnStateDelta       *bool
	responsesWSDisableAutoCompact   *bool
	responsesWSCompactMaxItems      *int
	responsesWSCompactMaxBytes      *int
	responsesWSCompactKeepTail      *int
	compactUpstreamChunkBytes       *int
	compactUpstreamChunkConcurrency *int
	compactUpstreamMaxAttempts      *int
	noMetrics                       *bool
}

func registerServeFlags(fs *flag.FlagSet) serveFlags {
	return serveFlags{
		port:                            fs.String("port", getEnv("PORT", "1337"), "Listen port"),
		host:                            fs.String("host", getEnv("HOST", "127.0.0.1"), "Listen host"),
		tokenDir:                        fs.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)"),
		providersConfigPath:             fs.String("providers-config", getEnv("PROVIDERS_CONFIG", ""), "Path to JSON or YAML provider configuration"),
		logLevel:                        fs.String("log-level", getEnv("LOG_LEVEL", "info"), "Log level"),
		streamingUpstreamTimeout:        fs.Duration("streaming-upstream-timeout", getEnvDuration("STREAMING_UPSTREAM_TIMEOUT", proxy.DefaultStreamingUpstreamTimeout()), "Timeout for streaming upstream inference requests"),
		copilotEditorVersion:            fs.String("copilot-editor-version", getEnv("COPILOT_EDITOR_VERSION", ""), "Upstream Copilot editor-version header"),
		copilotPluginVersion:            fs.String("copilot-plugin-version", getEnv("COPILOT_PLUGIN_VERSION", ""), "Upstream Copilot editor-plugin-version header"),
		copilotUserAgent:                fs.String("copilot-user-agent", getEnv("COPILOT_USER_AGENT", ""), "Upstream Copilot user-agent header"),
		copilotIntegrationID:            fs.String("copilot-integration-id", getEnv("COPILOT_INTEGRATION_ID", ""), "Upstream Copilot copilot-integration-id header"),
		copilotGitHubAPIVersion:         fs.String("copilot-github-api-version", getEnv("COPILOT_GITHUB_API_VERSION", ""), "Upstream Copilot x-github-api-version header"),
		copilotOpenAIIntent:             fs.String("copilot-openai-intent", getEnv("COPILOT_OPENAI_INTENT", ""), "Upstream Copilot openai-intent header"),
		responsesWSEnabled:              fs.Bool("responses-ws-enabled", getEnvBool("RESPONSES_WS_ENABLED", false), "Enable proxy-owned Codex websocket bridge on GET /v1/responses"),
		responsesWSTurnStateDelta:       fs.Bool("responses-ws-turn-state-delta", getEnvBool("RESPONSES_WS_TURN_STATE_DELTA", false), "Attempt delta-only replay when upstream returns X-Codex-Turn-State"),
		responsesWSDisableAutoCompact:   fs.Bool("responses-ws-disable-auto-compact", getEnvBool("RESPONSES_WS_DISABLE_AUTO_COMPACT", false), "Disable automatic websocket-session history compaction"),
		responsesWSCompactMaxItems:      fs.Int("responses-ws-auto-compact-max-items", getEnvInt("RESPONSES_WS_AUTO_COMPACT_MAX_ITEMS", proxy.DefaultResponsesWebSocketConfig().AutoCompactMaxItems), "Auto-compact websocket session history after this many items"),
		responsesWSCompactMaxBytes:      fs.Int("responses-ws-auto-compact-max-bytes", getEnvInt("RESPONSES_WS_AUTO_COMPACT_MAX_BYTES", proxy.DefaultResponsesWebSocketConfig().AutoCompactMaxBytes), "Auto-compact websocket session history after this many raw bytes"),
		responsesWSCompactKeepTail:      fs.Int("responses-ws-auto-compact-keep-tail", getEnvInt("RESPONSES_WS_AUTO_COMPACT_KEEP_TAIL", proxy.DefaultResponsesWebSocketConfig().AutoCompactKeepTail), "When auto-compacting websocket history, keep this many most recent items verbatim"),
		compactUpstreamChunkBytes:       fs.Int("compact-upstream-chunk-bytes", getEnvInt("COMPACT_UPSTREAM_CHUNK_BYTES", proxy.DefaultCompactUpstreamChunkBytes()), "Target body size (bytes) for chunked /v1/responses/compact retries after an upstream 413; halved on each recursive 413 down to a 64 KiB floor"),
		compactUpstreamChunkConcurrency: fs.Int("compact-upstream-chunk-concurrency", getEnvInt("COMPACT_UPSTREAM_CHUNK_CONCURRENCY", proxy.DefaultCompactUpstreamChunkConcurrency()), "Maximum sibling chunk compaction calls to run concurrently after the first chunk succeeds"),
		compactUpstreamMaxAttempts:      fs.Int("compact-upstream-max-attempts", getEnvInt("COMPACT_UPSTREAM_MAX_ATTEMPTS", proxy.DefaultCompactUpstreamMaxAttempts()), "Maximum logical compaction calls the /v1/responses/compact 413 fallback may issue per inbound request. Each call may add one extra HTTP POST for model-fallback and is subject to the shared transport-retry policy"),
		noMetrics:                       fs.Bool("no-metrics", getEnvBool("NO_METRICS", false), "Disable the Prometheus /metrics endpoint"),
	}
}

func (f serveFlags) copilotHeaderConfig() proxy.CopilotHeaderConfig {
	return proxy.CopilotHeaderConfig{
		EditorVersion:       *f.copilotEditorVersion,
		EditorPluginVersion: *f.copilotPluginVersion,
		UserAgent:           *f.copilotUserAgent,
		IntegrationID:       *f.copilotIntegrationID,
		GitHubAPIVersion:    *f.copilotGitHubAPIVersion,
		OpenAIIntent:        *f.copilotOpenAIIntent,
	}
}

func (f serveFlags) responsesWebSocketConfig() proxy.ResponsesWebSocketConfig {
	return proxy.ResponsesWebSocketConfig{
		Enabled:             *f.responsesWSEnabled,
		TurnStateDelta:      *f.responsesWSTurnStateDelta,
		DisableAutoCompact:  *f.responsesWSDisableAutoCompact,
		AutoCompactMaxItems: *f.responsesWSCompactMaxItems,
		AutoCompactMaxBytes: *f.responsesWSCompactMaxBytes,
		AutoCompactKeepTail: *f.responsesWSCompactKeepTail,
	}
}

type serveLifecycleServer interface {
	Start() error
	Stop(context.Context) error
}

type serveStartupAuthenticator interface {
	GetToken(context.Context) (string, error)
}

type dynamicProviderModelValidator interface {
	ValidateDynamicProviderModels(context.Context) error
}

type startupAuthenticationGate interface {
	SetStartupAuthenticationPending(bool)
}

type serveStartupCancellation struct {
	cause   error
	stopErr error
}

func (e *serveStartupCancellation) Error() string {
	if e.stopErr != nil {
		return fmt.Sprintf("startup canceled: %v; shutdown error: %v", e.cause, e.stopErr)
	}
	return fmt.Sprintf("startup canceled: %v", e.cause)
}

func (e *serveStartupCancellation) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.stopErr != nil {
		errs = append(errs, e.stopErr)
	}
	return errs
}

func cancelServeStartup(ctx context.Context, srv serveLifecycleServer, log *logger.Logger) error {
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return &serveStartupCancellation{
		cause:   cause,
		stopErr: stopServeServer(srv, log),
	}
}

func startServeServer(ctx context.Context, srv serveLifecycleServer, authenticator serveStartupAuthenticator, usesCopilot bool, log *logger.Logger) error {
	if gate, ok := srv.(startupAuthenticationGate); ok {
		gate.SetStartupAuthenticationPending(usesCopilot)
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("server start error: %w", err)
	}

	if !usesCopilot {
		return nil
	}

	authDone := make(chan error, 1)
	go func() {
		if log != nil {
			log.Info("authenticating with GitHub Copilot...")
		}
		_, err := authenticator.GetToken(ctx)
		if err == nil && log != nil {
			log.Info("authenticated successfully")
		}
		authDone <- err
	}()

	select {
	case <-ctx.Done():
		return cancelServeStartup(ctx, srv, log)
	case err := <-authDone:
		if err != nil {
			if ctx.Err() != nil {
				return cancelServeStartup(ctx, srv, log)
			}
			authErr := fmt.Errorf("authentication failed: %w", err)
			if stopErr := stopServeServer(srv, log); stopErr != nil {
				return errors.Join(authErr, stopErr)
			}
			return authErr
		}
		if gate, ok := srv.(startupAuthenticationGate); ok {
			gate.SetStartupAuthenticationPending(false)
		}
		if validator, ok := srv.(dynamicProviderModelValidator); ok {
			if err := validator.ValidateDynamicProviderModels(ctx); err != nil {
				if ctx.Err() != nil {
					return cancelServeStartup(ctx, srv, log)
				}
				validationErr := fmt.Errorf("provider model validation failed: %w", err)
				if stopErr := stopServeServer(srv, log); stopErr != nil {
					return errors.Join(validationErr, stopErr)
				}
				return validationErr
			}
		}
	}
	return nil
}

func serveUntilContextDone(ctx context.Context, srv serveLifecycleServer, authenticator serveStartupAuthenticator, usesCopilot bool, log *logger.Logger) error {
	if err := startServeServer(ctx, srv, authenticator, usesCopilot, log); err != nil {
		var canceled *serveStartupCancellation
		if errors.As(err, &canceled) {
			return canceled.stopErr
		}
		return err
	}
	<-ctx.Done()
	return stopServeServer(srv, log)
}

func stopServeServer(srv serveLifecycleServer, log *logger.Logger) error {
	if log != nil {
		log.Info("shutting down...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := srv.Stop(ctx)
	// Keep the startup gate closed until Stop has begun the server's shutdown
	// admission transition. Clearing it before Stop briefly reopens inference
	// routes that were intentionally gated while authentication was pending.
	if gate, ok := srv.(startupAuthenticationGate); ok {
		gate.SetStartupAuthenticationPending(false)
	}
	if err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}
	if log != nil {
		log.Info("server stopped")
	}
	return nil
}

func runServe() {
	serve := registerServeFlags(flag.CommandLine)
	flag.Parse()

	log := logger.New(logger.ParseLevel(*serve.logLevel))

	authenticator, err := auth.NewAuthenticator(*serve.tokenDir)
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(*serve.providersConfigPath)
	if err != nil {
		log.Fatal("failed to load providers config", logger.Err(err))
	}

	srv, err := server.New(
		authenticator,
		log,
		*serve.host,
		*serve.port,
		server.WithStreamingUpstreamTimeout(*serve.streamingUpstreamTimeout),
		server.WithCopilotHeaderConfig(serve.copilotHeaderConfig()),
		server.WithResponsesWebSocketConfig(serve.responsesWebSocketConfig()),
		server.WithCompactUpstreamChunkBytes(*serve.compactUpstreamChunkBytes),
		server.WithCompactUpstreamChunkConcurrency(*serve.compactUpstreamChunkConcurrency),
		server.WithCompactUpstreamMaxAttempts(*serve.compactUpstreamMaxAttempts),
		server.WithProxyOptions(
			proxy.WithProvidersConfig(providersCfg),
			proxy.WithDeferredDynamicProviderModelValidation(providersCfg.UsesCopilot()),
			proxy.WithMetricsEnabled(!*serve.noMetrics),
			proxy.WithBuildInfo(version, commit, goVersion),
		),
	)
	if err != nil {
		log.Fatal("failed to initialize server", logger.Err(err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := serveUntilContextDone(ctx, srv, authenticator, providersCfg.UsesCopilot(), log); err != nil {
		log.Fatal("serve error", logger.Err(err))
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q (expected bool), using default %v\n", key, v, fallback)
		return fallback
	}
	return parsed
}

func getEnvInt(key string, fallback int) int {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q (expected integer), using default %d\n", key, v, fallback)
		return fallback
	}
	return parsed
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q (expected duration like 5m), using default %v\n", key, v, fallback)
		return fallback
	}
	return parsed
}
