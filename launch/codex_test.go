package launch

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCodexAdapterPrepare(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	contextWindow := int64(400000)
	maxContextWindow := int64(1000000)
	prepared, err := (CodexAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210/",
		Model: ModelInfo{
			ID:                 "gpt-public",
			Name:               "GPT Public",
			SupportedEndpoints: []string{"/responses"},
			ContextWindow:      &contextWindow,
			MaxContextWindow:   &maxContextWindow,
			Capabilities: ModelCapabilities{
				Supports: ModelCapabilitySupports{
					ReasoningEffort:   []string{"low", "medium", "high"},
					ParallelToolCalls: true,
					Vision:            true,
				},
			},
		},
		Binary:        binary,
		ForwardedArgs: []string{"exec", "--ephemeral", "hello"},
		LocalToken:    "test-token-placeholder",
		SensitiveEnv:  []string{"MY_PROVIDER_TOKEN"},
		DryRun:        true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Path != binary {
		t.Fatalf("Path = %q, want %q", prepared.Path, binary)
	}
	if got := prepared.EnvSet[codexLocalTokenEnv]; got != "test-token-placeholder" {
		t.Fatalf("EnvSet[%q] = %q", codexLocalTokenEnv, got)
	}
	for _, key := range []string{
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "MY_PROVIDER_TOKEN",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
	} {
		if !containsString(prepared.EnvUnset, key) {
			t.Fatalf("EnvUnset missing %q: %#v", key, prepared.EnvUnset)
		}
	}
	joined := strings.Join(prepared.Args, "\n")
	if strings.Contains(joined, "test-token-placeholder") {
		t.Fatal("Codex argv exposed the local proxy token")
	}
	providerID := codexProviderID("test-token-placeholder")
	for _, want := range []string{
		`model_provider="` + providerID + `"`,
		`model_providers.` + providerID + `.base_url="http://127.0.0.1:43210/v1"`,
		`model_providers.` + providerID + `.wire_api="responses"`,
		`model_providers.` + providerID + `.env_key="` + codexLocalTokenEnv + `"`,
		`model_providers.` + providerID + `.requires_openai_auth=false`,
		`model_providers.` + providerID + `.supports_websockets=false`,
	} {
		if !containsString(prepared.Args, want) {
			t.Fatalf("Codex args missing %q: %#v", want, prepared.Args)
		}
	}
	if !containsAdjacent(prepared.Args, "-m", "gpt-public") {
		t.Fatalf("Codex args did not pin model: %#v", prepared.Args)
	}
	if !reflect.DeepEqual(prepared.Args[len(prepared.Args)-3:], []string{"exec", "--ephemeral", "hello"}) {
		t.Fatalf("forwarded args changed: %#v", prepared.Args)
	}

	catalogPath := codexCatalogPathFromArgs(t, prepared.Args)
	body, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read Codex model catalog: %v", err)
	}
	var catalog codexCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatalf("decode Codex model catalog: %v", err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0]["slug"] != "gpt-public" {
		t.Fatalf("unexpected Codex model catalog: %#v", catalog.Models)
	}
	if got := int64(catalog.Models[0]["context_window"].(float64)); got != contextWindow {
		t.Fatalf("catalog context_window = %d, want %d", got, contextWindow)
	}
	if got := int64(catalog.Models[0]["max_context_window"].(float64)); got != maxContextWindow {
		t.Fatalf("catalog max_context_window = %d, want %d", got, maxContextWindow)
	}
	if modalities, ok := catalog.Models[0]["input_modalities"].([]interface{}); !ok || len(modalities) != 2 {
		t.Fatalf("catalog input_modalities = %#v", catalog.Models[0]["input_modalities"])
	}
	if prepared.Cleanup == nil {
		t.Fatal("Codex catalog did not register cleanup")
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatalf("cleanup Codex catalog: %v", err)
	}
	if _, err := os.Stat(catalogPath); !os.IsNotExist(err) {
		t.Fatalf("Codex catalog still exists after cleanup: %v", err)
	}
}

