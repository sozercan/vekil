package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
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

func main() {
	if code := runCLI(os.Args); code != 0 {
		os.Exit(code)
	}
}

func runCLI(args []string) int {
	return runCLIWithDeps(args, defaultCLIDeps())
}

type cliDeps struct {
	stderr    io.Writer
	runServe  func([]string, bool) int
	runLogin  func([]string, bool) int
	runLogout func([]string, bool) int
}

func defaultCLIDeps() cliDeps {
	return cliDeps{
		stderr: os.Stderr,
		runServe: func(args []string, quiet bool) int {
			runServe(args, quiet)
			return 0
		},
		runLogin: func(args []string, quiet bool) int {
			return runLoginWithDeps(args, defaultLoginDeps(), quiet)
		},
		runLogout: func(args []string, quiet bool) int {
			return runLogoutWithDeps(args, defaultLogoutDeps(), quiet)
		},
	}
}

func runCLIWithDeps(args []string, deps cliDeps) int {
	deps = normalizeCLIDeps(deps)

	filteredArgs, quiet, err := stripGlobalQuietFlags(args)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 2
	}

	// Dispatch subcommands before falling through to the default server mode.
	switch commandFromArgs(filteredArgs) {
	case cliCommandLogin:
		return deps.runLogin(filteredArgs[2:], quiet)
	case cliCommandLogout:
		return deps.runLogout(filteredArgs[2:], quiet)
	default:
		return deps.runServe(filteredArgs[1:], quiet)
	}
}

func normalizeCLIDeps(deps cliDeps) cliDeps {
	defaults := defaultCLIDeps()
	if deps.stderr == nil {
		deps.stderr = defaults.stderr
	}
	if deps.runServe == nil {
		deps.runServe = defaults.runServe
	}
	if deps.runLogin == nil {
		deps.runLogin = defaults.runLogin
	}
	if deps.runLogout == nil {
		deps.runLogout = defaults.runLogout
	}
	return deps
}

func stripGlobalQuietFlags(args []string) ([]string, bool, error) {
	filtered := make([]string, 0, len(args))
	quiet := false

	for _, arg := range args {
		switch arg {
		case "--quiet", "-quiet", "--q", "-q":
			quiet = true
			continue
		case "--quiet=true", "-quiet=true", "--q=true", "-q=true":
			quiet = true
			continue
		case "--quiet=false", "-quiet=false", "--q=false", "-q=false":
			quiet = false
			continue
		}

		filtered = append(filtered, arg)
	}

	if len(filtered) == 0 {
		return nil, false, fmt.Errorf("invalid arguments")
	}
	return filtered, quiet, nil
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
	stderr           io.Writer
	newAuthenticator func(string) (loginAuthenticator, error)
	openURL          func(string) error
}

func runLogin(args []string, quiet bool) {
	if code := runLoginWithDeps(args, defaultLoginDeps(), quiet); code != 0 {
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

func runLoginWithDeps(args []string, deps loginDeps, quiet bool) int {
	deps = normalizeLoginDeps(deps)
	infoOut := deps.stderr
	if quiet {
		infoOut = io.Discard
	}

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
		_, _ = fmt.Fprintln(infoOut, "Login successful.")
		return 0
	}

	if !opts.force {
		if _, err := authenticator.RefreshTokenNonInteractive(ctx); err == nil {
			_, _ = fmt.Fprintln(infoOut, "Already logged in.")
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

	_, _ = fmt.Fprintf(infoOut, "Opening browser to %s\n", dcResp.VerificationURI)
	_, _ = fmt.Fprintf(infoOut, "Enter code: %s\n", dcResp.UserCode)

	if err := deps.openURL(dcResp.VerificationURI); err != nil {
		_, _ = fmt.Fprintf(infoOut, "Could not open browser automatically, please visit the URL above.\n")
	}

	if err := authenticator.PollForAuthorization(ctx, dcResp); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(infoOut, "Login successful.")
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

type logoutDeps struct {
	stderr           io.Writer
	newAuthenticator func(string) (*auth.Authenticator, error)
}

func defaultLogoutDeps() logoutDeps {
	return logoutDeps{
		stderr: os.Stderr,
		newAuthenticator: func(tokenDir string) (*auth.Authenticator, error) {
			return auth.NewAuthenticator(tokenDir)
		},
	}
}

func normalizeLogoutDeps(deps logoutDeps) logoutDeps {
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if deps.newAuthenticator == nil {
		deps.newAuthenticator = defaultLogoutDeps().newAuthenticator
	}
	return deps
}

func runLogout(args []string, quiet bool) {
	if code := runLogoutWithDeps(args, defaultLogoutDeps(), quiet); code != 0 {
		os.Exit(code)
	}
}

func runLogoutWithDeps(args []string, deps logoutDeps, quiet bool) int {
	deps = normalizeLogoutDeps(deps)
	infoOut := deps.stderr
	if quiet {
		infoOut = io.Discard
	}

	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	tokenDir := fs.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	authenticator, err := deps.newAuthenticator(*tokenDir)
	if err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	if err := authenticator.SignOut(); err != nil {
		_, _ = fmt.Fprintf(deps.stderr, "error: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(infoOut, "Logged out. Vekil will not use GitHub CLI automatically until you run vekil login --github-cli.")
	return 0
}

type serveFlags struct {
	quiet                           *bool
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
	quiet := false
	fs.BoolVar(&quiet, "quiet", false, "Suppress non-error CLI output")
	fs.BoolVar(&quiet, "q", false, "Alias for --quiet")

	return serveFlags{
		quiet:                           &quiet,
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
	fs := flag.NewFlagSet("vekil", flag.ExitOnError)
	serve := registerServeFlags(fs)
	fs.Parse(args) //nolint:errcheck

	quiet = quiet || *serve.quiet

	logLevel := logger.ParseLevel(*serve.logLevel)
	if quiet && logLevel < logger.LevelError {
		logLevel = logger.LevelError
	}
	log := logger.New(logLevel)

	authenticator, err := auth.NewAuthenticator(*serve.tokenDir)
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}
	if quiet {
		authenticator.SetInteractiveOutput(io.Discard)
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
