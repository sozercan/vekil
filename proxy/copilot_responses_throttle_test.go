package proxy

import (
	"context"
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

func TestCopilotResponsesBackedChatStreamThrottleCreatesCooldown(t *testing.T) {
	const progress = "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp-cooldown","created_at":1,"status":"in_progress","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg-cooldown","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
		"data: " + `{"type":"response.content_part.added","sequence_number":2,"item_id":"msg-cooldown","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
		"data: " + `{"type":"response.output_text.delta","sequence_number":3,"item_id":"msg-cooldown","output_index":0,"content_index":0,"delta":"hello"}` + "\n\n"
	for _, tc := range []struct {
		name, prefix, event string
		headers             http.Header
	}{
		{
			name:    "HTTP reset",
			headers: http.Header{"Retry-After": {"86400"}},
			event:   `{"type":"response.failed","sequence_number":0,"response":{"status":"failed","error":{"code":"user_model_rate_limited","message":"model reset"}}}`,
		},
		{
			name:  "embedded reset",
			event: `{"type":"error","sequence_number":0,"code":"user_model_rate_limited","message":"model reset","headers":{"retry-after-ms":86400000}}`,
		},
		{
			name: "failure with output after progress", prefix: progress,
			headers: http.Header{"Retry-After": {"1"}},
			event:   `{"type":"response.failed","sequence_number":4,"response":{"status":"failed","output":[{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("x", maxCopilotThrottleEvidenceBytes+64) + `"}]}],"error":{"code":"user_model_rate_limited","message":"model reset","headers":{"retry-after":"86400"}}}}`,
		},
	} {
		for _, protocol := range []string{"OpenAI", "Anthropic", "Gemini"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, protocol, streaming), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == providerEndpointModels {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"data":[{"id":"responses-model","supported_endpoints":["/responses"]}]}`)
							return
						}
						sends.Add(1)
						var request struct {
							Stream bool `json:"stream"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if r.URL.Path != providerEndpointResponses || !request.Stream {
							t.Errorf("upstream request: path=%s stream=%t", r.URL.Path, request.Stream)
						}
						for key, values := range tc.headers {
							w.Header()[key] = values
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, tc.prefix+"data: "+tc.event+"\n\n")
					}))
					defer upstream.Close()
					h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(h.BeginShutdown)
					now := time.Now()
					h.copilotTraffic.now = func() time.Time { return now }
					if err := h.ValidateDynamicProviderModels(t.Context()); err != nil {
						t.Fatal(err)
					}
					request := func() *httptest.ResponseRecorder {
						w := httptest.NewRecorder()
						switch protocol {
						case "OpenAI":
							body := fmt.Sprintf(`{"model":"responses-model","stream":%t,"messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, streaming)
							h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
						case "Anthropic":
							body := fmt.Sprintf(`{"model":"responses-model","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, streaming)
							h.HandleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
						case "Gemini":
							action := "generateContent"
							if streaming {
								action = "streamGenerateContent"
							}
							body := `{"contents":[{"role":"user","parts":[{"text":"lookup"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`
							h.HandleGeminiModels(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/responses-model:"+action, strings.NewReader(body)))
						}
						return w
					}
					first := request()
					wantStatus := http.StatusTooManyRequests
					if streaming && tc.prefix != "" {
						wantStatus = http.StatusOK
					}
					if first.Code != wantStatus || !strings.Contains(first.Body.String(), "model reset") {
						t.Fatalf("streamed failure: status=%d body=%s", first.Code, first.Body.String())
					}
					second := request()
					if sends.Load() != 1 || second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "86400" || !strings.Contains(second.Body.String(), "rate limit is still active") {
						t.Fatalf("stream cooldown: sends=%d status=%d reset=%s body=%s", sends.Load(), second.Code, second.Header().Get("Retry-After"), second.Body.String())
					}
					usage := h.stats.taskUsage.snapshot()
					if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Errors != 1 || usage.Totals.Throttled != 1 {
						t.Fatalf("physical-send accounting = %+v", usage)
					}
				})
			}
		}
	}
}

func TestCopilotResponsesStreamThrottleRequiresStructuredEvidence(t *testing.T) {
	const failure = `{"type":"response.failed","response":{"status":"failed","error":{"code":"user_model_rate_limited"}}}`
	frame := "data: " + failure + "\n\n"
	generated, err := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": frame})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body   string
		wantCooldown bool
	}{
		{"valid failure", frame, true},
		{"event name fallback", "event: response.failed\ndata: " + strings.Replace(failure, `"type":"response.failed",`, "", 1) + "\n\n", true},
		{"CRLF", strings.ReplaceAll(frame, "\n", "\r\n"), true},
		{"EOF event", strings.TrimSuffix(frame, "\n\n"), true},
		{"generated text", "data: " + string(generated) + "\n\n", false},
		{"generated error object", "data: " + `{"type":"response.output_text.delta","response":{"error":{"code":"user_model_rate_limited"}}}` + "\n\n", false},
		{"duplicate code", strings.Replace(frame, `"code":"user_model_rate_limited"`, `"code":"user_global_rate_limited","code":"user_model_rate_limited"`, 1), false},
		{"conflicting event type", "event: response.completed\n" + frame, false},
		{"trailing JSON", "data: " + failure + "{}\n\n", false},
		{"after terminal success", "data: " + `{"type":"response.completed","response":{"status":"completed"}}` + "\n\n" + frame, false},
		{"overflowed event", "data: " + failure + strings.Repeat(" ", openAIStreamScannerMaxBuffer) + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{}
			req := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0)
			req = req.WithContext(context.WithValue(req.Context(), responsesChatStreamContextKey{}, true))
			req = withCopilotInferenceRequest(req, &providerRuntime{id: "copilot", kind: providerTypeCopilot}, providerEndpointResponses, []byte(`{"model":"model"}`))
			resp := routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"60"}}, "")
			resp.Body = io.NopCloser(&copilotChatThrottleChunkReader{Reader: strings.NewReader(tc.body), chunk: 4093})
			h.finishCopilotInference(req, resp, nil, nil)
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(got) != tc.body {
				t.Fatalf("stream bytes changed: error=%v bytes=%d", err, len(got))
			}
			permit, blocked, err := h.acquireCopilotInference(req)
			permit.release()
			if err != nil || (blocked != nil) != tc.wantCooldown {
				t.Fatalf("Responses cooldown = %t error=%v, want %t", blocked != nil, err, tc.wantCooldown)
			}
			if blocked != nil {
				_ = blocked.Body.Close()
			}
		})
	}
}
