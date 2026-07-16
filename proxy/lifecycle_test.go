package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestNewProxyHandlerInitializesLifecycle(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	if handler.lifecycleCtx == nil || handler.lifecycleCancel == nil {
		t.Fatal("NewProxyHandler() did not initialize the lifecycle context")
	}
	if err := handler.lifecycleCtx.Err(); err != nil {
		t.Fatalf("new lifecycle context error = %v, want active context", err)
	}
}

func TestWaitLifecycleWorkersCompletedWinsExpiredContext(t *testing.T) {
	handler := &ProxyHandler{}
	if !handler.beginLifecycleWorker() {
		t.Fatal("beginLifecycleWorker() = false, want worker registration")
	}
	handler.endLifecycleWorker()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for i := 0; i < 100; i++ {
		if err := handler.WaitLifecycleWorkers(ctx); err != nil {
			t.Fatalf("WaitLifecycleWorkers() iteration %d error = %v, want completed workers to win", i, err)
		}
	}
}

func TestInferenceLifecycleDetachesClientCancellationUntilShutdown(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	})

	inboundCtx, cancelInbound := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`),
	).WithContext(inboundCtx)
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handler.HandleOpenAIChatCompletions(w, req)
	}()

	waitForLifecycleSignal(t, upstreamStarted, "ordinary upstream request to start")
	cancelInbound()

	select {
	case <-upstreamCanceled:
		t.Fatal("inbound client cancellation unexpectedly canceled detached upstream work")
	case <-handlerDone:
		t.Fatal("handler returned after inbound cancellation while detached upstream work was still active")
	case <-time.After(100 * time.Millisecond):
	}

	handler.BeginShutdown()
	waitForLifecycleSignal(t, upstreamCanceled, "shutdown cancellation to reach detached upstream work")
	waitForLifecycleSignal(t, handlerDone, "handler to return after lifecycle cancellation")
	if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("shutdown status = %d, want 503", got)
	}
	if !summary.StatsSuppressed() {
		t.Fatal("shutdown-canceled request was not suppressed from provider stats")
	}
}

func TestBeginShutdownCancelsChatAndResponsesUpstreamWork(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         string
		streaming    bool
		streamPrefix string
		handle       func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name: "chat nonstream pre-header",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleOpenAIChatCompletions(w, r)
			},
		},
		{
			name:         "chat stream",
			path:         "/v1/chat/completions",
			body:         `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			streaming:    true,
			streamPrefix: "data: {\"id\":\"chatcmpl-life\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n",
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleOpenAIChatCompletions(w, r)
			},
		},
		{
			name: "responses nonstream pre-header",
			path: "/v1/responses",
			body: `{"model":"gpt-5.4","input":"hello"}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleResponses(w, r)
			},
		},
		{
			name:         "responses stream",
			path:         "/v1/responses",
			body:         `{"model":"gpt-5.4","input":"hello","stream":true}`,
			streaming:    true,
			streamPrefix: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-life\",\"status\":\"in_progress\"}}\n\n",
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleResponses(w, r)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamStarted := make(chan struct{})
			upstreamCanceled := make(chan struct{})
			handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tt.streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tt.streamPrefix)
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}
				close(upstreamStarted)
				<-r.Context().Done()
				close(upstreamCanceled)
			})

			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := newLifecycleStreamResponseWriter()
			handlerDone := make(chan struct{})
			go func() {
				defer close(handlerDone)
				tt.handle(handler, w, req)
			}()

			waitForLifecycleSignal(t, upstreamStarted, "upstream work to start")
			if tt.streaming {
				waitForLifecycleSignal(t, w.flushed, "stream response to flush")
			}
			handler.BeginShutdown()
			waitForLifecycleSignal(t, upstreamCanceled, "upstream work to observe shutdown")
			waitForLifecycleSignal(t, handlerDone, "handler to return after shutdown")
			if strings.Contains(w.BodyString(), "upstream request failed") {
				t.Fatalf("shutdown emitted an upstream failure response: %s", w.BodyString())
			}
			wantStatus := http.StatusServiceUnavailable
			if tt.streaming {
				wantStatus = http.StatusOK
			}
			if got := w.StatusCode(); got != wantStatus {
				t.Fatalf("shutdown status = %d, want %d", got, wantStatus)
			}
			if !summary.StatsSuppressed() {
				t.Fatal("shutdown-canceled request was not suppressed from provider stats")
			}
		})
	}
}

func TestBeginShutdownCancelsCompactFanoutAndMemoryShim(t *testing.T) {
	t.Run("compact sibling fanout", func(t *testing.T) {
		var calls atomic.Int32
		siblingStarted := make(chan struct{}, 4)
		siblingCanceled := make(chan struct{}, 4)
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if calls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp-first","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first chunk summary"}]}]}`)
				return
			}
			siblingStarted <- struct{}{}
			<-r.Context().Done()
			siblingCanceled <- struct{}{}
		})
		WithCompactUpstreamChunkBytes(compactUpstreamChunkBodyFloor)(handler)
		WithCompactUpstreamChunkConcurrency(3)(handler)

		input := make([]map[string]interface{}, 0, 5)
		for i := range 5 {
			input = append(input, map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{{
					"type": "input_text",
					"text": strings.Repeat(string(rune('a'+i)), 40<<10),
				}},
			})
		}
		body, err := json.Marshal(map[string]interface{}{
			"model": "gpt-5.4",
			"input": input,
		})
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handlerDone := make(chan struct{})
		go func() {
			defer close(handlerDone)
			handler.HandleCompact(w, req)
		}()

		for range 3 {
			waitForLifecycleSignal(t, siblingStarted, "parallel compact sibling to start")
		}
		handler.BeginShutdown()
		for range 3 {
			waitForLifecycleSignal(t, siblingCanceled, "parallel compact sibling to observe shutdown")
		}
		waitForLifecycleSignal(t, handlerDone, "compact handler to return after fanout cancellation")
		if strings.Contains(w.Body.String(), "upstream request failed") {
			t.Fatalf("shutdown emitted a compact failure response: %s", w.Body.String())
		}
		if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
			t.Fatalf("compact shutdown status = %d, want 503", got)
		}
		if !summary.StatsSuppressed() {
			t.Fatal("compact shutdown request was not stats-suppressed")
		}
	})

	t.Run("memory summarize shim", func(t *testing.T) {
		upstreamStarted := make(chan struct{})
		upstreamCanceled := make(chan struct{})
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(upstreamStarted)
			<-r.Context().Done()
			close(upstreamCanceled)
		})

		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/memories/trace_summarize",
			strings.NewReader(`{"model":"gpt-5.4","traces":[{"id":"trace-1","items":[]}]}`),
		)
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handlerDone := make(chan struct{})
		go func() {
			defer close(handlerDone)
			handler.HandleMemorySummarize(w, req)
		}()

		waitForLifecycleSignal(t, upstreamStarted, "memory shim upstream work to start")
		handler.BeginShutdown()
		waitForLifecycleSignal(t, upstreamCanceled, "memory shim upstream work to observe shutdown")
		waitForLifecycleSignal(t, handlerDone, "memory shim handler to return after shutdown")
		if strings.Contains(w.Body.String(), "upstream request failed") {
			t.Fatalf("shutdown emitted a memory failure response: %s", w.Body.String())
		}
		if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
			t.Fatalf("memory shutdown status = %d, want 503", got)
		}
		if !summary.StatsSuppressed() {
			t.Fatal("memory shutdown request was not stats-suppressed")
		}
	})
}

