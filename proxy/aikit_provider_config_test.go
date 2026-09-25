package proxy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func writeProvidersConfig(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadProvidersConfigAcceptsAIKitProvider(t *testing.T) {
	path := writeProvidersConfig(t, "providers.yaml", `
providers:
  - id: local
    type: aikit
    default: true
    aikit:
      model: qwen3.8:27b
      context_size: 65536
      runtime: podman
      keep: true
      load_timeout: 15m
    models:
      - public_id: local-qwen
`)
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile: %v", err)
	}
	if !cfg.HasAIKitProviders() || !cfg.Providers[0].IsAIKit() {
		t.Fatalf("aikit provider lost: %+v", cfg.Providers[0])
	}
	block := cfg.Providers[0].AIKit
	if block == nil || block.Model != "qwen3.8:27b" || block.ContextSize != 65536 || block.Runtime != "podman" || !block.Keep {
		t.Fatalf("aikit block = %+v", block)
	}
	if timeout, err := block.LoadTimeoutDuration(); err != nil || timeout.Minutes() != 15 {
		t.Fatalf("load timeout = %v, %v", timeout, err)
	}
	if cfg.Providers[0].BaseURL != "" || cfg.Providers[0].UpstreamDialect != "" {
		t.Fatalf("validation shadow leaked into the loaded config: %+v", cfg.Providers[0])
	}

	resolved, found, err := ResolveStaticProviderModel(cfg, "local-qwen")
	if err != nil || !found {
		t.Fatalf("ResolveStaticProviderModel = %v, %v", found, err)
	}
	if !reflect.DeepEqual(resolved.Endpoints, AIKitModelEndpoints()) {
		t.Fatalf("endpoints = %v", resolved.Endpoints)
	}

	_, err = NewProxyHandler(auth.NewTestAuthenticator("t"), logger.New(logger.LevelError), WithProvidersConfig(cfg))
	if err == nil || !strings.Contains(err.Error(), "must start before serving") {
		t.Fatalf("unmaterialized aikit provider error = %v", err)
	}
}

func TestLoadProvidersConfigAIKitRouteTarget(t *testing.T) {
	path := writeProvidersConfig(t, "providers.yaml", `
schema_version: 2
state_bindings:
  mode: memory
providers:
  - id: copilot
    type: copilot
  - id: local
    type: aikit
    aikit:
      model: qwen3.8:27b
model_routes:
  - id: coder
    public_id: coder
    endpoints: [/chat/completions]
    targets:
      - {id: local, provider: local, upstream_model: qwen-3.8-27b}
      - {id: cloud, provider: copilot, upstream_model: gpt-5.4-mini}
    routing:
      mode: priority_failover
      max_target_attempts: 2
      max_upstream_sends: 3
      failover_on_context_overflow: true
`)
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile: %v", err)
	}
	if !cfg.Providers[1].IsAIKit() || !cfg.ModelRoutes[0].Routing.FailoverOnContextOverflow {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadProvidersConfigRejectsInvalidAIKitProviders(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		want    string
	}{
		{name: "missing block", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n", want: "providers[0].aikit: is required"},
		{name: "missing model", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n    aikit: {context_size: 1}\n", want: "providers[0].aikit.model: is required"},
		{name: "prefixed model", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n    aikit: {model: 'aikit:qwen3.8:27b'}\n", want: "must not include the aikit: prefix"},
		{name: "managed base_url", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n    base_url: http://localhost:8080/v1\n    aikit: {model: qwen3.8:27b}\n", want: "providers[0].base_url: is managed by vekil"},
		{name: "bad runtime", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n    aikit: {model: qwen3.8:27b, runtime: nerdctl}\n", want: "must be auto, docker, or podman"},
		{name: "bad backend", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n    aikit: {model: qwen3.8:27b, backend: diffusers}\n", want: "must be llama-cpp or vllm-cpp"},
		{name: "bad timeout", file: "p.yaml", content: "providers:\n  - id: local\n    type: aikit\n    aikit: {model: qwen3.8:27b, load_timeout: soon}\n", want: "providers[0].aikit.load_timeout"},
		{name: "block on other type", file: "p.yaml", content: "providers:\n  - id: local\n    type: openai-compatible\n    base_url: http://x/v1\n    aikit: {model: qwen3.8:27b}\n    models: [{public_id: m}]\n", want: "requires type aikit"},
		{name: "unknown yaml key", file: "p.yaml", content: "schema_version: 2\nproviders:\n  - id: local\n    type: aikit\n    aikit: {model: qwen3.8:27b, gpu: true}\n", want: "gpu"},
		{name: "unknown json key", file: "p.json", content: `{"providers":[{"id":"local","type":"aikit","aikit":{"model":"qwen3.8:27b","gpu":true}}]}`, want: "gpu"},
		{name: "failover without priority", file: "p.yaml", content: `
schema_version: 2
state_bindings: {mode: memory}
providers:
  - {id: a, type: openai-compatible, default: true, base_url: 'http://a/v1'}
model_routes:
  - id: r
    public_id: r
    endpoints: [/chat/completions]
    targets: [{id: a, provider: a, upstream_model: m}]
    routing: {failover_on_context_overflow: true}
`, want: "routing.failover_on_context_overflow: requires routing.mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadProvidersConfigFile(writeProvidersConfig(t, tt.file, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateProvidersConfigUpstreamDialect(t *testing.T) {
	cfg := ProvidersConfig{Providers: []ProviderConfig{{
		ID: "azure", Type: "azure-openai", BaseURL: "https://example.openai.azure.com", APIKey: "k", UpstreamDialect: "localai",
		Models: []ProviderModelConfig{{PublicID: "m", Deployment: "d"}},
	}}}
	if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "only supported for openai-compatible") {
		t.Fatalf("azure dialect error = %v", err)
	}
	cfg.Providers[0] = ProviderConfig{ID: "local", Type: "openai-compatible", BaseURL: "http://localhost:8080/v1", UpstreamDialect: "localai", Models: []ProviderModelConfig{{PublicID: "m"}}}
	if err := ValidateProvidersConfig(cfg); err != nil {
		t.Fatalf("localai dialect: %v", err)
	}
}
