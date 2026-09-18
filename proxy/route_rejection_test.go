package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func TestExplicitRouteHTTPRejectionWithProgressNeverReplays(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens":10,"output_tokens":1}}`,
		`{"error":{"code":"rate_limit_exceeded"},"usage":{"prompt_tokens":10,"completion_tokens":1}}`,
		`{"error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens_details":{"cached_tokens":1}}}`,
		`{"error":{"code":"rate_limit_exceeded"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
		`{"error":{"code":"rate_limit_exceeded"},"choices":[{"message":{"tool_calls":[{"id":"call_1"}]}}]}`,
		`{"error":{"code":"rate_limit_exceeded"},"response":{"usage":{"input_tokens":1}}}`,
		`{"error":{"code":"rate_limit_exceeded","usage":{"input_tokens":1}}}`,
		`{"error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens":1},"usage":null}`,
		`{"error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens":"unknown"}}`,
		`{"error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens":1e-400}}`,
		`{"error":{"code":"rate_limit_exceeded"},"status":"completed","output":[]}`,
		`{"error":{"code":"rate_limit_exceeded"},"output":{}}`,
		`{"error":{"code":"rate_limit_exceeded"},"usage":`,
		`{"error":{"code":"rate_limit_exceeded"}}` + strings.Repeat(" ", upstreamErrorDetailMaxBodyBytes) + `{"usage":{"input_tokens":1}}`,
	} {
		for _, pinned := range []bool{false, true} {
			t.Run(body[:min(72, len(body))]+map[bool]string{false: "/failover", true: "/pinned"}[pinned], func(t *testing.T) {
				var calls atomic.Int32
				h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					return routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"1"}}, body), nil
				})}, routeModePriorityFailover, 2, 3,
					explicitRouteTestProvider("primary", "http://primary.example", "key"),
					explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
				t.Cleanup(h.BeginShutdown)
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				op := newRouteOperation(route, ctx)
				if pinned {
					if err := op.forcePinnedTarget(route.targets[0].id); err != nil {
						t.Fatal(err)
					}
				}
				ctx = withRouteOperation(ctx, op)
				resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
				if err != nil || resp == nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
				sends, switches, _ := op.snapshot()
				if calls.Load() != 1 || sends != 1 || switches != 0 || resp.StatusCode != 429 {
					t.Fatalf("replayed ambiguous rejection: calls=%d sends=%d switches=%d status=%d", calls.Load(), sends, switches, resp.StatusCode)
				}
			})
		}
	}
}

func TestExplicitRouteChatCannotRecertifyIncompleteCapture(t *testing.T) {
	for _, incompleteRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncated body", true: "read failure"}[incompleteRead], func(t *testing.T) {
			var calls atomic.Int32
			body := `{"error":{"code":"rate_limit_exceeded"}}`
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				resp := routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"1"}}, body)
				if incompleteRead {
					resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(body), iotest.ErrReader(io.ErrUnexpectedEOF)))
				} else {
					resp.Body = io.NopCloser(strings.NewReader(body + strings.Repeat(" ", upstreamErrorDetailMaxBodyBytes)))
				}
				return resp, nil
			})}, routeModePriorityFailover, 2, 2,
				explicitRouteTestProvider("primary", "http://primary.example", "key"),
				explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			op := newRouteOperation(route, t.Context())
			ctx := withRouteOperation(t.Context(), op)
			send := func(ctx context.Context) (*http.Response, error) {
				return h.executeExplicitRouteRequest(ctx, route, providerEndpointChatCompletions,
					[]byte(`{"model":"public-model","messages":[]}`), nil, "public-model", false)
			}
			resp, err := h.executeExplicitRouteSurfaceRequest(ctx, providerEndpointChatCompletions, false, explicitRouteStreamOpenAIChat, send)
			if err != nil || resp == nil {
				t.Fatalf("response unavailable: %v", err)
			}
			_ = resp.Body.Close()
			if calls.Load() != 1 || resp.StatusCode != 429 {
				t.Fatalf("outer Chat adapter retried an incomplete capture: calls=%d status=%d", calls.Load(), resp.StatusCode)
			}
		})
	}
}

func TestExplicitRouteOverloadRejectionWithProgressNeverReplays(t *testing.T) {
	target := targetBinding{provider: explicitRouteTestProvider("primary", "http://primary.example", "key")}
	for _, endpoint := range []string{providerEndpointResponses, providerEndpointChatCompletions} {
		for _, status := range []int{429, 503, 529} {
			response := &capturedRouteResponse{statusCode: status, body: []byte(`{"error":{"code":"model_overloaded","type":"overloaded_error"},"usage":{"input_tokens":3}}`)}
			if routeAdapterCertifiesHTTPRejection(target, endpoint, response) {
				t.Fatalf("certified executed overload: endpoint=%s status=%d", endpoint, status)
			}
		}
	}
	target.provider.kind = providerTypeAnthropicCompatible
	if routeAdapterCertifiesHTTPRejection(target, providerEndpointMessages, &capturedRouteResponse{
		statusCode: 529, body: []byte(`{"type":"error","error":{"type":"overloaded_error"},"content":[{"type":"text","text":"partial"}]}`),
	}) {
		t.Fatal("certified native Messages output")
	}
}

func TestExplicitRoutePlainTextRejectionCannotHideStructuredProgress(t *testing.T) {
	target := targetBinding{provider: explicitRouteTestProvider("primary", "http://primary.example", "key")}
	for _, tc := range []struct {
		name, body string
		wantRetry  bool
	}{
		{"plain gateway rejection", "terminal unavailable", true},
		{"JSON output", `{"error":{"code":"rate_limit_exceeded"},"output":[{"type":"message"}]}`, false},
		{"malformed JSON", `{"usage":`, false},
		{"JSON with BOM", "\xef\xbb\xbf" + `{"usage":{"output_tokens":1}}`, false},
		{"stream after comment", ": heartbeat\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &capturedRouteResponse{statusCode: 429, header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, body: []byte(tc.body)}
			if got := routeAdapterCertifiesHTTPRejection(target, providerEndpointResponses, response); got != tc.wantRetry {
				t.Fatalf("replay=%t want=%t", got, tc.wantRetry)
			}
		})
	}
}

func TestExplicitRouteHTTPRejectionWithUsageRetainsPhysicalLedger(t *testing.T) {
	var calls atomic.Int32
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"1"}}, `{"error":{"type":"rate_limit_error","message":"quota"},"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}`), nil
	})}, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", "http://primary.example", "key"),
		explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
	t.Cleanup(h.BeginShutdown)
	h.stats = newStatsCollector()
	op := newRouteOperation(route, t.Context())
	resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil || resp == nil {
		t.Fatalf("response unavailable: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	snap := h.stats.snapshot()
	if resp.StatusCode != 429 || calls.Load() != 1 || len(snap.RecentAttempts) != 1 {
		t.Fatalf("executed rejection was replayed: calls=%d status=%d attempts=%+v", calls.Load(), resp.StatusCode, snap.RecentAttempts)
	}
	if snap.PhysicalUsage.TotalTokens != 5 || snap.WastedUsage.TotalTokens != 5 {
		t.Fatalf("rejected work disappeared from accounting: physical=%+v wasted=%+v", snap.PhysicalUsage, snap.WastedUsage)
	}
	attempt := snap.RecentAttempts[0]
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 || attempt.RetryDecision != routeRetrySuppressedProgress ||
		attempt.Delivery != requestDeliveredOrAmbiguous || !attempt.CleanupComplete {
		t.Fatalf("incorrect rejection diagnostics: %+v", attempt)
	}
}

func TestExplicitRouteAzureJSONFailedWithUsagePreservesFailure(t *testing.T) {
	body := `{"id":"resp_failed","status":"failed","usage":{"input_tokens":10,"output_tokens":1},"output":[],"error":{"code":"rate_limit_exceeded"}}`
	var calls atomic.Int32
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return routeExecutorTestResponse(req, 200, http.Header{"Retry-After": {"1"}}, body), nil
	})}, routeModePrimaryOnly, 1, 3, explicitRouteTestProvider("primary", "http://primary.example", "key"))
	t.Cleanup(h.BeginShutdown)
	op := newRouteOperation(route, t.Context())
	resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil || resp == nil {
		t.Fatalf("response unavailable: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(got) != body || resp.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("failure was changed or replayed: calls=%d status=%d body=%s err=%v", calls.Load(), resp.StatusCode, got, err)
	}
	h.azureTraffic.mu.Lock()
	defer h.azureTraffic.mu.Unlock()
	if len(h.azureTraffic.cooldowns) != 1 {
		t.Fatal("JSON rate limit did not update the deployment cooldown")
	}
}

