package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func observeNonStreamingRouteAttempt(t *testing.T, endpoint, body string) recentRouteAttempt {
	t.Helper()
	h := &ProxyHandler{stats: newStatsCollector()}
	record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
		OperationID:  "operation-large-envelope",
		RouteID:      "route-large-envelope",
		TargetID:     "target-large-envelope",
		ProviderID:   "provider-large-envelope",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
	})
	trace := routeAttemptTrace{
		Sequence:   1,
		TargetID:   "target-large-envelope",
		ProviderID: "provider-large-envelope",
		Kind:       routeAttemptNormal,
		StatusCode: http.StatusOK,
		Delivery:   requestDeliveredOrAmbiguous,
		Progress:   upstreamProgressNone,
		Commitment: downstreamCommitmentNone,
		Decision:   routeRetryAccepted,
	}
	resp := observeRouteAttemptResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, record, nil, trace, nil, endpoint, false)

	var got bytes.Buffer
	readBuf := make([]byte, 7)
	for {
		n, err := resp.Body.Read(readBuf)
		if n > 0 {
			_, _ = got.Write(readBuf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read observed response: %v", err)
		}
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close observed response: %v", err)
	}
	if got.String() != body {
		t.Fatal("attempt observer changed the passthrough response body")
	}

	snap := h.stats.snapshot()
	if len(snap.RecentAttempts) != 1 {
		t.Fatalf("recent attempts = %+v, want one", snap.RecentAttempts)
	}
	return snap.RecentAttempts[0]
}

func routeAttemptBySequence(t *testing.T, attempts []recentRouteAttempt, sequence int) recentRouteAttempt {
	t.Helper()
	for _, attempt := range attempts {
		if attempt.Sequence == sequence {
			return attempt
		}
	}
	t.Fatalf("attempt sequence %d not found in %+v", sequence, attempts)
	return recentRouteAttempt{}
}

func TestPhysicalAttemptLedgerPrimaryFailureSecondarySuccess(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "999999999999")
		w.Header().Set("X-Request-Id", "req-primary")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"quota"},"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-secondary")
		_, _ = io.WriteString(w, `{"id":"resp-secondary","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}`)
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	h.stats = newStatsCollector()
	requestCtx, summary := WithRequestSummary(context.Background())
	summary.setRoute("/v1/responses", "public-model", false)
	operation := newRouteOperation(route, requestCtx)
	ctx := withRouteOperation(requestCtx, operation)

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","input":"hello"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	observeResponsesUsage(requestCtx, sniffResponsesUsageBody(body))
	h.RecordRequest(summary, http.StatusOK, "codex/1", 10*time.Millisecond)

	snap := h.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.TotalTokens != 15 {
		t.Fatalf("client ledger = requests:%d tokens:%d, want 1/15", snap.Totals.Requests, snap.Totals.TotalTokens)
	}
	if snap.PhysicalUsage.TotalTokens != 20 || snap.WastedUsage.TotalTokens != 5 {
		t.Fatalf("physical/wasted usage = %+v / %+v, want 20 / 5", snap.PhysicalUsage, snap.WastedUsage)
	}
	if len(snap.RecentAttempts) != 2 {
		t.Fatalf("recent attempts len = %d, want 2: %+v", len(snap.RecentAttempts), snap.RecentAttempts)
	}

	first := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if first.TargetID != "target-primary" || first.ProviderID != "primary" || first.AttemptKind != routeAttemptNormal {
		t.Fatalf("primary identity = %+v", first)
	}
	if first.StatusCode != http.StatusTooManyRequests || first.Outcome != routeAttemptOutcomeRejected || first.RetryDecision != routeRetrySwitchTarget {
		t.Fatalf("primary outcome = %+v", first)
	}
	if first.RetryAfterSeconds == nil || *first.RetryAfterSeconds != int64(maxRetryAfter/time.Second) {
		t.Fatalf("primary retry-after = %v, want clamped %d", first.RetryAfterSeconds, int64(maxRetryAfter/time.Second))
	}
	if first.ReportedUsage == nil || first.ReportedUsage.TotalTokens != 5 {
		t.Fatalf("primary reported usage = %+v, want 5", first.ReportedUsage)
	}
	if first.UpstreamRequestID != "req-primary" || !first.CleanupComplete {
		t.Fatalf("primary diagnostics = %+v", first)
	}

	second := routeAttemptBySequence(t, snap.RecentAttempts, 2)
	if second.TargetID != "target-secondary" || second.ProviderID != "secondary" || second.AttemptKind != routeAttemptFailover {
		t.Fatalf("secondary identity = %+v", second)
	}
	if second.StatusCode != http.StatusOK || second.Outcome != routeAttemptOutcomeSucceeded || second.RetryDecision != routeRetryAccepted {
		t.Fatalf("secondary outcome = %+v", second)
	}
	if second.ReportedUsage == nil || second.ReportedUsage.TotalTokens != 15 {
		t.Fatalf("secondary reported usage = %+v, want 15", second.ReportedUsage)
	}
	if second.UpstreamRequestID != "req-secondary" || !second.CleanupComplete {
		t.Fatalf("secondary diagnostics = %+v", second)
	}

	byTarget := make(map[string]statsTargetBreakdown, len(snap.ByTarget))
	for _, row := range snap.ByTarget {
		byTarget[row.Target] = row
	}
	if got := byTarget["target-primary"].PhysicalUsage.TotalTokens; got != 5 {
		t.Fatalf("primary physical tokens = %d, want 5", got)
	}
	if got := byTarget["target-primary"].WastedUsage.TotalTokens; got != 5 {
		t.Fatalf("primary wasted tokens = %d, want 5", got)
	}
	if got := byTarget["target-secondary"].PhysicalUsage.TotalTokens; got != 15 {
		t.Fatalf("secondary physical tokens = %d, want 15", got)
	}
}

func TestPhysicalAttemptLedgerPartialTerminalFailureIsWasted(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-partial\"}}\n\n"+
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-partial\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\",\"headers\":{\"retry-after-ms\":\"1200\",\"x-request-id\":\"event-request-id\"}},\"usage\":{\"input_tokens\":11,\"output_tokens\":4,\"total_tokens\":15}}}\n\n")
	}))
	defer primary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 1, 1,
		explicitRouteTestProvider("primary", primary.URL, "one"),
	)
	h.stats = newStatsCollector()
	requestCtx, summary := WithRequestSummary(context.Background())
	summary.setRoute("/v1/responses", "public-model", true)
	operation := newRouteOperation(route, requestCtx)
	ctx := withRouteOperation(requestCtx, operation)

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want translated terminal failure", resp)
	}
	if got := upstreamStatusCode(err, 0); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d error=%v, want 429", got, err)
	}
	h.RecordRequest(summary, http.StatusTooManyRequests, "codex/1", 10*time.Millisecond)

	snap := h.stats.snapshot()
	if snap.Totals.TotalTokens != 15 || snap.ByProvider[0].Tokens != 15 {
		t.Fatalf("client failure ledger lost terminal partial usage: totals=%+v providers=%+v", snap.Totals, snap.ByProvider)
	}
	if snap.PhysicalUsage.TotalTokens != 15 || snap.WastedUsage.TotalTokens != 15 {
		t.Fatalf("physical/wasted usage = %+v / %+v, want 15 / 15", snap.PhysicalUsage, snap.WastedUsage)
	}
	if len(snap.RecentAttempts) != 1 {
		t.Fatalf("recent attempts = %+v", snap.RecentAttempts)
	}
	attempt := snap.RecentAttempts[0]
	if attempt.Outcome != routeAttemptOutcomeFailed || attempt.StatusCode != http.StatusTooManyRequests || attempt.SemanticProgress != upstreamProgressTerminalFailure || attempt.RetryDecision != routeRetrySuppressedProgress {
		t.Fatalf("partial failure attempt = %+v", attempt)
	}
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 15 {
		t.Fatalf("partial reported usage = %+v", attempt.ReportedUsage)
	}
	if attempt.RetryAfterSeconds == nil || *attempt.RetryAfterSeconds != 2 {
		t.Fatalf("retry-after = %v, want 2 seconds", attempt.RetryAfterSeconds)
	}
	if attempt.UpstreamRequestID != "event-request-id" || !attempt.CleanupComplete {
		t.Fatalf("partial diagnostics = %+v", attempt)
	}
}

