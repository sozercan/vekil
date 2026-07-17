package proxy

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

var benchmarkProviderSetupSink *providerSetup

func TestLoadProvidersConfigFileAzureV1BaseURLAndModelMetadata(t *testing.T) {
	t.Parallel()

	providersPath := filepath.Join(t.TempDir(), "providers.json")
	body := []byte(`{
  "providers": [
    {
      "id": "copilot",
      "type": "copilot",
      "default": true,
      "exclude_models": ["gpt-5.4-pro", "gpt-5.4"]
    },
    {
      "id": "azure-openai",
      "type": "azure-openai",
      "base_url": "https://example.openai.azure.com/openai/v1",
      "api_key": "test-key",
      "api_version": "2025-04-01-preview",
      "models": [
        {
          "public_id": "gpt-5.4-pro",
          "deployment": "gpt-5.4-pro",
          "endpoints": ["/responses"],
          "name": "GPT-5.4 Pro"
        },
        {
          "public_id": "gpt-5.4",
          "deployment": "gpt-5.4",
          "endpoints": ["/responses"],
          "name": "GPT-5.4",
          "model_picker_category": "powerful",
          "reasoning_effort": ["low", "medium", "high"],
          "vision": true,
          "parallel_tool_calls": true,
          "use_max_completion_tokens": true,
          "context_window": 400000
        }
      ]
    }
  ]
}`)
	if err := os.WriteFile(providersPath, body, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := LoadProvidersConfigFile(providersPath)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	providers, _, defaultProviderID, err := handler.buildProviders(cfg)
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}

	if defaultProviderID != "copilot" {
		t.Fatalf("default provider = %q, want copilot", defaultProviderID)
	}

	provider := providers["azure-openai"]
	if provider == nil {
		t.Fatal("expected azure-openai provider to be built")
	}

	if provider.baseURL != "https://example.openai.azure.com/openai/v1" {
		t.Fatalf("provider.baseURL = %q, want Azure /openai/v1 endpoint", provider.baseURL)
	}

	modelsURL, err := handler.providerRequestURL(provider, "/models", "")
	if err != nil {
		t.Fatalf("providerRequestURL() error = %v", err)
	}
	if modelsURL != "https://example.openai.azure.com/openai/v1/models" {
		t.Fatalf("providerRequestURL() = %q", modelsURL)
	}

	proModel, ok := provider.staticModels["gpt-5.4-pro"]
	if !ok {
		t.Fatal("expected static model gpt-5.4-pro")
	}
	if !reflect.DeepEqual(proModel.supportedEndpoints, []string{"/responses"}) {
		t.Fatalf("gpt-5.4-pro endpoints = %v, want [/responses]", proModel.supportedEndpoints)
	}

	cfgModel, ok := provider.staticConfigs["gpt-5.4"]
	if !ok {
		t.Fatal("expected static config for gpt-5.4")
	}
	if cfgModel.ModelPickerCategory != "powerful" {
		t.Fatalf("model_picker_category = %q, want powerful", cfgModel.ModelPickerCategory)
	}
	if !reflect.DeepEqual(cfgModel.ReasoningEffort, []string{"low", "medium", "high"}) {
		t.Fatalf("reasoning_effort = %v, want [low medium high]", cfgModel.ReasoningEffort)
	}
	if cfgModel.Vision == nil || !*cfgModel.Vision {
		t.Fatalf("vision = %v, want true", cfgModel.Vision)
	}
	if cfgModel.ParallelToolCalls == nil || !*cfgModel.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v, want true", cfgModel.ParallelToolCalls)
	}
	if cfgModel.UseMaxCompletionTokens == nil || !*cfgModel.UseMaxCompletionTokens {
		t.Fatalf("use_max_completion_tokens = %v, want true", cfgModel.UseMaxCompletionTokens)
	}
	if cfgModel.ContextWindow == nil || *cfgModel.ContextWindow != 400000 {
		t.Fatalf("context_window = %v, want 400000", cfgModel.ContextWindow)
	}
}

