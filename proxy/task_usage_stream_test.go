package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestTaskUsagePublicStreamingBilling(t *testing.T) {
	const initialBilling = `"copilot_usage":{"total_nano_aiu":7,"compute_units":1}`
	const finalBilling = `"copilot_usage":{"total_nano_aiu":31,"compute_units":2,"token_details":[{"model":"excluded-accounting-model"}]}`
	for _, tc := range []struct {
		name, endpoint, request string
		events                  []string
	}{
		{
			name: "Responses", endpoint: providerEndpointResponses,
			request: `{"model":"billing-model","input":"hello","stream":true}`,
			events: []string{
				`{"type":"response.in_progress","response":{"id":"resp-billing","usage":{"input_tokens":11,"output_tokens":2}},` + initialBilling + `}`,
				`{"type":"response.output_text.delta","delta":"literal \"copilot_usage\":{\"total_nano_aiu\":999999}","metadata":{"copilot_usage":{"total_nano_aiu":999999}}}`,
				`{"type":"response.in_progress","copilot_usage":{"total_nano_aiu":999999,"compute_units":"invalid"}}`,
				`{"type":"response.in_progress","copilot_usage":{"total_nano_aiu":999999,"padding":"` + strings.Repeat("x", 64<<10) + `"}}`,
				`{"type":"response.completed","response":{"id":"resp-billing","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}},` + finalBilling + `}`,
				`{"type":"response.completed","response":{"id":"resp-billing","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}},` + finalBilling + `}`,
			},
		},
		{
			name: "Anthropic", endpoint: providerEndpointMessages,
			request: `{"model":"billing-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			events: []string{
				`{"type":"message_start","message":{"id":"msg-billing","type":"message","role":"assistant","model":"billing-model","content":[],"usage":{"input_tokens":11,"output_tokens":0}},` + initialBilling + `}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"literal \"copilot_usage\":{\"total_nano_aiu\":999999}"},"metadata":{"copilot_usage":{"total_nano_aiu":999999}}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"ping","copilot_usage":{"total_nano_aiu":999999,"compute_units":"invalid"}}`,
				`{"type":"ping","copilot_usage":{"total_nano_aiu":999999,"padding":"` + strings.Repeat("x", 64<<10) + `"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7},` + finalBilling + `}`,
				`{"type":"message_stop",` + finalBilling + `}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream strings.Builder
			for _, event := range tc.events {
				if !json.Valid([]byte(event)) {
					t.Fatalf("invalid fixture: %s", event)
				}
				stream.WriteString("data: " + event + "\n\n")
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == providerEndpointModels {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[{"id":"billing-model","supported_endpoints":["/responses","/v1/messages"]}]}`)
					return
				}
				if r.URL.Path != tc.endpoint {
					t.Errorf("upstream path = %s, want %s", r.URL.Path, tc.endpoint)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream.String())
			}))
			defer upstream.Close()
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.ValidateDynamicProviderModels(t.Context()); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			if tc.endpoint == providerEndpointResponses {
				h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.request)))
			} else {
				h.HandleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.request)))
			}
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), finalBilling) {
				t.Fatalf("stream lost billing data: status=%d body=%s", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			h.HandleStatsJSON(w, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
			var stats struct {
				TaskUsage taskUsageSnapshot `json:"task_usage"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
				t.Fatal(err)
			}
			snapshot := stats.TaskUsage
			totals := snapshot.Totals
			if snapshot.Inflight != 0 || totals.Sends != 1 || totals.Completed != 1 || totals.Errors != 0 || totals.ReportedUsageSends != 1 || totals.Usage.TotalTokens != 18 {
				t.Fatalf("stream ledger = %+v", snapshot)
			}
			if totals.CopilotUsage != (copilotUsageTotals{TotalNanoAIU: 31, ComputeUnits: 2}) {
				t.Fatalf("stream billing was lost or counted repeatedly: %+v", totals.CopilotUsage)
			}
			if len(snapshot.ByKind) != 1 || snapshot.ByKind[0].Kind != "inference" || snapshot.ByKind[0].CopilotUsage != totals.CopilotUsage {
				t.Fatalf("billing missing from inference totals: %+v", snapshot.ByKind)
			}
			if strings.Contains(w.Body.String(), "excluded-accounting-model") {
				t.Fatal("stats retained billing metadata")
			}
		})
	}
}