func TestPhysicalAttemptLedgerBoundsAndRedactsRecentTrace(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	long := strings.Repeat("x", statOperationalLabelMaxLen*3) + "\n\tsecret"
	retryAfter := int64(999999)
	for i := 0; i < statsRecentRouteAttempts+32; i++ {
		record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
			OperationID:  long,
			RouteID:      long + string(rune(i)),
			TargetID:     long + string(rune(i)),
			ProviderID:   long,
			ProviderKind: long,
			Sequence:     i + 1,
			AttemptKind:  routeAttemptProtocolRecovery,
		})
		record.complete(routeAttemptCompletion{
			StatusCode:        http.StatusServiceUnavailable,
			Outcome:           routeAttemptOutcomeFailed,
			Delivery:          requestDeliveredOrAmbiguous,
			RetryDecision:     routeRetrySuppressedDelivery,
			RetryAfterSeconds: &retryAfter,
			UpstreamRequestID: long,
			CleanupComplete:   true,
			Wasted:            true,
		})
	}

	snap := h.stats.snapshot()
	if len(snap.RecentAttempts) != statsRecentRouteAttempts {
		t.Fatalf("recent attempts len = %d, want %d", len(snap.RecentAttempts), statsRecentRouteAttempts)
	}
	if len(snap.ByTarget) > statsBreakdownRows {
		t.Fatalf("by_target rows = %d, want <= %d", len(snap.ByTarget), statsBreakdownRows)
	}
	h.stats.mu.Lock()
	targetKeys := len(h.stats.byTarget)
	h.stats.mu.Unlock()
	if targetKeys > statsMaxKeys+1 {
		t.Fatalf("target aggregate cardinality = %d, want <= %d", targetKeys, statsMaxKeys+1)
	}

	encoded, err := json.Marshal(snap.RecentAttempts)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "\n") || strings.Contains(string(encoded), "\t") {
		t.Fatalf("control characters retained in recent attempts: %s", encoded)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("unbounded suffix leaked into recent attempts: %s", encoded)
	}
	for _, attempt := range snap.RecentAttempts {
		for label, value := range map[string]string{
			"operation": attempt.OperationID,
			"route":     attempt.RouteID,
			"target":    attempt.TargetID,
			"provider":  attempt.ProviderID,
			"kind":      attempt.ProviderKind,
			"request":   attempt.UpstreamRequestID,
		} {
			if got := len([]rune(value)); got > statOperationalLabelMaxLen+1 {
				t.Errorf("%s retained %d runes, want <= %d", label, got, statOperationalLabelMaxLen+1)
			}
			if strings.ContainsAny(value, "\n\r\t") {
				t.Errorf("%s retained control characters: %q", label, value)
			}
		}
		if attempt.RetryAfterSeconds == nil || *attempt.RetryAfterSeconds != int64(maxRetryAfter/time.Second) {
			t.Errorf("retry-after = %v, want clamped %d", attempt.RetryAfterSeconds, int64(maxRetryAfter/time.Second))
		}
	}
}

func TestPhysicalAttemptLedgerWebSocketStyleContextWithoutSummary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "ws-upstream-id")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-ws\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-ws\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9}}}\n\n")
	}))
	defer upstream.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", upstream.URL, "one"),
	)
	h.stats = newStatsCollector()
	operation := newRouteOperation(route, context.Background())
	operation.mu.Lock()
	operation.id = "vekil-ws-test:1"
	operation.mu.Unlock()
	ctx := withRouteOperation(context.Background(), operation) // deliberately no RequestSummary

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}
	_ = resp.Body.Close()

	snap := h.stats.snapshot()
	if snap.Totals.Requests != 0 {
		t.Fatalf("physical attempt created a client request row: %+v", snap.Totals)
	}
	if snap.UpstreamAttempts != 1 || snap.PhysicalUsage.TotalTokens != 9 || snap.WastedUsage.TotalTokens != 0 {
		t.Fatalf("physical ledger = attempts:%d usage:%+v wasted:%+v", snap.UpstreamAttempts, snap.PhysicalUsage, snap.WastedUsage)
	}
	if len(snap.RecentAttempts) != 1 {
		t.Fatalf("recent attempts = %+v", snap.RecentAttempts)
	}
	attempt := snap.RecentAttempts[0]
	if attempt.OperationID != "vekil-ws-test:1" || attempt.RouteID != "public-route" || attempt.TargetID != "target-primary" || attempt.ProviderID != "primary" {
		t.Fatalf("websocket-style attempt identity = %+v", attempt)
	}
	if attempt.Outcome != routeAttemptOutcomeSucceeded || attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 9 || attempt.UpstreamRequestID != "ws-upstream-id" {
		t.Fatalf("websocket-style attempt diagnostics = %+v", attempt)
	}

	// The websocket turn ledger remains a separate one-per-create record.
	usage := responsesUsage{InputTokens: 7, OutputTokens: 2, TotalTokens: 9}
	h.RecordResponsesTurn("public-model", "primary", "azure", "Codex CLI", http.StatusOK, usage)
	after := h.stats.snapshot()
	if after.Totals.Requests != 1 || after.Totals.TotalTokens != 9 || after.PhysicalUsage.TotalTokens != 9 {
		t.Fatalf("two-ledger websocket accounting = totals:%+v physical:%+v", after.Totals, after.PhysicalUsage)
	}
}

func TestRequestSummaryFinalUsageRemainsIndependentFromPhysicalWastedUsage(t *testing.T) {
	summary := &RequestSummary{}
	summary.setOpenAIUsage(&models.OpenAIUsage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15})
	summary.addInternalUsage(3, 1)

	got := readSummaryForStats(summary)
	if got.prompt != 14 || got.completion != 5 || got.total != 19 {
		t.Fatalf("client usage = prompt:%d completion:%d total:%d, want 14/5/19", got.prompt, got.completion, got.total)
	}
}

func TestRouteAttemptDiagnosticTailManyTinyReadsIsAmortized(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), routeAttemptObservationTail+1024)
	allocs := testing.AllocsPerRun(1, func() {
		observer := &routeAttemptResponseObserver{}
		body := &routeAttemptObservedBody{
			inner:    io.NopCloser(bytes.NewReader(payload)),
			observer: observer,
		}
		buf := make([]byte, 1)
		for read := 0; read < len(payload); {
			n, err := body.Read(buf)
			if err != nil {
				panic(err)
			}
			read += n
		}
	})
	if allocs > 16 {
		t.Fatalf("many tiny reads allocated %.0f objects, want <= 16", allocs)
	}

	observer := &routeAttemptResponseObserver{}
	for i := range payload {
		observer.observe(payload[i : i+1])
	}
	if got, want := observer.tail.bytes(), payload[len(payload)-routeAttemptObservationTail:]; !bytes.Equal(got, want) {
		t.Fatal("bounded diagnostic tail did not retain the exact suffix")
	}
	if len(observer.tail.buf) > routeAttemptObservationTail || cap(observer.tail.buf) > routeAttemptObservationTail {
		t.Fatalf("diagnostic tail len/cap = %d/%d, want <= %d", len(observer.tail.buf), cap(observer.tail.buf), routeAttemptObservationTail)
	}
}

func BenchmarkRouteAttemptDiagnosticTailTinyReads(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), routeAttemptObservationTail+1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		observer := &routeAttemptResponseObserver{}
		for i := range payload {
			observer.observe(payload[i : i+1])
		}
	}
}

func TestRouteAttemptObserverOversizedNonStreamingEnvelopes(t *testing.T) {
	padding := strings.Repeat("x", routeAttemptObservationTail+1024)
	responsesUsageJSON := `"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}`
	anthropicUsageJSON := `"usage":{"input_tokens":11,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}`

	tests := []struct {
		name       string
		endpoint   string
		body       string
		outcome    routeAttemptOutcome
		progress   upstreamSemanticProgress
		statusCode int
		usage      statsTokenUsage
	}{
		{
			name:       "responses completed",
			endpoint:   providerEndpointResponses,
			body:       `{"padding_before":"` + padding + `","status":"completed",` + responsesUsageJSON + `,"padding_after":"` + padding + `"}`,
			outcome:    routeAttemptOutcomeSucceeded,
			progress:   upstreamProgressTerminalSuccess,
			statusCode: http.StatusOK,
			usage: statsTokenUsage{
				PromptTokens:     11,
				CompletionTokens: 4,
				TotalTokens:      15,
				CachedTokens:     3,
				ReasoningTokens:  2,
			},
		},
		{
			name:       "responses failed",
			endpoint:   providerEndpointResponses,
			body:       `{"padding_before":"` + padding + `","status":"failed",` + responsesUsageJSON + `,"padding_after":"` + padding + `"}`,
			outcome:    routeAttemptOutcomeFailed,
			progress:   upstreamProgressTerminalFailure,
			statusCode: http.StatusOK,
			usage: statsTokenUsage{
				PromptTokens:     11,
				CompletionTokens: 4,
				TotalTokens:      15,
				CachedTokens:     3,
				ReasoningTokens:  2,
			},
		},
		{
			name:       "chat completion with early usage",
			endpoint:   providerEndpointChatCompletions,
			body:       `{"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}},"choices":[{"index":0,"message":{"role":"assistant","content":"` + padding + `"},"finish_reason":"stop"}]}`,
			outcome:    routeAttemptOutcomeSucceeded,
			progress:   upstreamProgressTerminalSuccess,
			statusCode: http.StatusOK,
			usage: statsTokenUsage{
				PromptTokens:     11,
				CompletionTokens: 4,
				TotalTokens:      15,
				CachedTokens:     3,
				ReasoningTokens:  2,
			},
		},
		{
			name:       "anthropic message",
			endpoint:   providerEndpointMessages,
			body:       `{"padding_before":"` + padding + `","type":"message",` + anthropicUsageJSON + `,"padding_after":"` + padding + `"}`,
			outcome:    routeAttemptOutcomeSucceeded,
			progress:   upstreamProgressTerminalSuccess,
			statusCode: http.StatusOK,
			usage: statsTokenUsage{
				PromptTokens:     16,
				CompletionTokens: 4,
				TotalTokens:      20,
				CachedTokens:     3,
			},
		},
		{
			name:       "anthropic error",
			endpoint:   providerEndpointMessages,
			body:       `{"padding_before":"` + padding + `","type":"error","error":{"type":"overloaded_error","message":"capacity"},` + anthropicUsageJSON + `,"padding_after":"` + padding + `"}`,
			outcome:    routeAttemptOutcomeFailed,
			progress:   upstreamProgressTerminalFailure,
			statusCode: http.StatusServiceUnavailable,
			usage: statsTokenUsage{
				PromptTokens:     16,
				CompletionTokens: 4,
				TotalTokens:      20,
				CachedTokens:     3,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attempt := observeNonStreamingRouteAttempt(t, tc.endpoint, tc.body)
			if attempt.Outcome != tc.outcome || attempt.SemanticProgress != tc.progress || attempt.StatusCode != tc.statusCode {
				t.Fatalf("classification = outcome:%q progress:%q status:%d, want %q/%q/%d", attempt.Outcome, attempt.SemanticProgress, attempt.StatusCode, tc.outcome, tc.progress, tc.statusCode)
			}
			if attempt.ReportedUsage == nil || *attempt.ReportedUsage != tc.usage {
				t.Fatalf("reported usage = %+v, want %+v", attempt.ReportedUsage, tc.usage)
			}
		})
	}
}