func TestBeginShutdownCancelsCanonicalAndVariantModelRefresh(t *testing.T) {
	for _, tt := range []struct {
		name          string
		requestTarget string
		seedCanonical bool
	}{
		{name: "canonical", requestTarget: "/v1/models"},
		{name: "variant", requestTarget: "/v1/models?client=codex", seedCanonical: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstreamStarted := make(chan struct{})
			upstreamCanceled := make(chan struct{})
			handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				close(upstreamStarted)
				<-r.Context().Done()
				close(upstreamCanceled)
			})
			if tt.seedCanonical {
				handler.models.entries = map[string]cachedModelsResponse{
					"": {
						body:       []byte(`{"object":"list","data":[],"models":[]}`),
						statusCode: http.StatusOK,
						expiry:     time.Now().Add(time.Minute),
					},
				}
			}

			req := httptest.NewRequest(http.MethodGet, tt.requestTarget, nil)
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			handlerDone := make(chan struct{})
			go func() {
				defer close(handlerDone)
				handler.HandleModels(w, req)
			}()

			waitForLifecycleSignal(t, upstreamStarted, "model refresh to start")
			handler.BeginShutdown()
			waitForLifecycleSignal(t, upstreamCanceled, "model refresh to observe shutdown")
			waitForLifecycleSignal(t, handlerDone, "models handler to return after shutdown")
			if strings.Contains(w.Body.String(), "upstream request failed") {
				t.Fatalf("shutdown emitted a models failure response: %s", w.Body.String())
			}
			if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
				t.Fatalf("models shutdown status = %d, want 503", got)
			}
			if !summary.StatsSuppressed() {
				t.Fatal("models shutdown request was not stats-suppressed")
			}
		})
	}
}

