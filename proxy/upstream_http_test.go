package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newProviderRoutingTestHandler(t *testing.T, endpoints []string) *ProxyHandler {
	t.Helper()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "azure",
				Type:    "azure-openai",
				Default: true,
				BaseURL: "https://example.openai.azure.com/openai/v1",
				APIKey:  "azure-test-key",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  append([]string(nil), endpoints...),
					Name:       "GPT-5 Public",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler
}

func TestExtractRequestModel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "top level model",
			body: `{"model":"gpt-4.1","input":"hello"}`,
			want: "gpt-4.1",
		},
		{
			name: "nested content before model",
			body: `{"input":{"messages":[{"role":"user","content":"hi"}],"metadata":{"nested":[1,2,3]}},"model":"  gpt-4o-mini  "}`,
			want: "gpt-4o-mini",
		},
		{
			name: "nested model key is ignored",
			body: `{"input":{"model":"wrong"},"metadata":{"flags":["a","b"]}}`,
			want: "",
		},
		{
			name: "non string model is ignored",
			body: `{"model":123,"input":"hello"}`,
			want: "",
		},
		{
			name: "non object payload is ignored",
			body: `["gpt-4.1"]`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractRequestModel([]byte(tt.body)); got != tt.want {
				t.Fatalf("extractRequestModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveProviderRequest_RewritesConfiguredResponsesModelToProviderDeployment(t *testing.T) {
	handler := newProviderRoutingTestHandler(t, []string{"/responses"})

	provider, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"gpt-5-public","input":"hello"}`), "/responses")
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if provider == nil {
		t.Fatal("resolveProviderRequest() provider = nil, want azure provider")
	}
	if provider.id != "azure" {
		t.Fatalf("resolveProviderRequest() provider.id = %q, want azure", provider.id)
	}
	if got := extractResponsesRequestModel(rewrittenBody); got != "gpt-5-4-prod" {
		t.Fatalf("rewritten model = %q, want gpt-5-4-prod", got)
	}

	var payload struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(rewrittenBody, &payload); err != nil {
		t.Fatalf("json.Unmarshal(rewrittenBody) error = %v", err)
	}
	if payload.Input != "hello" {
		t.Fatalf("rewritten input = %q, want hello", payload.Input)
	}
}

func TestResolveProviderRequest_RejectsKnownModelWithoutEndpointSupport(t *testing.T) {
	handler := newProviderRoutingTestHandler(t, []string{"/responses"})

	provider, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"gpt-5-public","messages":[{"role":"user","content":"hello"}]}`), "/chat/completions")
	if err == nil {
		t.Fatal("resolveProviderRequest() error = nil, want unsupported endpoint error")
	}
	if provider != nil {
		t.Fatalf("resolveProviderRequest() provider = %#v, want nil on error", provider)
	}
	if rewrittenBody != nil {
		t.Fatalf("resolveProviderRequest() rewrittenBody = %q, want nil on error", rewrittenBody)
	}

	var providerErr *providerRequestError
	if !errors.As(err, &providerErr) {
		t.Fatalf("resolveProviderRequest() error = %T, want *providerRequestError", err)
	}
	if providerErr.statusCode != http.StatusBadRequest {
		t.Fatalf("providerRequestError.statusCode = %d, want %d", providerErr.statusCode, http.StatusBadRequest)
	}
	if !strings.Contains(providerErr.Error(), `does not support /chat/completions`) {
		t.Fatalf("providerRequestError.Error() = %q, want unsupported endpoint message", providerErr.Error())
	}
}

func TestRewriteRequestModelForProvider_RewritesGenericJSONModelAndNoopsWhenUnchanged(t *testing.T) {
	originalBody := []byte(`{"model":"gpt-5-public","messages":[{"role":"user","content":"hello"}]}`)

	rewrittenBody, rewritten, err := rewriteRequestModelForProvider(originalBody, "gpt-5-4-prod")
	if err != nil {
		t.Fatalf("rewriteRequestModelForProvider() error = %v", err)
	}
	if !rewritten {
		t.Fatal("rewriteRequestModelForProvider() rewritten = false, want true")
	}
	if got := extractRequestModel(rewrittenBody); got != "gpt-5-4-prod" {
		t.Fatalf("rewritten model = %q, want gpt-5-4-prod", got)
	}

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rewrittenBody, &payload); err != nil {
		t.Fatalf("json.Unmarshal(rewrittenBody) error = %v", err)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Content != "hello" {
		t.Fatalf("rewritten messages = %+v, want original message content preserved", payload.Messages)
	}

	unchangedBody, rewritten, err := rewriteRequestModelForProvider(rewrittenBody, "gpt-5-4-prod")
	if err != nil {
		t.Fatalf("rewriteRequestModelForProvider(already mapped) error = %v", err)
	}
	if rewritten {
		t.Fatal("rewriteRequestModelForProvider(already mapped) rewritten = true, want false")
	}
	if string(unchangedBody) != string(rewrittenBody) {
		t.Fatalf("rewriteRequestModelForProvider(already mapped) body changed: got %s want %s", unchangedBody, rewrittenBody)
	}
}