func TestRouteAttemptObserverValidatesNonStreamingChatEnvelopes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		outcome  routeAttemptOutcome
		progress upstreamSemanticProgress
	}{
		{
			name:     "valid",
			body:     `{"id":"chatcmpl-valid","choices":[]}`,
			outcome:  routeAttemptOutcomeSucceeded,
			progress: upstreamProgressTerminalSuccess,
		},
		{
			name:     "empty",
			body:     "",
			outcome:  routeAttemptOutcomeIncomplete,
			progress: upstreamProgressUnknown,
		},
		{
			name:     "malformed",
			body:     `{"id":]}`,
			outcome:  routeAttemptOutcomeFailed,
			progress: upstreamProgressUnknown,
		},
		{
			name:     "truncated",
			body:     `{"id":"chatcmpl-truncated","choices":[`,
			outcome:  routeAttemptOutcomeIncomplete,
			progress: upstreamProgressUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := observeNonStreamingRouteAttempt(t, providerEndpointChatCompletions, tt.body)
			if attempt.Outcome != tt.outcome || attempt.SemanticProgress != tt.progress {
				t.Fatalf("chat envelope classification = %+v, want %s/%s", attempt, tt.outcome, tt.progress)
			}
		})
	}
}

func TestRouteAttemptObserverRejectsMalformedOversizedCompletedEnvelope(t *testing.T) {
	padding := strings.Repeat("x", routeAttemptObservationTail+1024)
	body := `{"status":"completed","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5},"padding":"` + padding + `","output":[true,]}`

	attempt := observeNonStreamingRouteAttempt(t, providerEndpointResponses, body)
	if attempt.Outcome != routeAttemptOutcomeFailed || attempt.SemanticProgress != upstreamProgressUnknown {
		t.Fatalf("malformed completed attempt = %+v, want failed/unknown", attempt)
	}
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
		t.Fatalf("malformed completed usage = %+v, want 5", attempt.ReportedUsage)
	}
}

func TestRouteAttemptObserverOverflowingUnfinishedResponsesEnvelopeIsIncomplete(t *testing.T) {
	padding := strings.Repeat("x", routeAttemptObservationTail+1024)
	body := `{"status":"completed","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5},"padding":"` + padding

	attempt := observeNonStreamingRouteAttempt(t, providerEndpointResponses, body)
	if attempt.Outcome != routeAttemptOutcomeIncomplete || attempt.SemanticProgress != upstreamProgressUnknown {
		t.Fatalf("unfinished oversized attempt = %+v, want incomplete/unknown", attempt)
	}
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
		t.Fatalf("unfinished oversized usage = %+v, want 5", attempt.ReportedUsage)
	}
}

func TestPhysicalAttemptLedgerRecordsDeterministicTTFT(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	now := start
	client := &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		now = start.Add(37 * time.Millisecond)
		return routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, `{"id":"resp-ttft","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`), nil
	})}
	h, route := explicitRouteTestHandler(t, client, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", "https://primary.example", "one"),
	)
	h.stats = newStatsCollector()
	h.stats.now = func() time.Time { return now }
	operation := newRouteOperation(route, context.Background())
	resp, err := h.executeExplicitRouteRequest(withRouteOperation(context.Background(), operation), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	attempt := h.stats.snapshot().RecentAttempts[0]
	if attempt.TTFTMs == nil || *attempt.TTFTMs != 37 {
		t.Fatalf("TTFT = %v, want 37ms; attempt=%+v", attempt.TTFTMs, attempt)
	}
}

func TestAcceptedStreamingAttemptRemainsInFlightUntilTerminalEvent(t *testing.T) {
	reader, writer := io.Pipe()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       reader,
			Request:    req,
		}, nil
	})}
	h, route := explicitRouteTestHandler(t, client, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", "https://primary.example", "one"),
	)
	h.stats = newStatsCollector()
	operation := newRouteOperation(route, context.Background())

	resp, err := h.executeExplicitRouteRequest(
		withRouteOperation(context.Background(), operation),
		route,
		providerEndpointChatCompletions,
		[]byte(`{"model":"public-model","stream":true}`),
		nil,
		"public-model",
		true,
	)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	defer func() { _ = writer.Close() }()

	active := h.stats.snapshot()
	attempt := routeAttemptBySequence(t, active.RecentAttempts, 1)
	if attempt.Outcome != routeAttemptOutcomeInFlight || attempt.CleanupComplete {
		t.Fatalf("active stream attempt = %+v, want in_flight with cleanup incomplete", attempt)
	}
	if !active.PhysicalUsage.isZero() || !active.WastedUsage.isZero() {
		t.Fatalf("active stream usage = physical:%+v wasted:%+v, want zero before terminal usage", active.PhysicalUsage, active.WastedUsage)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(writer,
			"data: {\"id\":\"chatcmpl-active\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1,\"total_tokens\":5}}\n\n"+
				"data: [DONE]\n\n")
		if writeErr == nil {
			writeErr = writer.Close()
		}
		writeDone <- writeErr
	}()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain active stream: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write active stream: %v", err)
	}

	completed := h.stats.snapshot()
	attempt = routeAttemptBySequence(t, completed.RecentAttempts, 1)
	if attempt.Outcome != routeAttemptOutcomeSucceeded || !attempt.CleanupComplete {
		t.Fatalf("terminal stream attempt = %+v, want succeeded with cleanup complete", attempt)
	}
	if completed.PhysicalUsage.TotalTokens != 5 || completed.WastedUsage.TotalTokens != 0 {
		t.Fatalf("terminal stream usage = physical:%+v wasted:%+v, want 5/0", completed.PhysicalUsage, completed.WastedUsage)
	}
}

func TestStreamingTerminalFailureCannotBeWeakenedByLaterSuccessInSameRead(t *testing.T) {
	tests := []struct {
		name             string
		firstEvent       string
		completedPadding string
		wantOutcome      routeAttemptOutcome
	}{
		{
			name: "failed then completed and done",
			firstEvent: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp-terminal","error":{"code":"rate_limit_exceeded","message":"quota"},"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}` + "\n\n",
			wantOutcome: routeAttemptOutcomeFailed,
		},
		{
			name: "failed then oversized completed",
			firstEvent: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp-terminal","error":{"code":"rate_limit_exceeded","message":"quota"},"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}` + "\n\n",
			completedPadding: strings.Repeat("x", routeAttemptObservationMaxLine+1),
			wantOutcome:      routeAttemptOutcomeFailed,
		},
		{
			name: "cancelled then completed and done",
			firstEvent: "event: response.cancelled\n" +
				`data: {"type":"response.cancelled","response":{"id":"resp-terminal","status":"cancelled","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}` + "\n\n",
			wantOutcome: routeAttemptOutcomeCanceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &ProxyHandler{stats: newStatsCollector()}
			record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
				OperationID:  "operation-terminal-monotonic",
				RouteID:      "route-terminal-monotonic",
				TargetID:     "target-terminal-monotonic",
				ProviderID:   "provider-terminal-monotonic",
				ProviderKind: "test",
				Sequence:     1,
				AttemptKind:  routeAttemptNormal,
			})
			trace := routeAttemptTrace{
				Sequence:   1,
				TargetID:   "target-terminal-monotonic",
				ProviderID: "provider-terminal-monotonic",
				Kind:       routeAttemptNormal,
				StatusCode: http.StatusOK,
				Delivery:   requestDeliveredOrAmbiguous,
				Progress:   upstreamProgressNone,
				Commitment: downstreamCommitmentNone,
				Decision:   routeRetryAccepted,
			}
			body := tt.firstEvent +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp-terminal","status":"completed","padding":"` + tt.completedPadding + `","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}` + "\n\n" +
				"data: [DONE]\n\n"
			resp := observeRouteAttemptResponse(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			}, record, nil, trace, nil, providerEndpointResponses, true)

			buf := make([]byte, len(body))
			n, err := resp.Body.Read(buf)
			if err != nil || n != len(body) {
				t.Fatalf("single terminal read = %d, %v, want %d bytes and nil", n, err, len(body))
			}
			if n, err := resp.Body.Read(buf[:1]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("terminal EOF read = %d, %v, want 0, EOF", n, err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close terminal stream: %v", err)
			}

			attempt := routeAttemptBySequence(t, h.stats.snapshot().RecentAttempts, 1)
			if attempt.Outcome != tt.wantOutcome || attempt.SemanticProgress != upstreamProgressTerminalFailure {
				t.Fatalf("terminal attempt = %+v, want %s/terminal_failure", attempt, tt.wantOutcome)
			}
			if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
				t.Fatalf("terminal usage = %+v, want 5", attempt.ReportedUsage)
			}
		})
	}
}

func TestRouteAttemptObserverParsesMixedSSELineEndingsAcrossReadBoundaries(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
		OperationID:  "operation-mixed-sse-lines",
		RouteID:      "route-mixed-sse-lines",
		TargetID:     "target-mixed-sse-lines",
		ProviderID:   "provider-mixed-sse-lines",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
	})
	trace := routeAttemptTrace{
		Sequence:   1,
		TargetID:   "target-mixed-sse-lines",
		ProviderID: "provider-mixed-sse-lines",
		Kind:       routeAttemptNormal,
		StatusCode: http.StatusOK,
		Delivery:   requestDeliveredOrAmbiguous,
		Progress:   upstreamProgressNone,
		Commitment: downstreamCommitmentNone,
		Decision:   routeRetryAccepted,
	}
	body := newSplitChunkEOFReadCloser(
		"event: response.completed\r",
		"\n",
		`data: {"response":{"id":"resp-mixed-lines","status":"completed","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`+"\r",
		"\r",
		"data: [DONE]\n",
		"\n",
	)
	resp := observeRouteAttemptResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}, record, nil, trace, nil, providerEndpointResponses, true)

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain mixed-line stream: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close mixed-line stream: %v", err)
	}

	snap := h.stats.snapshot()
	attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if attempt.Outcome != routeAttemptOutcomeSucceeded || attempt.SemanticProgress != upstreamProgressTerminalSuccess || !attempt.CleanupComplete {
		t.Fatalf("mixed-line terminal attempt = %+v", attempt)
	}
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 9 {
		t.Fatalf("mixed-line usage = %+v, want 9", attempt.ReportedUsage)
	}
	if snap.PhysicalUsage.TotalTokens != 9 || snap.WastedUsage.TotalTokens != 0 {
		t.Fatalf("mixed-line accounting = physical:%+v wasted:%+v", snap.PhysicalUsage, snap.WastedUsage)
	}
}

