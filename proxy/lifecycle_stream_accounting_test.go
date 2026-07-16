package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

type readSignalBody struct {
	io.ReadCloser
	once    sync.Once
	started chan struct{}
}

type singleChunkTerminalThenCancelBody struct {
	ctx       context.Context
	data      []byte
	sent      bool
	blocked   chan struct{}
	blockOnce sync.Once
	onBlocked func()
}

func newSingleChunkTerminalThenCancelBody(ctx context.Context, data string) *singleChunkTerminalThenCancelBody {
	return &singleChunkTerminalThenCancelBody{
		ctx:     ctx,
		data:    []byte(data),
		blocked: make(chan struct{}),
	}
}

func (b *singleChunkTerminalThenCancelBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.data), nil
	}
	b.blockOnce.Do(func() { close(b.blocked) })
	if b.onBlocked != nil {
		b.onBlocked()
	}
	<-b.ctx.Done()
	return 0, context.Canceled
}

func (b *singleChunkTerminalThenCancelBody) Close() error { return nil }

type splitChunkTerminalThenCancelBody struct {
	ctx         context.Context
	chunks      [][]byte
	chunkIndex  int
	chunkOffset int
	blocked     chan struct{}
	blockOnce   sync.Once
	onBlocked   func()
}

func newSplitChunkTerminalThenCancelBody(ctx context.Context, chunks ...string) *splitChunkTerminalThenCancelBody {
	body := &splitChunkTerminalThenCancelBody{ctx: ctx, blocked: make(chan struct{})}
	for _, chunk := range chunks {
		body.chunks = append(body.chunks, []byte(chunk))
	}
	return body
}

func (b *splitChunkTerminalThenCancelBody) Read(p []byte) (int, error) {
	if b.chunkIndex < len(b.chunks) {
		chunk := b.chunks[b.chunkIndex]
		n := copy(p, chunk[b.chunkOffset:])
		b.chunkOffset += n
		if b.chunkOffset == len(chunk) {
			b.chunkIndex++
			b.chunkOffset = 0
		}
		return n, nil
	}
	b.blockOnce.Do(func() { close(b.blocked) })
	if b.onBlocked != nil {
		b.onBlocked()
	}
	<-b.ctx.Done()
	return 0, context.Canceled
}

func (b *splitChunkTerminalThenCancelBody) Close() error { return nil }

type peekReadOutcomeRaceBody struct {
	prefix    *strings.Reader
	err       error
	onFailure func()
	failed    bool
}

func newPeekReadOutcomeRaceBody(prefix string, err error, onFailure func()) *peekReadOutcomeRaceBody {
	return &peekReadOutcomeRaceBody{prefix: strings.NewReader(prefix), err: err, onFailure: onFailure}
}

func (b *peekReadOutcomeRaceBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if !b.failed {
		b.failed = true
		if b.onFailure != nil {
			b.onFailure()
		}
	}
	return 0, b.err
}

func (b *peekReadOutcomeRaceBody) Close() error { return nil }

