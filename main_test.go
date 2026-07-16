package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

func TestGetEnvDuration(t *testing.T) {
	const envKey = "TEST_STREAMING_UPSTREAM_TIMEOUT"
	const fallback = 5 * time.Minute

	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{
			name: "empty uses fallback",
			want: fallback,
		},
		{
			name:  "valid duration parses",
			value: "17m",
			want:  17 * time.Minute,
		},
		{
			name:  "invalid duration uses fallback",
			value: "not-a-duration",
			want:  fallback,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.value)
			if got := getEnvDuration(envKey, fallback); got != tc.want {
				t.Fatalf("getEnvDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetEnvBool(t *testing.T) {
	const envKey = "TEST_BOOL_VAR"

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty uses fallback", value: "", want: false},
		{name: "true parses", value: "true", want: true},
		{name: "false parses", value: "false", want: false},
		{name: "1 parses", value: "1", want: true},
		{name: "invalid uses fallback", value: "nope", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.value)
			if got := getEnvBool(envKey, false); got != tc.want {
				t.Fatalf("getEnvBool() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetEnvInt(t *testing.T) {
	const envKey = "TEST_INT_VAR"

	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "empty uses fallback", value: "", want: 42},
		{name: "valid int parses", value: "100", want: 100},
		{name: "invalid uses fallback", value: "abc", want: 42},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.value)
			if got := getEnvInt(envKey, 42); got != tc.want {
				t.Fatalf("getEnvInt() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetEnvWarnsOnInvalidValue(t *testing.T) {
	const envKey = "TEST_WARN_VAR"

	// Capture stderr to verify warning is emitted.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	t.Setenv(envKey, "not-a-bool")
	getEnvBool(envKey, false)

	_ = w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	want := fmt.Sprintf("warning: ignoring invalid %s=%q", envKey, "not-a-bool")
	if !bytes.Contains([]byte(output), []byte(want)) {
		t.Errorf("expected stderr to contain %q, got %q", want, output)
	}
}

func parseServeFlagsForTest(t *testing.T, args ...string) serveFlags {
	t.Helper()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serve := registerServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse serve flags: %v", err)
	}
	return serve
}

func TestServeFlagsCopilotHeaderEnvDefaults(t *testing.T) {
	t.Setenv("COPILOT_INTEGRATION_ID", "env-integration")
	t.Setenv("COPILOT_OPENAI_INTENT", "env-intent")

	serve := parseServeFlagsForTest(t)
	cfg := serve.copilotHeaderConfig()

	if cfg.IntegrationID != "env-integration" {
		t.Fatalf("IntegrationID = %q, want env-integration", cfg.IntegrationID)
	}
	if cfg.OpenAIIntent != "env-intent" {
		t.Fatalf("OpenAIIntent = %q, want env-intent", cfg.OpenAIIntent)
	}
}

func TestServeFlagsCopilotHeaderCLIOverridesEnv(t *testing.T) {
	t.Setenv("COPILOT_INTEGRATION_ID", "env-integration")
	t.Setenv("COPILOT_OPENAI_INTENT", "env-intent")

	serve := parseServeFlagsForTest(t,
		"--copilot-integration-id", "cli-integration",
		"--copilot-openai-intent", "cli-intent",
	)
	cfg := serve.copilotHeaderConfig()

	if cfg.IntegrationID != "cli-integration" {
		t.Fatalf("IntegrationID = %q, want cli-integration", cfg.IntegrationID)
	}
	if cfg.OpenAIIntent != "cli-intent" {
		t.Fatalf("OpenAIIntent = %q, want cli-intent", cfg.OpenAIIntent)
	}
}

func TestServeFlagsResponsesWebSocketDisabledByDefault(t *testing.T) {
	serve := parseServeFlagsForTest(t)
	cfg := serve.responsesWebSocketConfig()

	if cfg.Enabled {
		t.Fatal("responses websocket bridge should be disabled by default")
	}
}

func TestServeFlagsResponsesWebSocketCanBeEnabled(t *testing.T) {
	t.Setenv("RESPONSES_WS_ENABLED", "true")

	envServe := parseServeFlagsForTest(t)
	if !envServe.responsesWebSocketConfig().Enabled {
		t.Fatal("RESPONSES_WS_ENABLED=true should enable responses websocket bridge")
	}

	cliServe := parseServeFlagsForTest(t, "--responses-ws-enabled=false")
	if cliServe.responsesWebSocketConfig().Enabled {
		t.Fatal("--responses-ws-enabled=false should override RESPONSES_WS_ENABLED=true")
	}
}

func TestServeUntilContextDoneCancelsActiveUpstreamWork(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(upstreamStarted)
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-releaseUpstream:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-main-stop","object":"chat.completion","choices":[]}`)
		}
	}))

	var serverLogs bytes.Buffer
	srv, err := server.New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &serverLogs),
		"127.0.0.1",
		"0",
		server.WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
	)
	if err != nil {
		close(releaseUpstream)
		upstream.Close()
		t.Fatalf("server.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	serveExited := make(chan struct{})
	go func() {
		defer close(serveExited)
		serveErr <- serveUntilContextDone(ctx, srv, nil, false, logger.NewWithWriter(logger.LevelError, io.Discard))
	}()
	t.Cleanup(func() {
		cancel()
		close(releaseUpstream)
		select {
		case <-serveExited:
		case <-time.After(time.Second):
		}
		upstream.Close()
	})

	baseURL := ""
	healthDeadline := time.Now().Add(time.Second)
	for {
		if addr := srv.Addr(); addr != "127.0.0.1:0" {
			baseURL = "http://" + addr
			resp, requestErr := http.Get(baseURL + "/healthz")
			if requestErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
		}
		select {
		case <-serveExited:
			t.Fatalf("server exited before becoming healthy: %v", <-serveErr)
		default:
		}
		if time.Now().After(healthDeadline) {
			t.Fatal("timed out waiting for proxy health endpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}

	type requestResult struct {
		status int
		body   []byte
		err    error
	}
	requestDone := make(chan requestResult, 1)
	go func() {
		resp, requestErr := http.Post(
			baseURL+"/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`),
		)
		result := requestResult{err: requestErr}
		if requestErr == nil {
			result.status = resp.StatusCode
			result.body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		requestDone <- result
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active upstream work")
	}

	start := time.Now()
	cancel() // signal.NotifyContext uses the same cancellation path for SIGTERM.
	select {
	case <-serveExited:
	case <-time.After(time.Second):
		t.Fatal("main serve lifecycle did not exit promptly after signal-context cancellation")
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("serveUntilContextDone() error = %v", err)
	}
	t.Logf("signal-context shutdown returned in %s", time.Since(start))

	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("signal-context shutdown did not cancel active upstream work")
	}
	select {
	case result := <-requestDone:
		if result.err != nil {
			t.Fatalf("client request error = %v", result.err)
		}
		if result.status != http.StatusServiceUnavailable {
			t.Fatalf("shutdown response status = %d, want 503; body=%s", result.status, result.body)
		}
		if !bytes.Contains(result.body, []byte("server shutting down")) {
			t.Fatalf("shutdown response missing service-unavailable detail: %s", result.body)
		}
	case <-time.After(time.Second):
		t.Fatal("client request did not return after signal-context shutdown")
	}

	foundSuppressed := false
	for _, line := range strings.Split(strings.TrimSpace(serverLogs.String()), "\n") {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode server log: %v", err)
		}
		if entry["path"] == "/v1/chat/completions" {
			if entry["status"] != float64(http.StatusServiceUnavailable) {
				t.Fatalf("logged shutdown status = %#v, want 503", entry["status"])
			}
			if entry["stats_suppressed"] != true {
				t.Fatalf("shutdown request log missing stats_suppressed: %#v", entry)
			}
			foundSuppressed = true
		}
	}
	if !foundSuppressed {
		t.Fatalf("missing shutdown request log: %s", serverLogs.String())
	}
}

func TestServeUntilContextDoneStartsServerBeforeCopilotAuthCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan string, 4)
	server := &fakeServeLifecycleServer{
		startFn: func() error {
			events <- "start"
			return nil
		},
		stopFn: func(context.Context) error {
			events <- "stop"
			return nil
		},
	}
	authenticator := &fakeServeStartupAuthenticator{
		getTokenFn: func(ctx context.Context) (string, error) {
			events <- "auth"
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- serveUntilContextDone(ctx, server, authenticator, true, logger.NewWithWriter(logger.LevelError, io.Discard))
	}()

	assertNextServeEvent(t, events, "start")
	assertNextServeEvent(t, events, "auth")

	select {
	case err := <-done:
		t.Fatalf("serveUntilContextDone returned while Copilot auth was still pending: %v", err)
	default:
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveUntilContextDone after cancellation returned error: %v", err)
	}
	assertNextServeEvent(t, events, "stop")
}

func TestServeUntilContextDoneTreatsValidationCancellationAsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	server := &fakeServeLifecycleServer{
		validateFn: func(ctx context.Context) error {
			cancel()
			return ctx.Err()
		},
	}
	authenticator := &fakeServeStartupAuthenticator{}

	if err := serveUntilContextDone(ctx, server, authenticator, true, logger.NewWithWriter(logger.LevelError, io.Discard)); err != nil {
		t.Fatalf("serveUntilContextDone returned error for shutdown during validation: %v", err)
	}
	if !server.stopped {
		t.Fatal("expected server to be stopped after shutdown during validation")
	}
}

func TestServeUntilContextDoneStopsServerOnCopilotAuthFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := &fakeServeLifecycleServer{}
	authenticator := &fakeServeStartupAuthenticator{
		getTokenFn: func(context.Context) (string, error) {
			return "", fmt.Errorf("device code flow timed out")
		},
	}

	err := serveUntilContextDone(ctx, server, authenticator, true, logger.NewWithWriter(logger.LevelError, io.Discard))
	if err == nil {
		t.Fatal("expected authentication failure")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("expected authentication failure error, got %v", err)
	}
	if !server.stopped {
		t.Fatal("expected server to be stopped after authentication failure")
	}
}

func assertNextServeEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("next serve event = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for serve event %q", want)
	}
}

type fakeServeLifecycleServer struct {
	startFn            func() error
	stopFn             func(context.Context) error
	validateFn         func(context.Context) error
	authPending        bool
	authPendingUpdates []bool
	started            bool
	stopped            bool
}

func (f *fakeServeLifecycleServer) Start() error {
	f.started = true
	if f.startFn != nil {
		return f.startFn()
	}
	return nil
}

func (f *fakeServeLifecycleServer) Stop(ctx context.Context) error {
	f.stopped = true
	if f.stopFn != nil {
		return f.stopFn(ctx)
	}
	return nil
}

func (f *fakeServeLifecycleServer) ValidateDynamicProviderModels(ctx context.Context) error {
	if f.validateFn != nil {
		return f.validateFn(ctx)
	}
	return nil
}

func (f *fakeServeLifecycleServer) SetStartupAuthenticationPending(pending bool) {
	f.authPending = pending
	f.authPendingUpdates = append(f.authPendingUpdates, pending)
}

type fakeServeStartupAuthenticator struct {
	getTokenFn func(context.Context) (string, error)
}

func (f *fakeServeStartupAuthenticator) GetToken(ctx context.Context) (string, error) {
	if f.getTokenFn != nil {
		return f.getTokenFn(ctx)
	}
	return "test-token", nil
}

func TestCommandFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want cliCommand
	}{
		{
			name: "no subcommand falls back to serve",
			args: []string{"vekil"},
			want: cliCommandServe,
		},
		{
			name: "login subcommand dispatches",
			args: []string{"vekil", "login"},
			want: cliCommandLogin,
		},
		{
			name: "logout subcommand dispatches",
			args: []string{"vekil", "logout"},
			want: cliCommandLogout,
		},
		{
			name: "config namespace dispatches without serving",
			args: []string{"vekil", "config"},
			want: cliCommandConfig,
		},
		{
			name: "config validate dispatches without serving",
			args: []string{"vekil", "config", "validate", "--providers-config", "/tmp/providers.yaml"},
			want: cliCommandConfig,
		},
		{
			name: "unknown subcommand falls back to serve",
			args: []string{"vekil", "serve"},
			want: cliCommandServe,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandFromArgs(tc.args); got != tc.want {
				t.Fatalf("commandFromArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRunConfigValidateSuccess(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var gotPath string
	calls := 0

	code := runConfigWithDeps([]string{"validate", "--providers-config", "/tmp/provider config.yaml"}, configValidateDeps{
		stdout: &stdout,
		stderr: &stderr,
		validateProvidersConfigFile: func(path string) error {
			calls++
			gotPath = path
			return nil
		},
	})

	if code != 0 {
		t.Fatalf("runConfigWithDeps() code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("ValidateProvidersConfigFile calls = %d, want 1", calls)
	}
	if gotPath != "/tmp/provider config.yaml" {
		t.Fatalf("ValidateProvidersConfigFile path = %q, want %q", gotPath, "/tmp/provider config.yaml")
	}
	if got := stdout.String(); got != "Providers config is valid: /tmp/provider config.yaml\n" {
		t.Fatalf("stdout = %q, want success message", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunConfigValidateFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	validationErr := errors.New("model_routes[1].targets[0].provider: unknown provider")

	code := runConfigWithDeps([]string{"validate", "--providers-config=/tmp/providers.yaml"}, configValidateDeps{
		stdout: &stdout,
		stderr: &stderr,
		validateProvidersConfigFile: func(path string) error {
			if path != "/tmp/providers.yaml" {
				t.Fatalf("ValidateProvidersConfigFile path = %q, want /tmp/providers.yaml", path)
			}
			return validationErr
		},
	})

	if code != 1 {
		t.Fatalf("runConfigWithDeps() code = %d, want 1", code)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	want := "error: providers config validation failed: " + validationErr.Error() + "\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRunConfigValidateUsageErrorsDoNotValidate(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantText string
	}{
		{
			name:     "missing providers config",
			args:     []string{"validate"},
			wantText: "--providers-config PATH is required",
		},
		{
			name:     "unknown flag",
			args:     []string{"validate", "--unknown"},
			wantText: "flag provided but not defined: -unknown",
		},
		{
			name:     "unexpected positional argument",
			args:     []string{"validate", "--providers-config", "/tmp/providers.yaml", "extra"},
			wantText: `unexpected argument "extra"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			validated := false

			code := runConfigWithDeps(tc.args, configValidateDeps{
				stdout: &stdout,
				stderr: &stderr,
				validateProvidersConfigFile: func(string) error {
					validated = true
					return nil
				},
			})

			if code != 2 {
				t.Fatalf("runConfigWithDeps() code = %d, want 2", code)
			}
			if validated {
				t.Fatal("usage error called ValidateProvidersConfigFile")
			}
			if got := stdout.String(); got != "" {
				t.Fatalf("stdout = %q, want empty", got)
			}
			output := stderr.String()
			for _, want := range []string{tc.wantText, "Usage: vekil config validate --providers-config PATH"} {
				if !strings.Contains(output, want) {
					t.Fatalf("stderr missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestRunConfigHelpDoesNotValidate(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "config short help", args: []string{"-h"}},
		{name: "config long help", args: []string{"--help"}},
		{name: "config help command", args: []string{"help"}},
		{name: "validate short help", args: []string{"validate", "-h"}},
		{name: "validate long help", args: []string{"validate", "--help"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			validated := false

			code := runConfigWithDeps(tc.args, configValidateDeps{
				stdout: &stdout,
				stderr: &stderr,
				validateProvidersConfigFile: func(string) error {
					validated = true
					return nil
				},
			})

			if code != 0 {
				t.Fatalf("runConfigWithDeps(%v) code = %d, want 0", tc.args, code)
			}
			if validated {
				t.Fatal("help called ValidateProvidersConfigFile")
			}
			if got := stdout.String(); got != "" {
				t.Fatalf("stdout = %q, want empty", got)
			}
			output := stderr.String()
			for _, want := range []string{"Usage: vekil config validate --providers-config PATH", "validate"} {
				if !strings.Contains(output, want) {
					t.Fatalf("help output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestRunConfigRejectsMissingOrUnknownCommand(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantText string
	}{
		{name: "missing command", args: nil, wantText: "config command is required"},
		{name: "unknown command", args: []string{"check"}, wantText: `unknown config command "check"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			validated := false
			code := runConfigWithDeps(tc.args, configValidateDeps{
				stderr: &stderr,
				validateProvidersConfigFile: func(string) error {
					validated = true
					return nil
				},
			})

			if code != 2 {
				t.Fatalf("runConfigWithDeps(%v) code = %d, want 2", tc.args, code)
			}
			if validated {
				t.Fatal("invalid config command called ValidateProvidersConfigFile")
			}
			output := stderr.String()
			for _, want := range []string{tc.wantText, "Usage: vekil config validate --providers-config PATH"} {
				if !strings.Contains(output, want) {
					t.Fatalf("stderr missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestRunLoginHelpIncludesAuthFlags(t *testing.T) {
	for _, helpArg := range []string{"-h", "--help"} {
		t.Run(helpArg, func(t *testing.T) {
			var stderr bytes.Buffer
			constructed := false

			code := runLoginWithDeps([]string{helpArg}, loginDeps{
				stderr: &stderr,
				newAuthenticator: func(string) (loginAuthenticator, error) {
					constructed = true
					return &fakeLoginAuthenticator{}, nil
				},
			})

			if code != 0 {
				t.Fatalf("runLoginWithDeps(%q) code = %d, want 0", helpArg, code)
			}
			if constructed {
				t.Fatalf("help constructed an authenticator")
			}

			output := stderr.String()
			for _, want := range []string{"github-cli", "gh", "force"} {
				if !strings.Contains(output, want) {
					t.Fatalf("help output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestRunLoginRejectsGitHubCLIWithForceBeforeAuthConstruction(t *testing.T) {
	var stderr bytes.Buffer
	constructed := false

	code := runLoginWithDeps([]string{"--github-cli", "--force"}, loginDeps{
		stderr: &stderr,
		newAuthenticator: func(string) (loginAuthenticator, error) {
			constructed = true
			return &fakeLoginAuthenticator{}, nil
		},
	})

	if code != 2 {
		t.Fatalf("runLoginWithDeps() code = %d, want 2", code)
	}
	if constructed {
		t.Fatalf("conflicting login flags constructed an authenticator")
	}
	if got := stderr.String(); !strings.Contains(got, "--github-cli/--gh cannot be used with --force") {
		t.Fatalf("stderr missing conflict error, got %q", got)
	}
}

func TestRunLoginGHAliasUsesGitHubCLI(t *testing.T) {
	var stderr bytes.Buffer
	fake := &fakeLoginAuthenticator{}
	var gotTokenDir string

	code := runLoginWithDeps([]string{"--gh", "--token-dir", "/tmp/vekil-test-tokens"}, loginDeps{
		stderr: &stderr,
		newAuthenticator: func(tokenDir string) (loginAuthenticator, error) {
			gotTokenDir = tokenDir
			return fake, nil
		},
	})

	if code != 0 {
		t.Fatalf("runLoginWithDeps() code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if gotTokenDir != "/tmp/vekil-test-tokens" {
		t.Fatalf("newAuthenticator tokenDir = %q, want /tmp/vekil-test-tokens", gotTokenDir)
	}
	if fake.signInWithGitHubCLICalls != 1 {
		t.Fatalf("SignInWithGitHubCLI calls = %d, want 1", fake.signInWithGitHubCLICalls)
	}
	if fake.refreshCalls != 0 || fake.requestDeviceCodeCalls != 0 || fake.pollForAuthorizationCalls != 0 {
		t.Fatalf("unexpected auth flow calls: refresh=%d request=%d poll=%d", fake.refreshCalls, fake.requestDeviceCodeCalls, fake.pollForAuthorizationCalls)
	}
	if got := stderr.String(); !strings.Contains(got, "Login successful.") {
		t.Fatalf("stderr missing success message, got %q", got)
	}
}

func TestRunLoginForceSkipsRefreshAndStartsDeviceFlow(t *testing.T) {
	var stderr bytes.Buffer
	fake := &fakeLoginAuthenticator{
		deviceCodeResponse: &auth.DeviceCodeResponse{
			DeviceCode:      "device-code",
			UserCode:        "ABCD-EFGH",
			VerificationURI: "https://github.com/login/device",
			Interval:        1,
		},
	}
	var openedURL string

	code := runLoginWithDeps([]string{"--force"}, loginDeps{
		stderr: &stderr,
		newAuthenticator: func(string) (loginAuthenticator, error) {
			return fake, nil
		},
		openURL: func(url string) error {
			openedURL = url
			return nil
		},
	})

	if code != 0 {
		t.Fatalf("runLoginWithDeps() code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("RefreshTokenNonInteractive calls = %d, want 0", fake.refreshCalls)
	}
	if fake.requestDeviceCodeCalls != 1 {
		t.Fatalf("RequestDeviceCode calls = %d, want 1", fake.requestDeviceCodeCalls)
	}
	if fake.pollForAuthorizationCalls != 1 {
		t.Fatalf("PollForAuthorization calls = %d, want 1", fake.pollForAuthorizationCalls)
	}
	if fake.polledDeviceCode != fake.deviceCodeResponse {
		t.Fatalf("PollForAuthorization received %#v, want %#v", fake.polledDeviceCode, fake.deviceCodeResponse)
	}
	if openedURL != fake.deviceCodeResponse.VerificationURI {
		t.Fatalf("opened URL = %q, want %q", openedURL, fake.deviceCodeResponse.VerificationURI)
	}
	output := stderr.String()
	for _, want := range []string{"Opening browser to https://github.com/login/device", "Enter code: ABCD-EFGH", "Login successful."} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr missing %q, got %q", want, output)
		}
	}
}

type fakeLoginAuthenticator struct {
	signInWithGitHubCLICalls int
	signInWithGitHubCLIErr   error

	refreshCalls int
	refreshToken string
	refreshErr   error

	requestDeviceCodeCalls int
	deviceCodeResponse     *auth.DeviceCodeResponse
	requestDeviceCodeErr   error

	pollForAuthorizationCalls int
	polledDeviceCode          *auth.DeviceCodeResponse
	pollForAuthorizationErr   error
}

func (f *fakeLoginAuthenticator) SignInWithGitHubCLI(context.Context) error {
	f.signInWithGitHubCLICalls++
	return f.signInWithGitHubCLIErr
}

func (f *fakeLoginAuthenticator) RefreshTokenNonInteractive(context.Context) (string, error) {
	f.refreshCalls++
	return f.refreshToken, f.refreshErr
}

func (f *fakeLoginAuthenticator) RequestDeviceCode(context.Context) (*auth.DeviceCodeResponse, error) {
	f.requestDeviceCodeCalls++
	if f.deviceCodeResponse == nil {
		f.deviceCodeResponse = &auth.DeviceCodeResponse{
			DeviceCode:      "device-code",
			UserCode:        "USER-CODE",
			VerificationURI: "https://github.com/login/device",
			Interval:        1,
		}
	}
	return f.deviceCodeResponse, f.requestDeviceCodeErr
}

func (f *fakeLoginAuthenticator) PollForAuthorization(_ context.Context, dcResp *auth.DeviceCodeResponse) error {
	f.pollForAuthorizationCalls++
	f.polledDeviceCode = dcResp
	return f.pollForAuthorizationErr
}