func TestResponseHeaderBindingFailureRetainsChatAndMessagesUsage(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		providerKind providerType
		body         string
		wantPrompt   int64
		wantOutput   int64
		wantTotal    int64
	}{
		{
			name:         "chat completions",
			endpoint:     providerEndpointChatCompletions,
			providerKind: providerTypeAzureOpenAI,
			body:         `{"id":"chatcmpl-binding","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`,
			wantPrompt:   6,
			wantOutput:   2,
			wantTotal:    8,
		},
		{
			name:         "chat completions usage before oversized choices",
			endpoint:     providerEndpointChatCompletions,
			providerKind: providerTypeAzureOpenAI,
			body:         `{"id":"chatcmpl-binding-large","usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13},"choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("x", routeAttemptObservationTail+1024) + `"},"finish_reason":"stop"}]}`,
			wantPrompt:   9,
			wantOutput:   4,
			wantTotal:    13,
		},
		{
			name:         "messages",
			endpoint:     providerEndpointMessages,
			providerKind: providerTypeAnthropicCompatible,
			body:         `{"id":"msg-binding","type":"message","role":"assistant","content":[],"usage":{"input_tokens":5,"output_tokens":3}}`,
			wantPrompt:   5,
			wantOutput:   3,
			wantTotal:    8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Add("X-Codex-Turn-State", "state-one")
				w.Header().Add("X-Codex-Turn-State", "state-two")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()

			provider := explicitRouteTestProvider("primary", upstream.URL, "one")
			provider.kind = tt.providerKind
			provider.paths = providerEndpointPolicyFor(tt.providerKind).defaultEndpointPaths()
			route := &modelRoute{
				public: publicModelContract{
					id:        "public-model",
					routeID:   "public-route",
					endpoints: []string{tt.endpoint},
				},
				targets: []targetBinding{{id: "target-primary", provider: provider, upstreamModel: "deployment-a"}},
				policy:  routePolicy{mode: routeModePrimaryOnly, maxTargetAttempts: 1, maxUpstreamSends: 1},
			}
			registry, err := newModelRouteRegistry([]*modelRoute{route})
			if err != nil {
				t.Fatalf("newModelRouteRegistry() error = %v", err)
			}
			h := &ProxyHandler{
				client: http.DefaultClient,
				providersState: &providerSetup{
					providers:          map[string]*providerRuntime{provider.id: provider},
					providerOrder:      []string{provider.id},
					defaultProviderID:  provider.id,
					routes:             registry,
					models:             map[string]providerModel{},
					hasConfiguredState: true,
				},
				stats: newStatsCollector(),
			}
			h.initializeLifecycle()
			operation := newRouteOperation(route, context.Background())

			resp, err := h.executeExplicitRouteRequest(
				withRouteOperation(context.Background(), operation),
				route,
				tt.endpoint,
				[]byte(`{"model":"public-model"}`),
				nil,
				"public-model",
				false,
			)
			if resp != nil {
				_ = resp.Body.Close()
				t.Fatalf("response = %#v, want binding failure", resp)
			}
			if err == nil || !strings.Contains(err.Error(), "conflicting X-Codex-Turn-State") {
				t.Fatalf("executeExplicitRouteRequest() error = %v, want turn-state binding conflict", err)
			}

			snap := h.stats.snapshot()
			if snap.PhysicalUsage.PromptTokens != tt.wantPrompt || snap.PhysicalUsage.CompletionTokens != tt.wantOutput || snap.PhysicalUsage.TotalTokens != tt.wantTotal {
				t.Fatalf("physical usage = %+v, want %d/%d/%d", snap.PhysicalUsage, tt.wantPrompt, tt.wantOutput, tt.wantTotal)
			}
			if snap.WastedUsage != snap.PhysicalUsage {
				t.Fatalf("wasted usage = %+v, want physical usage %+v", snap.WastedUsage, snap.PhysicalUsage)
			}
			attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
			if attempt.Outcome != routeAttemptOutcomeFailed || attempt.RetryDecision != routeRetrySuppressedState || !attempt.CleanupComplete || attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != tt.wantTotal {
				t.Fatalf("binding failure attempt = %+v", attempt)
			}
		})
	}
}

type routeAttemptContextStalledBody struct {
	ctx         context.Context
	data        []byte
	offset      int
	readStarted chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
	closeCalls  atomic.Int64
}

func newRouteAttemptContextStalledBody(ctx context.Context, data string) *routeAttemptContextStalledBody {
	return &routeAttemptContextStalledBody{
		ctx:         ctx,
		data:        []byte(data),
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *routeAttemptContextStalledBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *routeAttemptContextStalledBody) Close() error {
	b.closeCalls.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestDrainRouteAttemptBodyWithTimeoutCancelsDedicatedAttemptsWithoutGlobalBackpressure(t *testing.T) {
	const attempts = 24
	created := make(chan *routeAttemptContextStalledBody, attempts)
	client := &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := newRouteAttemptContextStalledBody(req.Context(), "")
		created <- body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
			Request:    req,
		}, nil
	})}
	h := &ProxyHandler{client: client}

	responses := make([]*http.Response, 0, attempts)
	bodies := make([]*routeAttemptContextStalledBody, 0, attempts)
	for i := 0; i < attempts; i++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://upstream.example/v1/chat/completions", nil)
		if err != nil {
			t.Fatalf("new request %d: %v", i, err)
		}
		resp, err := h.singleInferenceSend(req, nil)
		if err != nil {
			t.Fatalf("send request %d: %v", i, err)
		}
		resp.Body = newLifecycleAwareReadCloser(&routeAttemptObservedBody{inner: resp.Body}, resp.Request.Context())
		responses = append(responses, resp)
		bodies = append(bodies, <-created)
	}

	results := make(chan bool, attempts)
	started := time.Now()
	for _, resp := range responses {
		go func(resp *http.Response) {
			results <- drainRouteAttemptBodyWithTimeout(resp.Body, 10*time.Millisecond)
		}(resp)
	}
	for i := 0; i < attempts; i++ {
		select {
		case cleanup := <-results:
			if cleanup {
				t.Fatalf("stalled cleanup %d = true, want timeout", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("stalled cleanup %d did not return; elapsed=%v", i, time.Since(started))
		}
	}

	for i, body := range bodies {
		select {
		case <-body.readStarted:
		case <-time.After(time.Second):
			t.Fatalf("body %d never entered stalled Read", i)
		}
		select {
		case <-body.closed:
		case <-time.After(time.Second):
			t.Fatalf("body %d was not closed after request cancellation", i)
		}
		if calls := body.closeCalls.Load(); calls != 1 {
			t.Fatalf("body %d Close calls = %d, want exactly one", i, calls)
		}
	}
}

type routeAttemptUsageThenBlockingCleanupBody struct {
	data          []byte
	offset        int
	readBlocked   chan struct{}
	readRelease   <-chan struct{}
	readReturned  chan struct{}
	closeStarted  chan struct{}
	closeRelease  <-chan struct{}
	closeReturned chan struct{}
	readOnce      sync.Once
	readDoneOnce  sync.Once
	closeOnce     sync.Once
	closeDoneOnce sync.Once
}

func (b *routeAttemptUsageThenBlockingCleanupBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	b.readOnce.Do(func() { close(b.readBlocked) })
	<-b.readRelease
	if b.readReturned != nil {
		b.readDoneOnce.Do(func() { close(b.readReturned) })
	}
	return 0, io.EOF
}

func (b *routeAttemptUsageThenBlockingCleanupBody) Close() error {
	b.closeOnce.Do(func() { close(b.closeStarted) })
	<-b.closeRelease
	if b.closeReturned != nil {
		b.closeDoneOnce.Do(func() { close(b.closeReturned) })
	}
	return nil
}

func TestDrainRouteAttemptBodyWithTimeoutPublishesCapturedUsageWithoutCleanup(t *testing.T) {
	readRelease := make(chan struct{})
	closeRelease := make(chan struct{})
	body := &routeAttemptUsageThenBlockingCleanupBody{
		data:          []byte(`{"id":"chatcmpl-blocked-cleanup","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`),
		readBlocked:   make(chan struct{}),
		readRelease:   readRelease,
		readReturned:  make(chan struct{}),
		closeStarted:  make(chan struct{}),
		closeRelease:  closeRelease,
		closeReturned: make(chan struct{}),
	}
	var releaseReadOnce sync.Once
	var releaseCloseOnce sync.Once
	releaseRead := func() { releaseReadOnce.Do(func() { close(readRelease) }) }
	releaseClose := func() { releaseCloseOnce.Do(func() { close(closeRelease) }) }
	t.Cleanup(func() {
		releaseRead()
		releaseClose()
	})

	h := &ProxyHandler{stats: newStatsCollector()}
	record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
		OperationID:  "operation-blocked-cleanup",
		RouteID:      "route-blocked-cleanup",
		TargetID:     "target-blocked-cleanup",
		ProviderID:   "provider-blocked-cleanup",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
	})
	trace := routeAttemptTrace{
		Sequence:   1,
		TargetID:   "target-blocked-cleanup",
		ProviderID: "provider-blocked-cleanup",
		Kind:       routeAttemptNormal,
		StatusCode: http.StatusOK,
		Delivery:   requestDeliveredOrAmbiguous,
		Progress:   upstreamProgressNone,
		Commitment: downstreamCommitmentNone,
		Decision:   routeRetryAccepted,
	}
	resp := observeRouteAttemptResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}, record, nil, trace, nil, providerEndpointChatCompletions, false)

	cleanup := drainRouteAttemptBodyWithTimeout(resp.Body, 20*time.Millisecond)
	if cleanup {
		t.Fatal("blocking usage cleanup = true, want timeout")
	}
	select {
	case <-body.readBlocked:
	default:
		t.Fatal("drain did not read the available usage before timing out")
	}
	select {
	case <-body.closeStarted:
		t.Fatal("cleanup called Close concurrently with the blocked Read")
	default:
	}

	record.complete(routeAttemptCompletion{
		StatusCode:      http.StatusOK,
		Outcome:         routeAttemptOutcomeFailed,
		Delivery:        requestDeliveredOrAmbiguous,
		RetryDecision:   routeRetrySuppressedLifecycle,
		CleanupComplete: cleanup,
		Wasted:          true,
	})
	snap := h.stats.snapshot()
	attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if attempt.CleanupComplete {
		t.Fatalf("timed-out cleanup attempt = %+v, want cleanup incomplete", attempt)
	}
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 8 {
		t.Fatalf("timed-out cleanup usage = %+v, want 8", attempt.ReportedUsage)
	}
	if snap.PhysicalUsage.TotalTokens != 8 || snap.WastedUsage.TotalTokens != 8 {
		t.Fatalf("timed-out cleanup accounting = physical:%+v wasted:%+v, want 8/8", snap.PhysicalUsage, snap.WastedUsage)
	}

	releaseRead()
	select {
	case <-body.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup owner did not close the body after Read returned")
	}
	releaseClose()
	select {
	case <-body.closeReturned:
	case <-time.After(time.Second):
		t.Fatal("cleanup owner did not finish Close")
	}
}

