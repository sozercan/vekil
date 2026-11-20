package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sozercan/vekil/logger"
)

func passthroughTestHandler(t *testing.T, enabled bool, transport routeExecutorRoundTripFunc) *ProxyHandler {
	t.Helper()
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: transport}, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("azure", "http://127.0.0.1/openai/v1", "fixture"))
	route.responsesStreamPassthrough = enabled
	h.log = logger.NewWithWriter(logger.LevelFatal, io.Discard)
	h.stateBindings, _ = newDurableStoreFixture(t, 32)
	h.stateBindingsOnce.Do(func() {})
	t.Cleanup(h.BeginShutdown)
	return h
}

func passthroughTestRequest(ctx context.Context, h *ProxyHandler, payload string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleResponses(w, r)
	return w
}

const passthroughStreamingRequest = `{"model":"public-model","stream":true,"input":"fixture"}`

func TestResponsesStreamPassthroughOpaque429(t *testing.T) {
	const canary = "opaque-fixture-marker"
	for _, delay := range []string{"0", "1", "Fri, 31 Dec 2094 23:59:59 GMT", "", canary} {
		t.Run(delay, func(t *testing.T) {
			var sends atomic.Int32
			h := passthroughTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				hdr := http.Header{
					"Content-Type":        {"text/plain; charset=utf-8"},
					"Retry-After-Ms":      {"45000.5", canary},
					"X-Ms-Retry-After-Ms": {"10000"},
					"X-Foo":               {canary},
					"Set-Cookie":          {canary},
					"X-Request-Id":        {canary},
					"X-Codex-Turn-State":  {canary},
				}
				if delay != "" {
					hdr.Set("Retry-After", delay)
				}
				return routeExecutorTestResponse(r, 429, hdr, canary+"\n"), nil
			})
			w := passthroughTestRequest(t.Context(), h, passthroughStreamingRequest)
			var body struct {
				Error struct{ Code, Message string }
			}
			if sends.Load() != 1 || w.Code != 429 || w.Header().Get("Content-Type") != "application/json" ||
				json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != "rate_limit_exceeded" ||
				body.Error.Message != "Upstream rate limit exceeded." || strings.Contains(w.Body.String(), canary) {
				t.Fatalf("want one send and fixed JSON rejection, got %d sends, HTTP %d", sends.Load(), w.Code)
			}
			wantDelay := delay
			if wantDelay == canary {
				wantDelay = ""
			}
			if w.Header().Get("Retry-After") != wantDelay ||
				strings.Join(w.Header().Values("Retry-After-Ms"), ",") != "45000.5" ||
				w.Header().Get("X-Ms-Retry-After-Ms") != "10000" {
				t.Error("did not preserve only the valid delay fields")
			}
			for _, values := range w.Header() {
				if strings.Contains(strings.Join(values, " "), canary) {
					t.Fatal("opaque upstream value exposed in response headers")
				}
			}
		})
	}
}

