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
	restore := setSuppressEnvWarnings(false)
	defer restore()

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

func TestStripGlobalQuietFlags(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantQuiet    bool
		wantFiltered []string
	}{
		{
			name:         "long form true is stripped",
			args:         []string{"vekil", "--quiet=true", "login", "--gh"},
			wantQuiet:    true,
			wantFiltered: []string{"vekil", "login", "--gh"},
		},
		{
			name:         "long form false is stripped",
			args:         []string{"vekil", "--quiet=false", "login"},
			wantQuiet:    false,
			wantFiltered: []string{"vekil", "login"},
		},
		{
			name:         "short form is stripped",
			args:         []string{"vekil", "-q", "logout"},
			wantQuiet:    true,
			wantFiltered: []string{"vekil", "logout"},
		},
		{
			name:         "quiet-looking args after subcommand are preserved",
			args:         []string{"vekil", "login", "--quiet=false", "-q"},
			wantQuiet:    false,
			wantFiltered: []string{"vekil", "login", "--quiet=false", "-q"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filtered, quiet, err := stripGlobalQuietFlags(tc.args)
			if err != nil {
				t.Fatalf("stripGlobalQuietFlags() error = %v", err)
			}
			if quiet != tc.wantQuiet {
				t.Fatalf("quiet = %v, want %v", quiet, tc.wantQuiet)
			}
			if got, want := strings.Join(filtered, " "), strings.Join(tc.wantFiltered, " "); got != want {
				t.Fatalf("filtered args = %q, want %q", got, want)
			}
		})
	}
}

func TestRunCLIWithQuietSuppressesLoginInfoButKeepsErrors(t *testing.T) {
	t.Run("suppresses success output", func(t *testing.T) {
		var stderr bytes.Buffer
		fake := &fakeLoginAuthenticator{}
		oldStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() error = %v", err)
		}
		os.Stdout = w
		t.Cleanup(func() {
			os.Stdout = oldStdout
			_ = r.Close()
		})

		code := runCLIWithDeps([]string{"vekil", "--quiet", "login", "--gh"}, cliDeps{
			stderr: &stderr,
			runServe: func([]string, bool) int {
				t.Fatal("unexpected serve dispatch")
				return 1
			},
			runLogout: func([]string, bool) int {
				t.Fatal("unexpected logout dispatch")
				return 1
			},
			runLogin: func(args []string, quiet bool) int {
				return runLoginWithDeps(args, loginDeps{
					stderr: &stderr,
					newAuthenticator: func(string) (loginAuthenticator, error) {
						return fake, nil
					},
				}, quiet)
			},
		})
		if code != 0 {
			t.Fatalf("runCLIWithDeps() code = %d, want 0", code)
		}
		_ = w.Close()
		var stdout bytes.Buffer
		_, _ = stdout.ReadFrom(r)
		if got := stdout.String(); got != "" {
			t.Fatalf("stdout = %q, want empty", got)
		}
		if got := stderr.String(); got != "" {
			t.Fatalf("stderr = %q, want empty", got)
		}
	})

	t.Run("keeps error output", func(t *testing.T) {
		var stderr bytes.Buffer
		fake := &fakeLoginAuthenticator{
			signInWithGitHubCLIErr: fmt.Errorf("boom"),
		}

		code := runCLIWithDeps([]string{"vekil", "--quiet", "login", "--gh"}, cliDeps{
			stderr: &stderr,
			runServe: func([]string, bool) int {
				t.Fatal("unexpected serve dispatch")
				return 1
			},
			runLogout: func([]string, bool) int {
				t.Fatal("unexpected logout dispatch")
				return 1
			},
			runLogin: func(args []string, quiet bool) int {
				return runLoginWithDeps(args, loginDeps{
					stderr: &stderr,
					newAuthenticator: func(string) (loginAuthenticator, error) {
						return fake, nil
					},
				}, quiet)
			},
		})
		if code != 1 {
			t.Fatalf("runCLIWithDeps() code = %d, want 1", code)
		}
		if got := stderr.String(); !strings.Contains(got, "error signing in with GitHub CLI: boom") {
			t.Fatalf("stderr missing error, got %q", got)
		}
	})
}

