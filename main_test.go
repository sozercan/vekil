package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
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

func TestRegisterServeFlagsQuietSuppressesEnvWarnings(t *testing.T) {
	const envKey = "RESPONSES_WS_ENABLED"
	t.Setenv(envKey, "not-a-bool")

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_ = registerServeFlags(fs, true)

	_ = w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if got := buf.String(); got != "" {
		t.Fatalf("expected no stderr output in quiet mode, got %q", got)
	}
}

func parseServeFlagsForTest(t *testing.T, args ...string) serveFlags {
	t.Helper()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serve := registerServeFlags(fs, false)
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
			}, false)

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
	}, false)

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
	}, false)

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
	}, false)

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

func TestRunLoginQuietSuppressesNonErrorOutput(t *testing.T) {
	fake := &fakeLoginAuthenticator{}

	var normalStderr bytes.Buffer
	if code := runLoginWithDeps(nil, loginDeps{
		stderr: &normalStderr,
		newAuthenticator: func(string) (loginAuthenticator, error) {
			return fake, nil
		},
	}, false); code != 0 {
		t.Fatalf("runLoginWithDeps() code = %d, want 0", code)
	}
	if got := normalStderr.String(); !strings.Contains(got, "Already logged in.") {
		t.Fatalf("expected normal output to contain status message, got %q", got)
	}

	var quietStderr bytes.Buffer
	if code := runLoginWithDeps([]string{"--quiet"}, loginDeps{
		stderr: &quietStderr,
		newAuthenticator: func(string) (loginAuthenticator, error) {
			return fake, nil
		},
	}, false); code != 0 {
		t.Fatalf("runLoginWithDeps(--quiet) code = %d, want 0", code)
	}
	if got := quietStderr.String(); got != "" {
		t.Fatalf("expected quiet output to be empty, got %q", got)
	}
}

func TestRunLoginQuietStillPrintsErrors(t *testing.T) {
	var stderr bytes.Buffer

	code := runLoginWithDeps([]string{"--quiet"}, loginDeps{
		stderr: &stderr,
		newAuthenticator: func(string) (loginAuthenticator, error) {
			return nil, fmt.Errorf("boom")
		},
	}, false)

	if code != 1 {
		t.Fatalf("runLoginWithDeps(--quiet) code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, "error: boom") {
		t.Fatalf("stderr missing error output, got %q", got)
	}
}

func TestRunLogoutQuietSuppressesSuccessOutput(t *testing.T) {
	var normalStderr bytes.Buffer
	if code := runLogoutWithDeps(nil, logoutDeps{
		stderr: &normalStderr,
		newAuthenticator: func(string) (logoutAuthenticator, error) {
			return &fakeLogoutAuthenticator{}, nil
		},
	}, false); code != 0 {
		t.Fatalf("runLogoutWithDeps() code = %d, want 0", code)
	}
	if got := normalStderr.String(); !strings.Contains(got, "Logged out.") {
		t.Fatalf("expected normal output to contain logout message, got %q", got)
	}

	var quietStderr bytes.Buffer
	if code := runLogoutWithDeps([]string{"--quiet"}, logoutDeps{
		stderr: &quietStderr,
		newAuthenticator: func(string) (logoutAuthenticator, error) {
			return &fakeLogoutAuthenticator{}, nil
		},
	}, false); code != 0 {
		t.Fatalf("runLogoutWithDeps(--quiet) code = %d, want 0", code)
	}
	if got := quietStderr.String(); got != "" {
		t.Fatalf("expected quiet output to be empty, got %q", got)
	}
}

func TestRunLogoutQuietStillPrintsErrors(t *testing.T) {
	var stderr bytes.Buffer

	code := runLogoutWithDeps([]string{"--quiet"}, logoutDeps{
		stderr: &stderr,
		newAuthenticator: func(string) (logoutAuthenticator, error) {
			return &fakeLogoutAuthenticator{signOutErr: fmt.Errorf("logout failed")}, nil
		},
	}, false)

	if code != 1 {
		t.Fatalf("runLogoutWithDeps(--quiet) code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, "error: logout failed") {
		t.Fatalf("stderr missing error output, got %q", got)
	}
}

func TestEffectiveServeLogLevelQuietSuppression(t *testing.T) {
	tests := []struct {
		name  string
		level string
		quiet bool
		want  string
	}{
		{name: "info upgraded to error when quiet", level: "info", quiet: true, want: "error"},
		{name: "debug upgraded to error when quiet", level: "debug", quiet: true, want: "error"},
		{name: "error remains error when quiet", level: "error", quiet: true, want: "error"},
		{name: "info stays info when not quiet", level: "info", quiet: false, want: "info"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveServeLogLevel(logger.ParseLevel(tc.level), tc.quiet)
			if got != logger.ParseLevel(tc.want) {
				t.Fatalf("effectiveServeLogLevel(%q, quiet=%v) = %v, want %v", tc.level, tc.quiet, got, logger.ParseLevel(tc.want))
			}
		})
	}
}

func TestExtractGlobalQuietFlag(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantQuiet    bool
		wantArgs     []string
		wantErrMatch string
	}{
		{
			name:      "quiet before subcommand",
			args:      []string{"vekil", "--quiet", "login", "--force"},
			wantQuiet: true,
			wantArgs:  []string{"vekil", "login", "--force"},
		},
		{
			name:      "short quiet after subcommand",
			args:      []string{"vekil", "login", "-q"},
			wantQuiet: true,
			wantArgs:  []string{"vekil", "login"},
		},
		{
			name:      "explicit false keeps output",
			args:      []string{"vekil", "--quiet=false", "login"},
			wantQuiet: false,
			wantArgs:  []string{"vekil", "login"},
		},
		{
			name:         "invalid quiet value errors",
			args:         []string{"vekil", "--quiet=maybe", "login"},
			wantErrMatch: `invalid value for --quiet: "maybe"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotQuiet, gotArgs, err := extractGlobalQuietFlag(tc.args)
			if tc.wantErrMatch != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrMatch) {
					t.Fatalf("extractGlobalQuietFlag(%v) err = %v, want match %q", tc.args, err, tc.wantErrMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractGlobalQuietFlag(%v) unexpected err: %v", tc.args, err)
			}
			if gotQuiet != tc.wantQuiet {
				t.Fatalf("extractGlobalQuietFlag(%v) quiet = %v, want %v", tc.args, gotQuiet, tc.wantQuiet)
			}
			if strings.Join(gotArgs, "\x00") != strings.Join(tc.wantArgs, "\x00") {
				t.Fatalf("extractGlobalQuietFlag(%v) args = %v, want %v", tc.args, gotArgs, tc.wantArgs)
			}
		})
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

type fakeLogoutAuthenticator struct {
	signOutErr error
}

func (f *fakeLogoutAuthenticator) SignOut() error {
	return f.signOutErr
}
