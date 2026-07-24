package launch

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeAdapterPrepare(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	adapter := ClaudeAdapter{}
	prepared, err := adapter.Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210/",
		Model: ModelInfo{
			ID:                 "claude-public",
			Name:               "Claude Public",
			SupportedEndpoints: []string{"/chat/completions"},
		},
		Binary:        binary,
		ForwardedArgs: []string{"--print", "hello"},
		LocalToken:    "test-token-placeholder",
		SensitiveEnv:  []string{"MY_PROVIDER_TOKEN"},
		NoProxy:       "internal.example,127.0.0.1,localhost",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Path != binary {
		t.Fatalf("Path = %q, want %q", prepared.Path, binary)
	}
	if len(prepared.Args) != 4 || prepared.Args[0] != "--settings" ||
		!reflect.DeepEqual(prepared.Args[2:], []string{"--print", "hello"}) {
		t.Fatalf("Args = %#v", prepared.Args)
	}
	if strings.Contains(prepared.Args[1], "test-token-placeholder") {
		t.Fatal("managed settings exposed the local proxy token in argv")
	}
	if prepared.Cleanup == nil {
		t.Fatal("managed settings did not register cleanup")
	}
	settingsPath := prepared.Args[1]
	settingsBody, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read managed settings file: %v", err)
	}
	var managedSettings struct {
		Env                  map[string]string `json:"env"`
		DisableRemoteControl bool              `json:"disableRemoteControl"`
	}
	if err := json.Unmarshal(settingsBody, &managedSettings); err != nil {
		t.Fatalf("decode managed settings: %v", err)
	}
	for _, key := range []string{"ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_FOUNDRY_AUTH_TOKEN", "ANTHROPIC_AWS_API_KEY", "MY_PROVIDER_TOKEN"} {
		if value, ok := managedSettings.Env[key]; !ok || value != "" {
			t.Fatalf("managed settings env[%q] = %q, present=%v", key, value, ok)
		}
	}
	if got := managedSettings.Env["NO_PROXY"]; got != "internal.example,127.0.0.1,localhost" {
		t.Fatalf("managed NO_PROXY = %q", got)
	}
	if !managedSettings.DisableRemoteControl {
		t.Fatal("managed settings did not disable remote control")
	}
	for _, key := range []string{"CLAUDE_CODE_DISABLE_AGENT_VIEW", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "CLAUDE_CODE_DISABLE_CRON"} {
		if got := managedSettings.Env[key]; got != "1" {
			t.Fatalf("managed settings env[%q] = %q, want 1", key, got)
		}
	}
	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                        "http://127.0.0.1:43210",
		"ANTHROPIC_AUTH_TOKEN":                      "test-token-placeholder",
		"ANTHROPIC_API_KEY":                         "",
		"ANTHROPIC_MODEL":                           "claude-public",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":             "claude-public",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":            "claude-public",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":              "claude-public",
		"ANTHROPIC_DEFAULT_FABLE_MODEL":             "claude-public",
		"ANTHROPIC_CUSTOM_MODEL_OPTION":             "claude-public",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":        "Claude Public",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Routed through Vekil",
		"CLAUDE_CODE_SUBAGENT_MODEL":                "claude-public",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":      "vekil",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":          "1",
	} {
		if got := managedSettings.Env[key]; got != want {
			t.Errorf("managed settings env[%q] = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                        "http://127.0.0.1:43210",
		"ANTHROPIC_AUTH_TOKEN":                      "test-token-placeholder",
		"ANTHROPIC_API_KEY":                         "",
		"ANTHROPIC_MODEL":                           "claude-public",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":             "claude-public",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":            "claude-public",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":              "claude-public",
		"ANTHROPIC_DEFAULT_FABLE_MODEL":             "claude-public",
		"ANTHROPIC_CUSTOM_MODEL_OPTION":             "claude-public",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME":        "Claude Public",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Routed through Vekil",
		"CLAUDE_CODE_SUBAGENT_MODEL":                "claude-public",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST":      "vekil",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":          "1",
	} {
		if got := prepared.EnvSet[key]; got != want {
			t.Errorf("EnvSet[%q] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY",
		"MY_PROVIDER_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_CUSTOM_HEADERS",
		"OPENAI_API_KEY",
		"CODEX_API_KEY",
		"COPILOT_GITHUB_TOKEN",
		"GH_TOKEN",
		"GITHUB_TOKEN",
	} {
		if !containsString(prepared.EnvUnset, key) {
			t.Fatalf("expected %s to be removed", key)
		}
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatalf("cleanup managed settings: %v", err)
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("managed settings file still exists after cleanup: %v", err)
	}
}

func TestClaudeAdapterPrepareWithoutModelUsesClaudeDefault(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210/",
		Model: ModelInfo{
			ID:                 " \t ",
			OwnedBy:            PolicyModelOwner,
			SupportedEndpoints: []string{"/embeddings"},
		},
		Binary:        binary,
		ForwardedArgs: []string{"--print", "hello"},
		LocalToken:    "test-token-placeholder",
		DryRun:        true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Cleanup == nil {
		t.Fatal("managed settings did not register cleanup")
	}
	t.Cleanup(func() { _ = prepared.Cleanup() })
	if len(prepared.Unresolved) != 0 {
		t.Fatalf("Unresolved = %#v, want no model compatibility checks", prepared.Unresolved)
	}
	if got := prepared.Args[len(prepared.Args)-2:]; !reflect.DeepEqual(got, []string{"--print", "hello"}) {
		t.Fatalf("forwarded args changed: %#v", prepared.Args)
	}

	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                   "http://127.0.0.1:43210",
		"ANTHROPIC_AUTH_TOKEN":                 "test-token-placeholder",
		"ANTHROPIC_API_KEY":                    "",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "vekil",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":     "1",
	} {
		if got := prepared.EnvSet[key]; got != want {
			t.Errorf("EnvSet[%q] = %q, want %q", key, got, want)
		}
	}

	modelKeys := []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_FABLE_MODEL",
		"ANTHROPIC_CUSTOM_MODEL_OPTION",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
		"CLAUDE_CODE_SUBAGENT_MODEL",
	}
	for _, key := range modelKeys {
		if _, ok := prepared.EnvSet[key]; ok {
			t.Errorf("EnvSet unexpectedly pins %q", key)
		}
		if containsString(prepared.EnvUnset, key) {
			t.Errorf("EnvUnset unexpectedly removes configured default %q", key)
		}
	}

	settingsBody, err := os.ReadFile(prepared.Args[1])
	if err != nil {
		t.Fatalf("read managed settings file: %v", err)
	}
	var managedSettings struct {
		Env                  map[string]string `json:"env"`
		DisableRemoteControl bool              `json:"disableRemoteControl"`
	}
	if err := json.Unmarshal(settingsBody, &managedSettings); err != nil {
		t.Fatalf("decode managed settings: %v", err)
	}
	if !managedSettings.DisableRemoteControl {
		t.Fatal("managed settings did not disable remote control")
	}
	for _, key := range modelKeys {
		if _, ok := managedSettings.Env[key]; ok {
			t.Errorf("managed settings unexpectedly pin %q", key)
		}
	}
	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":                   "http://127.0.0.1:43210",
		"ANTHROPIC_AUTH_TOKEN":                 "test-token-placeholder",
		"ANTHROPIC_API_KEY":                    "",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "vekil",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":     "1",
		"CLAUDE_CODE_DISABLE_AGENT_VIEW":       "1",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1",
		"CLAUDE_CODE_DISABLE_CRON":             "1",
	} {
		if got := managedSettings.Env[key]; got != want {
			t.Errorf("managed settings env[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestClaudeAdapterSanitizesVersionProbeEnvironment(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	capturePath := t.TempDir() + "/version-env.json"
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210",
		Model: ModelInfo{
			ID:                 "claude-public",
			SupportedEndpoints: []string{"/v1/messages"},
		},
		Binary:     binary,
		LocalToken: "local-session-token",
		SensitiveEnv: []string{
			"MY_PROVIDER_TOKEN",
		},
		Environment: []string{
			"LAUNCH_VERSION_ENV_CAPTURE=" + capturePath,
			"ANTHROPIC_API_KEY=upstream-api-key",
			"ANTHROPIC_AUTH_TOKEN=upstream-auth-token",
			"ANTHROPIC_BASE_URL=https://upstream.example",
			"ANTHROPIC_CUSTOM_HEADERS=X-Secret: upstream",
			"MY_PROVIDER_TOKEN=upstream-provider-token",
		},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	defer func() {
		if prepared.Cleanup != nil {
			_ = prepared.Cleanup()
		}
	}()
	body, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read version probe environment: %v", err)
	}
	var captured map[string]string
	if err := json.Unmarshal(body, &captured); err != nil {
		t.Fatalf("decode version probe environment: %v", err)
	}
	want := map[string]string{
		"api_key":        "",
		"auth_token":     "local-session-token",
		"base_url":       "http://127.0.0.1:43210",
		"custom_headers": "",
		"provider_token": "",
	}
	if !reflect.DeepEqual(captured, want) {
		t.Fatalf("version probe environment = %#v, want %#v", captured, want)
	}
}

func TestClaudeAdapterAcceptsResponsesBackedModel(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210",
		Model: ModelInfo{
			ID:                 "responses-only",
			SupportedEndpoints: []string{"/responses"},
		},
		Binary:     binary,
		LocalToken: "test-token-placeholder",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Cleanup != nil {
		defer func() { _ = prepared.Cleanup() }()
	}
}

func TestClaudeAdapterRejectsIncompatibleModel(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	for _, dryRun := range []bool{false, true} {
		_, err = (ClaudeAdapter{}).Prepare(PrepareInput{
			BaseURL: "http://127.0.0.1:43210",
			Model: ModelInfo{
				ID:                 "embeddings-only",
				SupportedEndpoints: []string{"/embeddings"},
			},
			Binary:     binary,
			LocalToken: "test-token-placeholder",
			DryRun:     dryRun,
		})
		if err == nil || !strings.Contains(err.Error(), "not Claude-compatible") {
			t.Fatalf("Prepare(dryRun=%v) error = %v, want compatibility error", dryRun, err)
		}
	}
}

func TestClaudeAdapterDryRunMarksUnknownEndpointCompatibilityUnresolved(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL:    "http://127.0.0.1:43210",
		Model:      ModelInfo{ID: "catalog-model"},
		Binary:     binary,
		LocalToken: "test-token-placeholder",
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Cleanup != nil {
		defer func() { _ = prepared.Cleanup() }()
	}
	if got := strings.Join(prepared.Unresolved, "\n"); !strings.Contains(got, "/v1/messages") {
		t.Fatalf("unresolved metadata = %q, want Claude endpoint disclosure", got)
	}
}

func TestClaudeAdapterAcceptsPolicyOwnedChatModel(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	var model ModelInfo
	if err := json.Unmarshal([]byte(`{"id":"policy-public","owned_by":"vekil-policy","supported_endpoints":["/chat/completions"]}`), &model); err != nil {
		t.Fatalf("decode model metadata: %v", err)
	}

	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL:    "http://127.0.0.1:43210",
		Model:      model,
		Binary:     binary,
		LocalToken: "test-token-placeholder",
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Cleanup != nil {
		t.Cleanup(func() { _ = prepared.Cleanup() })
	}
	if got := prepared.EnvSet["ANTHROPIC_MODEL"]; got != "policy-public" {
		t.Fatalf("ANTHROPIC_MODEL = %q, want policy-public", got)
	}
}

func TestClaudeAdapterRejectsMissingEndpointMetadata(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	_, err = (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210",
		Model:   ModelInfo{ID: "missing-endpoints"},
		Binary:  binary,
	})
	if err == nil || !strings.Contains(err.Error(), "not Claude-compatible") {
		t.Fatalf("Prepare() error = %v, want endpoint metadata error", err)
	}
}

func TestClaudeAdapterRejectsUnsupportedForwardedModes(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "settings", args: []string{"--settings", "{}"}, want: "settings-source overrides"},
		{name: "managed settings", args: []string{"--managed-settings", "{}"}, want: "settings-source overrides"},
		{name: "init only", args: []string{"--init-only"}, want: "initialization-only"},
		{name: "attached init only", args: []string{"--init-only=true"}, want: "initialization-only"},
		{name: "background", args: []string{"--background"}, want: "detached Claude sessions"},
		{name: "tmux", args: []string{"--tmux"}, want: "detached Claude sessions"},
		{name: "model", args: []string{"--model", "responses-only"}, want: "model or session overrides"},
		{name: "model after prompt", args: []string{"--print", "hello", "--model", "responses-only"}, want: "model or session overrides"},
		{name: "fallback model", args: []string{"--fallback-model", "other"}, want: "model or session overrides"},
		{name: "dangerous permission bypass", args: []string{"--dangerously-skip-permissions"}, want: "permission bypass"},
		{name: "enable dangerous permission bypass", args: []string{"--allow-dangerously-skip-permissions"}, want: "permission bypass"},
		{name: "accept edits permission mode", args: []string{"--permission-mode", "acceptEdits"}, want: "permission mode"},
		{name: "auto permission mode", args: []string{"--permission-mode=auto"}, want: "permission mode"},
		{name: "bypass permission mode", args: []string{"--permission-mode", "bypassPermissions"}, want: "permission mode"},
		{name: "dont ask permission mode", args: []string{"--permission-mode=dontAsk"}, want: "permission mode"},
		{name: "plan permission mode", args: []string{"--permission-mode", "plan"}, want: "permission mode"},
		{name: "resume", args: []string{"--resume"}, want: "model or session overrides"},
		{name: "attached short resume", args: []string{"-rsession-id"}, want: "model or session overrides"},
		{name: "session id", args: []string{"--session-id", "00000000-0000-0000-0000-000000000000"}, want: "model or session overrides"},
		{name: "attached session id", args: []string{"--session-id=00000000-0000-0000-0000-000000000000"}, want: "model or session overrides"},
		{name: "custom agent", args: []string{"--agent", "reviewer"}, want: "model or session overrides"},
		{name: "teleport", args: []string{"--teleport", "session"}, want: "model or session overrides"},
		{name: "subcommand after option", args: []string{"--verbose", "remote-control"}, want: "agent-management commands"},
		{name: "subcommand after max turns", args: []string{"--max-turns", "5", "agents"}, want: "agent-management commands"},
		{name: "subcommand after name", args: []string{"--name", "session", "agents"}, want: "agent-management commands"},
		{name: "subcommand after short name", args: []string{"-n", "session", "agents"}, want: "agent-management commands"},
		{name: "subcommand after permission prompt tool", args: []string{"--permission-prompt-tool", "mcp__permissions", "agents"}, want: "agent-management commands"},
		{name: "subcommand after system prompt file", args: []string{"--system-prompt-file", "prompt.txt", "agents"}, want: "agent-management commands"},
		{name: "subcommand after appended system prompt file", args: []string{"--append-system-prompt-file", "prompt.txt", "agents"}, want: "agent-management commands"},
		{name: "attach subcommand", args: []string{"attach", "session-id"}, want: "agent-management commands"},
		{name: "gateway subcommand", args: []string{"gateway"}, want: "agent-management commands"},
		{name: "auth subcommand", args: []string{"auth"}, want: "agent-management commands"},
		{name: "doctor subcommand", args: []string{"doctor"}, want: "agent-management commands"},
		{name: "install subcommand", args: []string{"install"}, want: "agent-management commands"},
		{name: "subcommand after delimiter", args: []string{"--", "remote-control"}, want: "agent-management commands"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (ClaudeAdapter{}).Prepare(PrepareInput{
				BaseURL:       "http://127.0.0.1:43210",
				Model:         ModelInfo{ID: "claude-public", SupportedEndpoints: []string{"/chat/completions"}},
				Binary:        binary,
				LocalToken:    "test-token-placeholder",
				ForwardedArgs: tc.args,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Prepare() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClaudeAdapterAllowsDefaultPermissionModes(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	for _, args := range [][]string{
		{"--permission-mode", "manual", "--print", "hello"},
		{"--permission-mode=manual", "--print", "hello"},
		{"--permission-mode", "default", "--print", "hello"},
		{"--permission-mode=default", "--print", "hello"},
	} {
		prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
			BaseURL:       "http://127.0.0.1:43210",
			Model:         ModelInfo{ID: "claude-public", SupportedEndpoints: []string{"/chat/completions"}},
			Binary:        binary,
			LocalToken:    "test-token-placeholder",
			ForwardedArgs: args,
		})
		if err != nil {
			t.Fatalf("Prepare(%#v) error = %v", args, err)
		}
		if prepared.Cleanup != nil {
			defer func(cleanup func() error) { _ = cleanup() }(prepared.Cleanup)
		}
		if got := prepared.Args[len(prepared.Args)-len(args):]; !reflect.DeepEqual(got, args) {
			t.Fatalf("forwarded args = %#v, want %#v", got, args)
		}
	}
}

func TestClaudeAdapterTreatsAgentsAsPrintPrompt(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL:       "http://127.0.0.1:43210",
		Model:         ModelInfo{ID: "claude-public", SupportedEndpoints: []string{"/chat/completions"}},
		Binary:        binary,
		LocalToken:    "test-token-placeholder",
		ForwardedArgs: []string{"--print", "agents"},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() { _ = prepared.Cleanup() })
}

func TestClaudeAdapterHonorsEndOfOptionsDelimiter(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL:       "http://127.0.0.1:43210",
		Model:         ModelInfo{ID: "claude-public", SupportedEndpoints: []string{"/chat/completions"}},
		Binary:        binary,
		LocalToken:    "test-token-placeholder",
		ForwardedArgs: []string{"--print", "--", "--model=literal", "agents"},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() { _ = prepared.Cleanup() })
	if got := prepared.Args[len(prepared.Args)-2:]; !reflect.DeepEqual(got, []string{"--model=literal", "agents"}) {
		t.Fatalf("literal args = %#v", got)
	}
}

func TestClaudeAdapterReportsMissingBinary(t *testing.T) {
	_, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210",
		Model: ModelInfo{
			ID:                 "claude-public",
			SupportedEndpoints: []string{"/chat/completions"},
		},
		Binary:     "/definitely/not/a/claude/binary",
		LocalToken: "test-token-placeholder",
	})
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Fatalf("Prepare() error = %v, want ErrBinaryNotFound", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCompareClaudeVersion(t *testing.T) {
	if got := compareVersion(2, 1, 82, 2, 1, 83); got >= 0 {
		t.Fatalf("old version comparison = %d", got)
	}
	if got := compareVersion(2, 1, 83, 2, 1, 83); got != 0 {
		t.Fatalf("minimum version comparison = %d", got)
	}
	if got := compareVersion(2, 2, 0, 2, 1, 83); got <= 0 {
		t.Fatalf("new version comparison = %d", got)
	}
}
