package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestTaskUsagePublicChatIncludesFailedRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == providerEndpointModels {
			_, _ = io.WriteString(w, `{"data":[{"id":"usage-model","supported_endpoints":["/chat/completions"]}]}`)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"},"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3},"copilot_usage":{"total_nano_aiu":3,"compute_units":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"usage-response","model":"usage-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8},"copilot_usage":{"total_nano_aiu":9,"compute_units":2,"token_details":[{"model":"private-accounting-model"}]}}`)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL), WithAllowedModels("usage-model"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	h.maxRetries, h.retryBaseDelay = 2, time.Nanosecond
	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"usage-model","messages":[{"role":"user","content":"hello"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	snapshot := h.stats.snapshot().TaskUsage
	totals := snapshot.Totals
	if calls.Load() != 2 || snapshot.Inflight != 0 || totals.Sends != 2 || totals.Completed != 2 || totals.Errors != 1 || totals.Throttled != 1 || totals.ReportedUsageSends != 2 {
		t.Fatalf("retry ledger = %+v, calls=%d", snapshot, calls.Load())
	}
	if totals.Usage.PromptTokens != 7 || totals.Usage.CompletionTokens != 4 || totals.CopilotUsage.TotalNanoAIU != 12 || totals.CopilotUsage.ComputeUnits != 3 {
		t.Fatalf("failed retry spend was lost: %+v", totals)
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "private-accounting-model") {
		t.Fatal("task ledger retained accounting model metadata")
	}
	w = httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"not-allowed","messages":[{"role":"user","content":"hello"}]}`)))
	if w.Code != http.StatusBadRequest || h.stats.snapshot().TaskUsage.Totals.Sends != 2 {
		t.Fatal("local validation added an upstream send")
	}
}

func TestTaskUsageNativeCountIsSizingOnly(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErrors int64
	}{
		{"valid", `{"input_tokens":8000}`, http.StatusOK, 0},
		{"missing", `{}`, http.StatusOK, 1},
		{"negative", `{"input_tokens":-1}`, http.StatusOK, 1},
		{"truncated", `{"input_tokens":80`, http.StatusOK, 1},
		{"throttled", `{"error":{"type":"rate_limit_error"}}`, http.StatusTooManyRequests, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{stats: newStatsCollector()}
			req := httptest.NewRequest(http.MethodPost, "/custom/count", nil)
			req = withTaskInferenceRequest(req, providerEndpointMessagesCount)
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			receipt := h.beginTaskInferenceSend(req)
			receipt.finish(resp, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			snapshot := h.stats.snapshot().TaskUsage
			if snapshot.Inflight != 0 || snapshot.Totals.Sends != 1 || snapshot.Totals.Completed != 1 || snapshot.Totals.Errors != tc.wantErrors || !snapshot.Totals.Usage.isZero() || snapshot.Totals.ReportedUsageSends != 0 {
				t.Fatalf("native sizing ledger = %+v", snapshot)
			}
			if len(snapshot.ByKind) != 1 || snapshot.ByKind[0].Kind != "token_count" {
				t.Fatalf("native count classification = %+v", snapshot.ByKind)
			}
		})
	}
}

func TestTaskUsageResponsesTerminalAccounting(t *testing.T) {
	const billing = `"copilot_usage":{"total_nano_aiu":31,"compute_units":2}`
	for _, status := range []string{"completed", "incomplete", "cancelled", "canceled"} {
		response := `{"id":"resp-accounting","status":"` + status + `","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9},` + billing + `}`
		terminal := "event: response." + status + "\ndata: " + `{"type":"response.` + status + `","response":` + response + `,` + billing + "}\n\n"
		pending := "event: response.in_progress\ndata: " + `{"type":"response.in_progress","response":{"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}},` + billing + "}\n\n"
		for _, tc := range []struct {
			name, contentType, body   string
			statusCode                int
			readErr                   error
			wantErrors, wantThrottles int64
		}{
			{name: "JSON terminal", contentType: "application/json", body: response, statusCode: http.StatusOK},
			{name: "SSE terminal", contentType: "text/event-stream", body: terminal, statusCode: http.StatusOK},
			{name: "SSE terminal with Chat sentinel", contentType: "text/event-stream", body: terminal + "data: [DONE]\n\n", statusCode: http.StatusOK},
			{name: "Chat sentinel before SSE terminal", contentType: "text/event-stream", body: pending + "data: [DONE]\n\n" + terminal, statusCode: http.StatusOK},
			{name: "cancellation after terminal", contentType: "text/event-stream", body: terminal, statusCode: http.StatusOK, readErr: context.Canceled},
			{name: "HTTP failure", contentType: "application/json", body: response, statusCode: http.StatusTooManyRequests, wantErrors: 1, wantThrottles: 1},
			{name: "transport cancellation", contentType: "text/event-stream", body: pending, statusCode: http.StatusOK, readErr: context.Canceled, wantErrors: 1},
			{name: "transport deadline", contentType: "text/event-stream", body: pending, statusCode: http.StatusOK, readErr: context.DeadlineExceeded, wantErrors: 1},
			{name: "missing terminal", contentType: "text/event-stream", body: pending, statusCode: http.StatusOK, wantErrors: 1},
			{name: "Chat sentinel without terminal", contentType: "text/event-stream", body: pending + "data: [DONE]\n\n", statusCode: http.StatusOK, wantErrors: 1},
			{name: "Chat sentinel before cancellation", contentType: "text/event-stream", body: pending + "data: [DONE]\n\n", statusCode: http.StatusOK, readErr: context.Canceled, wantErrors: 1},
			{name: "failed terminal", contentType: "text/event-stream", body: pending + "data: " + `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}` + "\n\n", statusCode: http.StatusOK, wantErrors: 1, wantThrottles: 1},
		} {
			t.Run(status+"/"+tc.name, func(t *testing.T) {
				h := &ProxyHandler{stats: newStatsCollector()}
				var reader io.Reader = strings.NewReader(tc.body)
				if tc.readErr != nil {
					reader = io.MultiReader(reader, &fixedErrorReadCloser{err: tc.readErr})
				}
				resp := &http.Response{StatusCode: tc.statusCode, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(reader)}
				h.beginTaskInferenceSend(httptest.NewRequest(http.MethodPost, providerEndpointResponses, nil)).finish(resp, nil)
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				snapshot := h.stats.taskUsage.snapshot()
				if snapshot.Inflight != 0 || snapshot.Totals.Sends != 1 || snapshot.Totals.Completed != 1 || snapshot.Totals.Errors != tc.wantErrors || snapshot.Totals.Throttled != tc.wantThrottles || snapshot.Totals.Usage.TotalTokens != 9 {
					t.Fatalf("terminal accounting = %+v", snapshot)
				}
				if snapshot.Totals.CopilotUsage != (copilotUsageTotals{TotalNanoAIU: 31, ComputeUnits: 2}) {
					t.Fatalf("terminal lost reported billing: %+v", snapshot.Totals.CopilotUsage)
				}
			})
		}
	}
}

func TestTaskUsageAuxiliaryKindsAndCustomEndpoint(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector(), client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: req,
			Body: io.NopCloser(strings.NewReader(`{"status":"completed","usage":{"input_tokens":10,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}`))}, nil
	})}}
	for kind := taskInferenceKind(0); kind < taskInferenceKinds; kind++ {
		ctx := withTaskInferenceKind(context.Background(), kind)
		detached, cancel := h.newInferenceUpstreamContextFrom(ctx, false)
		resp, err := h.doInferenceWithRetry(func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(detached, http.MethodPost, "http://example.test/custom/deployment", strings.NewReader("{}"))
			if err != nil {
				return nil, err
			}
			return withTaskInferenceRequest(req, providerEndpointResponses), nil
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
	}
	snapshot := h.stats.snapshot().TaskUsage
	if snapshot.Totals.Sends != 6 || snapshot.Totals.Completed != 6 || snapshot.Totals.Errors != 0 || snapshot.Totals.Usage.PromptTokens != 60 || snapshot.Totals.Usage.ReasoningTokens != 6 || len(snapshot.ByKind) != 6 {
		t.Fatalf("auxiliary ledger = %+v", snapshot)
	}
	for _, row := range snapshot.ByKind {
		if row.Sends != 1 || row.Usage.PromptTokens != 10 || row.ReportedUsageSends != 1 {
			t.Fatalf("auxiliary kind lost its usage: %+v", row)
		}
	}
}

