package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeChatAggregateErrorHeaders(t *testing.T) {
	for _, routing := range []string{"direct", "policy"} {
		for _, protocol := range []string{"openai", "anthropic"} {
			for _, failure := range []struct {
				code   string
				status int
			}{{"rate_limit_exceeded", http.StatusTooManyRequests}, {"model_overloaded", http.StatusServiceUnavailable}} {
				t.Run(routing+"/"+protocol+"/"+failure.code, func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sends.Add(1)
						var request struct {
							Stream bool `json:"stream"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if !request.Stream {
							t.Error("tool request was not force-streamed")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Retry-After", "900")
						w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
						w.Header().Set("X-Copilot-Service-Request-Id", "service-request")
						w.Header().Set("X-Quota-Snapshot-Test", "120")
						w.Header().Set("Authorization", "private-token")
						w.Header().Set("Set-Cookie", "private-cookie")
						_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"prefix"}}]}`+"\n\n")
						_, _ = fmt.Fprintf(w, "data: {\"error\":{\"type\":\"server_error\",\"code\":%q,\"message\":\"physical-terminal unavailable\"}}\n\n", failure.code)
					}))
					defer upstream.Close()
					model := "public-model"
					var h *ProxyHandler
					if routing == "policy" {
						var err error
						h, err = NewProxyHandler(nil, nil,
							WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
							WithPolicyRoutingMode(PolicyRoutingModeOff))
						if err != nil {
							t.Fatal(err)
						}
						model = "coding-economy"
					} else {
						h = newExplicitRouteSurfaceHandlerWithRouting(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, upstream.URL, upstream.URL, ModelRouteRoutingConfig{
							Mode: string(routeModePrimaryOnly), MaxTargetAttempts: 1, MaxUpstreamSends: 1,
						})
					}
					t.Cleanup(h.BeginShutdown)
					body := map[string]any{"model": model, "messages": []any{map[string]string{"role": "user", "content": "lookup"}}, "stream": false}
					path, handle := "/v1/chat/completions", h.HandleOpenAIChatCompletions
					if protocol == "anthropic" {
						path, handle = "/v1/messages", h.HandleAnthropicMessages
						body["max_tokens"] = 1024
						body["tools"] = []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}
					} else {
						body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
					}
					payload, _ := json.Marshal(body)
					recorder := httptest.NewRecorder()
					handle(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(payload))))
					if recorder.Code != failure.status || !json.Valid(recorder.Body.Bytes()) {
						t.Fatalf("status = %d, want %d: %s", recorder.Code, failure.status, recorder.Body.String())
					}
					for name, want := range map[string]string{"Retry-After": "900", "X-Ratelimit-Remaining-Requests": "0", "X-Copilot-Service-Request-Id": "service-request", "X-Quota-Snapshot-Test": "120"} {
						if routing == "policy" && (name == "X-Copilot-Service-Request-Id" || name == "X-Quota-Snapshot-Test") {
							want = ""
						}
						if got := recorder.Header().Get(name); got != want {
							t.Errorf("%s = %q, want %q", name, got, want)
						}
					}
					for _, name := range []string{"Authorization", "Set-Cookie"} {
						if recorder.Header().Get(name) != "" {
							t.Errorf("unsafe header %q survived translation", name)
						}
					}
					if routing == "policy" && strings.Contains(recorder.Body.String(), "physical-terminal") {
						t.Error("policy error leaked terminal identity")
					}
					if sends.Load() != 1 {
						t.Errorf("upstream sends = %d, want 1", sends.Load())
					}
				})
			}
		}
	}
}
