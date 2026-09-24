package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const routeAuthProtocolCredentialError = "DefaultAzureCredential: AzureCLICredential: Azure CLI not found on PATH"

func TestExplicitRouteCredentialFailureProtocol(t *testing.T) {
	for _, protocol := range []string{"HTTP", "WebSocket"} {
		t.Run(protocol, func(t *testing.T) {
			var sends atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				return nil, fmt.Errorf("unexpected upstream request: %s %s", req.Method, req.URL)
			})
			handler, sources := newRouteAuthProtocolHandler(t, false, transport)

			status, payload := sendRouteAuthProtocolRequest(t, handler, protocol)
			if status != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503; payload = %+v", status, payload)
			}
			detail, ok := payload["error"].(map[string]interface{})
			if !ok {
				t.Fatalf("error = %T, want object; payload = %+v", payload["error"], payload)
			}
			if got := detail["code"]; got != "upstream_auth_unavailable" {
				t.Errorf("error code = %v, want upstream_auth_unavailable", got)
			}
			message, _ := detail["message"].(string)
			for _, want := range []string{`provider "east"`, "Azure identity auth failed", routeAuthProtocolCredentialError} {
				if !strings.Contains(message, want) {
					t.Errorf("error message = %q, want credential diagnostic %q", message, want)
				}
			}
			if got := sources["east"].calls.Load(); got != 1 {
				t.Errorf("Azure credential calls = %d, want 1", got)
			}
			if got := sends.Load(); got != 0 {
				t.Errorf("upstream sends = %d, want 0 after local credential failure", got)
			}
		})
	}
}

func TestExplicitRouteCredentialFailoverProtocol(t *testing.T) {
	const response = `{"id":"resp_credential_fallback","object":"response","model":"copilot-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	for _, protocol := range []string{"HTTP", "WebSocket"} {
		t.Run(protocol, func(t *testing.T) {
			var azureSends, copilotSends atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "copilot.example" {
					azureSends.Add(1)
					return nil, fmt.Errorf("unexpected Azure upstream send: %s %s", req.Method, req.URL)
				}
				if req.Method == http.MethodGet && req.URL.Path == "/models" {
					return routeExecutorTestResponse(req, http.StatusOK, nil, `{"data":[{"id":"copilot-model","supported_endpoints":["/responses"]}]}`), nil
				}
				copilotSends.Add(1)
				if req.Method != http.MethodPost || req.URL.Path != "/responses" {
					t.Errorf("Copilot inference request = %s %s, want POST /responses", req.Method, req.URL)
				}
				if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
					t.Errorf("Copilot Authorization = %q, want mock Copilot credential", got)
				}
				var body struct {
					Model  string `json:"model"`
					Store  *bool  `json:"store"`
					Stream bool   `json:"stream"`
				}
				defer func() { _ = req.Body.Close() }()
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, fmt.Errorf("decode Copilot inference request: %w", err)
				}
				if body.Model != "copilot-model" || body.Store == nil || *body.Store {
					t.Errorf("Copilot request = %+v, want copilot-model with store=false", body)
				}
				if body.Stream {
					return routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, "data: "+`{"type":"response.completed","response":`+response+"}\n\n"), nil
				}
				return routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"application/json"}}, response), nil
			})
			handler, sources := newRouteAuthProtocolHandler(t, true, transport)

			status, payload := sendRouteAuthProtocolRequest(t, handler, protocol)
			if status != http.StatusOK || payload["status"] != "completed" || payload["id"] != "resp_credential_fallback" {
				t.Errorf("fallback status = %d, payload = %+v, want completed Copilot response", status, payload)
			}
			if got := payload["model"]; got != "credential-model" {
				t.Errorf("response model = %v, want public credential-model", got)
			}
			for providerID, source := range sources {
				if got := source.calls.Load(); got != 1 {
					t.Errorf("%s credential calls = %d, want 1", providerID, got)
				}
			}
			if got := azureSends.Load(); got != 0 {
				t.Errorf("Azure inference sends = %d, want 0", got)
			}
			if got := copilotSends.Load(); got != 1 {
				t.Errorf("Copilot inference sends = %d, want 1", got)
			}
		})
	}
}

func newRouteAuthProtocolHandler(t *testing.T, failoverToCopilot bool, transport http.RoundTripper) (*ProxyHandler, map[string]*staticAzureTokenSource) {
	t.Helper()
	providerIDs := []string{"east"}
	routing := ModelRouteRoutingConfig{Mode: "primary_only", MaxTargetAttempts: 1, MaxUpstreamSends: 1}
	if failoverToCopilot {
		providerIDs = append(providerIDs, "west")
		routing = ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 3, MaxUpstreamSends: 3}
	}
	config := ProvidersConfig{SchemaVersion: 2, StateBindings: &StateBindingsConfig{Mode: "memory"}}
	route := ModelRouteConfig{ID: "credential-route", PublicID: "credential-model", Endpoints: []string{providerEndpointResponses}, Routing: routing}
	sources := make(map[string]*staticAzureTokenSource, len(providerIDs))
	for _, providerID := range providerIDs {
		config.Providers = append(config.Providers, ProviderConfig{
			ID: providerID, Type: string(providerTypeAzureOpenAI), Default: providerID == "east",
			BaseURL: "https://" + providerID + ".example/openai/v1", AuthMode: string(providerAuthModeAzureIdentity),
		})
		route.Targets = append(route.Targets, ModelRouteTargetConfig{ID: providerID, Provider: providerID, UpstreamModel: "deployment-" + providerID})
		sources[providerID] = &staticAzureTokenSource{err: errors.New(routeAuthProtocolCredentialError)}
	}
	if failoverToCopilot {
		config.Providers = append(config.Providers, ProviderConfig{ID: "copilot", Type: string(providerTypeCopilot)})
		route.Targets = append(route.Targets, ModelRouteTargetConfig{ID: "copilot", Provider: "copilot", UpstreamModel: "copilot-model"})
	}
	config.ModelRoutes = []ModelRouteConfig{route}
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(config), WithCopilotBaseURL("https://copilot.example"),
		withAzureIdentityTokenSourceFactoryForTest(func(providerID, _ string) (azureTokenSource, error) {
			if source := sources[providerID]; source != nil {
				return source, nil
			}
			return nil, fmt.Errorf("unexpected Azure provider %q", providerID)
		}),
		func(h *ProxyHandler) { h.client = &http.Client{Transport: transport} },
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(handler.BeginShutdown)
	return handler, sources
}

func sendRouteAuthProtocolRequest(t *testing.T, handler *ProxyHandler, protocol string) (int, map[string]interface{}) {
	t.Helper()
	if protocol == "HTTP" {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"credential-model","input":"hello","store":false}`))
		w := httptest.NewRecorder()
		handler.HandleResponses(w, req)
		var payload map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode HTTP response %q: %v", w.Body.String(), err)
		}
		return w.Code, payload
	}
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{"role": "user", "content": "hello"},
	})
	request["model"] = "credential-model"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("write response.create: %v", err)
	}
	frame := mustReadWebSocketJSONSkipMetadata(t, conn)
	if frame["type"] == "error" {
		status, ok := frame["status_code"].(float64)
		if !ok {
			t.Fatalf("error frame has no numeric status_code: %+v", frame)
		}
		return int(status), frame
	}
	if frame["type"] != "response.completed" {
		t.Fatalf("WebSocket frame = %+v, want response.completed or error", frame)
	}
	response, ok := frame["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("response.completed has no response object: %+v", frame)
	}
	return http.StatusOK, response
}
