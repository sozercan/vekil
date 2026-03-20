package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/proxy"
	"github.com/sozercan/copilot-proxy/server"
)

func main() {
	port := flag.String("port", getEnv("PORT", "1337"), "Listen port")
	host := flag.String("host", getEnv("HOST", "0.0.0.0"), "Listen host")
	tokenDir := flag.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/copilot-proxy)")
	logLevel := flag.String("log-level", getEnv("LOG_LEVEL", "info"), "Log level")
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

	log := logger.New(logger.ParseLevel(*logLevel))

	authenticator := auth.NewAuthenticator(*tokenDir)

	log.Info("authenticating with GitHub Copilot...")
	ctx := context.Background()
	if _, err := authenticator.GetToken(ctx); err != nil {
		log.Fatal("authentication failed", logger.Err(err))
	}
	log.Info("authenticated successfully")

	srv := server.New(
		authenticator,
		log,
		*host,
		*port,
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
	)

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
		return fallback
	}
	return parsed
}