func TestPreparedResponsesCleanupTimeoutSuppressesRetryForLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{
			name: "replay-safe rejection",
			event: "event: response.failed\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-cleanup-safe\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"}}}\n\n",
		},
		{
			name: "terminal failure with usage",
			event: "event: response.failed\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-cleanup-usage\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"},\"usage\":{\"input_tokens\":6,\"output_tokens\":2,\"total_tokens\":8}}}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readRelease := make(chan struct{})
			closeRelease := make(chan struct{})
			body := &routeAttemptUsageThenBlockingCleanupBody{
				data:          []byte(tt.event),
				readBlocked:   make(chan struct{}),
				readRelease:   readRelease,
				readReturned:  make(chan struct{}),
				closeStarted:  make(chan struct{}),
				closeRelease:  closeRelease,
				closeReturned: make(chan struct{}),
			}
			var releaseReadOnce sync.Once
			var releaseCloseOnce sync.Once
			releaseRead := func() { releaseReadOnce.Do(func() { close(readRelease) }) }
			releaseClose := func() { releaseCloseOnce.Do(func() { close(closeRelease) }) }
			t.Cleanup(func() {
				releaseRead()
				releaseClose()
			})

			provider := explicitRouteTestProvider("primary", "https://primary.example", "one")
			provider.kind = providerTypeOpenAICompatible
			route := &modelRoute{
				public: publicModelContract{id: "public-model", routeID: "public-route", endpoints: []string{providerEndpointResponses}},
				targets: []targetBinding{{
					id:            "target-primary",
					provider:      provider,
					upstreamModel: "deployment-a",
				}},
				policy: routePolicy{mode: routeModePriorityFailover, maxTargetAttempts: 2, maxUpstreamSends: 2},
			}
			operation := newRouteOperation(route, context.Background())
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}

			accepted, failure := (&ProxyHandler{}).prepareExplicitResponsesStream(context.Background(), operation, route, route.targets[0], resp)
			if accepted != nil {
				_ = accepted.Body.Close()
				t.Fatalf("prepared response = %#v, want cleanup failure", accepted)
			}
			if failure == nil {
				t.Fatal("prepared cleanup failure = nil")
			}
			if failure.cleanupDone {
				t.Fatalf("prepared cleanup = true, want timeout: %+v", failure)
			}
			if failure.decision != routeRetrySuppressedLifecycle {
				t.Fatalf("prepared cleanup retry decision = %q, want %q", failure.decision, routeRetrySuppressedLifecycle)
			}

			releaseRead()
			releaseClose()
			select {
			case <-body.readReturned:
			case <-time.After(time.Second):
				t.Fatal("prepared stream Read did not return after release")
			}
			select {
			case <-body.closeReturned:
			case <-time.After(time.Second):
				t.Fatal("prepared stream Close did not return after release")
			}
		})
	}
}

type routeAttemptUsageThenStallBody struct {
	ctx         context.Context
	data        []byte
	offset      int
	readRelease chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
	closeCalls  atomic.Int64
}

func newRouteAttemptUsageThenStallBody(data string) *routeAttemptUsageThenStallBody {
	return &routeAttemptUsageThenStallBody{
		data:        []byte(data),
		readRelease: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *routeAttemptUsageThenStallBody) releaseRead() {
	b.readOnce.Do(func() { close(b.readRelease) })
}

func (b *routeAttemptUsageThenStallBody) setContext(ctx context.Context) {
	b.ctx = ctx
}

func (b *routeAttemptUsageThenStallBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	if b.ctx == nil {
		<-b.readRelease
		return 0, io.EOF
	}
	select {
	case <-b.readRelease:
		return 0, io.EOF
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *routeAttemptUsageThenStallBody) Close() error {
	b.closeCalls.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestPreparedResponsesHeaderBindingFailureRetainsTerminalUsage(t *testing.T) {
	body := newRouteAttemptUsageThenStallBody(
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-binding-prepared\"}}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-binding-prepared\",\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n",
	)
	t.Cleanup(body.releaseRead)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body.setContext(req.Context())
		header := make(http.Header)
		header.Add("X-Codex-Turn-State", "state-one")
		header.Add("X-Codex-Turn-State", "state-two")
		header.Set("Content-Type", "text/event-stream")
		header.Set("X-Request-Id", "prepared-header-request")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body, Request: req}, nil
	})}
	h, route := explicitRouteTestHandler(t, client, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", "https://primary.example", "one"),
		explicitRouteTestProvider("secondary", "https://secondary.example", "two"),
	)
	h.stats = newStatsCollector()
	operation := newRouteOperation(route, context.Background())

	started := time.Now()
	resp, err := h.executeExplicitRouteRequest(
		withRouteOperation(context.Background(), operation),
		route,
		providerEndpointResponses,
		[]byte(`{"model":"public-model","stream":true}`),
		nil,
		"public-model",
		true,
	)
	elapsed := time.Since(started)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want binding failure with no prepared stream exposure", resp)
	}
	if err == nil || !strings.Contains(err.Error(), "conflicting X-Codex-Turn-State") {
		t.Fatalf("executeExplicitRouteRequest() error = %v, want turn-state binding conflict", err)
	}
	if strings.Contains(err.Error(), "resp-binding-prepared") {
		t.Fatalf("binding error exposed prepared upstream payload: %v", err)
	}
	if elapsed >= upstreamErrorDetailDrainTimeout+time.Second {
		t.Fatalf("prepared binding failure drain elapsed = %v, want bounded near %v", elapsed, upstreamErrorDetailDrainTimeout)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prepared response body cleanup")
	}
	if calls := body.closeCalls.Load(); calls != 1 {
		t.Fatalf("prepared response body Close calls = %d, want exactly one", calls)
	}

	deadline := time.Now().Add(time.Second)
	for {
		snap := h.stats.snapshot()
		attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
		if snap.PhysicalUsage.TotalTokens == 13 && snap.WastedUsage.TotalTokens == 13 && attempt.CleanupComplete && attempt.ReportedUsage != nil && attempt.ReportedUsage.TotalTokens == 13 {
			if attempt.Outcome != routeAttemptOutcomeFailed || attempt.RetryDecision != routeRetrySuppressedState || attempt.UpstreamRequestID != "prepared-header-request" {
				t.Fatalf("prepared binding failure attempt = %+v", attempt)
			}
			if snap.UpstreamAttempts != 1 || snap.TargetSwitches != 0 {
				t.Fatalf("prepared binding cleanup admitted failover: attempts=%d switches=%d", snap.UpstreamAttempts, snap.TargetSwitches)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("prepared binding accounting did not settle: physical:%+v wasted:%+v attempt:%+v", snap.PhysicalUsage, snap.WastedUsage, attempt)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResponseHeaderBindingFailureUsageDrainIsTimeoutBounded(t *testing.T) {
	body := newRouteAttemptUsageThenStallBody(`{"id":"chatcmpl-binding-stall","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`)
	t.Cleanup(body.releaseRead)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body.setContext(req.Context())
		header := make(http.Header)
		header.Add("X-Codex-Turn-State", "state-one")
		header.Add("X-Codex-Turn-State", "state-two")
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body, Request: req}, nil
	})}
	h, route := explicitRouteTestHandler(t, client, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", "https://primary.example", "one"),
		explicitRouteTestProvider("secondary", "https://secondary.example", "two"),
	)
	h.stats = newStatsCollector()
	operation := newRouteOperation(route, context.Background())

	started := time.Now()
	resp, err := h.executeExplicitRouteRequest(
		withRouteOperation(context.Background(), operation),
		route,
		providerEndpointChatCompletions,
		[]byte(`{"model":"public-model"}`),
		nil,
		"public-model",
		false,
	)
	elapsed := time.Since(started)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want binding failure", resp)
	}
	if err == nil || !strings.Contains(err.Error(), "conflicting X-Codex-Turn-State") {
		t.Fatalf("executeExplicitRouteRequest() error = %v, want turn-state binding conflict", err)
	}
	if elapsed >= upstreamErrorDetailDrainTimeout+time.Second {
		t.Fatalf("binding failure drain elapsed = %v, want bounded near %v", elapsed, upstreamErrorDetailDrainTimeout)
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stalled response body cleanup")
	}
	if calls := body.closeCalls.Load(); calls != 1 {
		t.Fatalf("stalled response body Close calls = %d, want exactly one", calls)
	}

	deadline := time.Now().Add(time.Second)
	for {
		snap := h.stats.snapshot()
		attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
		if snap.PhysicalUsage.TotalTokens == 8 && snap.WastedUsage.TotalTokens == 8 && attempt.CleanupComplete && attempt.ReportedUsage != nil && attempt.ReportedUsage.TotalTokens == 8 {
			if snap.UpstreamAttempts != 1 || snap.TargetSwitches != 0 {
				t.Fatalf("binding cleanup admitted failover: attempts=%d switches=%d", snap.UpstreamAttempts, snap.TargetSwitches)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("binding cleanup accounting did not settle: physical:%+v wasted:%+v attempt:%+v", snap.PhysicalUsage, snap.WastedUsage, attempt)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPhysicalAttemptLedgerWebSocketFailureDoesNotDoubleCountUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-ws-failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"},\"usage\":{\"input_tokens\":6,\"output_tokens\":2,\"total_tokens\":8}}}\n\n")
	}))
	defer upstream.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", upstream.URL, "one"),
	)
	h.stats = newStatsCollector()
	operation := newRouteOperation(route, context.Background())
	operation.mu.Lock()
	operation.id = "vekil-ws-test:failed"
	operation.mu.Unlock()
	ctx := withRouteOperation(context.Background(), operation) // deliberately no RequestSummary

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want translated failure", resp)
	}
	if got := upstreamStatusCode(err, 0); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d error=%v, want 429", got, err)
	}
	before := h.stats.snapshot()
	if before.PhysicalUsage.TotalTokens != 8 || before.WastedUsage.TotalTokens != 8 {
		t.Fatalf("attempt usage before turn record = physical:%+v wasted:%+v", before.PhysicalUsage, before.WastedUsage)
	}

	usage := responsesUsage{InputTokens: 6, OutputTokens: 2, TotalTokens: 8}
	h.RecordResponsesTurn("public-model", "primary", "azure", "Codex CLI", http.StatusTooManyRequests, usage, operation.operationID())
	after := h.stats.snapshot()
	if after.Totals.Requests != 1 || after.Totals.Errors != 1 || after.Totals.TotalTokens != 8 {
		t.Fatalf("websocket client ledger = %+v", after.Totals)
	}
	if after.PhysicalUsage.TotalTokens != 8 || after.WastedUsage.TotalTokens != 8 {
		t.Fatalf("websocket usage double-counted = physical:%+v wasted:%+v", after.PhysicalUsage, after.WastedUsage)
	}
}