func TestBeginShutdownCancelsDashboardInsightInternalCall(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	handler.stats = newStatsCollector()
	handler.providersConfig.InsightModel = "gpt-5"

	req := httptest.NewRequest(http.MethodPost, "/dashboard/insight", strings.NewReader(`{"shown":"existing summary"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handler.HandleDashboardInsight(w, req)
	}()

	waitForLifecycleSignal(t, upstreamStarted, "dashboard insight upstream work to start")
	handler.BeginShutdown()
	waitForLifecycleSignal(t, upstreamCanceled, "dashboard insight upstream work to observe shutdown")
	waitForLifecycleSignal(t, handlerDone, "dashboard insight handler to return after shutdown")
}

func TestBeginShutdownIsConcurrentIdempotentAndCancelsFutureContexts(t *testing.T) {
	handler := &ProxyHandler{streamingUpstreamTimeout: time.Hour}
	start := make(chan struct{})
	type contextWithCancel struct {
		ctx    context.Context
		cancel context.CancelFunc
	}
	contexts := make(chan contextWithCancel, 32)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				handler.BeginShutdown()
				return
			}
			ctx, cancel := handler.newInferenceUpstreamContext(i%3 == 0)
			contexts <- contextWithCancel{ctx: ctx, cancel: cancel}
		}(i)
	}
	close(start)
	wg.Wait()
	close(contexts)

	handler.BeginShutdown()
	for item := range contexts {
		select {
		case <-item.ctx.Done():
			if item.ctx.Err() != context.Canceled {
				t.Fatalf("context error = %v, want context.Canceled", item.ctx.Err())
			}
		case <-time.After(time.Second):
			t.Fatal("concurrently installed upstream context was not canceled")
		}
		item.cancel()
	}

	for _, streaming := range []bool{false, true} {
		ctx, cancel := handler.newInferenceUpstreamContext(streaming)
		select {
		case <-ctx.Done():
			if ctx.Err() != context.Canceled {
				t.Fatalf("future streaming=%v context error = %v, want context.Canceled", streaming, ctx.Err())
			}
		default:
			t.Fatalf("future streaming=%v context was not canceled immediately", streaming)
		}
		cancel()
	}
	catalogCtx, cancelCatalog := handler.newLifecycleUpstreamContext(modelsUpstreamTimeout)
	defer cancelCatalog()
	select {
	case <-catalogCtx.Done():
		if catalogCtx.Err() != context.Canceled {
			t.Fatalf("future catalog context error = %v, want context.Canceled", catalogCtx.Err())
		}
	default:
		t.Fatal("future catalog context was not canceled immediately")
	}
}

func TestDrainingPublicationCancelsNewContextsBeforeLifecycleRoot(t *testing.T) {
	var clientCalls atomic.Int64
	handler := &ProxyHandler{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clientCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    req,
			}, nil
		})},
		maxRetries:               1,
		streamingUpstreamTimeout: time.Hour,
	}
	handler.initializeLifecycle()
	t.Cleanup(handler.BeginShutdown)
	if err := handler.lifecycleCtx.Err(); err != nil {
		t.Fatalf("lifecycle root error before draining publication = %v", err)
	}

	// Publish admission closure without canceling the lifecycle root. This is
	// the narrow ordering window that new child contexts must close themselves.
	handler.draining.Store(true)
	if err := handler.lifecycleCtx.Err(); err != nil {
		t.Fatalf("lifecycle root error after draining publication = %v, want live root", err)
	}

	manager := NewToolOptimizerManager(ToolOptimizersConfig{}, nil)
	type contextCase struct {
		name   string
		ctx    context.Context
		cancel context.CancelFunc
	}
	contexts := make([]contextCase, 0, 5)
	for _, streaming := range []bool{false, true} {
		ctx, cancel := handler.newInferenceUpstreamContext(streaming)
		contexts = append(contexts, contextCase{
			name:   fmt.Sprintf("inference streaming=%v", streaming),
			ctx:    ctx,
			cancel: cancel,
		})
	}
	catalogCtx, cancelCatalog := handler.newLifecycleUpstreamContext(modelsUpstreamTimeout)
	contexts = append(contexts, contextCase{name: "catalog", ctx: catalogCtx, cancel: cancelCatalog})
	for _, stage := range []string{toolOptimizerStageCommandRewrite, toolOptimizerStageOutputReduce} {
		ctx, cancel := handler.withToolOptimizerStageContext(context.Background(), manager, stage)
		contexts = append(contexts, contextCase{name: "optimizer " + stage, ctx: ctx, cancel: cancel})
	}

	for _, item := range contexts {
		select {
		case <-item.ctx.Done():
			if !errors.Is(item.ctx.Err(), context.Canceled) {
				t.Errorf("%s context error = %v, want context.Canceled", item.name, item.ctx.Err())
			}
		default:
			t.Errorf("%s context was not canceled before return", item.name)
		}

		resp, err := handler.doWithRetry(func() (*http.Request, error) {
			return http.NewRequestWithContext(item.ctx, http.MethodGet, "http://upstream.test/draining", nil)
		})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s doWithRetry() error = %v, want context.Canceled", item.name, err)
		}
		item.cancel()
	}
	if got := clientCalls.Load(); got != 0 {
		t.Fatalf("Client.Do calls = %d, want 0 after draining publication", got)
	}
}

func TestLifecycleUpstreamContextCancellationCauses(t *testing.T) {
	t.Run("draining publication uses lifecycle shutdown cause", func(t *testing.T) {
		handler := &ProxyHandler{}
		handler.initializeLifecycle()
		t.Cleanup(handler.BeginShutdown)
		handler.draining.Store(true)
		if err := handler.lifecycleCtx.Err(); err != nil {
			t.Fatalf("lifecycle root error = %v, want live root", err)
		}

		ctx, cancel := handler.newLifecycleUpstreamContext(time.Hour)
		defer cancel()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("child error = %v, want context.Canceled", ctx.Err())
		}
		if !errors.Is(context.Cause(ctx), errProxyLifecycleShutdown) {
			t.Fatalf("child cause = %v, want errProxyLifecycleShutdown", context.Cause(ctx))
		}
	})

	t.Run("returned cancel remains ordinary cancellation", func(t *testing.T) {
		handler := &ProxyHandler{}
		ctx, cancel := handler.newLifecycleUpstreamContext(time.Hour)
		cancel()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("child error = %v, want context.Canceled", ctx.Err())
		}
		if !errors.Is(context.Cause(ctx), context.Canceled) || errors.Is(context.Cause(ctx), errProxyLifecycleShutdown) {
			t.Fatalf("child cause = %v, want ordinary context.Canceled", context.Cause(ctx))
		}
	})

	t.Run("timeout remains deadline exceeded", func(t *testing.T) {
		handler := &ProxyHandler{}
		ctx, cancel := handler.newLifecycleUpstreamContext(time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatalf("timeout error/cause = %v/%v, want DeadlineExceeded", ctx.Err(), context.Cause(ctx))
		}
	})
}

func TestCompactionNonOKBodyCancellationPreservesStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		status := status
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Run("direct compact singleflight", func(t *testing.T) {
				started := make(chan struct{})
				canceled := make(chan struct{})
				var startedOnce sync.Once
				var canceledOnce sync.Once
				var upstreamCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/problem+json")
					w.Header().Set("X-Known-Status", strconv.Itoa(status))
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"partial-detail`)
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					startedOnce.Do(func() { close(started) })
					<-r.Context().Done()
					canceledOnce.Do(func() { close(canceled) })
				}))
				defer upstream.Close()

				handler := newLifecycleStreamDefaultHandler(t, upstream.URL)
				handler.stats = newStatsCollector()
				bodyReadStarted := make(chan struct{})
				baseTransport := handler.client.Transport
				handler.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					resp, err := baseTransport.RoundTrip(req)
					if err == nil && resp != nil && resp.Body != nil {
						resp.Body = &readSignalBody{ReadCloser: resp.Body, started: bodyReadStarted}
					}
					return resp, err
				})
				const callers = 2
				type result struct {
					status      int
					contentType string
					body        []byte
					summary     *RequestSummary
				}
				results := make(chan result, callers)
				start := make(chan struct{})
				requestBody := []byte(`{"model":"gpt-5.4","input":"shared compact cancellation"}`)
				for range callers {
					go func() {
						<-start
						req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(requestBody))
						req.Header.Set("Content-Type", "application/json")
						ctx, summary := WithRequestSummary(req.Context())
						req = req.WithContext(ctx)
						w := httptest.NewRecorder()
						handler.HandleCompact(w, req)
						resp := w.Result()
						body, _ := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						handler.RecordRequest(summary, resp.StatusCode, "compact-race-test", time.Millisecond)
						results <- result{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: body, summary: summary}
					}()
				}
				close(start)
				waitForLifecycleSignal(t, started, "non-OK compact response headers")
				waitForLifecycleSignal(t, bodyReadStarted, "non-OK compact response body read")
				waitForCompactInflightWaiters(t, handler, callers-1)
				handler.BeginShutdown()
				waitForLifecycleSignal(t, canceled, "non-OK compact body cancellation")

				for range callers {
					got := <-results
					if got.status != status {
						t.Fatalf("status = %d, want preserved %d; body=%s", got.status, status, got.body)
					}
					if got.contentType != "application/problem+json" {
						t.Fatalf("Content-Type = %q, want upstream content type", got.contentType)
					}
					if len(got.body) > compactUpstreamErrorBodySize {
						t.Fatalf("body length = %d, want <= %d", len(got.body), compactUpstreamErrorBodySize)
					}
					if got.summary.StatsSuppressed() {
						t.Fatal("known compact status was stats-suppressed")
					}
				}
				if got := upstreamCalls.Load(); got != 1 {
					t.Fatalf("upstream calls = %d, want one singleflight leader", got)
				}
				snap := handler.stats.snapshot()
				if snap.Totals.Requests != callers || snap.Totals.Errors != callers {
					t.Fatalf("stats = requests:%d errors:%d, want %d/%d", snap.Totals.Requests, snap.Totals.Errors, callers, callers)
				}
			})

			t.Run("responses compaction trigger", func(t *testing.T) {
				started := make(chan struct{})
				canceled := make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/problem+json")
					w.Header().Set("X-Known-Status", strconv.Itoa(status))
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"partial-detail`)
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					close(started)
					<-r.Context().Done()
					close(canceled)
				}))
				defer upstream.Close()

				handler := newLifecycleStreamDefaultHandler(t, upstream.URL)
				handler.stats = newStatsCollector()
				bodyReadStarted := make(chan struct{})
				baseTransport := handler.client.Transport
				handler.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					resp, err := baseTransport.RoundTrip(req)
					if err == nil && resp != nil && resp.Body != nil {
						resp.Body = &readSignalBody{ReadCloser: resp.Body, started: bodyReadStarted}
					}
					return resp, err
				})
				requestBody := `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"history"}]},{"type":"compaction_trigger"}],"stream":false}`
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
				req.Header.Set("Content-Type", "application/json")
				ctx, summary := WithRequestSummary(req.Context())
				req = req.WithContext(ctx)
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					handler.HandleResponses(w, req)
				}()
				waitForLifecycleSignal(t, started, "responses compaction response headers")
				waitForLifecycleSignal(t, bodyReadStarted, "responses compaction response body read")
				handler.BeginShutdown()
				waitForLifecycleSignal(t, canceled, "responses compaction body cancellation")
				waitForLifecycleSignal(t, done, "responses compaction handler")

				resp := w.Result()
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != status {
					t.Fatalf("status = %d, want preserved %d; body=%s", resp.StatusCode, status, body)
				}
				if resp.Header.Get("X-Known-Status") != strconv.Itoa(status) {
					t.Fatalf("X-Known-Status = %q, want %d", resp.Header.Get("X-Known-Status"), status)
				}
				if len(body) > compactUpstreamErrorBodySize {
					t.Fatalf("body length = %d, want <= %d", len(body), compactUpstreamErrorBodySize)
				}
				if summary.StatsSuppressed() {
					t.Fatal("known responses fallback status was stats-suppressed")
				}
				handler.RecordRequest(summary, resp.StatusCode, "responses-race-test", time.Millisecond)
				snap := handler.stats.snapshot()
				if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
					t.Fatalf("stats = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
				}
			})
		})
	}
}