type taskUsagePausedClassifier struct {
	policyClassifier
	started chan struct{}
	resume  <-chan struct{}
}

func (c *taskUsagePausedClassifier) Classify(ctx context.Context, facts policyClassifierFacts) (policyClassifierSignals, error) {
	close(c.started)
	select {
	case <-c.resume:
		return c.policyClassifier.Classify(ctx, facts)
	case <-ctx.Done():
		return policyClassifierSignals{}, context.Cause(ctx)
	}
}

func TestTaskUsageStatsIncludesUndispatchedClassifier(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeObserve)
	cfg.PolicyProfiles[0].Classifier.TimeoutMS = 5000
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeObserve))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	profile := h.policyRoutingController.(*chatPolicyRoutingController).profiles[cfg.PolicyProfiles[0].ID]
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	classifier := &taskUsagePausedClassifier{policyClassifier: profile.classifierAdapter, started: make(chan struct{}), resume: resume}
	profile.classifierAdapter = classifier
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.WaitLifecycleWorkers(ctx); err != nil {
			t.Error(err)
		}
	})

	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan a refactor"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-classifier.started:
	case <-time.After(5 * time.Second):
		t.Fatal("observe classifier did not start")
	}
	readStats := func() (int64, taskUsageSnapshot) {
		t.Helper()
		w := httptest.NewRecorder()
		h.HandleStatsJSON(w, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
		var snapshot struct {
			AuxiliaryInflight int64             `json:"auxiliary_inflight"`
			TaskUsage         taskUsageSnapshot `json:"task_usage"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot.AuxiliaryInflight, snapshot.TaskUsage
	}
	workers, usage := readStats()
	if workers != 1 || usage.Inflight != 0 || usage.Totals.Sends != 2 {
		t.Fatalf("pending classifier was hidden or counted as a send: workers=%d usage=%+v", workers, usage)
	}
	release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.WaitLifecycleWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	workers, usage = readStats()
	if workers != 0 || usage.Inflight != 0 || usage.Totals.Sends != 3 || usage.Totals.Usage.PromptTokens != 22 {
		t.Fatalf("settled stats omitted classifier usage: workers=%d usage=%+v", workers, usage)
	}
}

func TestTaskUsageStreamKeepsEarlyAccounting(t *testing.T) {
	var body strings.Builder
	fmt.Fprint(&body, "data: ", `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18},"copilot_usage":{"total_nano_aiu":31,"compute_units":2}}`, "\n\n")
	for range 600 {
		text, _ := json.Marshal(strings.Repeat("x", 1024) + `"copilot_usage":{"total_nano_aiu":999999}`)
		fmt.Fprintf(&body, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", text)
	}
	fmt.Fprint(&body, "data: ", `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "\n\ndata: [DONE]\n\n")
	h := &ProxyHandler{stats: newStatsCollector()}
	receipt := h.beginTaskInferenceSend(httptest.NewRequest(http.MethodPost, "/chat/completions", nil))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body.String()))}
	receipt.finish(resp, nil)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	snapshot := h.stats.snapshot().TaskUsage
	if snapshot.Totals.Errors != 0 || snapshot.Totals.ReportedUsageSends != 1 || snapshot.Totals.Usage.PromptTokens != 11 || snapshot.Totals.CopilotUsage.TotalNanoAIU != 31 {
		t.Fatalf("early accounting lost or text mistaken for accounting: %+v", snapshot)
	}
}