func TestLateRouteAttemptReclassificationUpdatesWastedUsageAfterRingEvictionWithoutSnapshot(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	operation := &routeOperation{id: "operation-evicted-reclassification"}
	record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
		OperationID:  operation.id,
		RouteID:      "route-evicted-reclassification",
		TargetID:     "target-evicted-reclassification",
		ProviderID:   "provider-evicted-reclassification",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
		operation:    operation,
	})
	operation.appendTrace(routeAttemptTrace{
		Sequence:    1,
		TargetID:    "target-evicted-reclassification",
		ProviderID:  "provider-evicted-reclassification",
		Kind:        routeAttemptNormal,
		StatusCode:  http.StatusOK,
		Delivery:    requestDeliveredOrAmbiguous,
		Progress:    upstreamProgressTerminalSuccess,
		Commitment:  downstreamCommitmentNone,
		Decision:    routeRetryAccepted,
		UpstreamID:  "request-evicted-reclassification",
		CleanupDone: true,
	})
	usage := statsTokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	record.complete(routeAttemptCompletion{
		StatusCode:           http.StatusOK,
		Outcome:              routeAttemptOutcomeSucceeded,
		Delivery:             requestDeliveredOrAmbiguous,
		SemanticProgress:     upstreamProgressTerminalSuccess,
		DownstreamCommitment: downstreamCommitmentNone,
		RetryDecision:        routeRetryAccepted,
		UpstreamRequestID:    "request-evicted-reclassification",
		CleanupComplete:      true,
		ReportedUsage:        &usage,
	})

	// Do not call snapshot before reclassification. Overwrite the recent-attempt
	// ring so correctness cannot depend on polling or retention of the row.
	for i := 0; i < statsRecentRouteAttempts; i++ {
		filler := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
			OperationID:  "operation-filler",
			RouteID:      "route-filler",
			TargetID:     "target-filler",
			ProviderID:   "provider-filler",
			ProviderKind: "test",
			Sequence:     i + 1,
			AttemptKind:  routeAttemptNormal,
		})
		filler.complete(routeAttemptCompletion{
			StatusCode:           http.StatusOK,
			Outcome:              routeAttemptOutcomeSucceeded,
			Delivery:             requestDeliveredOrAmbiguous,
			SemanticProgress:     upstreamProgressTerminalSuccess,
			DownstreamCommitment: downstreamCommitmentNone,
			RetryDecision:        routeRetryAccepted,
			CleanupComplete:      true,
		})
	}

	if !operation.reclassifyAcceptedRouteAttempt(
		"target-evicted-reclassification",
		http.StatusBadGateway,
		requestDeliveredOrAmbiguous,
		upstreamProgressTerminalFailure,
		downstreamCommitmentNone,
		routeRetrySuppressedProgress,
		"request-evicted-reclassification",
		false,
		true,
	) {
		t.Fatal("accepted attempt was not reclassified")
	}

	h.stats.mu.Lock()
	physical := h.stats.physicalUsage
	wasted := h.stats.wastedUsage
	var targetWasted statsTokenUsage
	for _, counter := range h.stats.byTarget {
		if counter.target == "target-evicted-reclassification" {
			targetWasted = counter.wastedUsage
			break
		}
	}
	h.stats.mu.Unlock()
	if physical.TotalTokens != 10 || wasted.TotalTokens != 10 {
		t.Fatalf("no-poll aggregate usage = physical:%+v wasted:%+v, want 10/10", physical, wasted)
	}
	if targetWasted.TotalTokens != 10 {
		t.Fatalf("no-poll target wasted usage = %+v, want 10 tokens", targetWasted)
	}

	// A terminal observer can publish its previously accepted success after the
	// surface has already reclassified the attempt. It may add usage, but must not
	// weaken the failure status or double-account wasted spend.
	record.complete(routeAttemptCompletion{
		StatusCode:           http.StatusOK,
		Outcome:              routeAttemptOutcomeSucceeded,
		Delivery:             requestDeliveredOrAmbiguous,
		SemanticProgress:     upstreamProgressTerminalSuccess,
		DownstreamCommitment: downstreamCommitmentNone,
		RetryDecision:        routeRetryAccepted,
		CleanupComplete:      true,
		ReportedUsage:        &usage,
	})
	record.state.mu.Lock()
	row := record.state.row
	accounted := record.state.wastedAccounted
	record.state.mu.Unlock()
	if row.Outcome != routeAttemptOutcomeFailed || row.StatusCode != http.StatusBadGateway {
		t.Fatalf("late success weakened reclassified attempt = %+v", row)
	}
	if accounted.TotalTokens != 10 {
		t.Fatalf("late success double-accounted wasted usage = %+v", accounted)
	}
}

