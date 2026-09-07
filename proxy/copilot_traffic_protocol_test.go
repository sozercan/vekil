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
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestCopilotCooldownPreservesNativeMessagesErrorEnvelope(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("configured=%t/stream=%t", configured, streaming), func(t *testing.T) {
				var sends atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api" + providerEndpointModels:
						_, _ = io.WriteString(w, `{"data":[{"id":"physical-model","supported_endpoints":["/v1/messages"]}]}`)
					case "/api" + providerEndpointMessages:
						sends.Add(1)
						w.Header().Set("Retry-After", "86400")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","code":"user_global_rate_limited","message":"account reset"}}`)
					default:
						t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer upstream.Close()
				opts := []Option{WithCopilotBaseURL(upstream.URL + "/api")}
				if configured {
					opts = append(opts, WithProvidersConfig(ProvidersConfig{
						SchemaVersion: ProvidersConfigSchemaVersion1,
						Providers: []ProviderConfig{{
							ID: "configured", Type: string(providerTypeCopilot), Default: true,
						}},
					}))
				}
				h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), opts...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.BeginShutdown)
				now := time.Now()
				h.copilotTraffic.now = func() time.Time { return now }
				body := fmt.Sprintf(`{"model":"physical-model","stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, streaming)
				for attempt := range 2 {
					w := httptest.NewRecorder()
					h.HandleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body)))
					if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "86400" || w.Header().Get("Content-Type") != "application/json" {
						t.Fatalf("attempt %d: status/headers/body = %d/%v/%s", attempt, w.Code, w.Header(), w.Body.String())
					}
					var envelope struct {
						Type  string `json:"type"`
						Error struct {
							Type    string `json:"type"`
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.Type != "error" || envelope.Error.Type != "rate_limit_error" || envelope.Error.Code != "user_global_rate_limited" {
						t.Fatalf("attempt %d: invalid Anthropic error envelope: %s", attempt, w.Body.String())
					}
					if attempt == 1 && !strings.Contains(envelope.Error.Message, "rate limit is still active") {
						t.Fatalf("second request did not use the local cooldown response: %s", w.Body.String())
					}
				}
				if sends.Load() != 1 {
					t.Fatalf("sent %d upstream requests during the same cooldown, want 1", sends.Load())
				}
				usage := h.stats.taskUsage.snapshot()
				if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Throttled != 1 || usage.Totals.Errors != 1 {
					t.Fatalf("local cooldown changed physical-send accounting: %+v", usage)
				}
			})
		}
	}
}