func TestExplicitRouteAzureJSONInspectionPreservesBodyAndReadError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		readErr    error
	}{
		{"large success", `{"id":"resp_ok","status":"completed","output":[],"padding":"` + strings.Repeat("x", upstreamErrorDetailMaxBodyBytes) + `"}`, nil},
		{"read error after complete rejection", `{"status":"failed","error":{"code":"rate_limit_exceeded"}}`, io.ErrUnexpectedEOF},
		{"read error after partial success", `{"id":"resp_ok"`, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				resp := routeExecutorTestResponse(req, 200, http.Header{"Retry-After": {"1"}}, tc.body)
				resp.ContentLength = -1
				if tc.readErr != nil {
					resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(tc.body), iotest.ErrReader(tc.readErr)))
				}
				return resp, nil
			})}, routeModePrimaryOnly, 1, 3, explicitRouteTestProvider("primary", "http://primary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			resp, err := h.executeExplicitRouteRequest(t.Context(), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if err != nil || resp == nil {
				t.Fatalf("response unavailable: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(got) != tc.body || !errors.Is(err, tc.readErr) || calls.Load() != 1 || resp.StatusCode != 200 {
				t.Fatalf("body or error changed: calls=%d status=%d body bytes=%d error=%v", calls.Load(), resp.StatusCode, len(got), err)
			}
		})
	}
}