func TestCodexCatalogClearsUnsupportedDonorCapabilities(t *testing.T) {
	body, err := buildCodexModelCatalog(resolvedExecutable{}, nil, ModelInfo{ID: "text-only"}, true)
	if err != nil {
		t.Fatalf("buildCodexModelCatalog() error = %v", err)
	}
	var catalog codexCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Models) != 1 {
		t.Fatalf("catalog models = %#v", catalog.Models)
	}
	model := catalog.Models[0]
	if got, _ := model["supports_parallel_tool_calls"].(bool); got {
		t.Fatal("text-only model inherited parallel tool-call support")
	}
	reasoningLevels, ok := model["supported_reasoning_levels"].([]interface{})
	if !ok || len(reasoningLevels) != 0 {
		t.Fatalf("non-reasoning model supported_reasoning_levels = %#v, want empty", model["supported_reasoning_levels"])
	}
	if got := model["default_reasoning_level"]; got != "none" {
		t.Fatalf("non-reasoning model default_reasoning_level = %#v, want none", got)
	}
	modalities, ok := model["input_modalities"].([]interface{})
	if !ok || !reflect.DeepEqual(modalities, []interface{}{"text"}) {
		t.Fatalf("text-only model input_modalities = %#v, want [text]", model["input_modalities"])
	}
}

func TestCodexAdapterRejectsIncompatibleModel(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = (CodexAdapter{}).Prepare(PrepareInput{
		BaseURL:    "http://127.0.0.1:43210",
		Model:      ModelInfo{ID: "chat-only", SupportedEndpoints: []string{"/chat/completions"}},
		Binary:     binary,
		LocalToken: "token",
	})
	if err == nil || !strings.Contains(err.Error(), "expected /responses") {
		t.Fatalf("Prepare() error = %v, want Responses compatibility error", err)
	}
}

func TestCodexAdapterRejectsManagedOverridesAndNonAgentModes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "model", args: []string{"--model", "other"}, want: "model or provider overrides"},
		{name: "attached model", args: []string{"-mother"}, want: "model or provider overrides"},
		{name: "config", args: []string{"-c", `model_provider="other"`}, want: "model or provider overrides"},
		{name: "profile", args: []string{"-p", "other"}, want: "model or provider overrides"},
		{name: "remote", args: []string{"--remote", "ws://example"}, want: "remote Codex sessions"},
		{name: "resume", args: []string{"resume", "last"}, want: "resumed Codex sessions"},
		{name: "exec resume", args: []string{"exec", "resume", "last"}, want: "resumed Codex sessions"},
		{name: "alias exec resume", args: []string{"e", "resume", "last"}, want: "resumed Codex sessions"},
		{name: "app server", args: []string{"app-server"}, want: "detached or server"},
		{name: "login", args: []string{"login"}, want: "not an agent session"},
		{name: "help", args: []string{"help"}, want: "not an agent session"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCodexForwardedArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
	for _, args := range [][]string{{"exec", "--ephemeral", "hello"}, {"e", "--ephemeral", "hello"}, {"review", "--uncommitted"}, {"hello"}} {
		if err := validateCodexForwardedArgs(args); err != nil {
			t.Fatalf("validateCodexForwardedArgs(%#v) = %v", args, err)
		}
	}
}

func TestDefaultReasoningEffort(t *testing.T) {
	if got := defaultReasoningEffort([]string{"low", "medium", "high"}); got != "medium" {
		t.Fatalf("defaultReasoningEffort() = %q", got)
	}
	if got := defaultReasoningEffort([]string{"xhigh", "high"}); got != "xhigh" {
		t.Fatalf("defaultReasoningEffort() = %q", got)
	}
}

func codexCatalogPathFromArgs(t *testing.T, args []string) string {
	t.Helper()
	for _, arg := range args {
		if !strings.HasPrefix(arg, "model_catalog_json=") {
			continue
		}
		var path string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "model_catalog_json=")), &path); err != nil {
			t.Fatalf("decode model_catalog_json override: %v", err)
		}
		return path
	}
	t.Fatalf("model_catalog_json override missing from %#v", args)
	return ""
}

func containsAdjacent(values []string, first, second string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == first && values[i+1] == second {
			return true
		}
	}
	return false
}