func TestResponsesStreamPassthroughRejectionBoundary(t *testing.T) {
	const failed = "event: response.failed\ndata: " + `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"quota"},"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}` + "\n\n"
	const created = "event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp-fixture"}}` + "\n\n"
	const delta = "event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","delta":"visible output"}` + "\n\n"
	const tool = "event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","item":{"id":"call-fixture","type":"function_call","name":"read"}}` + "\n\n"
	const topError = "event: error\ndata: " + `{"type":"error","error":{"code":"rate_limit_exceeded","message":"quota","headers":{"retry-after-ms":"10000"}}}` + "\n\n"
	for _, tc := range []struct {
		name        string
		optIn       bool
		status      int
		contentType string
		body        string
		wantStatus  int
		contains    string
	}{
		{"first failure with usage", true, 200, "text/event-stream", failed, 200, "input_tokens"},
		{"preamble then failure", true, 200, "text/event-stream", created + failed, 200, "response.created"},
		{"output then failure", true, 200, "text/event-stream", delta + failed, 200, "visible output"},
		{"tool then failure", true, 200, "text/event-stream", tool + failed, 200, "function_call"},
		{"top level error with hint", true, 200, "text/event-stream", topError, 200, "response.failed"},
		{"ordinary JSON rejection", true, 429, "application/json", `{"error":{"code":"rate_limit_exceeded","message":"specific fixture detail"}}`, 429, "specific fixture detail"},
		{"opt out retains translation", false, 200, "text/event-stream", failed, 429, "quota"},
		{"opt out retains durable error validation", false, 429, "text/plain", "opaque fixture", 502, ""},
		{"mislabelled JSON", true, 429, "text/plain", `{"usage":`, 502, ""},
		{"mislabelled SSE", true, 429, "text/plain", failed, 502, ""},
		{"not plain text", true, 429, "text/plain", "fixture\x00\xff", 502, ""},
		{"contradictory output", true, 429, "application/json", `{"output":[{"type":"message"}]}`, 502, ""},
		{"oversize", true, 429, "text/plain", strings.Repeat("x", upstreamErrorDetailMaxBodyBytes+1), 502, ""},
		{"failed body read", true, 429, "text/plain", "", 502, ""},
		{"unknown delivery", true, 0, "", "", 502, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			h := passthroughTestHandler(t, tc.optIn, func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				if tc.status == 0 {
					return nil, io.ErrUnexpectedEOF
				}
				resp := routeExecutorTestResponse(r, tc.status, http.Header{"Content-Type": {tc.contentType}}, tc.body)
				if tc.name == "failed body read" {
					resp.Body = io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))
				}
				return resp, nil
			})
			ctx, summary := WithRequestSummary(t.Context())
			w := passthroughTestRequest(ctx, h, passthroughStreamingRequest)
			if sends.Load() != 1 || w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("want one send and HTTP %d with expected detail, got %d sends and HTTP %d", tc.wantStatus, sends.Load(), w.Code)
			}
			if w.Code == 200 && (summary.FailureStatus() != 429 || !strings.Contains(w.Body.String(), "response.failed")) {
				t.Fatal("lost the independent streamed failure or its accounting")
			}
		})
	}
}

func TestResponsesStreamPassthroughQueuedCancellation(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			ctx, stop := context.WithCancel(t.Context())
			if kind == "deadline" {
				stop()
				ctx, stop = context.WithTimeout(t.Context(), 250*time.Millisecond)
			}
			defer stop()
			queued, release := make(chan struct{}), make(chan struct{})
			var sends atomic.Int32
			h := passthroughTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
				close(queued)
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case <-release:
					sends.Add(1)
					return nil, errors.New("unwanted physical send")
				}
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				passthroughTestRequest(ctx, h, passthroughStreamingRequest)
			}()
			defer func() {
				close(release)
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("physical request did not drain")
				}
			}()
			select {
			case <-queued:
			case <-time.After(time.Second):
				t.Fatal("never entered physical queue")
			}
			if kind == "cancel" {
				stop()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("physical request outlived caller while still queued")
			}
			if sends.Load() != 0 {
				t.Fatal("queued request dispatched after revocation")
			}
		})
	}
}

func TestResponsesStreamPassthroughDirectIngressOnly(t *testing.T) {
	var sends atomic.Int32
	h := passthroughTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return routeExecutorTestResponse(r, 200, http.Header{"Content-Type": {"text/event-stream"}},
			"event: response.failed\ndata: "+`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"quota"}}}`+"\n\n"), nil
	})
	for _, payload := range []string{
		`{"model":"public-model","input":"non-stream"}`,
		`{"model":"public-model","stream":true,"input":[{"type":"compaction_trigger"}]}`,
	} {
		if got := passthroughTestRequest(t.Context(), h, payload).Code; got != 400 || sends.Load() != 0 {
			t.Fatalf("unsupported request dispatched: status=%d sends=%d", got, sends.Load())
		}
	}
	// Other consumers of this same route do not carry the direct HTTP
	// marker. They retain their existing precommit error translation.
	route, _ := h.resolveModelRouteForRequest("public-model", providerEndpointResponses)
	resp, err := h.executeExplicitRouteRequest(t.Context(), route, providerEndpointResponses, []byte(passthroughStreamingRequest), nil, "public-model", true)
	status := routeErrorStatus(err)
	if resp != nil {
		status = resp.StatusCode
		_ = resp.Body.Close()
	}
	if status != 429 || sends.Load() != 1 {
		t.Fatalf("changed unmarked ingress: HTTP %d sends=%d", status, sends.Load())
	}
}