func TestLateRouteAttemptReclassificationAccountsWastedUsageExactlyOnce(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	ctx, _ := WithRequestSummary(context.Background())
	operation := &routeOperation{
		id: "operation-late-reclassification",
		trace: []routeAttemptTrace{{
			Sequence:    1,
			TargetID:    "target-late-reclassification",
			ProviderID:  "provider-late-reclassification",
			Kind:        routeAttemptNormal,
			StatusCode:  http.StatusOK,
			Delivery:    requestDeliveredOrAmbiguous,
			Progress:    upstreamProgressTerminalSuccess,
			Commitment:  downstreamCommitmentNone,
			Decision:    routeRetryAccepted,
			UpstreamID:  "request-late-reclassification",
			CleanupDone: true,
		}},
	}
	record := h.RecordUpstreamAttempt(ctx, RouteAttemptObservation{
		OperationID:  operation.id,
		RouteID:      "route-late-reclassification",
		TargetID:     "target-late-reclassification",
		ProviderID:   "provider-late-reclassification",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
		operation:    operation,
	})
	usage := statsTokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	record.complete(routeAttemptCompletion{
		StatusCode:           http.StatusOK,
		Outcome:              routeAttemptOutcomeSucceeded,
		Delivery:             requestDeliveredOrAmbiguous,
		SemanticProgress:     upstreamProgressTerminalSuccess,
		DownstreamCommitment: downstreamCommitmentNone,
		RetryDecision:        routeRetryAccepted,
		UpstreamRequestID:    "request-late-reclassification",
		CleanupComplete:      true,
		ReportedUsage:        &usage,
	})

	before := h.stats.snapshot()
	if before.PhysicalUsage.TotalTokens != 10 || before.WastedUsage.TotalTokens != 0 {
		t.Fatalf("usage before reclassification = physical:%+v wasted:%+v, want 10/0", before.PhysicalUsage, before.WastedUsage)
	}
	if !operation.reclassifyAcceptedRouteAttempt(
		"target-late-reclassification",
		http.StatusBadGateway,
		requestDeliveredOrAmbiguous,
		upstreamProgressTerminalFailure,
		downstreamCommitmentNone,
		routeRetrySuppressedProgress,
		"request-late-reclassification",
		false,
		true,
	) {
		t.Fatal("accepted attempt was not reclassified")
	}

	const readers = 32
	snapshots := make(chan statsSnapshot, readers)
	for range readers {
		go func() { snapshots <- h.stats.snapshot() }()
	}
	for range readers {
		snap := <-snapshots
		attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
		if attempt.Outcome != routeAttemptOutcomeFailed || attempt.StatusCode != http.StatusBadGateway {
			t.Fatalf("reclassified recent attempt = %+v", attempt)
		}
	}

	after := h.stats.snapshot()
	if after.PhysicalUsage.TotalTokens != 10 || after.WastedUsage.TotalTokens != 10 {
		t.Fatalf("usage after reclassification = physical:%+v wasted:%+v, want 10/10", after.PhysicalUsage, after.WastedUsage)
	}
	var target statsTargetBreakdown
	for _, candidate := range after.ByTarget {
		if candidate.Target == "target-late-reclassification" {
			target = candidate
			break
		}
	}
	if target.WastedUsage.TotalTokens != 10 {
		t.Fatalf("target wasted usage = %+v, want 10 tokens", target.WastedUsage)
	}
	record.state.mu.Lock()
	accounted := record.state.wastedAccounted
	record.state.mu.Unlock()
	if accounted.TotalTokens != 10 {
		t.Fatalf("state wastedAccounted = %+v, want 10 tokens", accounted)
	}
	if repeated := h.stats.snapshot().WastedUsage.TotalTokens; repeated != 10 {
		t.Fatalf("repeated snapshot double-counted wasted usage = %d, want 10", repeated)
	}
}

func TestWebSocketLegacyTurnDoesNotContaminateExplicitRouteUsage(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	operation := &routeOperation{id: "operation-stale-explicit"}
	record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
		OperationID:  operation.id,
		RouteID:      "route-stale-explicit",
		TargetID:     "target-stale-explicit",
		ProviderID:   "shared-provider",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
		operation:    operation,
	})
	staleUsage := statsTokenUsage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}
	record.complete(routeAttemptCompletion{
		StatusCode:           http.StatusOK,
		Outcome:              routeAttemptOutcomeSucceeded,
		Delivery:             requestDeliveredOrAmbiguous,
		SemanticProgress:     upstreamProgressTerminalSuccess,
		DownstreamCommitment: downstreamCommitmentNone,
		RetryDecision:        routeRetryAccepted,
		CleanupComplete:      true,
		ReportedUsage:        &staleUsage,
	})

	legacyUsage := responsesUsage{InputTokens: 7, OutputTokens: 2, TotalTokens: 9}
	h.RecordResponsesTurn("legacy-model", "shared-provider", "test", "Codex CLI", http.StatusBadGateway, legacyUsage)

	snap := h.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 || snap.Totals.TotalTokens != 9 {
		t.Fatalf("legacy client turn ledger = %+v", snap.Totals)
	}
	if snap.PhysicalUsage.TotalTokens != 4 || snap.WastedUsage.TotalTokens != 0 {
		t.Fatalf("legacy turn contaminated explicit usage: physical:%+v wasted:%+v, want 4/0", snap.PhysicalUsage, snap.WastedUsage)
	}
}

func TestWebSocketConcurrentOperationsDoNotCrossConsumePhysicalUsage(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	recordAttempt := func(operationID, targetID string, status int, outcome routeAttemptOutcome, usage statsTokenUsage) *routeOperation {
		operation := &routeOperation{id: operationID}
		record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
			OperationID:  operationID,
			RouteID:      "route-concurrent-operations",
			TargetID:     targetID,
			ProviderID:   "shared-provider",
			ProviderKind: "test",
			Sequence:     1,
			AttemptKind:  routeAttemptNormal,
			operation:    operation,
		})
		delivery := requestDeliveredOrAmbiguous
		progress := upstreamProgressTerminalSuccess
		decision := routeRetryAccepted
		if routeAttemptOutcomeIsWasted(outcome) {
			delivery = requestExplicitlyRejected
			progress = upstreamProgressTerminalFailure
			decision = routeRetrySuppressedNoTarget
		}
		record.complete(routeAttemptCompletion{
			StatusCode:           status,
			Outcome:              outcome,
			Delivery:             delivery,
			SemanticProgress:     progress,
			DownstreamCommitment: downstreamCommitmentNone,
			RetryDecision:        decision,
			CleanupComplete:      true,
			ReportedUsage:        &usage,
			Wasted:               routeAttemptOutcomeIsWasted(outcome),
		})
		return operation
	}

	operationA := recordAttempt("operation-concurrent-a", "target-a", http.StatusOK, routeAttemptOutcomeSucceeded, statsTokenUsage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4})
	operationB := recordAttempt("operation-concurrent-b", "target-b", http.StatusTooManyRequests, routeAttemptOutcomeRejected, statsTokenUsage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9})

	start := make(chan struct{})
	bRecorded := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		<-start
		h.RecordResponsesTurn("model-b", "shared-provider", "test", "Codex CLI", http.StatusTooManyRequests, responsesUsage{InputTokens: 7, OutputTokens: 2, TotalTokens: 9}, operationB.operationID())
		close(bRecorded)
		done <- struct{}{}
	}()
	go func() {
		<-start
		<-bRecorded
		h.RecordResponsesTurn("model-a", "shared-provider", "test", "Codex CLI", http.StatusOK, responsesUsage{InputTokens: 3, OutputTokens: 1, TotalTokens: 4}, operationA.operationID())
		done <- struct{}{}
	}()
	close(start)
	<-done
	<-done

	snap := h.stats.snapshot()
	if snap.Totals.Requests != 2 || snap.Totals.Errors != 1 || snap.Totals.TotalTokens != 13 {
		t.Fatalf("client turn ledger = %+v", snap.Totals)
	}
	if snap.PhysicalUsage.TotalTokens != 13 || snap.WastedUsage.TotalTokens != 9 {
		t.Fatalf("concurrent operations crossed usage ownership: physical:%+v wasted:%+v, want 13/9", snap.PhysicalUsage, snap.WastedUsage)
	}
}

func TestPhysicalAttemptLedgerResponsesCancelledIsWasted(t *testing.T) {
	t.Run("non-streaming envelope", func(t *testing.T) {
		attempt := observeNonStreamingRouteAttempt(t, providerEndpointResponses,
			`{"id":"resp-cancelled","status":"cancelled","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5},"output":[]}`)
		if attempt.Outcome != routeAttemptOutcomeCanceled || attempt.SemanticProgress != upstreamProgressTerminalFailure {
			t.Fatalf("cancelled attempt = %+v, want canceled terminal failure", attempt)
		}
		if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
			t.Fatalf("cancelled usage = %+v, want 5", attempt.ReportedUsage)
		}
	})

	t.Run("streaming event", func(t *testing.T) {
		h := &ProxyHandler{stats: newStatsCollector()}
		record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
			OperationID:  "operation-cancelled-stream",
			RouteID:      "route-cancelled-stream",
			TargetID:     "target-cancelled-stream",
			ProviderID:   "provider-cancelled-stream",
			ProviderKind: "test",
			Sequence:     1,
			AttemptKind:  routeAttemptNormal,
		})
		trace := routeAttemptTrace{
			Sequence:   1,
			TargetID:   "target-cancelled-stream",
			ProviderID: "provider-cancelled-stream",
			Kind:       routeAttemptNormal,
			StatusCode: http.StatusOK,
			Delivery:   requestDeliveredOrAmbiguous,
			Progress:   upstreamProgressNone,
			Commitment: downstreamCommitmentNone,
			Decision:   routeRetryAccepted,
		}
		body := "event: response.cancelled\n" +
			`data: {"type":"response.cancelled","response":{"id":"resp-cancelled","status":"cancelled","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}` + "\n\n"
		resp := observeRouteAttemptResponse(&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, record, nil, trace, nil, providerEndpointResponses, true)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("drain cancelled stream: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close cancelled stream: %v", err)
		}

		snap := h.stats.snapshot()
		attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
		if attempt.Outcome != routeAttemptOutcomeCanceled || attempt.SemanticProgress != upstreamProgressTerminalFailure || !attempt.CleanupComplete {
			t.Fatalf("cancelled stream attempt = %+v", attempt)
		}
		if snap.PhysicalUsage.TotalTokens != 5 || snap.WastedUsage.TotalTokens != 5 {
			t.Fatalf("cancelled stream usage = physical:%+v wasted:%+v, want 5/5", snap.PhysicalUsage, snap.WastedUsage)
		}
	})
}

