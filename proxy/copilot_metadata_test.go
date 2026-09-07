package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestCopilotMetadataBoundsAndIsolation(t *testing.T) {
	headers := http.Header{
		"X-Initiator": {"agent"}, "X-Interaction-Id": {"interaction-one"}, "X-Client-Session-Id": {"session-one"},
		"Authorization": {"Bearer client-token"}, "Copilot-Integration-Id": {"client-integration"},
	}
	h := &ProxyHandler{auth: auth.NewTestAuthenticator("provider-token")}
	ctx := withCopilotRequestMetadata(context.Background(), headers)
	upstreamCtx, cancel := h.newInferenceUpstreamContextFrom(ctx, false)
	defer cancel()
	headers.Set("X-Interaction-Id", "mutated")
	for _, kind := range []providerType{providerTypeCopilot, providerTypeOpenAICompatible} {
		provider := &providerRuntime{id: "provider", kind: kind, baseURL: "http://example.test", authType: "none"}
		req, err := h.newProviderJSONRequest(upstreamCtx, provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{"model":"fixture"}`), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if kind == providerTypeCopilot {
			if req.Header.Get("X-Interaction-Id") != "interaction-one" || req.Header.Get("X-Initiator") != "agent" {
				t.Fatal("detached inference lost caller attribution")
			}
			if req.Header.Get("Authorization") != "Bearer provider-token" || req.Header.Get("Copilot-Integration-Id") == "client-integration" {
				t.Fatal("caller metadata replaced provider authentication")
			}
		} else if req.Header.Get("X-Initiator") != "" || req.Header.Get("X-Client-Session-Id") != "" {
			t.Fatal("Copilot attribution reached another provider")
		}
	}
	invalid := http.Header{"X-Initiator": {"spoofed"}, "X-Interaction-Id": {"one", "two"}, "X-Client-Session-Id": {strings.Repeat("x", diagnosticHeaderValueLimit+1)}}
	dst := make(http.Header)
	copyCopilotRequestMetadata(dst, invalid)
	if len(dst) != 0 {
		t.Fatalf("invalid attribution survived: %v", dst)
	}
}

func TestCopilotDiagnosticHeadersSurviveTranslatedErrors(t *testing.T) {
	headers := http.Header{
		"X-Copilot-Service-Request-Id": {"service-request"}, "X-Quota-Snapshot-Test": {"120"},
		"X-Usage-Ratelimit-Test": {"0"}, "X-Ratelimit-Reset-Requests": {"900"},
		"Set-Cookie": {"private-cookie"}, "Authorization": {"private-token"},
	}
	w := httptest.NewRecorder()
	writeOpenAIErrorWithRetryAfter(w, http.StatusTooManyRequests, "upstream throttled", "rate_limit_error", "900", headers)
	for name, want := range map[string]string{"X-Copilot-Service-Request-Id": "service-request", "X-Quota-Snapshot-Test": "120", "X-Usage-Ratelimit-Test": "0", "Retry-After": "900"} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if w.Header().Get("Authorization") != "" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("private headers survived translation")
	}
	if UpstreamRequestID(w.Header()) != "service-request" {
		t.Fatal("service request ID not recognized")
	}
}

func TestCopilotAPIVersionCatalogCompatibility(t *testing.T) {
	for _, version := range []string{"", "2025-05-01"} {
		t.Run("version_"+version, func(t *testing.T) {
			wantVersion := version
			if wantVersion == "" {
				wantVersion = "2026-08-20"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Github-Api-Version"); got != wantVersion {
					t.Errorf("API version = %q, want %q", got, wantVersion)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"catalog-model","name":"Catalog model","supported_endpoints":["/responses"],"capabilities":{"limits":{"max_prompt_tokens":128000,"max_context_window_tokens":400000},"supports":{"reasoning_effort":["low","high"]}},"billing":{"is_premium":true},"pricing":{"input":2,"output":8}}]}`))
			}))
			defer upstream.Close()
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError), WithCopilotBaseURL(upstream.URL), WithCopilotHeaderConfig(CopilotHeaderConfig{GitHubAPIVersion: version}))
			if err != nil {
				t.Fatal(err)
			}
			defer h.BeginShutdown()
			w := httptest.NewRecorder()
			h.HandleModels(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("models status=%d body=%s", w.Code, w.Body.String())
			}
			var result struct {
				Data []map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Data) != 1 || len(result.Data[0]["billing"]) == 0 || len(result.Data[0]["pricing"]) == 0 {
				t.Fatalf("catalog lost extended metadata: %s", w.Body.String())
			}
		})
	}
}

func TestCopilotUsageSummaryIsNumericAndIdempotent(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	for range 2 {
		observeCopilotUsage(ctx, json.RawMessage(`{"total_nano_aiu":1234,"compute_units":7,"token_details":[{"model":"private-model","token_count":999}]} `))
	}
	observeCopilotUsage(ctx, json.RawMessage(`{"total_nano_aiu":-1,"compute_units":"private"}`))
	if summary.copilotUsage != (copilotUsageTotals{TotalNanoAIU: 1234, ComputeUnits: 7}) {
		t.Fatalf("usage = %+v", summary.copilotUsage)
	}
	encoded, err := json.Marshal(summary.LoggerFields())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "token_details") {
		t.Fatal("summary retained provider metadata")
	}
}