func TestWaitCompactInflightLifecyclePublicationGrace(t *testing.T) {
	newCanceledContext := func() context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errProxyLifecycleShutdown)
		return ctx
	}

	t.Run("stuck leader returns promptly", func(t *testing.T) {
		call := &compactInflightCall{done: make(chan struct{})}
		start := time.Now()
		_, resp, err := waitCompactInflight(newCanceledContext(), call)
		elapsed := time.Since(start)
		if resp != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("wait result = resp:%v err:%v, want context.Canceled", resp, err)
		}
		if elapsed > compactInflightPublicationGrace+250*time.Millisecond {
			t.Fatalf("stuck wait elapsed = %v, want bounded near %v", elapsed, compactInflightPublicationGrace)
		}
	})

	t.Run("authoritative response published within grace wins", func(t *testing.T) {
		call := &compactInflightCall{done: make(chan struct{})}
		go func() {
			time.Sleep(compactInflightPublicationGrace / 4)
			body := []byte(`{"error":{"message":"known 413"}}`)
			call.result = compactInflightResult{
				resp: &http.Response{
					StatusCode: http.StatusRequestEntityTooLarge,
					Header:     http.Header{"X-Known-Status": []string{"413"}},
				},
				respBody: body,
			}
			close(call.done)
		}()

		_, resp, err := waitCompactInflight(newCanceledContext(), call)
		if err != nil {
			t.Fatalf("waitCompactInflight() error = %v", err)
		}
		if resp == nil || resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("response = %#v, want authoritative 413", resp)
		}
		if resp.Header.Get("X-Known-Status") != "413" {
			t.Fatalf("response headers = %v", resp.Header)
		}
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil || !strings.Contains(string(body), "known 413") {
			t.Fatalf("response body = %q err=%v", body, readErr)
		}
	})
}

