package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterpolateProvidersConfigEnvResolvesNestedStrings(t *testing.T) {
	t.Setenv("VEKIL_TEST_HOST", "gateway.example.invalid")
	t.Setenv("VEKIL_TEST_PATH", "v1")
	t.Setenv("VEKIL_TEST_MODEL", "test-model")

	cfg := ProvidersConfig{Providers: []ProviderConfig{{
		ID:            "azure",
		Type:          "azure-openai",
		BaseURL:       "https://${env:VEKIL_TEST_HOST}/${env:VEKIL_TEST_PATH}",
		APIKeyEnv:     "${env:VEKIL_TEST_MODEL}",
		IncludeModels: []string{"prefix-${env:VEKIL_TEST_MODEL}"},
		ExtraHeaders:  map[string]string{"X-Test-Model": "${env:VEKIL_TEST_MODEL}"},
		Models: []ProviderModelConfig{{
			PublicID:   "public-model",
			Deployment: "${env:VEKIL_TEST_MODEL}",
		}},
	}}}

	if err := interpolateProvidersConfigEnv(&cfg, false); err != nil {
		t.Fatalf("interpolateProvidersConfigEnv() error = %v", err)
	}
	provider := cfg.Providers[0]
	if provider.BaseURL != "https://gateway.example.invalid/v1" {
		t.Fatalf("base_url = %q", provider.BaseURL)
	}
	if got := provider.IncludeModels[0]; got != "prefix-test-model" {
		t.Fatalf("include_models[0] = %q", got)
	}
	if got := provider.ExtraHeaders["X-Test-Model"]; got != "test-model" {
		t.Fatalf("extra_headers value = %q", got)
	}
	if got := provider.Models[0].Deployment; got != "test-model" {
		t.Fatalf("models[0].deployment = %q", got)
	}
	if provider.APIKeyEnv != "${env:VEKIL_TEST_MODEL}" {
		t.Fatalf("api_key_env = %q, want interpolation excluded", provider.APIKeyEnv)
	}
}

func TestInterpolateProvidersConfigEnvReportsMissingAndEmptyValues(t *testing.T) {
	const missingName = "VEKIL_TEST_INTERPOLATION_UNDEFINED"
	original, existed := os.LookupEnv(missingName)
	if err := os.Unsetenv(missingName); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(missingName, original)
		} else {
			_ = os.Unsetenv(missingName)
		}
	})

	cfg := ProvidersConfig{Providers: []ProviderConfig{{ID: "azure", APIKey: "${env:" + missingName + "}"}}}
	err := interpolateProvidersConfigEnv(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), `provider "azure" field "api_key" references undefined env var `+missingName) {
		t.Fatalf("missing variable error = %v", err)
	}

	t.Setenv("VEKIL_TEST_INTERPOLATION_EMPTY", "")
	cfg.Providers[0].APIKey = "${env:VEKIL_TEST_INTERPOLATION_EMPTY}"
	err = interpolateProvidersConfigEnv(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), "references empty env var VEKIL_TEST_INTERPOLATION_EMPTY") {
		t.Fatalf("empty variable error = %v", err)
	}
}

func TestInterpolateProvidersConfigEnvDoesNotDiscloseValues(t *testing.T) {
	t.Setenv("VEKIL_TEST_PRIVATE_VALUE", "value-that-must-not-appear")
	t.Setenv("VEKIL_TEST_UNDEFINED_AFTER_VALUE", "")
	cfg := ProvidersConfig{Providers: []ProviderConfig{{
		ID:      "azure",
		BaseURL: "${env:VEKIL_TEST_PRIVATE_VALUE}/${env:VEKIL_TEST_UNDEFINED_AFTER_VALUE}",
	}}}

	err := interpolateProvidersConfigEnv(&cfg, false)
	if err == nil {
		t.Fatal("interpolateProvidersConfigEnv() error = nil")
	}
	if strings.Contains(err.Error(), "value-that-must-not-appear") {
		t.Fatalf("error disclosed resolved value: %v", err)
	}
}

