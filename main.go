package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/browser"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

type cliCommand int

const (
	cliCommandServe cliCommand = iota
	cliCommandLogin
	cliCommandLogout
)

var envWarningWriter io.Writer = os.Stderr

func main() {
	quiet, args, err := parseGlobalFlags(os.Args)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if quiet {
		envWarningWriter = io.Discard
	}

	// Dispatch subcommands before falling through to the default server mode.
	switch commandFromArgs(args) {
	case cliCommandLogin:
		runLogin(args[2:], quiet)
		return
	case cliCommandLogout:
		runLogout(args[2:], quiet)
		return
	}

	runServe(args[1:], quiet)
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
	default:
		return cliCommandServe
	}
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
	stdout           io.Writer
	stderr           io.Writer
	newAuthenticator func(string) (loginAuthenticator, error)
	openURL          func(string) error
}

func runLogin(args []string, quiet bool) {
	deps := defaultLoginDeps()
	if quiet {
		deps.stdout = io.Discard
	}
	if code := runLoginWithDeps(args, deps); code != 0 {
		os.Exit(code)
	}
}

func defaultLoginDeps() loginDeps {
	return loginDeps{
		stdout: os.Stdout,
		stderr: os.Stderr,
		newAuthenticator: func(tokenDir string) (loginAuthenticator, error) {
			return auth.NewAuthenticator(tokenDir)
		},
		openURL: browser.OpenURL,
	}
}

func runLoginWithDeps(args []string, deps loginDeps) int {
	deps = normalizeLoginDeps(deps)

	opts, err := parseLoginOptions(args, deps.stdout)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
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
		_, _ = fmt.Fprintln(deps.stdout, "Login successful.")
		return 0
	}

	if !opts.force {
		if _, err := authenticator.RefreshTokenNonInteractive(ctx); err == nil {
			_, _ = fmt.Fprintln(deps.stdout, "Already logged in.")
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

	_, _ = fmt.Fprintf(deps.stdout, "Opening browser to %s\n", dcResp.VerificationURI)
	_, _ = fmt.Fprintf(deps.stdout, "Enter code: %s\n", dcResp.UserCode)

	if err := deps.openURL(dcResp.VerificationURI); err != nil {
		_, _ = fmt.Fprintf(deps.stdout, "Could not open browser automatically, please visit the URL above.\n")
	}

	if err := authenticator.PollForAuthorization(ctx, dcResp); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(deps.stdout, "Login successful.")
	return 0
}

func normalizeLoginDeps(deps loginDeps) loginDeps {
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
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
	registerGlobalFlags(fs)
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

type logoutOptions struct {
	tokenDir string
}

func runLogout(args []string, quiet bool) {
	var stdout io.Writer = os.Stdout
	if quiet {
		stdout = io.Discard
	}
	if code := runLogoutWithWriters(args, stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runLogoutWithWriters(args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	opts, err := parseLogoutOptions(args, stdout)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	authenticator, err := auth.NewAuthenticator(opts.tokenDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if err := authenticator.SignOut(); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "Logged out. Vekil will not use GitHub CLI automatically until you run vekil login --github-cli.")
	return 0
}

func parseLogoutOptions(args []string, usageOut io.Writer) (logoutOptions, error) {
	if usageOut == nil {
		usageOut = io.Discard
	}
	opts := logoutOptions{}
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	registerGlobalFlags(fs)
	tokenDir := fs.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	opts.tokenDir = *tokenDir
	return opts, nil
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

func runServe(args []string, quiet bool) {
	fs := flag.NewFlagSet("vekil", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	registerGlobalFlags(fs)
	serve := registerServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	logLevel := logger.ParseLevel(*serve.logLevel)
	if quiet && logLevel < logger.LevelError {
		logLevel = logger.LevelError
	}
	log := logger.New(logLevel)

	authenticator, err := auth.NewAuthenticator(*serve.tokenDir)
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(*serve.providersConfigPath)
	if err != nil {
		log.Fatal("failed to load providers config", logger.Err(err))
	}

	if providersCfg.UsesCopilot() {
		log.Info("authenticating with GitHub Copilot...")
		ctx := context.Background()
		if _, err := authenticator.GetToken(ctx); err != nil {
			log.Fatal("authentication failed", logger.Err(err))
		}
		log.Info("authenticated successfully")
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
		server.WithProxyOptions(proxy.WithProvidersConfig(providersCfg)),
	)
	if err != nil {
		log.Fatal("failed to initialize server", logger.Err(err))
	}

	if err := srv.Start(); err != nil {
		log.Fatal("server start error", logger.Err(err))
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		log.Fatal("shutdown error", logger.Err(err))
	}
	log.Info("server stopped")
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
		_, _ = fmt.Fprintf(envWarningWriter, "warning: ignoring invalid %s=%q (expected bool), using default %v\n", key, v, fallback)
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
		_, _ = fmt.Fprintf(envWarningWriter, "warning: ignoring invalid %s=%q (expected integer), using default %d\n", key, v, fallback)
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
		_, _ = fmt.Fprintf(envWarningWriter, "warning: ignoring invalid %s=%q (expected duration like 5m), using default %v\n", key, v, fallback)
		return fallback
	}
	return parsed
}

func parseGlobalFlags(args []string) (bool, []string, error) {
	if len(args) == 0 {
		return false, nil, nil
	}

	quiet := false
	filtered := make([]string, 0, len(args))
	filtered = append(filtered, args[0])

	for _, arg := range args[1:] {
		switch {
		case arg == "--quiet", arg == "-q":
			quiet = true
		case strings.HasPrefix(arg, "--quiet="):
			v := strings.TrimPrefix(arg, "--quiet=")
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return false, nil, fmt.Errorf("invalid boolean value %q for --quiet", v)
			}
			quiet = parsed
		case strings.HasPrefix(arg, "-q="):
			v := strings.TrimPrefix(arg, "-q=")
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return false, nil, fmt.Errorf("invalid boolean value %q for -q", v)
			}
			quiet = parsed
		default:
			filtered = append(filtered, arg)
		}
	}

	return quiet, filtered, nil
}

func registerGlobalFlags(fs *flag.FlagSet) {
	fs.Bool("quiet", false, "Suppress non-error CLI output")
	fs.Bool("q", false, "Alias for --quiet")
}