func TestResponses413BodyCancellationPreservesTrackedStatus(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Content-Length", "999")
		w.Header().Set("X-Known-Status", "413")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":{"message":"partial replay too large`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()

	handler := newLifecycleStreamDefaultHandler(t, upstream.URL)
	handler.stats = newStatsCollector()
	handler.responsesWS = ResponsesWebSocketConfig{AutoCompactKeepTail: 2}
	bodyReadStarted := make(chan struct{})
	baseTransport := handler.client.Transport
	handler.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := baseTransport.RoundTrip(req)
		if err == nil && resp != nil && resp.Body != nil {
			resp.Body = &readSignalBody{ReadCloser: resp.Body, started: bodyReadStarted}
		}
		return resp, err
	})

	requestBody := `{"model":"gpt-5.4","previous_response_id":"resp-prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"latest"}]}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleResponses(w, req)
	}()
	waitForLifecycleSignal(t, started, "HTTP responses 413 headers")
	waitForLifecycleSignal(t, bodyReadStarted, "HTTP responses 413 body read")
	handler.BeginShutdown()
	waitForLifecycleSignal(t, canceled, "HTTP responses 413 body cancellation")
	waitForLifecycleSignal(t, done, "HTTP responses 413 handler")

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Known-Status") != "413" {
		t.Fatalf("X-Known-Status = %q, want 413", resp.Header.Get("X-Known-Status"))
	}
	if resp.Header.Get("Content-Length") != "" {
		t.Fatalf("stale Content-Length = %q, want empty", resp.Header.Get("Content-Length"))
	}
	if len(body) > compactUpstreamErrorBodySize {
		t.Fatalf("body length = %d, want <= %d", len(body), compactUpstreamErrorBodySize)
	}
	if summary.StatsSuppressed() {
		t.Fatal("known HTTP 413 was stats-suppressed")
	}
	handler.RecordRequest(summary, resp.StatusCode, "responses-413-race", time.Millisecond)
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("stats = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 with compaction skipped", got)
	}
}

func waitForLifecycleSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestGeminiCountTokensPostHeaderBodyCancellationReturnsShutdown503(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1beta/models/gemini-2.5-pro:countTokens",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
	)
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleGeminiModels(w, req)
	}()

	waitForLifecycleSignal(t, upstreamStarted, "Gemini countTokens response headers")
	handler.BeginShutdown()
	waitForLifecycleSignal(t, upstreamCanceled, "Gemini countTokens body cancellation")
	waitForLifecycleSignal(t, done, "Gemini countTokens handler to return")

	if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", got, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"UNAVAILABLE"`) {
		t.Fatalf("Gemini shutdown response missing UNAVAILABLE: %s", w.Body.String())
	}
	if !summary.StatsSuppressed() {
		t.Fatal("Gemini shutdown request was not suppressed from provider stats")
	}
}

func TestForcedAggregateLifecycleCancellationReturnsShutdown503(t *testing.T) {
	for _, tt := range []struct {
		name   string
		path   string
		body   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
		marker string
		prefix string
	}{
		{
			name:   "openai chat",
			path:   "/v1/chat/completions",
			body:   `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleOpenAIChatCompletions(w, r) },
			marker: `"type":"service_unavailable"`,
			prefix: "data: {\"id\":\"chatcmpl-force\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
		},
		{
			name:   "gemini",
			path:   "/v1beta/models/gemini-2.5-pro:generateContent",
			body:   `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleGeminiModels(w, r) },
			marker: `"status":"UNAVAILABLE"`,
			prefix: "data: {\"id\":\"chatcmpl-force\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.prefix)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				close(started)
				<-r.Context().Done()
				close(canceled)
			})
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); tt.handle(handler, w, req) }()
			waitForLifecycleSignal(t, started, "forced stream to start")
			handler.BeginShutdown()
			waitForLifecycleSignal(t, canceled, "forced stream cancellation")
			waitForLifecycleSignal(t, done, "forced stream handler")
			if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
				t.Fatalf("status=%d want 503: %s", got, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.marker) {
				t.Fatalf("missing shutdown marker %s: %s", tt.marker, w.Body.String())
			}
			if !summary.StatsSuppressed() {
				t.Fatal("forced stream shutdown was not stats-suppressed")
			}
		})
	}
}

