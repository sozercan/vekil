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
	// Dispatch subcommands before falling through to the default server mode.
	switch commandFromArgs(os.Args) {
	case cliCommandLogin:
		runLogin(os.Args[2:])
		return
	case cliCommandLogout:
		runLogout(os.Args[2:])
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

func runServe() {
	port := flag.String("port", getEnv("PORT", "1337"), "Listen port")
	host := flag.String("host", getEnv("HOST", "0.0.0.0"), "Listen host")
	tokenDir := flag.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/vekil)")
	providersConfigPath := flag.String("providers-config", getEnv("PROVIDERS_CONFIG", ""), "Path to JSON or YAML provider configuration")
	logLevel := flag.String("log-level", getEnv("LOG_LEVEL", "info"), "Log level")
	metricsEnabled := flag.Bool("metrics", getEnvBool("METRICS", true), "Expose Prometheus metrics at /metrics")
	noMetrics := flag.Bool("no-metrics", false, "Disable the Prometheus /metrics endpoint")
	streamingUpstreamTimeout := flag.Duration("streaming-upstream-timeout", getEnvDuration("STREAMING_UPSTREAM_TIMEOUT", proxy.DefaultStreamingUpstreamTimeout()), "Timeout for streaming upstream inference requests")
	copilotEditorVersion := flag.String("copilot-editor-version", getEnv("COPILOT_EDITOR_VERSION", ""), "Upstream Copilot editor-version header")
	copilotPluginVersion := flag.String("copilot-plugin-version", getEnv("COPILOT_PLUGIN_VERSION", ""), "Upstream Copilot editor-plugin-version header")
	copilotUserAgent := flag.String("copilot-user-agent", getEnv("COPILOT_USER_AGENT", ""), "Upstream Copilot user-agent header")
	copilotGitHubAPIVersion := flag.String("copilot-github-api-version", getEnv("COPILOT_GITHUB_API_VERSION", ""), "Upstream Copilot x-github-api-version header")
	responsesWSTurnStateDelta := flag.Bool("responses-ws-turn-state-delta", getEnvBool("RESPONSES_WS_TURN_STATE_DELTA", false), "Attempt delta-only replay when upstream returns X-Codex-Turn-State")
	responsesWSDisableAutoCompact := flag.Bool("responses-ws-disable-auto-compact", getEnvBool("RESPONSES_WS_DISABLE_AUTO_COMPACT", false), "Disable automatic websocket-session history compaction")
	responsesWSCompactMaxItems := flag.Int("responses-ws-auto-compact-max-items", getEnvInt("RESPONSES_WS_AUTO_COMPACT_MAX_ITEMS", proxy.DefaultResponsesWebSocketConfig().AutoCompactMaxItems), "Auto-compact websocket session history after this many items")
	responsesWSCompactMaxBytes := flag.Int("responses-ws-auto-compact-max-bytes", getEnvInt("RESPONSES_WS_AUTO_COMPACT_MAX_BYTES", proxy.DefaultResponsesWebSocketConfig().AutoCompactMaxBytes), "Auto-compact websocket session history after this many raw bytes")
	responsesWSCompactKeepTail := flag.Int("responses-ws-auto-compact-keep-tail", getEnvInt("RESPONSES_WS_AUTO_COMPACT_KEEP_TAIL", proxy.DefaultResponsesWebSocketConfig().AutoCompactKeepTail), "When auto-compacting websocket history, keep this many most recent items verbatim")
	flag.Parse()
	if *noMetrics {
		*metricsEnabled = false
	}

	log := logger.New(logger.ParseLevel(*logLevel))

	authenticator, err := auth.NewAuthenticator(*tokenDir)
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(*providersConfigPath)
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
		*host,
		*port,
		server.WithBuildVersion(normalizedBuildVersion()),
		server.WithMetricsEnabled(*metricsEnabled),
		server.WithStreamingUpstreamTimeout(*streamingUpstreamTimeout),
		server.WithCopilotHeaderConfig(proxy.CopilotHeaderConfig{
			EditorVersion:       *copilotEditorVersion,
			EditorPluginVersion: *copilotPluginVersion,
			UserAgent:           *copilotUserAgent,
			GitHubAPIVersion:    *copilotGitHubAPIVersion,
		}),
		server.WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{
			TurnStateDelta:      *responsesWSTurnStateDelta,
			DisableAutoCompact:  *responsesWSDisableAutoCompact,
			AutoCompactMaxItems: *responsesWSCompactMaxItems,
			AutoCompactMaxBytes: *responsesWSCompactMaxBytes,
			AutoCompactKeepTail: *responsesWSCompactKeepTail,
		}),
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