func TestLoadProvidersConfigFileYAML(t *testing.T) {
	t.Parallel()

	for _, ext := range []string{".yaml", ".yml"} {
		ext := ext
		t.Run(ext, func(t *testing.T) {
			t.Parallel()

			providersPath := filepath.Join(t.TempDir(), "providers"+ext)
			body := []byte(`providers:
  - id: copilot
    type: copilot
    default: true
    exclude_models:
      - gpt-5.4-pro
  - id: azure-openai
    type: azure-openai
    base_url: https://example.openai.azure.com/openai/v1
    api_key: test-key
    api_version: 2025-04-01-preview
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses
        name: GPT-5.4 Pro
        model_picker_enabled: false
        model_picker_category: powerful
        reasoning_effort:
          - low
          - medium
          - high
        vision: true
        parallel_tool_calls: true
        use_max_completion_tokens: true
        context_window: 400000
  - id: openai-codex
    type: openai-codex
    include_models:
      - gpt-5.5
`)
			if err := os.WriteFile(providersPath, body, 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg, err := LoadProvidersConfigFile(providersPath)
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}

			if len(cfg.Providers) != 3 {
				t.Fatalf("providers count = %d, want 3", len(cfg.Providers))
			}
			if !cfg.Providers[0].Default {
				t.Fatal("copilot default = false, want true")
			}
			if !reflect.DeepEqual(cfg.Providers[0].ExcludeModels, []string{"gpt-5.4-pro"}) {
				t.Fatalf("exclude_models = %v, want [gpt-5.4-pro]", cfg.Providers[0].ExcludeModels)
			}

			provider := cfg.Providers[1]
			if provider.ID != "azure-openai" || provider.Type != "azure-openai" {
				t.Fatalf("provider = %#v, want azure-openai", provider)
			}
			if provider.BaseURL != "https://example.openai.azure.com/openai/v1" {
				t.Fatalf("base_url = %q", provider.BaseURL)
			}
			if provider.APIKey != "test-key" {
				t.Fatalf("api_key = %q, want test-key", provider.APIKey)
			}
			if provider.APIVersion != "2025-04-01-preview" {
				t.Fatalf("api_version = %q, want 2025-04-01-preview", provider.APIVersion)
			}
			if len(provider.Models) != 1 {
				t.Fatalf("models count = %d, want 1", len(provider.Models))
			}

			model := provider.Models[0]
			if model.PublicID != "gpt-5.4-pro" || model.Deployment != "gpt-5.4-pro" {
				t.Fatalf("model IDs = (%q, %q), want gpt-5.4-pro", model.PublicID, model.Deployment)
			}
			if !reflect.DeepEqual(model.Endpoints, []string{"/responses"}) {
				t.Fatalf("endpoints = %v, want [/responses]", model.Endpoints)
			}
			if model.ModelPickerEnabled == nil || *model.ModelPickerEnabled {
				t.Fatalf("model_picker_enabled = %v, want false", model.ModelPickerEnabled)
			}
			if model.ModelPickerCategory != "powerful" {
				t.Fatalf("model_picker_category = %q, want powerful", model.ModelPickerCategory)
			}
			if !reflect.DeepEqual(model.ReasoningEffort, []string{"low", "medium", "high"}) {
				t.Fatalf("reasoning_effort = %v, want [low medium high]", model.ReasoningEffort)
			}
			if model.Vision == nil || !*model.Vision {
				t.Fatalf("vision = %v, want true", model.Vision)
			}
			if model.ParallelToolCalls == nil || !*model.ParallelToolCalls {
				t.Fatalf("parallel_tool_calls = %v, want true", model.ParallelToolCalls)
			}
			if model.UseMaxCompletionTokens == nil || !*model.UseMaxCompletionTokens {
				t.Fatalf("use_max_completion_tokens = %v, want true", model.UseMaxCompletionTokens)
			}
			if model.ContextWindow == nil || *model.ContextWindow != 400000 {
				t.Fatalf("context_window = %v, want 400000", model.ContextWindow)
			}

			codexProvider := cfg.Providers[2]
			if codexProvider.ID != "openai-codex" || codexProvider.Type != "openai-codex" {
				t.Fatalf("codex provider = %#v, want openai-codex", codexProvider)
			}
			if !reflect.DeepEqual(codexProvider.IncludeModels, []string{"gpt-5.5"}) {
				t.Fatalf("codex include_models = %v, want [gpt-5.5]", codexProvider.IncludeModels)
			}

			handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
			providers, _, defaultProviderID, err := handler.buildProviders(cfg)
			if err != nil {
				t.Fatalf("buildProviders() error = %v", err)
			}
			if defaultProviderID != "copilot" {
				t.Fatalf("default provider = %q, want copilot", defaultProviderID)
			}
			codexRuntime := providers["openai-codex"]
			if codexRuntime == nil {
				t.Fatal("expected openai-codex provider to be built")
			}
			if codexRuntime.baseURL != defaultOpenAICodexBaseURL {
				t.Fatalf("codex baseURL = %q, want %q", codexRuntime.baseURL, defaultOpenAICodexBaseURL)
			}
			if !codexRuntime.allowsModel("gpt-5.5") {
				t.Fatal("expected YAML include_models to allow gpt-5.5")
			}
			if codexRuntime.allowsModel("gpt-5.4") {
				t.Fatal("expected YAML include_models to block gpt-5.4")
			}
		})
	}
}

