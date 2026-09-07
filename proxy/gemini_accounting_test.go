package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func TestGeminiRequestObservesAccounting(t *testing.T) {
	const accounting = `{"total_nano_aiu":27,"compute_units":2,"token_details":[{"model":"private-accounting-model"}]}`
	for _, explicit := range []bool{false, true} {
		for _, mode := range []struct {
			name, action string
			tools, fail  bool
		}{
			{name: "JSON", action: "generateContent"},
			{name: "forced stream", action: "generateContent", tools: true},
			{name: "stream", action: "streamGenerateContent"},
			{name: "failed stream", action: "streamGenerateContent", fail: true},
		} {
			t.Run(fmt.Sprintf("explicit=%t/%s", explicit, mode.name), func(t *testing.T) {
				h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
					var request models.OpenAIRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					if request.Stream != nil && *request.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+`{"id":"chat-accounting","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n\n")
						_, _ = io.WriteString(w, "data: "+`{"choices":[],"copilot_usage":`+accounting+"}\n\n")
						if mode.fail {
							_, _ = io.WriteString(w, "event: error\ndata: "+`{"error":{"type":"server_error","message":"stream failed"}}`+"\n\n")
						} else {
							_, _ = io.WriteString(w, "data: [DONE]\n\n")
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"chat-accounting","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"copilot_usage":`+accounting+"}")
				})
				h.stats = newStatsCollector()
				h.log = logger.NewWithWriter(logger.LevelError, io.Discard)
				model := "chat-model"
				if explicit {
					h = newExplicitRouteSurfaceHandler(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, h.copilotURL, h.copilotURL)
					model = "public-model"
				}
				t.Cleanup(h.BeginShutdown)
				body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]`
				if mode.tools {
					body += `,"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]`
				}
				body += "}"
				ctx, summary := WithRequestSummary(context.Background())
				request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":"+mode.action, strings.NewReader(body)).WithContext(ctx)
				w := httptest.NewRecorder()
				h.HandleGeminiModels(w, request)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "done") || (mode.fail && !strings.Contains(w.Body.String(), "stream failed")) {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if got := summary.copilotUsage; got != (copilotUsageTotals{TotalNanoAIU: 27, ComputeUnits: 2}) {
					t.Errorf("request accounting = %+v", got)
				}
				if got := h.stats.snapshot().TaskUsage.Totals.CopilotUsage; got != (copilotUsageTotals{TotalNanoAIU: 27, ComputeUnits: 2}) {
					t.Errorf("task accounting = %+v", got)
				}
				fields, err := json.Marshal(summary.LoggerFields())
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(fields), "private-accounting-model") || strings.Contains(w.Body.String(), "copilot_usage") {
					t.Fatal("provider accounting metadata leaked into logs or Gemini output")
				}
			})
		}
	}
}