func (b *readSignalBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

type lifecycleStreamSurface struct {
	name           string
	prefix         string
	semanticPrefix string
	semanticStatus int
	newHandler     func(testing.TB, string) *ProxyHandler
	newRequest     func() *http.Request
	handle         func(*ProxyHandler, http.ResponseWriter, *http.Request)
}

func TestLifecycleShutdownSuppressesHTTPStreamFailureAccounting(t *testing.T) {
	openAIChunk := "data: {\"id\":\"chatcmpl-life\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	openAIError := "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}\n\n"
	surfaces := []lifecycleStreamSurface{
		{
			name:           "openai chat passthrough",
			prefix:         openAIChunk,
			semanticPrefix: openAIError,
			semanticStatus: http.StatusTooManyRequests,
			newHandler:     newLifecycleStreamDefaultHandler,
			newRequest: func() *http.Request {
				return lifecycleStreamRequest("/v1/chat/completions", `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
			},
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleOpenAIChatCompletions(w, r)
			},
		},
		{
			name:           "translated anthropic",
			prefix:         openAIChunk,
			semanticPrefix: openAIError,
			semanticStatus: http.StatusTooManyRequests,
			newHandler:     newLifecycleStreamDefaultHandler,
			newRequest: func() *http.Request {
				return lifecycleStreamRequest("/v1/messages", `{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
			},
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleAnthropicMessages(w, r)
			},
		},
		{
			name:           "direct anthropic",
			prefix:         "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-life\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-upstream\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			semanticPrefix: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"capacity\"}}\n\n",
			semanticStatus: http.StatusServiceUnavailable,
			newHandler:     newLifecycleStreamDirectAnthropicHandler,
			newRequest: func() *http.Request {
				return lifecycleStreamRequest("/v1/messages", `{"model":"claude-public","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
			},
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleAnthropicMessages(w, r)
			},
		},
		{
			name:           "gemini",
			prefix:         openAIChunk,
			semanticPrefix: openAIError,
			semanticStatus: http.StatusTooManyRequests,
			newHandler:     newLifecycleStreamDefaultHandler,
			newRequest: func() *http.Request {
				return lifecycleStreamRequest("/v1beta/models/gemini-2.5-pro:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
			},
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleGeminiModels(w, r)
			},
		},
		{
			name:           "responses",
			prefix:         "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-life\",\"status\":\"in_progress\"}}\n\n",
			semanticPrefix: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-failed\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"too long\"}}}\n\n",
			semanticStatus: http.StatusInternalServerError,
			newHandler:     newLifecycleStreamDefaultHandler,
			newRequest: func() *http.Request {
				return lifecycleStreamRequest("/v1/responses", `{"model":"gpt-5.4","input":"hello","stream":true}`)
			},
			handle: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleResponses(w, r)
			},
		},
	}

	for _, surface := range surfaces {
		surface := surface
		t.Run(surface.name, func(t *testing.T) {
			t.Run("shutdown cancellation is suppressed", func(t *testing.T) {
				runLifecycleStreamAccountingCase(t, surface, lifecycleStreamTransportCancel, 0, true)
			})
			t.Run("real truncation remains a provider failure", func(t *testing.T) {
				runLifecycleStreamAccountingCase(t, surface, lifecycleStreamTruncation, http.StatusBadGateway, false)
			})
			t.Run("semantic error survives concurrent shutdown", func(t *testing.T) {
				runLifecycleStreamAccountingCase(t, surface, lifecycleStreamSemanticError, surface.semanticStatus, false)
			})
		})
	}
}

func TestHTTP2LifecycleBodyCancellationReturnsShutdown503(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	proto := make(chan int, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		proto <- r.ProtoMajor
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="upstream.gz"`)
		w.Header().Set("ETag", `"upstream-etag"`)
		w.Header().Set("X-Upstream-Leak", "must-not-survive")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	handler := newLifecycleStreamDefaultHandler(t, upstream.URL)
	readStarted := make(chan struct{})
	baseTransport := upstream.Client().Transport.(*http.Transport).Clone()
	baseTransport.DisableCompression = true
	handler.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := baseTransport.RoundTrip(req)
		if err == nil && resp != nil && resp.Body != nil {
			resp.Body = &readSignalBody{ReadCloser: resp.Body, started: readStarted}
		}
		return resp, err
	})}

	req := lifecycleStreamRequest("/v1/chat/completions", `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.HandleOpenAIChatCompletions(w, req)
	}()

	waitForLifecycleSignal(t, readStarted, "HTTP/2 response body read")
	handler.BeginShutdown()
	waitForLifecycleSignal(t, upstreamCanceled, "HTTP/2 upstream cancellation")
	waitForLifecycleSignal(t, done, "HTTP/2 stream handler")

	select {
	case got := <-proto:
		if got != 2 {
			t.Fatalf("upstream protocol major = %d, want HTTP/2", got)
		}
	default:
		t.Fatal("upstream protocol was not observed")
	}
	resp := w.Result()
	if got := resp.StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", got, w.Body.String())
	}
	for _, name := range []string{"Content-Encoding", "Content-Disposition", "ETag", "X-Upstream-Leak"} {
		if got := resp.Header.Get(name); got != "" {
			t.Errorf("shutdown response inherited %s: %q", name, got)
		}
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatalf("shutdown body is not valid JSON: %q", w.Body.String())
	}
	if !summary.StatsSuppressed() {
		t.Fatal("HTTP/2 lifecycle cancellation was not stats-suppressed")
	}
	if got := summary.FailureStatus(); got != 0 {
		t.Fatalf("FailureStatus = %d, want 0", got)
	}
}

func TestTranslatedAnthropicEagerMessageStartAndShutdownError(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamStarted)
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-releaseUpstream:
		}
	}))

	handler := newLifecycleStreamDefaultHandler(t, upstream.URL)
	summaryCh := make(chan *RequestSummary, 1)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, summary := WithRequestSummary(r.Context())
		summaryCh <- summary
		handler.HandleAnthropicMessages(w, r.WithContext(ctx))
	}))
	t.Cleanup(func() {
		handler.BeginShutdown()
		releaseOnce.Do(func() { close(releaseUpstream) })
		downstream.Close()
		upstream.Close()
	})

	req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	type responseResult struct {
		resp *http.Response
		err  error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		resp, requestErr := http.DefaultClient.Do(req)
		responseCh <- responseResult{resp: resp, err: requestErr}
	}()

	waitForLifecycleSignal(t, upstreamStarted, "translated Anthropic upstream headers")
	var resp *http.Response
	select {
	case result := <-responseCh:
		if result.err != nil {
			t.Fatalf("Anthropic request error = %v", result.err)
		}
		resp = result.resp
	case <-time.After(time.Second):
		t.Fatal("message_start was not flushed before the first upstream model chunk")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	type eventResult struct {
		event string
		err   error
	}
	firstEventCh := make(chan eventResult, 1)
	go func() {
		event, readErr := readLifecycleSSEEvent(reader)
		firstEventCh <- eventResult{event: event, err: readErr}
	}()
	var firstEvent string
	select {
	case result := <-firstEventCh:
		if result.err != nil {
			t.Fatalf("read message_start: %v", result.err)
		}
		firstEvent = result.event
	case <-time.After(time.Second):
		t.Fatal("message_start body event was not flushed before the first upstream model chunk")
	}
	if !strings.Contains(firstEvent, "event: message_start") {
		t.Fatalf("first event = %q, want message_start", firstEvent)
	}

	handler.BeginShutdown()
	waitForLifecycleSignal(t, upstreamCanceled, "translated Anthropic upstream cancellation")
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read shutdown terminal event: %v", err)
	}
	events := parseSSEEvents(firstEvent + string(remainder))
	if len(events) != 2 || events[0].Event != "message_start" || events[1].Event != "error" {
		t.Fatalf("events = %#v, want message_start then error", events)
	}
	var terminal struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[1].Data), &terminal); err != nil {
		t.Fatalf("shutdown error event is not valid JSON: %v", err)
	}
	if terminal.Type != "error" || terminal.Error.Type != "overloaded_error" || terminal.Error.Message != "server shutting down" {
		t.Fatalf("shutdown error event = %#v", terminal)
	}
	summary := <-summaryCh
	if !summary.StatsSuppressed() {
		t.Fatal("translated Anthropic lifecycle cancellation was not stats-suppressed")
	}
}

func TestResponsesPeekBufferedTerminalPrecedesLifecycleCancellationHTTP(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-peek\"}}\n\n"
	for _, tt := range []struct {
		name             string
		terminal         string
		wantStatus       int
		wantFailure      int
		wantSSE          bool
		wantTerminalType string
	}{
		{
			name:             "completed",
			terminal:         "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-peek\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n",
			wantStatus:       http.StatusOK,
			wantSSE:          true,
			wantTerminalType: "response.completed",
		},
		{
			name:             "failed",
			terminal:         "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-peek\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n",
			wantStatus:       http.StatusOK,
			wantFailure:      http.StatusTooManyRequests,
			wantSSE:          true,
			wantTerminalType: "response.failed",
		},
		{
			name:             "incomplete",
			terminal:         "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-peek\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
			wantStatus:       http.StatusOK,
			wantFailure:      http.StatusConflict,
			wantSSE:          true,
			wantTerminalType: "response.incomplete",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bodyCh := make(chan *singleChunkTerminalThenCancelBody, 1)
			handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
				body := newSingleChunkTerminalThenCancelBody(req.Context(), created+tt.terminal)
				bodyCh <- body
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       body,
					Request:    req,
				}, nil
			})
			handler.stats = newStatsCollector()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.HandleResponses(w, req)
			}()
			body := <-bodyCh
			waitForLifecycleSignal(t, body.blocked, "Responses speculative body read")
			handler.BeginShutdown()
			waitForLifecycleSignal(t, done, "Responses buffered terminal handler")

			resp := w.Result()
			responseBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tt.wantStatus, responseBody)
			}
			if tt.wantSSE && (!bytes.Contains(responseBody, []byte("response.created")) || !bytes.Contains(responseBody, []byte(tt.wantTerminalType))) {
				t.Fatalf("buffered SSE terminal was not forwarded: %s", responseBody)
			}
			if summary.StatsSuppressed() {
				t.Fatal("buffered terminal was stats-suppressed")
			}
			if got := summary.FailureStatus(); got != tt.wantFailure {
				t.Fatalf("FailureStatus = %d, want %d", got, tt.wantFailure)
			}
			recordStatus := resp.StatusCode
			if tt.wantFailure != 0 {
				recordStatus = tt.wantFailure
			}
			handler.RecordRequest(summary, recordStatus, "responses-peek-race", time.Millisecond)
			snap := handler.stats.snapshot()
			wantErrors := int64(0)
			if tt.wantStatus >= http.StatusBadRequest || tt.wantFailure != 0 {
				wantErrors = 1
			}
			if snap.Totals.Requests != 1 || snap.Totals.Errors != wantErrors {
				t.Fatalf("stats = requests:%d errors:%d, want 1/%d", snap.Totals.Requests, snap.Totals.Errors, wantErrors)
			}
		})
	}
}

func TestResponsesPeekQueuedTerminalPrecedesLifecycleCancellationHTTP(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-peek-split\"}}\n\n"
	for _, tt := range []struct {
		name        string
		terminal    string
		wantStatus  int
		wantFailure int
	}{
		{name: "completed", terminal: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-peek-split\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", wantStatus: http.StatusOK},
		{name: "failed", terminal: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-peek-split\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n", wantStatus: http.StatusOK, wantFailure: http.StatusTooManyRequests},
		{name: "incomplete", terminal: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-peek-split\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", wantStatus: http.StatusOK, wantFailure: http.StatusConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bodyCh := make(chan *splitChunkTerminalThenCancelBody, 1)
			var handler *ProxyHandler
			handler = newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
				body := newSplitChunkTerminalThenCancelBody(req.Context(), created, tt.terminal)
				body.onBlocked = handler.BeginShutdown
				bodyCh <- body
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
			})
			handler.stats = newStatsCollector()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); handler.HandleResponses(w, req) }()
			body := <-bodyCh
			waitForLifecycleSignal(t, body.blocked, "queued terminal speculative read")
			waitForLifecycleSignal(t, done, "queued terminal HTTP handler")

			resp := w.Result()
			responseBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tt.wantStatus, responseBody)
			}
			if summary.StatsSuppressed() || summary.FailureStatus() != tt.wantFailure {
				t.Fatalf("accounting = suppressed:%v failure:%d, want false/%d", summary.StatsSuppressed(), summary.FailureStatus(), tt.wantFailure)
			}
			if tt.wantStatus == http.StatusOK && (!bytes.Contains(responseBody, []byte("response.created")) || !bytes.Contains(responseBody, []byte("response."+tt.name))) {
				t.Fatalf("queued terminal was not forwarded: %s", responseBody)
			}
			recordStatus := tt.wantStatus
			if tt.wantFailure != 0 {
				recordStatus = tt.wantFailure
			}
			handler.RecordRequest(summary, recordStatus, "responses-split-peek-race", time.Millisecond)
			snap := handler.stats.snapshot()
			wantErrors := int64(0)
			if recordStatus >= http.StatusBadRequest {
				wantErrors = 1
			}
			if snap.Totals.Requests != 1 || snap.Totals.Errors != wantErrors {
				t.Fatalf("stats = requests:%d errors:%d, want 1/%d", snap.Totals.Requests, snap.Totals.Errors, wantErrors)
			}
		})
	}
}

func TestResponsesPeekReadOutcomeCausalityHTTP(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-outcome-http\"}}\n\n"
	for _, tt := range []struct {
		name           string
		readErr        error
		wantStatus     int
		wantFailure    int
		wantSuppressed bool
	}{
		{name: "independent EOF", readErr: io.EOF, wantStatus: http.StatusOK, wantFailure: http.StatusBadGateway},
		{name: "independent reset", readErr: errors.New("independent reset"), wantStatus: http.StatusOK, wantFailure: http.StatusBadGateway},
		{name: "lifecycle cancellation", readErr: context.Canceled, wantStatus: http.StatusServiceUnavailable, wantSuppressed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var handler *ProxyHandler
			handler = newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
				body := newPeekReadOutcomeRaceBody(created, tt.readErr, handler.BeginShutdown)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			ctx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			handler.HandleResponses(w, req)

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tt.wantStatus, body)
			}
			if summary.StatsSuppressed() != tt.wantSuppressed || summary.FailureStatus() != tt.wantFailure {
				t.Fatalf("accounting = suppressed:%v failure:%d, want %v/%d", summary.StatsSuppressed(), summary.FailureStatus(), tt.wantSuppressed, tt.wantFailure)
			}
			if !tt.wantSuppressed && !bytes.Contains(body, []byte("response.created")) {
				t.Fatalf("independent outcome did not commit passthrough SSE: %s", body)
			}
		})
	}
}

func readLifecycleSSEEvent(r *bufio.Reader) (string, error) {
	var event strings.Builder
	for {
		line, err := r.ReadString('\n')
		event.WriteString(line)
		if line == "\n" || line == "\r\n" {
			return event.String(), nil
		}
		if err != nil {
			return event.String(), err
		}
	}
}

type lifecycleStreamCaseMode int

const (
	lifecycleStreamTransportCancel lifecycleStreamCaseMode = iota
	lifecycleStreamTruncation
	lifecycleStreamSemanticError
)

func runLifecycleStreamAccountingCase(t *testing.T, surface lifecycleStreamSurface, mode lifecycleStreamCaseMode, wantFailure int, wantSuppressed bool) {
	t.Helper()

	prefix := surface.prefix
	blockUpstream := mode != lifecycleStreamTruncation
	if mode == lifecycleStreamSemanticError {
		prefix = surface.semanticPrefix
	}

	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	var startedOnce sync.Once
	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, prefix)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startedOnce.Do(func() { close(upstreamStarted) })
		if blockUpstream {
			select {
			case <-r.Context().Done():
			case <-releaseUpstream:
			}
		}
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseUpstream) })
		upstream.Close()
	})

	handler := surface.newHandler(t, upstream.URL)
	handler.stats = newStatsCollector()
	var logs bytes.Buffer
	handler.log = logger.NewWithWriter(logger.LevelDebug, &logs)
	req := surface.newRequest()
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	writer := newLifecycleStreamResponseWriter()

	if blockUpstream {
		handlerDone := make(chan struct{})
		go func() {
			defer close(handlerDone)
			surface.handle(handler, writer, req)
		}()
		waitForLifecycleSignal(t, upstreamStarted, "stream upstream to start")
		waitForLifecycleSignal(t, writer.flushed, "stream response to flush")
		if mode == lifecycleStreamSemanticError {
			waitForLifecycleFailureStatus(t, summary, wantFailure)
		}
		handler.BeginShutdown()
		waitForLifecycleSignal(t, handlerDone, "stream handler to return after shutdown")
	} else {
		surface.handle(handler, writer, req)
	}

	if got := summary.FailureStatus(); got != wantFailure {
		t.Fatalf("FailureStatus = %d, want %d", got, wantFailure)
	}
	if got := summary.StatsSuppressed(); got != wantSuppressed {
		t.Fatalf("StatsSuppressed = %v, want %v", got, wantSuppressed)
	}
	if !summary.StatsSuppressed() {
		status := http.StatusOK
		if wantFailure != 0 {
			status = wantFailure
		}
		handler.RecordRequest(summary, status, "lifecycle-stream-test", time.Millisecond)
	}
	snapshot := handler.stats.snapshot()
	wantRequests := int64(1)
	wantErrors := int64(0)
	if wantSuppressed {
		wantRequests = 0
	} else if wantFailure != 0 {
		wantErrors = 1
	}
	if snapshot.Totals.Requests != wantRequests {
		t.Fatalf("provider requests = %d, want %d", snapshot.Totals.Requests, wantRequests)
	}
	if snapshot.Totals.Errors != wantErrors {
		t.Fatalf("provider errors = %d, want %d", snapshot.Totals.Errors, wantErrors)
	}
	if wantErrors == 0 && len(snapshot.Errors) != 0 {
		t.Fatalf("provider error targets = %#v, want none", snapshot.Errors)
	}
	if wantErrors == 1 && len(snapshot.Errors) == 0 {
		t.Fatal("provider failure did not produce error attribution")
	}
	if wantSuppressed {
		for _, forbidden := range []string{
			"upstream request failed",
			"responses stream copy failed after commit",
			"responses stream ended before terminal event",
		} {
			if strings.Contains(logs.String(), forbidden) {
				t.Fatalf("shutdown emitted failure log %q: %s", forbidden, logs.String())
			}
		}
	}
}

func waitForLifecycleFailureStatus(t *testing.T, summary *RequestSummary, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if summary.FailureStatus() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for FailureStatus %d; got %d", want, summary.FailureStatus())
}

func newLifecycleStreamDefaultHandler(t testing.TB, upstreamURL string) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(upstreamURL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler
}

func newLifecycleStreamDirectAnthropicHandler(t testing.TB, upstreamURL string) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:       "native",
				Type:     "anthropic-compatible",
				Default:  true,
				BaseURL:  upstreamURL,
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:   "claude-public",
					Deployment: "claude-upstream",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler
}

func lifecycleStreamRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type lifecycleStreamResponseWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	status  int
	flushes int
	flushed chan struct{}
	once    sync.Once
}

func newLifecycleStreamResponseWriter() *lifecycleStreamResponseWriter {
	return &lifecycleStreamResponseWriter{
		header:  make(http.Header),
		flushed: make(chan struct{}),
	}
}

func (w *lifecycleStreamResponseWriter) Header() http.Header {
	return w.header
}

func (w *lifecycleStreamResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *lifecycleStreamResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *lifecycleStreamResponseWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
	w.once.Do(func() { close(w.flushed) })
}

func (w *lifecycleStreamResponseWriter) StatusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *lifecycleStreamResponseWriter) BodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func (w *lifecycleStreamResponseWriter) FlushCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushes
}

var _ http.ResponseWriter = (*lifecycleStreamResponseWriter)(nil)
var _ http.Flusher = (*lifecycleStreamResponseWriter)(nil)

func TestRequestSummarySemanticFailureOverridesShutdownSuppression(t *testing.T) {
	summary := &RequestSummary{}
	summary.SuppressStats()
	if !summary.StatsSuppressed() {
		t.Fatal("SuppressStats did not mark the request suppressed")
	}

	summary.setFailureStatus(http.StatusTooManyRequests)
	if summary.StatsSuppressed() {
		t.Fatal("semantic provider failure did not restore provider accounting")
	}
	if got := summary.FailureStatus(); got != http.StatusTooManyRequests {
		t.Fatalf("FailureStatus = %d, want 429", got)
	}

	summary.SuppressStats()
	if summary.StatsSuppressed() {
		t.Fatal("shutdown suppression overrode an existing semantic provider failure")
	}
}

func TestDirectAnthropicLifecycleCancellationEmitsTerminalError(t *testing.T) {
	branches := []struct {
		name          string
		publicModel   string
		upstreamModel string
	}{
		{name: "byte exact", publicModel: "claude-model", upstreamModel: "claude-model"},
		{name: "model rewrite", publicModel: "claude-public", upstreamModel: "claude-upstream"},
	}
	stages := []struct {
		name       string
		prefix     string
		wantEvents int
	}{
		{name: "pre first frame", wantEvents: 1},
		{
			name: "midstream",
			prefix: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-direct\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-upstream\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
			wantEvents: 3,
		},
	}

	for _, branch := range branches {
		branch := branch
		t.Run(branch.name, func(t *testing.T) {
			for _, stage := range stages {
				stage := stage
				t.Run(stage.name, func(t *testing.T) {
					ctx, summary := WithRequestSummary(context.Background())
					upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
					innerBody := newBlockingSSEReadCloser(stage.prefix)
					readStarted := make(chan struct{})
					body := &readSignalBody{ReadCloser: innerBody, started: readStarted}
					writer := newLifecycleStreamResponseWriter()
					setSSEHeaders(writer)
					writer.WriteHeader(http.StatusOK)
					done := make(chan struct{})
					go func() {
						defer close(done)
						streamAnthropicPassthroughBody(ctx, writer, body, branch.publicModel, branch.upstreamModel, streamLifecycleHooks{
							transportCanceled: func() bool { return errors.Is(upstreamCtx.Err(), context.Canceled) },
							suppressStats:     func() { suppressRequestStats(ctx) },
						})
					}()

					waitForLifecycleSignal(t, readStarted, "direct Anthropic body read")
					if stage.prefix != "" {
						waitForLifecycleSignal(t, writer.flushed, "direct Anthropic prefix flush")
					}
					cancelUpstream()
					_ = body.Close()
					waitForLifecycleSignal(t, done, "direct Anthropic shutdown stream")

					if got := writer.StatusCode(); got != http.StatusOK {
						t.Fatalf("status = %d, want committed 200", got)
					}
					events := parseSSEEvents(writer.BodyString())
					if len(events) != stage.wantEvents {
						t.Fatalf("events = %#v, want %d events; raw=%s", events, stage.wantEvents, writer.BodyString())
					}
					for _, event := range events[:len(events)-1] {
						if event.Event == "message_stop" || event.Event == "error" {
							t.Fatalf("unexpected pre-terminal event %#v", event)
						}
					}
					terminal := events[len(events)-1]
					if terminal.Event != "error" {
						t.Fatalf("terminal event = %#v, want error", terminal)
					}
					var payload struct {
						Type  string `json:"type"`
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal([]byte(terminal.Data), &payload); err != nil {
						t.Fatalf("decode terminal error: %v", err)
					}
					if payload.Type != "error" || payload.Error.Type != "overloaded_error" || payload.Error.Message != "server shutting down" {
						t.Fatalf("terminal payload = %#v", payload)
					}
					wantFlushes := 1
					if stage.prefix != "" {
						wantFlushes = 2
					}
					if got := writer.FlushCount(); got < wantFlushes {
						t.Fatalf("flush count = %d, want at least %d", got, wantFlushes)
					}
					if !summary.StatsSuppressed() || summary.FailureStatus() != 0 {
						t.Fatalf("shutdown accounting = suppressed:%v failure:%d, want suppressed without provider failure", summary.StatsSuppressed(), summary.FailureStatus())
					}
				})
			}
		})
	}
}

func TestDirectAnthropicShutdownFrameValidity(t *testing.T) {
	branches := []struct {
		name          string
		publicModel   string
		upstreamModel string
		rewrite       bool
	}{
		{name: "byte exact", publicModel: "claude-upstream", upstreamModel: "claude-upstream"},
		{name: "model rewrite", publicModel: "claude-public", upstreamModel: "claude-upstream", rewrite: true},
	}
	stages := []struct {
		name            string
		prefix          string
		existingError   bool
		partialMarker   string
		pendingTerminal string
	}{
		{
			name:   "normal boundary",
			prefix: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-boundary\",\"model\":\"claude-upstream\"}}\n\n",
		},
		{
			name:          "mid line",
			prefix:        "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-mid-line\"",
			partialMarker: "msg-mid-line",
		},
		{
			name:          "mid frame",
			prefix:        "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-mid-frame\"}}\n",
			partialMarker: "msg-mid-frame",
		},
		{
			name:          "existing error",
			prefix:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream capacity\"}}\n\n",
			existingError: true,
		},
		{
			name:            "pending message stop delimiter",
			prefix:          "event: message_stop\ndata: {\"type\":\"message_stop\"}\n",
			partialMarker:   "message_stop",
			pendingTerminal: "message_stop",
		},
		{
			name:            "pending error delimiter",
			prefix:          "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"pending capacity\"}}\n",
			partialMarker:   "pending capacity",
			pendingTerminal: "error",
		},
	}

	for _, branch := range branches {
		branch := branch
		t.Run(branch.name, func(t *testing.T) {
			for _, stage := range stages {
				stage := stage
				t.Run(stage.name, func(t *testing.T) {
					ctx, summary := WithRequestSummary(context.Background())
					upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
					innerBody := newBlockingSSEReadCloser(stage.prefix)
					readStarted := make(chan struct{})
					body := &readSignalBody{ReadCloser: innerBody, started: readStarted}
					writer := newLifecycleStreamResponseWriter()
					setSSEHeaders(writer)
					writer.WriteHeader(http.StatusOK)
					done := make(chan struct{})
					go func() {
						defer close(done)
						streamAnthropicPassthroughBody(ctx, writer, body, branch.publicModel, branch.upstreamModel, streamLifecycleHooks{
							transportCanceled: func() bool { return errors.Is(upstreamCtx.Err(), context.Canceled) },
							suppressStats:     func() { suppressRequestStats(ctx) },
						})
					}()

					waitForLifecycleSignal(t, readStarted, "direct Anthropic partial read")
					if !branch.rewrite || stage.partialMarker == "" {
						waitForLifecycleSignal(t, writer.flushed, "direct Anthropic prefix flush")
					}
					if stage.existingError || (!branch.rewrite && stage.pendingTerminal == "error") {
						waitForLifecycleFailureStatus(t, summary, http.StatusServiceUnavailable)
					}
					cancelUpstream()
					_ = body.Close()
					waitForLifecycleSignal(t, done, "direct Anthropic framed shutdown")

					raw := writer.BodyString()
					events := parseSSEEvents(raw)
					errorEvents := make([]sseEvent, 0, 2)
					messageStopEvents := 0
					for _, event := range events {
						if event.Event == "message_stop" {
							messageStopEvents++
						}
						if event.Event == "error" {
							errorEvents = append(errorEvents, event)
						}
					}
					if stage.existingError {
						if len(errorEvents) != 1 || messageStopEvents != 0 {
							t.Fatalf("terminal events = errors:%#v message_stop:%d, want one upstream error; raw=%s", errorEvents, messageStopEvents, raw)
						}
						if strings.Contains(errorEvents[0].Data, "server shutting down") {
							t.Fatalf("existing upstream error was followed by shutdown error: %s", raw)
						}
						if summary.StatsSuppressed() || summary.FailureStatus() != http.StatusServiceUnavailable {
							t.Fatalf("existing error accounting = suppressed:%v failure:%d", summary.StatsSuppressed(), summary.FailureStatus())
						}
						return
					}
					if !branch.rewrite && stage.pendingTerminal != "" {
						if !anthropicSSETailEndsFrame([]byte(raw)) {
							t.Fatalf("pending terminal frame was not completed: %q", raw)
						}
						switch stage.pendingTerminal {
						case "message_stop":
							if messageStopEvents != 1 || len(errorEvents) != 0 {
								t.Fatalf("terminal events = errors:%#v message_stop:%d, want one message_stop; raw=%s", errorEvents, messageStopEvents, raw)
							}
							if summary.StatsSuppressed() || summary.FailureStatus() != 0 {
								t.Fatalf("pending message_stop accounting = suppressed:%v failure:%d", summary.StatsSuppressed(), summary.FailureStatus())
							}
						case "error":
							if len(errorEvents) != 1 || messageStopEvents != 0 || strings.Contains(errorEvents[0].Data, "server shutting down") {
								t.Fatalf("terminal events = errors:%#v message_stop:%d, want one upstream error; raw=%s", errorEvents, messageStopEvents, raw)
							}
							if summary.StatsSuppressed() || summary.FailureStatus() != http.StatusServiceUnavailable {
								t.Fatalf("pending error accounting = suppressed:%v failure:%d", summary.StatsSuppressed(), summary.FailureStatus())
							}
						}
						return
					}
					if !branch.rewrite && stage.partialMarker != "" {
						if raw != stage.prefix {
							t.Fatalf("raw partial frame changed from %q to %q", stage.prefix, raw)
						}
						if len(errorEvents) != 0 || messageStopEvents != 0 {
							t.Fatalf("arbitrary partial frame published a terminal event: errors:%#v message_stop:%d raw=%s", errorEvents, messageStopEvents, raw)
						}
						if !summary.StatsSuppressed() || summary.FailureStatus() != 0 {
							t.Fatalf("partial shutdown accounting = suppressed:%v failure:%d", summary.StatsSuppressed(), summary.FailureStatus())
						}
						return
					}

					if len(errorEvents) != 1 || messageStopEvents != 0 {
						t.Fatalf("terminal events = errors:%#v message_stop:%d, want one shutdown error; raw=%s", errorEvents, messageStopEvents, raw)
					}

					if !strings.Contains(errorEvents[0].Data, `"type":"overloaded_error"`) || !strings.Contains(errorEvents[0].Data, "server shutting down") {
						t.Fatalf("shutdown error event = %#v", errorEvents[0])
					}
					shutdownAt := strings.LastIndex(raw, "event: error")
					if shutdownAt < 0 {
						t.Fatalf("missing shutdown event: %s", raw)
					}
					if shutdownAt > 0 {
						before := raw[:shutdownAt]
						if !strings.HasSuffix(before, "\n\n") && !strings.HasSuffix(before, "\r\n\r\n") {
							t.Fatalf("shutdown event was concatenated into a partial frame: %q", raw)
						}
					}
					if branch.rewrite && stage.partialMarker != "" && strings.Contains(raw, stage.partialMarker) {
						t.Fatalf("model rewrite forwarded incomplete buffered frame %q: %s", stage.partialMarker, raw)
					}
					if !summary.StatsSuppressed() || summary.FailureStatus() != 0 {
						t.Fatalf("shutdown accounting = suppressed:%v failure:%d", summary.StatsSuppressed(), summary.FailureStatus())
					}
				})
			}
		})
	}
}

func TestLifecycleCancellationAfterTerminalEventPreservesAccounting(t *testing.T) {
	t.Run("direct anthropic", func(t *testing.T) {
		for _, tt := range []struct {
			name          string
			publicModel   string
			upstreamModel string
		}{
			{name: "raw passthrough", publicModel: "claude-model", upstreamModel: "claude-model"},
			{name: "model rewrite", publicModel: "claude-public", upstreamModel: "claude-upstream"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				ctx, summary := WithRequestSummary(context.Background())
				upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
				body := newBlockingSSEReadCloser("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				writer := newLifecycleStreamResponseWriter()
				done := make(chan struct{})
				go func() {
					defer close(done)
					streamAnthropicPassthroughBody(ctx, writer, body, tt.publicModel, tt.upstreamModel, streamLifecycleHooks{
						transportCanceled: func() bool { return errors.Is(upstreamCtx.Err(), context.Canceled) },
						suppressStats:     func() { suppressRequestStats(ctx) },
					})
				}()
				waitForLifecycleSignal(t, writer.flushed, "Anthropic message_stop to flush")
				cancelUpstream()
				_ = body.Close()
				waitForLifecycleSignal(t, done, "Anthropic terminal stream to return")
				if summary.StatsSuppressed() || summary.FailureStatus() != 0 {
					t.Fatalf("terminal Anthropic accounting = suppressed:%v failure:%d, want success", summary.StatsSuppressed(), summary.FailureStatus())
				}
			})
		}
	})

	t.Run("responses", func(t *testing.T) {
		for _, tt := range []struct {
			name       string
			terminal   string
			wantStatus int
		}{
			{
				name:     "completed",
				terminal: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-complete\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
			},
			{
				name:       "failed",
				terminal:   "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-failed\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"too long\"}}}\n\n",
				wantStatus: http.StatusInternalServerError,
			},
			{
				name:       "incomplete",
				terminal:   "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
				wantStatus: http.StatusConflict,
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				ctx, summary := WithRequestSummary(context.Background())
				upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
				body := newBlockingSSEReadCloser(tt.terminal)
				writer := newLifecycleStreamResponseWriter()
				handler := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
				done := make(chan struct{})
				go func() {
					defer close(done)
					streamResponsesPipeWithFailureLog(ctx, handler, writer, body, nil, nil, "", streamLifecycleHooks{
						transportCanceled: func() bool { return errors.Is(upstreamCtx.Err(), context.Canceled) },
						suppressStats:     func() { suppressRequestStats(ctx) },
					})
				}()
				waitForLifecycleSignal(t, writer.flushed, "Responses terminal event to flush")
				if tt.wantStatus != 0 {
					waitForLifecycleFailureStatus(t, summary, tt.wantStatus)
				}
				cancelUpstream()
				_ = body.Close()
				waitForLifecycleSignal(t, done, "Responses terminal stream to return")
				if summary.StatsSuppressed() {
					t.Fatal("terminal Responses event was suppressed after lifecycle cancellation")
				}
				if got := summary.FailureStatus(); got != tt.wantStatus {
					t.Fatalf("FailureStatus = %d, want %d", got, tt.wantStatus)
				}
			})
		}
	})
}

func TestResponseBodyWriteErrorCapturesLifecycleCausalityAtFailure(t *testing.T) {
	t.Run("lifecycle cancellation", func(t *testing.T) {
		handler := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
		upstreamCtx, cancel := handler.newInferenceUpstreamContext(false)
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		resp := &http.Response{StatusCode: http.StatusOK, Request: req.WithContext(upstreamCtx)}

		handler.BeginShutdown()
		bodyErr := newResponseBodyWriteError(resp, context.Canceled, false, true, true)
		w := httptest.NewRecorder()
		if !handler.handleResponseBodyWriteError(w, req, upstreamCtx, "openai", bodyErr) {
			t.Fatal("lifecycle body cancellation was not handled")
		}
		if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", got)
		}
		if !summary.StatsSuppressed() || summary.FailureStatus() != 0 {
			t.Fatalf("lifecycle accounting = suppressed:%v failure:%d, want suppressed without failure", summary.StatsSuppressed(), summary.FailureStatus())
		}
	})

	t.Run("independent unexpected EOF then shutdown", func(t *testing.T) {
		handler := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
		upstreamCtx, cancel := handler.newInferenceUpstreamContext(false)
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		resp := &http.Response{StatusCode: http.StatusOK, Request: req.WithContext(upstreamCtx)}
		bodyErr := newResponseBodyWriteError(resp, io.ErrUnexpectedEOF, true, true, false)

		handler.BeginShutdown()
		w := httptest.NewRecorder()
		w.WriteHeader(http.StatusOK)
		if !handler.handleResponseBodyWriteError(w, req, upstreamCtx, "openai", bodyErr) {
			t.Fatal("committed provider body failure was not handled")
		}
		if summary.StatsSuppressed() {
			t.Fatal("independent provider body failure was incorrectly suppressed")
		}
		if got := summary.FailureStatus(); got != http.StatusBadGateway {
			t.Fatalf("FailureStatus = %d, want 502", got)
		}
	})

	t.Run("independent context cancellation then shutdown", func(t *testing.T) {
		handler := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
		upstreamCtx, cancel := handler.newInferenceUpstreamContext(false)
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		resp := &http.Response{StatusCode: http.StatusOK, Request: req.WithContext(upstreamCtx)}
		bodyErr := newResponseBodyWriteError(resp, context.Canceled, true, true, false)

		handler.BeginShutdown()
		w := httptest.NewRecorder()
		w.WriteHeader(http.StatusOK)
		if !handler.handleResponseBodyWriteError(w, req, upstreamCtx, "openai", bodyErr) {
			t.Fatal("independent context cancellation was not handled")
		}
		if summary.StatsSuppressed() {
			t.Fatal("independent context cancellation was incorrectly suppressed")
		}
		if got := summary.FailureStatus(); got != http.StatusBadGateway {
			t.Fatalf("FailureStatus = %d, want 502", got)
		}
	})
}

type fixedErrorReadCloser struct{ err error }

func (r *fixedErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *fixedErrorReadCloser) Close() error             { return nil }

type callbackErrorReadCloser struct {
	beforeReturn func()
	err          error
}

func (r *callbackErrorReadCloser) Read([]byte) (int, error) {
	if r.beforeReturn != nil {
		r.beforeReturn()
	}
	return 0, r.err
}

func (r *callbackErrorReadCloser) Close() error { return nil }

func TestLifecycleAwareReaderCapturesCancellationAtReadFailure(t *testing.T) {
	t.Run("independent reset before shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := newLifecycleAwareReadCloser(io.NopCloser(&erroringReader{}), ctx)
		buf := make([]byte, 1)
		_, _ = reader.Read(buf)
		_, err := reader.Read(buf)
		if err != io.ErrUnexpectedEOF {
			t.Fatalf("Read() error = %v, want io.ErrUnexpectedEOF", err)
		}
		cancel()
		if reader.canceledAtFailure() {
			t.Fatal("independent reset was incorrectly attributed to later cancellation")
		}
		suppressed := false
		hooks := streamLifecycleHooks{
			transportCanceled: reader.canceledAtFailure,
			suppressStats:     func() { suppressed = true },
		}
		if hooks.suppressTransportCancellation(true) || suppressed {
			t.Fatal("independent reset was incorrectly suppressed after shutdown")
		}
	})

	t.Run("read error preserves lifecycle cause", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errProxyLifecycleShutdown)
		reader := newLifecycleAwareReadCloser(&fixedErrorReadCloser{err: errProxyLifecycleShutdown}, ctx)
		buf := make([]byte, 1)
		_, err := reader.Read(buf)
		if !errors.Is(err, errProxyLifecycleShutdown) {
			t.Fatalf("Read() error = %v, want lifecycle shutdown cause", err)
		}
		if !reader.canceledAtFailure() {
			t.Fatal("lifecycle shutdown cause was not captured from read error")
		}
	})

	t.Run("plain cancellation with lifecycle shutdown cause is causal", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		reader := newLifecycleAwareReadCloser(&callbackErrorReadCloser{
			beforeReturn: func() { cancel(errProxyLifecycleShutdown) },
			err:          context.Canceled,
		}, ctx)
		buf := make([]byte, 1)
		_, err := reader.Read(buf)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read() error = %v, want context.Canceled", err)
		}
		if !reader.canceledAtFailure() {
			t.Fatal("HTTP/2-style context cancellation was not attributed to lifecycle shutdown")
		}
	})

	t.Run("plain cancellation without lifecycle shutdown cause is not causal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := newLifecycleAwareReadCloser(&fixedErrorReadCloser{err: context.Canceled}, ctx)
		buf := make([]byte, 1)
		_, err := reader.Read(buf)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read() error = %v, want context.Canceled", err)
		}
		if reader.canceledAtFailure() {
			t.Fatal("ordinary context cancellation was attributed to lifecycle shutdown")
		}
	})

	t.Run("EOF racing with lifecycle cancellation is not causal", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		reader := newLifecycleAwareReadCloser(&callbackErrorReadCloser{
			beforeReturn: func() { cancel(errProxyLifecycleShutdown) },
			err:          io.EOF,
		}, ctx)
		buf := make([]byte, 1)
		_, err := reader.Read(buf)
		if err != io.EOF {
			t.Fatalf("Read() error = %v, want io.EOF", err)
		}
		if reader.canceledAtFailure() {
			t.Fatal("opaque EOF was incorrectly attributed to lifecycle cancellation")
		}
	})

	t.Run("deadline exceeded is not lifecycle cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		reader := newLifecycleAwareReadCloser(&callbackErrorReadCloser{
			beforeReturn: func() { cancel(errProxyLifecycleShutdown) },
			err:          context.DeadlineExceeded,
		}, ctx)
		buf := make([]byte, 1)
		_, err := reader.Read(buf)
		if err != context.DeadlineExceeded {
			t.Fatalf("Read() error = %v, want context.DeadlineExceeded", err)
		}
		if reader.canceledAtFailure() {
			t.Fatal("deadline was incorrectly attributed to lifecycle cancellation")
		}
	})
}