func TestLoadProvidersConfigFileCopilotHeaderProfiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ext  string
		body string
	}{
		{
			name: "json",
			ext:  ".json",
			body: `{
  "providers": [
    {
      "id": "copilot",
      "type": "copilot",
      "default": true,
      "headers": {
        "default": {
          "editor_version": "default-editor",
          "copilot_integration_id": "default-integration"
        },
        "chat_completions": {
          "user_agent": "chat-agent",
          "openai_intent": "chat-intent"
        },
        "responses": {
          "editor_version": "responses-editor",
          "github_api_version": "responses-api"
        }
      }
    }
  ]
}`,
		},
		{
			name: "yaml",
			ext:  ".yaml",
			body: `providers:
  - id: copilot
    type: copilot
    default: true
    headers:
      default:
        editor_version: default-editor
        copilot_integration_id: default-integration
      chat_completions:
        user_agent: chat-agent
        openai_intent: chat-intent
      responses:
        editor_version: responses-editor
        github_api_version: responses-api
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			providersPath := filepath.Join(t.TempDir(), "providers"+tt.ext)
			if err := os.WriteFile(providersPath, []byte(tt.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg, err := LoadProvidersConfigFile(providersPath)
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
			if len(cfg.Providers) != 1 {
				t.Fatalf("providers count = %d, want 1", len(cfg.Providers))
			}

			headers := cfg.Providers[0].Headers
			if headers.Default.EditorVersion != "default-editor" {
				t.Fatalf("default editor_version = %q, want default-editor", headers.Default.EditorVersion)
			}
			if headers.Default.IntegrationID != "default-integration" {
				t.Fatalf("default copilot_integration_id = %q, want default-integration", headers.Default.IntegrationID)
			}
			if headers.ChatCompletions.UserAgent != "chat-agent" {
				t.Fatalf("chat user_agent = %q, want chat-agent", headers.ChatCompletions.UserAgent)
			}
			if headers.ChatCompletions.OpenAIIntent != "chat-intent" {
				t.Fatalf("chat openai_intent = %q, want chat-intent", headers.ChatCompletions.OpenAIIntent)
			}
			if headers.Responses.EditorVersion != "responses-editor" {
				t.Fatalf("responses editor_version = %q, want responses-editor", headers.Responses.EditorVersion)
			}
			if headers.Responses.GitHubAPIVersion != "responses-api" {
				t.Fatalf("responses github_api_version = %q, want responses-api", headers.Responses.GitHubAPIVersion)
			}

			handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
			providers, _, defaultProviderID, err := handler.buildProviders(cfg)
			if err != nil {
				t.Fatalf("buildProviders() error = %v", err)
			}
			if defaultProviderID != "copilot" {
				t.Fatalf("default provider = %q, want copilot", defaultProviderID)
			}
			runtime := providers["copilot"]
			if runtime == nil {
				t.Fatal("expected copilot runtime")
			}
			if runtime.headerProfiles.Responses.EditorVersion != "responses-editor" {
				t.Fatalf("runtime responses editor_version = %q, want responses-editor", runtime.headerProfiles.Responses.EditorVersion)
			}
		})
	}
}

func TestBuildProvidersGenericOpenAICompatibleConfig(t *testing.T) {
	t.Setenv("TEST_GENERIC_API_KEY", "generic-key")

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	providers, _, defaultProviderID, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:                  "local-openai",
			Type:                "openai-compatible",
			Default:             true,
			BaseURL:             "http://localhost:1234",
			APIKeyEnv:           "TEST_GENERIC_API_KEY",
			AuthType:            "api-key-header",
			AuthHeader:          "X-API-Key",
			AuthPrefix:          "Token",
			ExtraHeaders:        map[string]string{"X-Provider": "local"},
			ChatCompletionsPath: "/v1/chat/completions",
			ResponsesPath:       "/v1/responses",
			ModelsPath:          "/api/tags",
			ModelDiscovery:      "ollama",
			Models: []ProviderModelConfig{{
				PublicID:   "local-chat",
				Deployment: "llama3.2:latest",
				Name:       "Local Chat",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	if defaultProviderID != "local-openai" {
		t.Fatalf("default provider = %q, want local-openai", defaultProviderID)
	}

	provider := providers["local-openai"]
	if provider == nil {
		t.Fatal("expected local-openai provider")
	}
	if provider.kind != providerTypeOpenAICompatible {
		t.Fatalf("provider.kind = %q, want %q", provider.kind, providerTypeOpenAICompatible)
	}
	if provider.authType != providerAuthTypeAPIKeyHeader {
		t.Fatalf("authType = %q, want api-key-header", provider.authType)
	}
	if provider.authHeader != "X-Api-Key" {
		t.Fatalf("authHeader = %q, want X-Api-Key", provider.authHeader)
	}
	if provider.authPrefix != "Token" {
		t.Fatalf("authPrefix = %q, want Token", provider.authPrefix)
	}
	if provider.apiKey != "generic-key" {
		t.Fatalf("apiKey = %q, want generic-key", provider.apiKey)
	}
	if got := provider.extraHeaders.Get("X-Provider"); got != "local" {
		t.Fatalf("extra header X-Provider = %q, want local", got)
	}
	if provider.paths.chatCompletions != "/v1/chat/completions" || provider.paths.responses != "/v1/responses" || provider.paths.models != "/api/tags" {
		t.Fatalf("paths = %+v, want configured generic paths", provider.paths)
	}
	if provider.modelDiscovery != providerModelDiscoveryOllama {
		t.Fatalf("modelDiscovery = %q, want ollama", provider.modelDiscovery)
	}

	model := provider.staticModels["local-chat"]
	if model.publicID != "local-chat" || model.upstreamModel != "llama3.2:latest" {
		t.Fatalf("static model = %+v, want public local-chat mapped to llama3.2:latest", model)
	}
	if !reflect.DeepEqual(model.supportedEndpoints, []string{"/chat/completions"}) {
		t.Fatalf("default openai-compatible endpoints = %v, want [/chat/completions]", model.supportedEndpoints)
	}
}

func TestBuildProvidersGenericAnthropicCompatibleConfig(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:             "anthropic-native",
			Type:           "anthropic-compatible",
			Default:        true,
			BaseURL:        "https://anthropic-compatible.example.com",
			AuthType:       "none",
			MessagesPath:   "/messages",
			ModelDiscovery: "static",
			Models: []ProviderModelConfig{{
				PublicID: "claude-local",
				Name:     "Claude Local",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}

	provider := providers["anthropic-native"]
	if provider == nil {
		t.Fatal("expected anthropic-native provider")
	}
	if provider.authType != providerAuthTypeNone {
		t.Fatalf("authType = %q, want none", provider.authType)
	}
	if provider.paths.messages != "/messages" {
		t.Fatalf("messages path = %q, want /messages", provider.paths.messages)
	}
	model := provider.staticModels["claude-local"]
	if !reflect.DeepEqual(model.supportedEndpoints, []string{"/v1/messages"}) {
		t.Fatalf("default anthropic-compatible endpoints = %v, want [/v1/messages]", model.supportedEndpoints)
	}
}

func TestBuildProvidersGenericValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  ProviderConfig
		want string
	}{
		{
			name: "static requires models",
			cfg: ProviderConfig{
				ID:      "local",
				Type:    "openai-compatible",
				BaseURL: "http://localhost:1234",
			},
			want: "must configure at least one model",
		},
		{
			name: "api key header requires header",
			cfg: ProviderConfig{
				ID:       "local",
				Type:     "openai-compatible",
				BaseURL:  "http://localhost:1234",
				APIKey:   "key",
				AuthType: "api-key-header",
				Models:   []ProviderModelConfig{{PublicID: "m"}},
			},
			want: "requires auth_header",
		},
		{
			name: "configured api key env must be set",
			cfg: ProviderConfig{
				ID:        "local",
				Type:      "openai-compatible",
				BaseURL:   "http://localhost:1234",
				APIKeyEnv: "VEKIL_TEST_MISSING_GENERIC_API_KEY",
				Models:    []ProviderModelConfig{{PublicID: "m"}},
			},
			want: "api_key_env",
		},
		{
			name: "path rejects query",
			cfg: ProviderConfig{
				ID:                  "local",
				Type:                "openai-compatible",
				BaseURL:             "http://localhost:1234",
				ChatCompletionsPath: "/v1/chat/completions?debug=true",
				Models:              []ProviderModelConfig{{PublicID: "m"}},
			},
			want: "no query string or fragment",
		},
		{
			name: "bad discovery",
			cfg: ProviderConfig{
				ID:             "local",
				Type:           "openai-compatible",
				BaseURL:        "http://localhost:1234",
				ModelDiscovery: "made-up",
				Models:         []ProviderModelConfig{{PublicID: "m"}},
			},
			want: "unsupported model_discovery",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
			_, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{tt.cfg}})
			if err == nil {
				t.Fatalf("buildProviders() error = nil, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("buildProviders() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadProvidersConfigFileRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		ext  string
		body string
	}{
		{name: "empty JSON", ext: ".json", body: ""},
		{name: "empty YAML", ext: ".yaml", body: ""},
		{name: "whitespace YAML", ext: ".yml", body: " \n\t \n"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			providersPath := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(providersPath, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, err := LoadProvidersConfigFile(providersPath)
			if err == nil {
				t.Fatal("LoadProvidersConfigFile() error = nil, want empty config error")
			}
			if !strings.Contains(err.Error(), "empty") {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want empty config error", err)
			}
		})
	}
}

func TestLoadProvidersConfigFileRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	t.Parallel()

	validProviderJSON := `{"id":"local","type":"openai-compatible","base_url":"http://localhost:1234","auth_type":"none","models":[{"public_id":"local-model"}]}`
	validProviderYAML := `
  - id: local
    type: openai-compatible
    base_url: http://localhost:1234
    auth_type: none
    models:
      - public_id: local-model`

	testCases := []struct {
		name string
		ext  string
		body string
		want string
	}{
		{
			name: "JSON unknown top-level field",
			ext:  ".json",
			body: `{"providers":[],"providerz":[]}`,
			want: "providerz",
		},
		{
			name: "YAML unknown top-level field",
			ext:  ".yaml",
			body: "providers: []\nproviderz: []\n",
			want: "providerz",
		},
		{
			name: "JSON unknown provider field",
			ext:  ".json",
			body: `{"providers":[` + strings.TrimSuffix(validProviderJSON, `}`) + `,"timeout_ms":1000}]}`,
			want: "timeout_ms",
		},
		{
			name: "YAML unknown provider field",
			ext:  ".yaml",
			body: "providers:" + validProviderYAML + "\n    timeout_ms: 1000\n",
			want: "timeout_ms",
		},
		{
			name: "JSON model endpoint typo",
			ext:  ".json",
			body: `{"providers":[{"id":"local","type":"openai-compatible","base_url":"http://localhost:1234","auth_type":"none","models":[{"public_id":"local-model","endpoint":["/chat/completions"]}]}]}`,
			want: "endpoint",
		},
		{
			name: "YAML model endpoint typo",
			ext:  ".yaml",
			body: "providers:" + validProviderYAML + "\n        endpoint:\n          - /chat/completions\n",
			want: "endpoint",
		},
		{
			name: "trailing JSON value",
			ext:  ".json",
			body: `{"providers":[]} {"providers":[]}`,
			want: "more than one JSON value",
		},
		{
			name: "multiple YAML documents",
			ext:  ".yaml",
			body: "providers: []\n---\nproviders: []\n",
			want: "more than one YAML document",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			providersPath := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(providersPath, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, err := LoadProvidersConfigFile(providersPath)
			if err == nil {
				t.Fatal("LoadProvidersConfigFile() error = nil, want strict decode error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadProvidersConfigFileAllowsExplicitEmptyProviders(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		ext  string
		body string
	}{
		{name: "JSON", ext: ".json", body: `{"providers": []}`},
		{name: "YAML", ext: ".yaml", body: "providers: []\n"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			providersPath := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(providersPath, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg, err := LoadProvidersConfigFile(providersPath)
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
			if len(cfg.Providers) != 0 {
				t.Fatalf("providers count = %d, want 0", len(cfg.Providers))
			}
		})
	}
}

func TestProviderRequestURLAzureLegacyBaseURLAppendsAPIVersion(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{}
	provider := &providerRuntime{
		kind:       providerTypeAzureOpenAI,
		baseURL:    "https://example.openai.azure.com/openai",
		apiVersion: "2025-04-01-preview",
	}

	modelsURL, err := handler.providerRequestURL(provider, "/models", "")
	if err != nil {
		t.Fatalf("providerRequestURL() error = %v", err)
	}
	if modelsURL != "https://example.openai.azure.com/openai/models?api-version=2025-04-01-preview" {
		t.Fatalf("providerRequestURL() = %q", modelsURL)
	}
}

func TestBuildProvidersAzureLegacyBaseURLAccepted(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:         "azure-openai",
			Type:       "azure-openai",
			BaseURL:    "https://example.openai.azure.com/openai",
			APIKey:     "test-key",
			APIVersion: "2025-04-01-preview",
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-4.1",
				Deployment: "gpt-4.1",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}

	provider := providers["azure-openai"]
	if provider == nil {
		t.Fatal("expected azure-openai provider to be built")
	}
	if provider.baseURL != "https://example.openai.azure.com/openai" {
		t.Fatalf("provider.baseURL = %q, want Azure /openai endpoint", provider.baseURL)
	}
}

func TestBuildConfiguredProviderSetupRejectsStaticModelCollision(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	_, err := handler.buildConfiguredProviderSetup(context.Background(), ProvidersConfig{
		Providers: []ProviderConfig{
			{
				ID:      "azure-a",
				Type:    "azure-openai",
				Default: true,
				BaseURL: "https://a.openai.azure.com/openai/v1",
				APIKey:  "test-key-a",
				Models: []ProviderModelConfig{{
					PublicID: "gpt-5.4",
				}},
			},
			{
				ID:      "azure-b",
				Type:    "azure-openai",
				BaseURL: "https://b.openai.azure.com/openai/v1",
				APIKey:  "test-key-b",
				Models: []ProviderModelConfig{{
					PublicID: "gpt-5.4",
				}},
			},
		},
	})
	if err == nil {
		t.Fatal("buildConfiguredProviderSetup() error = nil, want model collision")
	}
	if !strings.Contains(err.Error(), "gpt-5.4") || !strings.Contains(err.Error(), "azure-a") || !strings.Contains(err.Error(), "azure-b") {
		t.Fatalf("expected static collision details, got %v", err)
	}
}

func TestReplaceProviderModelsFiltersBeforeCollisionCheck(t *testing.T) {
	ps := &providerSetup{
		providers: map[string]*providerRuntime{
			"copilot": {
				id:            "copilot",
				kind:          providerTypeCopilot,
				includeModels: map[string]struct{}{},
				excludeModels: map[string]struct{}{"gpt-5.5": {}},
			},
			"codex": {
				id:            "codex",
				kind:          providerTypeOpenAICodex,
				includeModels: map[string]struct{}{"gpt-5.5": {}},
				excludeModels: map[string]struct{}{},
			},
		},
		models: map[string]providerModel{
			"gpt-5.5": {publicID: "gpt-5.5", providerID: "codex"},
			"legacy":  {publicID: "legacy", providerID: "copilot"},
		},
	}

	err := ps.replaceProviderModels("copilot", []providerModel{
		{publicID: "gpt-5.5", providerID: "copilot"},
		{publicID: "gpt-5.4", providerID: "copilot"},
	})
	if err != nil {
		t.Fatalf("replaceProviderModels returned error before filtering exclusions: %v", err)
	}

	if model, ok := ps.lookupModel("gpt-5.5"); !ok || model.providerID != "codex" {
		t.Fatalf("expected excluded gpt-5.5 to remain owned by codex, got %+v, ok=%v", model, ok)
	}
	if model, ok := ps.lookupModel("gpt-5.4"); !ok || model.providerID != "copilot" {
		t.Fatalf("expected allowed gpt-5.4 to be refreshed for copilot, got %+v, ok=%v", model, ok)
	}
	if _, ok := ps.lookupModel("legacy"); ok {
		t.Fatal("expected replacing copilot models to remove stale copilot-owned legacy model")
	}
}

func TestBuildProvidersOpenAICodexDefaultBaseURLAndFilters(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	providers, _, defaultProviderID, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:            "codex",
			Type:          "openai-codex",
			Default:       true,
			IncludeModels: []string{"gpt-5.5"},
			ExcludeModels: []string{"gpt-5.4"},
		}},
	})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	if defaultProviderID != "codex" {
		t.Fatalf("default provider = %q, want codex", defaultProviderID)
	}

	provider := providers["codex"]
	if provider == nil {
		t.Fatal("expected codex provider to be built")
	}
	if provider.baseURL != defaultOpenAICodexBaseURL {
		t.Fatalf("provider.baseURL = %q, want %q", provider.baseURL, defaultOpenAICodexBaseURL)
	}
	if !provider.allowsModel("gpt-5.5") {
		t.Fatal("expected include_models to allow gpt-5.5")
	}
	if provider.allowsModel("gpt-5.4") {
		t.Fatal("expected exclude_models to block gpt-5.4")
	}
	if provider.allowsModel("gpt-other") {
		t.Fatal("expected include_models to block gpt-other")
	}
}

func TestBuildProvidersOpenAICodexMalformedBaseURLRejected(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	_, _, _, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:      "codex",
			Type:    "openai-codex",
			BaseURL: "https://chatgpt.com/backend-api/codex?client_version=1.0.0",
		}},
	})
	if err == nil {
		t.Fatal("buildProviders() error = nil, want malformed OpenAI Codex base_url error")
	}
	if !strings.Contains(err.Error(), "no query string or fragment") {
		t.Fatalf("buildProviders() error = %v, want query/fragment guidance", err)
	}
}

func TestBuildProvidersAzureModelsBaseURLRejected(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	_, _, _, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:      "azure-openai",
			Type:    "azure-openai",
			BaseURL: "https://example.services.ai.azure.com/models",
			APIKey:  "test-key",
			Models: []ProviderModelConfig{{
				PublicID: "Kimi-K2.6",
			}},
		}},
	})
	if err == nil {
		t.Fatal("buildProviders() error = nil, want unsupported /models base_url error")
	}
	if !strings.Contains(err.Error(), "use the OpenAI-compatible endpoint ending in /openai/v1 instead") {
		t.Fatalf("buildProviders() error = %v, want /openai/v1 guidance", err)
	}
}

func TestBuildProvidersAzureUnsupportedBaseURLRejected(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	_, _, _, err := handler.buildProviders(ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:      "azure-openai",
			Type:    "azure-openai",
			BaseURL: "https://example.services.ai.azure.com/inference",
			APIKey:  "test-key",
			Models: []ProviderModelConfig{{
				PublicID: "Kimi-K2.6",
			}},
		}},
	})
	if err == nil {
		t.Fatal("buildProviders() error = nil, want unsupported Azure base_url error")
	}
	if !strings.Contains(err.Error(), "expected an absolute URL whose path ends in /openai/v1 or /openai") {
		t.Fatalf("buildProviders() error = %v, want supported Azure base_url guidance", err)
	}
}

func TestBuildProvidersAzureMalformedBaseURLRejected(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		baseURL string
	}{
		{name: "missing scheme", baseURL: "example.openai.azure.com/openai/v1"},
		{name: "missing host", baseURL: "https:///openai/v1"},
		{name: "query string", baseURL: "https://example.openai.azure.com/openai/v1?api-version=2025-04-01-preview"},
		{name: "empty query string", baseURL: "https://example.openai.azure.com/openai/v1?"},
		{name: "fragment", baseURL: "https://example.openai.azure.com/openai/v1#chat"},
		{name: "empty fragment", baseURL: "https://example.openai.azure.com/openai/v1#"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
			_, _, _, err := handler.buildProviders(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:      "azure-openai",
					Type:    "azure-openai",
					BaseURL: tc.baseURL,
					APIKey:  "test-key",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-4.1",
						Deployment: "gpt-4.1",
					}},
				}},
			})
			if err == nil {
				t.Fatalf("buildProviders() error = nil for base_url %q, want unsupported Azure base_url error", tc.baseURL)
			}
			if !strings.Contains(err.Error(), "expected an absolute URL whose path ends in /openai/v1 or /openai") {
				t.Fatalf("buildProviders() error = %v, want absolute Azure base_url guidance", err)
			}
			if !strings.Contains(err.Error(), "no query string or fragment") {
				t.Fatalf("buildProviders() error = %v, want query/fragment guidance", err)
			}
		})
	}
}

func TestLoadProvidersConfigFileAzureIdentityAuth(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		ext       string
		body      string
		wantScope string
	}{
		{
			name: "JSON",
			ext:  ".json",
			body: `{
  "providers": [{
    "id": "foundry",
    "type": "azure-openai",
    "auth_mode": "azure_identity",
    "token_scope": "https://custom.example/.default",
    "base_url": "https://example.services.ai.azure.com/api/projects/project/openai/v1",
    "models": [{"public_id":"gpt-5.4","deployment":"gpt-5.4","endpoints":["/responses"]}]
  }]
}`,
			wantScope: "https://custom.example/.default",
		},
		{
			name: "YAML",
			ext:  ".yaml",
			body: `providers:
  - id: foundry
    type: azure-openai
    auth_mode: azure_identity
    token_scope: https://custom.example/.default
    base_url: https://example.services.ai.azure.com/api/projects/project/openai/v1
    models:
      - public_id: gpt-5.4
        deployment: gpt-5.4
        endpoints:
          - /responses
