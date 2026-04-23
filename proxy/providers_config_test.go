package proxy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	if cfgModel.ContextWindow == nil || *cfgModel.ContextWindow != 400000 {
		t.Fatalf("context_window = %v, want 400000", cfgModel.ContextWindow)
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
	if !strings.Contains(err.Error(), "expected a URL ending in /openai/v1 or /openai") {
		t.Fatalf("buildProviders() error = %v, want supported Azure base_url suffixes", err)
	}
}
