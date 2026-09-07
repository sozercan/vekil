package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const geminiHeaderJSON = `{"id":"chat-headers","object":"chat.completion","model":"physical-primary","choices":[{"index":0,"message":{"role":"assistant","content":"header fixture"},"finish_reason":"stop"}]}`

const geminiHeaderStream = "data: {\"id\":\"chat-headers\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"header fixture\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chat-headers\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

const geminiHeaderStreamError = "data: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"header fixture throttled\"}}\n\n"

func TestHandleGeminiModelsNativeChatDiagnosticHeaders(t *testing.T) {
	for _, routing := range []string{"legacy", "configured", "explicit"} {
		for _, mode := range []string{"json", "stream", "aggregate", "http_error", "stream_error", "stream_admission_error", "aggregate_error"} {
			t.Run(routing+"/"+mode, func(t *testing.T) {
				var sends atomic.Int32
				upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					sends.Add(1)
					w.Header().Set("X-Copilot-Service-Request-Id", "service-request")
					w.Header().Set("X-Quota-Snapshot-Test", "120")
					w.Header().Set("X-Usage-Ratelimit-Test", "0")
					w.Header().Set("Retry-After", "900")
					w.Header().Set("Authorization", "private-token")
					w.Header().Set("Set-Cookie", "private-cookie")
					w.Header().Set("X-Quota-Snapshot-Oversized", strings.Repeat("x", diagnosticHeaderValueLimit+1))
					w.Header().Add("X-Ratelimit-Ambiguous", "one")
					w.Header().Add("X-Ratelimit-Ambiguous", "two")
					switch mode {
					case "json":
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, geminiHeaderJSON)
					case "http_error":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `{"error":{"type":"permission_error","message":"header fixture denied"}}`)
					case "stream_error", "aggregate_error":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"prefix\"}}]}\n\n"+geminiHeaderStreamError)
					case "stream_admission_error":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, geminiHeaderStreamError)
					default:
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, geminiHeaderStream)
					}
				})
				var h *ProxyHandler
				model := "public-model"
				if routing == "legacy" {
					h = newTestProxyHandler(t, upstream)
				} else {
					server := httptest.NewServer(upstream)
					t.Cleanup(server.Close)
					if routing == "configured" {
						h = newChatExecutionTestHandler(t, server.URL, []string{providerEndpointChatCompletions})
						model = "gpt-public"
					} else {
						h = newExplicitRouteSurfaceHandlerWithRouting(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, server.URL, server.URL, ModelRouteRoutingConfig{
							Mode: string(routeModePrimaryOnly), MaxTargetAttempts: 1, MaxUpstreamSends: 1,
						})
					}
				}

				response := serveGeminiHeaderRequest(t, h, model, mode)
				wantStatus := http.StatusOK
				if mode == "http_error" {
					wantStatus = http.StatusForbidden
				} else if mode == "aggregate_error" || routing == "explicit" && mode == "stream_admission_error" {
					wantStatus = http.StatusTooManyRequests
				}
				if response.StatusCode != wantStatus {
					t.Errorf("status = %d, want %d", response.StatusCode, wantStatus)
				}
				wantContentType := "application/json"
				if strings.HasPrefix(mode, "stream") && wantStatus == http.StatusOK {
					wantContentType = "text/event-stream"
				}
				if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, wantContentType) {
					t.Errorf("content type = %q, want %q", got, wantContentType)
				}
				for name, want := range map[string]string{
					"X-Copilot-Service-Request-Id": "service-request", "X-Quota-Snapshot-Test": "120",
					"X-Usage-Ratelimit-Test": "0", "Retry-After": "900",
				} {
					if got := response.Header.Get(name); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				for _, name := range []string{"Authorization", "Set-Cookie", "X-Quota-Snapshot-Oversized", "X-Ratelimit-Ambiguous"} {
					if got := response.Header.Get(name); got != "" {
						t.Errorf("unsafe %s survived translation: %q", name, got)
					}
				}
				if got := sends.Load(); got != 1 {
					t.Errorf("upstream sends = %d, want 1", got)
				}
			})
		}
	}
}

func TestHandleGeminiModelsNativeChatDiagnosticHeadersAfterFailover(t *testing.T) {
	for _, mode := range []string{"json", "stream", "aggregate", "aggregate_canonical_error", "aggregate_progress_error"} {
		t.Run(mode, func(t *testing.T) {
			var primarySends, secondarySends atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primarySends.Add(1)
				w.Header().Set("X-Copilot-Service-Request-Id", "primary-request")
				w.Header().Set("X-Quota-Snapshot-Primary", "1")
				w.Header().Set("Retry-After", "900")
				if strings.HasPrefix(mode, "aggregate") {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, geminiHeaderStreamError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"primary throttled"}}`)
			}))
			t.Cleanup(primary.Close)
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondarySends.Add(1)
				w.Header().Set("X-Copilot-Service-Request-Id", "secondary-request")
				w.Header().Set("X-Quota-Snapshot-Secondary", "2")
				switch mode {
				case "json":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, geminiHeaderJSON)
				case "aggregate_canonical_error":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"secondary throttled"}}`)
				case "aggregate_progress_error":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"prefix\"}}]}\n\n"+geminiHeaderStreamError)
				default:
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, geminiHeaderStream)
				}
			}))
			t.Cleanup(secondary.Close)
			h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
			response := serveGeminiHeaderRequest(t, h, "public-model", mode)
			wantID, wantStatus := "secondary-request", http.StatusOK
			if strings.HasSuffix(mode, "_error") {
				wantStatus = http.StatusTooManyRequests
			}
			if mode == "aggregate_canonical_error" {
				wantID = "primary-request"
			}
			if response.StatusCode != wantStatus {
				t.Errorf("status = %d, want %d", response.StatusCode, wantStatus)
			}
			if got := response.Header.Get("X-Copilot-Service-Request-Id"); got != wantID {
				t.Errorf("selected request ID = %q, want %q", got, wantID)
			}
			if wantID == "secondary-request" {
				if response.Header.Get("Retry-After") != "" || response.Header.Get("X-Quota-Snapshot-Primary") != "" {
					t.Errorf("failed primary headers survived: %v", response.Header)
				}
			} else if response.Header.Get("Retry-After") != "900" || response.Header.Get("X-Quota-Snapshot-Secondary") != "" {
				t.Errorf("canonical failure headers = %v", response.Header)
			}
			if primarySends.Load() != 1 || secondarySends.Load() != 1 {
				t.Errorf("sends primary=%d secondary=%d, want one each", primarySends.Load(), secondarySends.Load())
			}
		})
	}
}

func serveGeminiHeaderRequest(t *testing.T, h *ProxyHandler, model, mode string) *http.Response {
	t.Helper()
	action := "generateContent"
	body := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`
	if strings.HasPrefix(mode, "stream") {
		action = "streamGenerateContent"
	} else if strings.HasPrefix(mode, "aggregate") {
		body = `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`
	}
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":"+action, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleGeminiModels(w, request)
	response := w.Result()
	t.Cleanup(func() { _ = response.Body.Close() })
	if w.Body.Len() == 0 {
		t.Fatal("empty Gemini response")
	}
	return response
}