func TestInterpolateProviderConfigStringsHandlesEscapesAndNonStrings(t *testing.T) {
	t.Setenv("VEKIL_TEST_LITERAL", "test-value")
	text := "${env:VEKIL_TEST_LITERAL}"
	type fixture struct {
		Escaped string            `json:"escaped"`
		Pointer *string           `json:"pointer"`
		Dynamic any               `json:"dynamic"`
		Count   int               `json:"count"`
		Enabled bool              `json:"enabled"`
		Raw     json.RawMessage   `json:"raw"`
		Bytes   []byte            `json:"bytes"`
		Map     map[string]string `json:"map"`
	}
	value := fixture{
		Escaped: `\${env:VEKIL_TEST_LITERAL}`,
		Pointer: &text,
		Dynamic: []any{"${env:VEKIL_TEST_LITERAL}", 42},
		Count:   7,
		Enabled: true,
		Raw:     json.RawMessage(`{"value":"${env:VEKIL_TEST_LITERAL}"}`),
		Bytes:   []byte("${env:VEKIL_TEST_LITERAL}"),
		Map:     map[string]string{"${env:VEKIL_TEST_LITERAL}": "${env:VEKIL_TEST_LITERAL}"},
	}

	if err := interpolateProviderConfigStrings(&value, providerEnvInterpolationLocal); err != nil {
		t.Fatalf("interpolateProviderConfigStrings() error = %v", err)
	}
	if value.Escaped != "${env:VEKIL_TEST_LITERAL}" || value.Pointer == nil || *value.Pointer != "test-value" {
		t.Fatalf("escaped = %q, pointer = %v", value.Escaped, value.Pointer)
	}
	dynamic := value.Dynamic.([]any)
	if dynamic[0] != "test-value" || dynamic[1] != 42 {
		t.Fatalf("dynamic = %#v", dynamic)
	}
	if value.Count != 7 || !value.Enabled {
		t.Fatalf("non-string fields changed: count=%d enabled=%v", value.Count, value.Enabled)
	}
	if string(value.Raw) != `{"value":"${env:VEKIL_TEST_LITERAL}"}` || string(value.Bytes) != "${env:VEKIL_TEST_LITERAL}" {
		t.Fatalf("raw fields changed: raw=%s bytes=%s", value.Raw, value.Bytes)
	}
	if got := value.Map["${env:VEKIL_TEST_LITERAL}"]; got != "test-value" {
		t.Fatalf("map key changed or value unresolved: %#v", value.Map)
	}
}

func TestInterpolateProvidersConfigEnvRejectsMalformedExpressions(t *testing.T) {
	tests := []string{
		"${env:}",
		"${env:9INVALID}",
		"${env:VALID:-default}",
		"${env:UNCLOSED",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			cfg := ProvidersConfig{InsightModel: input}
			err := interpolateProvidersConfigEnv(&cfg, false)
			if err == nil || !strings.Contains(err.Error(), `field "insight_model" contains malformed env interpolation`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadProvidersConfigFileInterpolatesYAMLAndJSON(t *testing.T) {
	t.Setenv("VEKIL_TEST_AZURE_BASE_URL", "https://example.openai.azure.com/openai/v1")
	t.Setenv("VEKIL_TEST_AZURE_VALUE", "test-value")

	tests := map[string]string{
		"providers.yaml": `providers:
  - id: azure
    type: azure-openai
    base_url: ${env:VEKIL_TEST_AZURE_BASE_URL}
    api_key: ${env:VEKIL_TEST_AZURE_VALUE}
    models:
      - public_id: test-model
        deployment: test-deployment
`,
		"providers.json": `{
  "providers": [{
    "id": "azure",
    "type": "azure-openai",
    "base_url": "${env:VEKIL_TEST_AZURE_BASE_URL}",
    "api_key": "${env:VEKIL_TEST_AZURE_VALUE}",
    "models": [{"public_id":"test-model","deployment":"test-deployment"}]
  }]
}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			cfg, err := LoadProvidersConfigFile(path)
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
			if got := cfg.Providers[0].BaseURL; got != "https://example.openai.azure.com/openai/v1" {
				t.Fatalf("base_url = %q", got)
			}
			if got := cfg.Providers[0].APIKey; got != "test-value" {
				t.Fatalf("api_key = %q", got)
			}
		})
	}
}

func TestLoadProvidersConfigFileRemoteInterpolationPolicy(t *testing.T) {
	t.Setenv("VEKIL_TEST_REMOTE_VALUE", "test-value")
	body := `providers:
  - id: copilot
    type: copilot
    headers:
      default:
        editor_version: ${env:VEKIL_TEST_REMOTE_VALUE}
`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(body))
	}))
	defer server.Close()

	_, err := LoadProvidersConfigFile(server.URL + "/providers.yaml")
	if err == nil || !strings.Contains(err.Error(), "not allowed in HTTP(S)-loaded provider configs") {
		t.Fatalf("remote interpolation error = %v", err)
	}

	escapedBody := strings.Replace(body, "${env:", `\${env:`, 1)
	escapedServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(escapedBody))
	}))
	defer escapedServer.Close()
	cfg, err := LoadProvidersConfigFile(escapedServer.URL + "/providers.yaml")
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile(escaped) error = %v", err)
	}
	if got := cfg.Providers[0].Headers.Default.EditorVersion; got != "${env:VEKIL_TEST_REMOTE_VALUE}" {
		t.Fatalf("escaped remote value = %q", got)
	}
}