func TestNonStreamingAttemptEarlyCloseIsIncompleteAndWasted(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
		OperationID:  "operation-early-close",
		RouteID:      "route-early-close",
		TargetID:     "target-early-close",
		ProviderID:   "provider-early-close",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
	})
	trace := routeAttemptTrace{
		Sequence:   1,
		TargetID:   "target-early-close",
		ProviderID: "provider-early-close",
		Kind:       routeAttemptNormal,
		StatusCode: http.StatusOK,
		Delivery:   requestDeliveredOrAmbiguous,
		Progress:   upstreamProgressNone,
		Commitment: downstreamCommitmentNone,
		Decision:   routeRetryAccepted,
	}
	partial := `{"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5},"choices":[`
	resp := observeRouteAttemptResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(partial)),
	}, record, nil, trace, nil, providerEndpointChatCompletions, false)

	buf := make([]byte, len(partial))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read partial response without EOF: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close partial response: %v", err)
	}

	snap := h.stats.snapshot()
	attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if attempt.Outcome != routeAttemptOutcomeIncomplete || attempt.SemanticProgress != upstreamProgressUnknown || !attempt.CleanupComplete {
		t.Fatalf("early-close attempt = %+v, want incomplete/unknown/clean", attempt)
	}
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
		t.Fatalf("early-close reported usage = %+v, want 5", attempt.ReportedUsage)
	}
	if snap.PhysicalUsage.TotalTokens != 5 || snap.WastedUsage.TotalTokens != 5 {
		t.Fatalf("early-close usage = physical:%+v wasted:%+v, want 5/5", snap.PhysicalUsage, snap.WastedUsage)
	}
}

func TestNonStreamingAttemptEarlyCloseUsesCanceledRequestContexts(t *testing.T) {
	tests := []struct {
		name          string
		cancelInbound bool
		cancelAttempt bool
	}{
		{name: "inbound context", cancelInbound: true},
		{name: "attempt context", cancelAttempt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inboundCtx, cancelInbound := context.WithCancel(context.Background())
			defer cancelInbound()
			attemptCtx, cancelAttempt := context.WithCancel(context.Background())
			defer cancelAttempt()
			if tt.cancelInbound {
				cancelInbound()
			}
			if tt.cancelAttempt {
				cancelAttempt()
			}

			h := &ProxyHandler{stats: newStatsCollector()}
			record := h.RecordUpstreamAttempt(context.Background(), RouteAttemptObservation{
				OperationID:  "operation-canceled-early-close",
				RouteID:      "route-canceled-early-close",
				TargetID:     "target-canceled-early-close",
				ProviderID:   "provider-canceled-early-close",
				ProviderKind: "test",
				Sequence:     1,
				AttemptKind:  routeAttemptNormal,
			})
			trace := routeAttemptTrace{
				Sequence:   1,
				TargetID:   "target-canceled-early-close",
				ProviderID: "provider-canceled-early-close",
				Kind:       routeAttemptNormal,
				StatusCode: http.StatusOK,
				Delivery:   requestDeliveredOrAmbiguous,
				Progress:   upstreamProgressNone,
				Commitment: downstreamCommitmentNone,
				Decision:   routeRetryAccepted,
			}
			operation := &routeOperation{inbound: inboundCtx}
			partial := `{"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5},"choices":[`
			request := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/chat/completions", nil).WithContext(attemptCtx)
			resp := observeRouteAttemptResponse(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(partial)),
				Request:    request,
			}, record, operation, trace, nil, providerEndpointChatCompletions, false)

			buf := make([]byte, len(partial))
			if _, err := io.ReadFull(resp.Body, buf); err != nil {
				t.Fatalf("read canceled partial response without EOF: %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close canceled partial response: %v", err)
			}

			attempt := routeAttemptBySequence(t, h.stats.snapshot().RecentAttempts, 1)
			if attempt.Outcome != routeAttemptOutcomeCanceled || attempt.SemanticProgress != upstreamProgressUnknown || !attempt.CleanupComplete {
				t.Fatalf("canceled early-close attempt = %+v, want canceled/unknown/clean", attempt)
			}
			if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
				t.Fatalf("canceled early-close usage = %+v, want 5", attempt.ReportedUsage)
			}
		})
	}
}

func TestPreparedResponsesTerminalUsageSurvivesEarlyDownstreamClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-prepared-usage","status":"completed","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}`+"\n\n")
	}))
	defer upstream.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", upstream.URL, "one"),
	)
	h.stats = newStatsCollector()
	operation := newRouteOperation(route, context.Background())
	resp, err := h.executeExplicitRouteRequest(
		withRouteOperation(context.Background(), operation),
		route,
		providerEndpointResponses,
		[]byte(`{"model":"public-model","stream":true}`),
		nil,
		"public-model",
		true,
	)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close prepared response without replay: %v", err)
	}

	snap := h.stats.snapshot()
	attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if attempt.ReportedUsage == nil || attempt.ReportedUsage.TotalTokens != 5 {
		t.Fatalf("prepared terminal usage = %+v, want 5", attempt.ReportedUsage)
	}
	if attempt.Outcome != routeAttemptOutcomeIncomplete && attempt.Outcome != routeAttemptOutcomeCanceled {
		t.Fatalf("prepared early-close outcome = %q, want incomplete/canceled", attempt.Outcome)
	}
	if snap.PhysicalUsage.TotalTokens != 5 || snap.WastedUsage.TotalTokens != 5 {
		t.Fatalf("prepared early-close usage = physical:%+v wasted:%+v, want 5/5", snap.PhysicalUsage, snap.WastedUsage)
	}
}

type routeAttemptSignaledReadCloser struct {
	inner   io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *routeAttemptSignaledReadCloser) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.inner.Read(p)
}

func (b *routeAttemptSignaledReadCloser) Close() error { return b.inner.Close() }

func TestPrepareExplicitResponsesStreamCancellationIsNotProviderFailure(t *testing.T) {
	tests := []struct {
		name         string
		cancelSource responsesPreparedAwaitSource
		wantDecision routeRetryDecision
	}{
		{name: "inbound client", cancelSource: responsesPreparedAwaitInbound, wantDecision: routeRetrySuppressedAdmission},
		{name: "upstream lifecycle", cancelSource: responsesPreparedAwaitUpstream, wantDecision: routeRetrySuppressedLifecycle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
				explicitRouteTestProvider("primary", "https://primary.example", "one"),
			)
			inboundCtx := context.Background()
			upstreamCtx := context.Background()
			var cancel context.CancelFunc
			if tt.cancelSource == responsesPreparedAwaitInbound {
				inboundCtx, cancel = context.WithCancel(context.Background())
			} else {
				upstreamCtx, cancel = context.WithCancel(context.Background())
			}
			operation := newRouteOperation(route, inboundCtx)
			reader, writer := io.Pipe()
			defer func() { _ = writer.Close() }()
			started := make(chan struct{})
			body := &routeAttemptSignaledReadCloser{inner: reader, started: started}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
				Request:    httptest.NewRequest(http.MethodPost, "https://primary.example/responses", nil),
			}

			result := make(chan *routeAttemptFailure, 1)
			go func() {
				_, failure := h.prepareExplicitResponsesStream(upstreamCtx, operation, route, route.targets[0], resp)
				result <- failure
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("prepared stream did not start reading")
			}
			cancel()
			select {
			case failure := <-result:
				if failure == nil {
					t.Fatal("canceled precommit wait returned no failure")
				}
				if failure.outcome != routeAttemptOutcomeCanceled || failure.decision != tt.wantDecision {
					t.Fatalf("canceled precommit failure = %+v, want canceled/%s", failure, tt.wantDecision)
				}
				if !errors.Is(failure.err, context.Canceled) {
					t.Fatalf("canceled precommit error = %v, want context.Canceled", failure.err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled precommit wait did not return")
			}
		})
	}
}

func TestRouteAttemptObserverRejectsInvalidNonStreamingEnvelopes(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{name: "Responses null", endpoint: providerEndpointResponses, body: `null`},
		{name: "Messages null", endpoint: providerEndpointMessages, body: `null`},
		{name: "Messages unknown type", endpoint: providerEndpointMessages, body: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := observeNonStreamingRouteAttempt(t, tt.endpoint, tt.body)
			if attempt.Outcome == routeAttemptOutcomeSucceeded {
				t.Fatalf("invalid %s envelope recorded as success: %+v", tt.endpoint, attempt)
			}
			if attempt.SemanticProgress != upstreamProgressUnknown {
				t.Fatalf("invalid %s progress = %q, want unknown", tt.endpoint, attempt.SemanticProgress)
			}
		})
	}
}

func TestExplicitRouteDispatchMarksHTTPUpstreamAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
	}))
	defer upstream.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", upstream.URL, "key"),
	)
	h.stats = newStatsCollector()
	h.metrics = NewMetricsCollector()

	inbound, summary := WithRequestSummary(context.Background())
	summary.setRoute("responses", "public-model", false)
	summary.setProviderModel("primary", "azure-openai", true, "public-model")
	operation := newRouteOperation(route, inbound)
	ctx := withRouteOperation(context.Background(), operation)

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	_ = resp.Body.Close()
	h.RecordRequest(summary, resp.StatusCode, "codex/1", time.Millisecond)

	if !readSummaryForStats(summary).upstreamAttempted {
		t.Fatal("explicit HTTP route did not mark the request summary as upstream-attempted")
	}
	families, err := h.metrics.registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	if got := getCounterValue(families, "vekil_upstream_errors_total", map[string]string{
		"provider":     "primary",
		"public_model": "public-model",
		"code":         "503",
	}); got != 1 {
		t.Fatalf("explicit HTTP upstream error metric = %v, want 1", got)
	}
}

func TestExplicitRouteDispatchMarksWebSocketUpstreamAttemptObserver(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
	}))
	defer upstream.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", upstream.URL, "key"),
	)
	upstreamCtx, observer := withUpstreamAttemptObserver(context.Background())
	operation := newRouteOperation(route, context.Background())
	ctx := withRouteOperation(upstreamCtx, operation)

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	_ = resp.Body.Close()
	if !observer.Attempted() {
		t.Fatal("explicit websocket route did not mark the detached upstream observer")
	}
}