`,
			wantScope: "https://custom.example/.default",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			providersPath := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(providersPath, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg, err := LoadProvidersConfigFile(providersPath)
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
			providerCfg := cfg.Providers[0]
			if providerCfg.AuthMode != "azure_identity" {
				t.Fatalf("auth_mode = %q, want azure_identity", providerCfg.AuthMode)
			}
			if providerCfg.TokenScope != tc.wantScope {
				t.Fatalf("token_scope = %q, want %q", providerCfg.TokenScope, tc.wantScope)
			}

			factory := &recordingAzureIdentityFactory{source: &staticAzureTokenSource{token: "entra-token"}}
			handler := &ProxyHandler{
				copilotURL:                      "https://copilot.example.com",
				azureIdentityTokenSourceFactory: factory.factory,
			}
			providers, _, defaultProviderID, err := handler.buildProviders(cfg)
			if err != nil {
				t.Fatalf("buildProviders() error = %v", err)
			}
			if defaultProviderID != "foundry" {
				t.Fatalf("default provider = %q, want foundry", defaultProviderID)
			}
			provider := providers["foundry"]
			if provider == nil {
				t.Fatal("expected foundry provider to be built")
			}
			if provider.authMode != providerAuthModeAzureIdentity {
				t.Fatalf("provider.authMode = %q, want azure_identity", provider.authMode)
			}
			if provider.tokenScope != tc.wantScope || factory.scope != tc.wantScope {
				t.Fatalf("token scopes = provider %q factory %q, want %q", provider.tokenScope, factory.scope, tc.wantScope)
			}
			if provider.apiKey != "" {
				t.Fatalf("provider.apiKey = %q, want empty for Azure identity", provider.apiKey)
			}
			if provider.azureToken == nil {
				t.Fatal("provider.azureToken = nil, want configured token source")
			}
			if factory.calls.Load() != 1 {
				t.Fatalf("Azure identity factory calls = %d, want 1", factory.calls.Load())
			}
		})
	}
}

func TestBuildProvidersAzureIdentityDefaultScope(t *testing.T) {
	t.Parallel()

	factory := &recordingAzureIdentityFactory{source: &staticAzureTokenSource{token: "entra-token"}}
	handler := &ProxyHandler{
		copilotURL:                      "https://copilot.example.com",
		azureIdentityTokenSourceFactory: factory.factory,
	}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:       "foundry",
		Type:     "azure-openai",
		BaseURL:  "https://example.services.ai.azure.com/api/projects/project/openai/v1",
		AuthMode: "azure_identity",
		Models: []ProviderModelConfig{{
			PublicID: "gpt-5.4",
		}},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["foundry"]
	if provider == nil {
		t.Fatal("expected foundry provider to be built")
	}
	if provider.tokenScope != defaultAzureIdentityTokenScope {
		t.Fatalf("provider.tokenScope = %q, want %q", provider.tokenScope, defaultAzureIdentityTokenScope)
	}
	if factory.scope != defaultAzureIdentityTokenScope {
		t.Fatalf("factory scope = %q, want %q", factory.scope, defaultAzureIdentityTokenScope)
	}
}

func TestBuildProvidersAzureAuthModeValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		cfg     ProviderConfig
		wantErr string
	}{
		{
			name: "unknown auth mode",
			cfg: ProviderConfig{
				AuthMode: "managed_identity",
			},
			wantErr: "unsupported auth_mode",
		},
		{
			name: "azure identity rejects api key",
			cfg: ProviderConfig{
				AuthMode: "azure_identity",
				APIKey:   "key",
			},
			wantErr: "cannot be combined with api_key or api_key_env",
		},
		{
			name: "azure identity rejects api key env",
			cfg: ProviderConfig{
				AuthMode:  "azure_identity",
				APIKeyEnv: "AZURE_OPENAI_API_KEY",
			},
			wantErr: "cannot be combined with api_key or api_key_env",
		},
		{
			name: "api key mode rejects token scope",
			cfg: ProviderConfig{
				AuthMode:   "api_key",
				APIKey:     "key",
				TokenScope: "https://ai.azure.com/.default",
			},
			wantErr: "token_scope is only valid",
		},
		{
			name:    "api key mode still requires key",
			cfg:     ProviderConfig{},
			wantErr: "must set api_key or api_key_env",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := tc.cfg
			cfg.ID = "azure"
			cfg.Type = "azure-openai"
			cfg.BaseURL = "https://example.openai.azure.com/openai/v1"
			cfg.Models = []ProviderModelConfig{{PublicID: "gpt-5.4"}}

			handler := &ProxyHandler{
				copilotURL: "https://copilot.example.com",
				azureIdentityTokenSourceFactory: func(string, string) (azureTokenSource, error) {
					return &staticAzureTokenSource{token: "entra-token"}, nil
				},
			}
			_, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{cfg}})
			if err == nil {
				t.Fatal("buildProviders() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("buildProviders() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInitializeProvidersCachesDefaultProviderSetup(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{copilotURL: "https://copilot.example.test/"}
	if err := handler.initializeProviders(); err != nil {
		t.Fatalf("initializeProviders() error = %v", err)
	}

	first := handler.providerSetup()
	second := handler.providerSetup()
	if first == nil {
		t.Fatal("providerSetup() = nil, want default setup")
	}
	if first != second {
		t.Fatal("providerSetup() returned different default setup pointers; want cached setup")
	}
	if first.hasConfiguredState {
		t.Fatal("default setup hasConfiguredState = true, want false")
	}
	if got, want := first.defaultProviderID, "copilot"; got != want {
		t.Fatalf("defaultProviderID = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(first.providerOrder, []string{"copilot"}) {
		t.Fatalf("providerOrder = %v, want [copilot]", first.providerOrder)
	}

	provider := first.defaultProvider()
	if provider == nil {
		t.Fatal("defaultProvider() = nil, want copilot provider")
	}
	if got, want := provider.baseURL, "https://copilot.example.test"; got != want {
		t.Fatalf("default provider baseURL = %q, want %q", got, want)
	}
}

func TestInitializeProvidersConfiguredSetupStillUsesConfiguredState(t *testing.T) {
	t.Parallel()

	handler := &ProxyHandler{
		copilotURL: "https://copilot.example.test",
		providersConfig: ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:       "local-openai",
				Type:     "openai-compatible",
				Default:  true,
				BaseURL:  "https://local-openai.example.test",
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID: "local-chat",
				}},
			}},
		},
	}
	if err := handler.initializeProviders(); err != nil {
		t.Fatalf("initializeProviders() error = %v", err)
	}

	setup := handler.providerSetup()
	if setup == nil {
		t.Fatal("providerSetup() = nil, want configured setup")
	}
	if !setup.hasConfiguredState {
		t.Fatal("configured setup hasConfiguredState = false, want true")
	}
	if got, want := setup.defaultProviderID, "local-openai"; got != want {
		t.Fatalf("defaultProviderID = %q, want %q", got, want)
	}
	if _, ok := setup.providers["copilot"]; ok {
		t.Fatal("configured setup unexpectedly contains zero-config copilot provider")
	}
	model, ok := setup.lookupModel("local-chat")
	if !ok {
		t.Fatal("lookupModel(local-chat) = false, want configured model")
	}
	if got, want := model.providerID, "local-openai"; got != want {
		t.Fatalf("model providerID = %q, want %q", got, want)
	}
}

func BenchmarkDefaultProviderSetup(b *testing.B) {
	handler := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	if err := handler.initializeProviders(); err != nil {
		b.Fatalf("initializeProviders() error = %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkProviderSetupSink = handler.providerSetup()
	}
}

func TestAllowedModelsRestrictsCentralModelResolution(t *testing.T) {
	cfg := ProvidersConfig{Providers: []ProviderConfig{{
		ID:             "local",
		Type:           "openai-compatible",
		BaseURL:        "http://127.0.0.1:9/v1",
		AuthType:       "none",
		ModelDiscovery: "static",
		Models: []ProviderModelConfig{
			{PublicID: "allowed", Endpoints: []string{"/chat/completions"}},
			{PublicID: "other", Endpoints: []string{"/chat/completions"}},
		},
	}}}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithAllowedModels("allowed"),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	if provider, _, _ := h.resolveProviderModel("allowed", providerEndpointChatCompletions); provider == nil {
		t.Fatal("allowed model did not resolve")
	}
	if provider, _, _ := h.resolveProviderModel("other", providerEndpointChatCompletions); provider != nil {
		t.Fatal("disallowed model resolved")
	}

	_, _, _, err = h.resolveProviderRequestForModel(
		[]byte(`{"model":"other"}`),
		providerEndpointResponses,
		"other",
	)
	if got := upstreamStatusCode(err, http.StatusInternalServerError); got != http.StatusBadRequest {
		t.Fatalf("disallowed Responses model status = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("disallowed Responses model error = %v, want model-not-allowed error", err)
	}

	_, err = h.resolveChatRoute(context.Background(), "other")
	if got := upstreamStatusCode(err, http.StatusInternalServerError); got != http.StatusBadRequest {
		t.Fatalf("disallowed Chat model status = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("disallowed Chat model error = %v, want model-not-allowed error", err)
	}
}

func TestAllowedModelsDoesNotNormalizeKnownDisallowedModel(t *testing.T) {
	cfg := ProvidersConfig{Providers: []ProviderConfig{
		{
			ID:             "raw-owner",
			Type:           "anthropic-compatible",
			BaseURL:        "http://127.0.0.1:9/v1",
			AuthType:       "none",
			Default:        true,
			ModelDiscovery: "static",
			Models: []ProviderModelConfig{{
				PublicID:  "claude-sonnet-4-5",
				Endpoints: []string{"/v1/messages"},
			}},
		},
		{
			ID:             "normalized-owner",
			Type:           "anthropic-compatible",
			BaseURL:        "http://127.0.0.1:10/v1",
			AuthType:       "none",
			ModelDiscovery: "static",
			Models: []ProviderModelConfig{{
				PublicID:  "claude-sonnet-4.5",
				Endpoints: []string{"/v1/messages"},
			}},
		},
	}}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithAllowedModels("claude-sonnet-4.5"),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	if h.modelAllowedForRequest("claude-sonnet-4-5", providerEndpointMessages) {
		t.Fatal("known disallowed raw model was accepted as a normalized alias")
	}
	_, _, _, err = h.resolveProviderRequestForModel(
		[]byte(`{"model":"claude-sonnet-4-5"}`),
		providerEndpointMessages,
		"claude-sonnet-4-5",
	)
	if got := upstreamStatusCode(err, http.StatusInternalServerError); got != http.StatusBadRequest {
		t.Fatalf("known disallowed raw model status = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
}
