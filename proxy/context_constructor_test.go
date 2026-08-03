package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestNewProxyHandlerContextCancelsDynamicProviderInitialization(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := NewProxyHandlerContext(
			ctx,
			auth.NewTestAuthenticator("test-token"),
			logger.NewWithWriter(logger.LevelError, io.Discard),
			WithCopilotBaseURL(upstream.URL),
			WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
				ID: "copilot", Type: "copilot", Default: true, IncludeModels: []string{"wanted-model"},
			}}}),
		)
		done <- err
	}()
	select {
	case <-started:
	case <-t.Context().Done():
		t.Fatal("dynamic provider discovery did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("NewProxyHandlerContext() error = %v, want context canceled", err)
		}
	case <-t.Context().Done():
		t.Fatal("constructor did not honor cancellation")
	}
}

func TestProviderSecretResolverIsAuthoritative(t *testing.T) {
	const reference = "VEKIL_MANAGED_PROVIDER_API_KEY_1"
	t.Setenv(reference, "environment-secret")
	cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Default: true,
		BaseURL: "https://example.test/v1", AuthType: "bearer", APIKeyEnv: reference,
		ModelDiscovery: "static", Models: []ProviderModelConfig{{PublicID: "test-model", Endpoints: []string{"/chat/completions"}}},
	}}}

	_, err := NewProxyHandlerContext(
		t.Context(), auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithProviderSecretResolver(ProviderSecretResolverFunc(func(string, string) (string, bool) { return "", false })),
	)
	if err == nil {
		t.Fatal("authoritative resolver unexpectedly fell back to os.Environ")
	}

	h, err := NewProxyHandlerContext(
		t.Context(), auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithProviderSecretResolver(ProviderSecretResolverFunc(func(providerID, gotReference string) (string, bool) {
			if providerID != "local" || gotReference != reference {
				t.Fatalf("resolver scope = (%q, %q)", providerID, gotReference)
			}
			return "managed-secret", true
		})),
	)
	if err != nil {
		t.Fatalf("NewProxyHandlerContext() error = %v", err)
	}
	if got := h.providerSetup().providerByID("local").apiKey; got != "managed-secret" {
		t.Fatalf("provider API key = %q, want managed projection", got)
	}
}

func TestLoadProvidersConfigBytesUsesImmutableSnapshot(t *testing.T) {
	body := []byte("schema_version: 2\nproviders:\n  - id: copilot\n    type: copilot\n    default: true\n")
	cfg, err := LoadProvidersConfigBytes("providers.yaml", body)
	if err != nil {
		t.Fatalf("LoadProvidersConfigBytes() error = %v", err)
	}
	body[0] = 'X'
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "copilot" {
		t.Fatalf("decoded config changed with caller bytes: %+v", cfg)
	}
}

func TestInitialManagedCopilotConfigMatchesZeroConfigProviderSemantics(t *testing.T) {
	body := []byte("schema_version: 2\nproviders:\n  - id: copilot\n    type: copilot\n    default: true\n")
	cfg, err := LoadProvidersConfigBytes("providers.yaml", body)
	if err != nil {
		t.Fatal(err)
	}
	newHandler := func(opts ...Option) *ProxyHandler {
		h, err := NewProxyHandlerContext(t.Context(), auth.NewTestAuthenticator("token"), logger.NewWithWriter(logger.LevelError, io.Discard), opts...)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	legacy := newHandler()
	managed := newHandler(WithProvidersConfig(cfg), WithDeferredDynamicProviderModelValidation(true))
	legacyProvider := legacy.providerSetup().providerByID("copilot")
	managedProvider := managed.providerSetup().providerByID("copilot")
	if legacyProvider == nil || managedProvider == nil {
		t.Fatal("missing Copilot provider")
	}
	if legacyProvider.kind != managedProvider.kind || legacyProvider.baseURL != managedProvider.baseURL || legacyProvider.paths != managedProvider.paths || !legacyProvider.isDefault || !managedProvider.isDefault {
		t.Fatalf("legacy provider = %+v, managed provider = %+v", legacyProvider, managedProvider)
	}
	if legacy.UsesCopilot() != managed.UsesCopilot() {
		t.Fatal("UsesCopilot differs")
	}
	for _, model := range []string{"gpt-5", "claude-sonnet-4.5", "unknown-model"} {
		if legacy.ModelUsesCopilot(model) != managed.ModelUsesCopilot(model) {
			t.Fatalf("ModelUsesCopilot(%q) differs", model)
		}
	}
}