func TestBufferedJSONLifecycleCancellationReturnsShutdown503(t *testing.T) {
	for _, tt := range []struct {
		name       string
		newHandler func(testing.TB, string) *ProxyHandler
		path       string
		body       string
		handle     func(*ProxyHandler, http.ResponseWriter, *http.Request)
		marker     string
	}{
		{
			name: "direct anthropic", newHandler: newLifecycleStreamDirectAnthropicHandler,
			path: "/v1/messages", body: `{"model":"claude-public","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleAnthropicMessages(w, r) }, marker: `"type":"overloaded_error"`,
		},
		{
			name: "gemini", newHandler: newLifecycleStreamDefaultHandler,
			path: "/v1beta/models/gemini-2.5-pro:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleGeminiModels(w, r) }, marker: `"status":"UNAVAILABLE"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"partial":`)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()
			handler := tt.newHandler(t, upstream.URL)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); tt.handle(handler, w, req) }()
			waitForLifecycleSignal(t, started, "JSON body to start")
			handler.BeginShutdown()
			waitForLifecycleSignal(t, canceled, "JSON body cancellation")
			waitForLifecycleSignal(t, done, "JSON handler")
			if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
				t.Fatalf("status=%d want 503: %s", got, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.marker) {
				t.Fatalf("missing shutdown marker %s: %s", tt.marker, w.Body.String())
			}
			if !summary.StatsSuppressed() {
				t.Fatal("JSON shutdown was not stats-suppressed")
			}
		})
	}
}

func TestHandleModelsAzureOverlayCancellationReturnsShutdown503(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	handler, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
		ID: "azure", Type: "azure-openai", Default: true, BaseURL: upstream.URL + "/openai/v1", APIKey: "test-key",
		Models: []ProviderModelConfig{{PublicID: "gpt-public", Deployment: "gpt-deployment", Endpoints: []string{"/responses"}}},
	}}}))
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); handler.HandleModels(w, req) }()
	waitForLifecycleSignal(t, started, "Azure overlay to start")
	handler.BeginShutdown()
	waitForLifecycleSignal(t, canceled, "Azure overlay cancellation")
	waitForLifecycleSignal(t, done, "models handler")
	if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503: %s", got, w.Body.String())
	}
	if !summary.StatsSuppressed() {
		t.Fatal("Azure models shutdown was not stats-suppressed")
	}
	handler.models.mu.RLock()
	cached := len(handler.models.entries)
	handler.models.mu.RUnlock()
	if cached != 0 {
		t.Fatalf("models cache entries=%d, want 0 after canceled refresh", cached)
	}
}

func TestDirectAnthropicCountTokensPostHeaderCancellationReturnsShutdown503(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Count", "not-committed")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()

	handler := newLifecycleStreamDirectAnthropicHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-public","messages":[{"role":"user","content":"count"}]}`))
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleAnthropicMessagesCountTokens(w, req)
	}()

	waitForLifecycleSignal(t, started, "direct count_tokens response headers")
	handler.BeginShutdown()
	waitForLifecycleSignal(t, canceled, "direct count_tokens body cancellation")
	waitForLifecycleSignal(t, done, "direct count_tokens handler")

	resp := w.Result()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, w.Body.String())
	}
	if got := resp.Header.Get("X-Upstream-Count"); got != "" {
		t.Fatalf("upstream success header leaked before buffered body completed: %q", got)
	}
	if !strings.Contains(w.Body.String(), `"type":"overloaded_error"`) {
		t.Fatalf("Anthropic shutdown response missing overloaded_error: %s", w.Body.String())
	}
	if !summary.StatsSuppressed() {
		t.Fatal("direct count_tokens shutdown was not stats-suppressed")
	}
}

