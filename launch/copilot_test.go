package launch

import (
	"os"
	"strings"
	"testing"
)

func TestCopilotAdapterPrepareResponses(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := (CopilotAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210/",
		Model: ModelInfo{
			ID:                 "public-route",
			SupportedEndpoints: []string{"/responses"},
			Capabilities: ModelCapabilities{
				Family: "gpt-5.4",
				Limits: ModelCapabilityLimits{
					MaxPromptTokens: 300000,
					MaxOutputTokens: 64000,
				},
			},
		},
		Binary:        binary,
		ForwardedArgs: []string{"--allow-all-tools", "-p", "hello", "-s"},
		LocalToken:    "test-token-placeholder",
		SensitiveEnv:  []string{"MY_PROVIDER_TOKEN"},
		DryRun:        true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for key, want := range map[string]string{
		"COPILOT_AUTO_UPDATE":                "false",
		"COPILOT_MODEL":                      "public-route",
		"COPILOT_OFFLINE":                    "true",
		"COPILOT_OTEL_ENABLED":               "false",
		"COPILOT_PROVIDER_API_KEY":           "",
		"COPILOT_PROVIDER_BASE_URL":          "http://127.0.0.1:43210/v1",
		"COPILOT_PROVIDER_BEARER_TOKEN":      "test-token-placeholder",
		"COPILOT_PROVIDER_MODEL_ID":          "gpt-5.4",
		"COPILOT_PROVIDER_TRANSPORT":         "http",
		"COPILOT_PROVIDER_TYPE":              "openai",
		"COPILOT_PROVIDER_WIRE_API":          "responses",
		"COPILOT_PROVIDER_WIRE_MODEL":        "public-route",
		"COPILOT_PROVIDER_MAX_PROMPT_TOKENS": "300000",
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS": "64000",
	} {
		if got := prepared.EnvSet[key]; got != want {
			t.Errorf("EnvSet[%q] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "MY_PROVIDER_TOKEN", "COPILOT_ALLOW_ALL", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		if !containsString(prepared.EnvUnset, key) {
			t.Fatalf("EnvUnset missing %q: %#v", key, prepared.EnvUnset)
		}
	}
	for _, want := range []string{"--no-auto-update", "--no-remote", "--no-remote-export", "--disable-builtin-mcps"} {
		if !containsString(prepared.Args, want) {
			t.Fatalf("Copilot args missing %q: %#v", want, prepared.Args)
		}
	}
	joined := strings.Join(prepared.Args, "\n")
	if strings.Contains(joined, "test-token-placeholder") {
		t.Fatal("Copilot argv exposed the local proxy token")
	}
	if !strings.Contains(joined, "--secret-env-vars=") || !strings.Contains(joined, "COPILOT_PROVIDER_BEARER_TOKEN") || !strings.Contains(joined, "MY_PROVIDER_TOKEN") {
		t.Fatalf("Copilot secret environment scrub is incomplete: %#v", prepared.Args)
	}
	if got := prepared.Args[len(prepared.Args)-4:]; strings.Join(got, "|") != "--allow-all-tools|-p|hello|-s" {
		t.Fatalf("forwarded args changed: %#v", prepared.Args)
	}
}

func TestCopilotAdapterSelectsChatCompletions(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := (CopilotAdapter{}).Prepare(PrepareInput{
		BaseURL:    "http://127.0.0.1:43210",
		Model:      ModelInfo{ID: "chat-model", SupportedEndpoints: []string{"/chat/completions"}},
		Binary:     binary,
		LocalToken: "token",
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got := prepared.EnvSet["COPILOT_PROVIDER_WIRE_API"]; got != "completions" {
		t.Fatalf("wire API = %q, want completions", got)
	}
}

func TestCopilotAdapterPrefersResponses(t *testing.T) {
	got, err := copilotWireAPI(ModelInfo{
		ID:                 "both",
		SupportedEndpoints: []string{"/chat/completions", "/responses"},
	}, false)
	if err != nil || got != "responses" {
		t.Fatalf("copilotWireAPI() = %q, %v", got, err)
	}
}

func TestCopilotAdapterRejectsIncompatibleModel(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = (CopilotAdapter{}).Prepare(PrepareInput{
		BaseURL:    "http://127.0.0.1:43210",
		Model:      ModelInfo{ID: "messages-only", SupportedEndpoints: []string{"/v1/messages"}},
		Binary:     binary,
		LocalToken: "token",
	})
	if err == nil || !strings.Contains(err.Error(), "expected /responses or /chat/completions") {
		t.Fatalf("Prepare() error = %v, want compatibility error", err)
	}
}

func TestCopilotAdapterRejectsManagedOverridesAndRemoteModes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "model", args: []string{"--model", "other"}, want: "model, agent, or environment"},
		{name: "agent", args: []string{"--agent", "other"}, want: "model, agent, or environment"},
		{name: "secrets", args: []string{"--secret-env-vars=OTHER"}, want: "model, agent, or environment"},
		{name: "managed safety", args: []string{"--no-remote=false"}, want: "safety overrides"},
		{name: "connect", args: []string{"--connect=session"}, want: "remote, resumed, or shared"},
		{name: "continue", args: []string{"--continue"}, want: "remote, resumed, or shared"},
		{name: "resume", args: []string{"--resume=abc"}, want: "remote, resumed, or shared"},
		{name: "attached short resume", args: []string{"-rabc"}, want: "remote, resumed, or shared"},
		{name: "remote", args: []string{"--remote"}, want: "remote, resumed, or shared"},
		{name: "share", args: []string{"--share-gist"}, want: "remote, resumed, or shared"},
		{name: "acp", args: []string{"--acp"}, want: "ACP server"},
		{name: "login", args: []string{"login"}, want: "not an agent session"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCopilotForwardedArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
	for _, args := range [][]string{{"--allow-all-tools", "-p", "hello", "-s"}, {"--plan"}, {"--autopilot"}} {
		if err := validateCopilotForwardedArgs(args); err != nil {
			t.Fatalf("validateCopilotForwardedArgs(%#v) = %v", args, err)
		}
	}
}
