package launch

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func localModelInfo() ModelInfo {
	return ModelInfo{ID: "qwen-3.8-27b", OwnedBy: "aikit", SupportedEndpoints: []string{"/chat/completions", "/responses"}}
}

func TestClaudeAdapterPassesLocalModelContextWindow(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL:    "http://127.0.0.1:43210",
		Model:      localModelInfo(),
		Binary:     binary,
		LocalToken: "token",
		LocalModel: &LocalModel{ContextTokens: 65536, FunctionToolsOnly: true},
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Cleanup != nil {
		t.Cleanup(func() { _ = prepared.Cleanup() })
	}
	if got := prepared.EnvSet["CLAUDE_CODE_MAX_CONTEXT_TOKENS"]; got != "65536" {
		t.Fatalf("CLAUDE_CODE_MAX_CONTEXT_TOKENS = %q", got)
	}

	remote, err := (ClaudeAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210", Model: localModelInfo(), Binary: binary, LocalToken: "token", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if remote.Cleanup != nil {
		t.Cleanup(func() { _ = remote.Cleanup() })
	}
	if _, set := remote.EnvSet["CLAUDE_CODE_MAX_CONTEXT_TOKENS"]; set {
		t.Fatal("a provider-routed model got a local context override")
	}
}

func TestCodexAdapterAppliesFunctionToolSafeguardsToLocalModels(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := (CodexAdapter{}).Prepare(PrepareInput{
		BaseURL:       "http://127.0.0.1:43210",
		Model:         localModelInfo(),
		Binary:        binary,
		LocalToken:    "token",
		LocalModel:    &LocalModel{ContextTokens: 65536, FunctionToolsOnly: true},
		ForwardedArgs: []string{"exec", "--ephemeral", "hello"},
		DryRun:        true,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Cleanup != nil {
		defer func() { _ = prepared.Cleanup() }()
	}
	for _, want := range []string{`web_search="disabled"`, `features.remote_compaction_v2=false`, `features.code_mode=false`} {
		if !containsString(prepared.Args, want) {
			t.Fatalf("local Codex args missing %q: %#v", want, prepared.Args)
		}
	}
	body, err := os.ReadFile(codexCatalogPathFromArgs(t, prepared.Args))
	if err != nil {
		t.Fatal(err)
	}
	var catalog codexCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	if value, exists := catalog.Models[0]["apply_patch_tool_type"]; !exists || value != nil {
		t.Fatalf("local apply_patch_tool_type = %#v, want explicit null", value)
	}
	// Direct Responses models keep an explicit non-reasoning default.
	if value := catalog.Models[0]["default_reasoning_level"]; value != "none" {
		t.Fatalf("local default_reasoning_level = %#v", value)
	}

	_, err = (CodexAdapter{}).Prepare(PrepareInput{
		BaseURL: "http://127.0.0.1:43210", Model: localModelInfo(), Binary: binary, LocalToken: "token",
		LocalModel: &LocalModel{FunctionToolsOnly: true}, ForwardedArgs: []string{"--search", "exec", "hi"}, DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "local models") {
		t.Fatalf("--search error = %v", err)
	}
}