func TestAzureModelsOverlayDeadlineFallbackAndCancellation(t *testing.T) {
	newHandler := func(t *testing.T) *ProxyHandler {
		t.Helper()
		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.NewWithWriter(logger.LevelError, io.Discard),
			WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
				ID: "azure", Type: "azure-openai", Default: true, BaseURL: "https://azure.example/openai/v1", APIKey: "test-key",
				Models: []ProviderModelConfig{{PublicID: "gpt-public", Deployment: "gpt-deployment", Endpoints: []string{"/responses"}}},
			}}}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler: %v", err)
		}
		handler.maxRetries = 1
		handler.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		})}
		return handler
	}

	t.Run("ordinary deadline falls back to static catalog", func(t *testing.T) {
		handler := newHandler(t)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		if got := w.Result().StatusCode; got != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", got, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"id":"gpt-public"`) {
			t.Fatalf("static Azure model missing after deadline fallback: %s", w.Body.String())
		}
		if summary.StatsSuppressed() {
			t.Fatal("ordinary Azure metadata timeout was incorrectly stats-suppressed")
		}
	})

	t.Run("wrapped canceled error with live parent falls back", func(t *testing.T) {
		handler := newHandler(t)
		handler.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("internal metadata cancellation: %w", context.Canceled)
		})}
		result, err := handler.fetchProviderModels(context.Background(), handler.providerSetup().defaultProvider(), "", "")
		if err != nil {
			t.Fatalf("fetchProviderModels() error = %v, want best-effort fallback", err)
		}
		if len(result.models) != 1 || result.models[0].publicID != "gpt-public" {
			t.Fatalf("fallback models = %#v, want configured gpt-public", result.models)
		}
	})

	t.Run("canceled parent propagates", func(t *testing.T) {
		handler := newHandler(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := handler.fetchProviderModels(ctx, handler.providerSetup().defaultProvider(), "", "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetchProviderModels() error = %v, want context.Canceled", err)
		}
	})
}

func TestForcedAggregateSemanticErrorPreservesStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n")
	}))
	defer upstream.Close()

	tests := []struct {
		name   string
		path   string
		body   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name: "openai chat",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleOpenAIChatCompletions(w, r)
			},
		},
		{
			name: "translated anthropic",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleAnthropicMessages(w, r)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newLifecycleStreamDefaultHandler(t, upstream.URL)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			tt.handle(handler, w, req)

			if got := w.Result().StatusCode; got != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429: %s", got, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "slow down") {
				t.Fatalf("semantic error detail was not preserved: %s", w.Body.String())
			}
		})
	}
}

func TestHandleShutdownErrorRequiresLifecycleCause(t *testing.T) {
	handler := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
	independentCtx, cancelIndependent := context.WithCancel(context.Background())
	cancelIndependent()
	handler.BeginShutdown()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	requestCtx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(requestCtx)
	w := httptest.NewRecorder()
	if handler.handleShutdownError(w, req, independentCtx, context.Canceled) {
		t.Fatal("independent cancellation was misclassified as lifecycle shutdown")
	}
	if summary.StatsSuppressed() {
		t.Fatal("independent cancellation was stats-suppressed")
	}

	lifecycleHandler := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
	upstreamCtx, cancelUpstream := lifecycleHandler.newInferenceUpstreamContext(false)
	defer cancelUpstream()
	lifecycleHandler.BeginShutdown()
	<-upstreamCtx.Done()

	lifecycleReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	lifecycleRequestCtx, lifecycleSummary := WithRequestSummary(lifecycleReq.Context())
	lifecycleReq = lifecycleReq.WithContext(lifecycleRequestCtx)
	lifecycleWriter := httptest.NewRecorder()
	if !lifecycleHandler.handleShutdownError(lifecycleWriter, lifecycleReq, upstreamCtx, context.Canceled) {
		t.Fatal("lifecycle cancellation was not handled as shutdown")
	}
	if got := lifecycleWriter.Result().StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("shutdown status = %d, want 503", got)
	}
	if !lifecycleSummary.StatsSuppressed() {
		t.Fatal("lifecycle cancellation was not stats-suppressed")
	}
}