func TestTaskUsageConcurrentEOFAndCloseCountsOnce(t *testing.T) {
	for range 30 {
		h := &ProxyHandler{stats: newStatsCollector()}
		receipt := h.beginTaskInferenceSend(httptest.NewRequest(http.MethodPost, "/chat/completions", nil))
		reader, writer := io.Pipe()
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: reader}
		receipt.finish(resp, nil)
		receipt.finish(resp, nil)
		var workers sync.WaitGroup
		workers.Add(1)
		go func() { defer workers.Done(); _, _ = io.Copy(io.Discard, resp.Body) }()
		_, _ = io.WriteString(writer, `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
		workers.Add(2)
		go func() { defer workers.Done(); _ = writer.Close() }()
		go func() { defer workers.Done(); _ = resp.Body.Close() }()
		workers.Wait()
		_ = resp.Body.Close()
		snapshot := h.stats.snapshot().TaskUsage
		if snapshot.Inflight != 0 || snapshot.Totals.Sends != 1 || snapshot.Totals.Completed != 1 || snapshot.Totals.ReportedUsageSends != 1 || snapshot.Totals.Usage.PromptTokens != 5 || snapshot.Totals.Errors > 1 {
			t.Fatalf("concurrent close duplicated or lost accounting: %+v", snapshot)
		}
	}
}

type taskUsageDelayedReadBody struct {
	first   []byte
	last    []byte
	started chan struct{}
	release chan struct{}
}

func (b *taskUsageDelayedReadBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	close(b.started)
	<-b.release
	return copy(p, b.last), nil
}

func (*taskUsageDelayedReadBody) Close() error { return nil }

func TestTaskUsageCloseWaitsForAlreadyReadingUsage(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, contentType, first, last string
	}{
		{"Chat JSON", providerEndpointChatCompletions, "application/json", "", `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`},
		{"Messages SSE", providerEndpointMessages, "text/event-stream",
			"event: message_start\ndata: " + `{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":0}}}` + "\n\n",
			"event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":3}}` + "\n\nevent: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{stats: newStatsCollector()}
			source := &taskUsageDelayedReadBody{first: []byte(tc.first), last: []byte(tc.last), started: make(chan struct{}), release: make(chan struct{})}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}, Body: source}
			h.beginTaskInferenceSend(httptest.NewRequest(http.MethodPost, tc.endpoint, nil)).finish(resp, nil)
			buffer := make([]byte, 4096)
			if tc.first != "" {
				if _, err := resp.Body.Read(buffer); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			go func() { defer close(done); _, _ = resp.Body.Read(buffer) }()
			<-source.started
			_ = resp.Body.Close()
			before := h.stats.snapshot().TaskUsage
			close(source.release)
			<-done
			if before.Inflight != 1 || before.Totals.Completed != 0 {
				t.Fatalf("completed while a read still owned bytes: %+v", before)
			}
			after := h.stats.snapshot().TaskUsage
			if after.Inflight != 0 || after.Totals.Completed != 1 || after.Totals.Errors != 0 || after.Totals.ReportedUsageSends != 1 || after.Totals.Usage.PromptTokens != 5 || after.Totals.Usage.CompletionTokens != 3 {
				t.Fatalf("late usage lost after close: %+v", after)
			}
		})
	}
}