func TestRunCLIWithQuietAffectsServeAndLogoutPaths(t *testing.T) {
	t.Run("short quiet form reaches serve", func(t *testing.T) {
		code := runCLIWithDeps([]string{"vekil", "-q", "--port", "1444"}, cliDeps{
			stderr: io.Discard,
			runServe: func(args []string, quiet bool) int {
				if !quiet {
					t.Fatal("quiet = false, want true")
				}
				if got, want := strings.Join(args, " "), "--port 1444"; got != want {
					t.Fatalf("serve args = %q, want %q", got, want)
				}
				return 0
			},
			runLogin: func([]string, bool) int {
				t.Fatal("unexpected login dispatch")
				return 1
			},
			runLogout: func([]string, bool) int {
				t.Fatal("unexpected logout dispatch")
				return 1
			},
		})
		if code != 0 {
			t.Fatalf("runCLIWithDeps() code = %d, want 0", code)
		}
	})

	t.Run("quiet false reaches logout unchanged", func(t *testing.T) {
		code := runCLIWithDeps([]string{"vekil", "--quiet=false", "logout"}, cliDeps{
			stderr: io.Discard,
			runServe: func([]string, bool) int {
				t.Fatal("unexpected serve dispatch")
				return 1
			},
			runLogin: func([]string, bool) int {
				t.Fatal("unexpected login dispatch")
				return 1
			},
			runLogout: func(args []string, quiet bool) int {
				if quiet {
					t.Fatal("quiet = true, want false")
				}
				if len(args) != 0 {
					t.Fatalf("logout args = %v, want empty", args)
				}
				return 0
			},
		})
		if code != 0 {
			t.Fatalf("runCLIWithDeps() code = %d, want 0", code)
		}
	})
}

func TestRunCLIWithQuietSuppressesEnvWarnings(t *testing.T) {
	const envKey = "TEST_CLI_QUIET_ENV_BOOL"
	t.Setenv(envKey, "not-a-bool")

	t.Run("quiet suppresses warnings", func(t *testing.T) {
		old := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() error = %v", err)
		}
		os.Stderr = w
		t.Cleanup(func() {
			os.Stderr = old
			_ = r.Close()
		})

		code := runCLIWithDeps([]string{"vekil", "--quiet"}, cliDeps{
			stderr: io.Discard,
			runServe: func([]string, bool) int {
				_ = getEnvBool(envKey, false)
				return 0
			},
			runLogin:  func([]string, bool) int { return 0 },
			runLogout: func([]string, bool) int { return 0 },
		})
		if code != 0 {
			t.Fatalf("runCLIWithDeps() code = %d, want 0", code)
		}

		_ = w.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		if got := buf.String(); got != "" {
			t.Fatalf("stderr = %q, want empty", got)
		}
	})

	t.Run("non-quiet keeps warnings", func(t *testing.T) {
		old := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() error = %v", err)
		}
		os.Stderr = w
		t.Cleanup(func() {
			os.Stderr = old
			_ = r.Close()
		})

		code := runCLIWithDeps([]string{"vekil"}, cliDeps{
			stderr: io.Discard,
			runServe: func([]string, bool) int {
				_ = getEnvBool(envKey, false)
				return 0
			},
			runLogin:  func([]string, bool) int { return 0 },
			runLogout: func([]string, bool) int { return 0 },
		})
		if code != 0 {
			t.Fatalf("runCLIWithDeps() code = %d, want 0", code)
		}

		_ = w.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		if got := buf.String(); !strings.Contains(got, "warning: ignoring invalid "+envKey) {
			t.Fatalf("stderr missing warning, got %q", got)
		}
	})
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
