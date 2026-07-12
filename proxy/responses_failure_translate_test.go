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
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

func TestHandleResponses_PrecommitFailureTranslation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		body             string
		headers          http.Header
		wantStatus       int
		wantContentType  string
		wantRetryAfter   string
		wantErrorType    string
		wantErrorMessage string
		wantRawBody      string
		assertHeaders    func(t *testing.T, headers http.Header)
	}{
		{
			name: "too_many_requests translates before commit",
			body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-rate-limit\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"try again later\"}}}\n\n",
			headers: http.Header{
				"Content-Type":       []string{"text/event-stream"},
				"retry-after-ms":     []string{"1500"},
				"X-Request-Id":       []string{"req-1"},
				"X-Azure-Request-Id": []string{"az-1"},
				"Openai-Request-Id":  []string{"oa-1"},
			},
			wantStatus:       http.StatusTooManyRequests,
			wantContentType:  "application/json",
			wantRetryAfter:   "2",
			wantErrorType:    "rate_limit_error",
			wantErrorMessage: "try again later",
			assertHeaders: func(t *testing.T, headers http.Header) {
				t.Helper()
				if got := headers.Get("X-Request-Id"); got != "req-1" {
					t.Fatalf("X-Request-Id = %q, want %q", got, "req-1")
				}
				if got := headers.Get("X-Azure-Request-Id"); got != "az-1" {
					t.Fatalf("X-Azure-Request-Id = %q, want %q", got, "az-1")
				}
				if got := headers.Get("Openai-Request-Id"); got != "oa-1" {
					t.Fatalf("Openai-Request-Id = %q, want %q", got, "oa-1")
				}
			},
		},
		{
			name: "model_overloaded uses retry-after seconds",
			body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-overloaded\",\"error\":{\"type\":\"server_error\",\"code\":\"model_overloaded\",\"message\":\"capacity\"}}}\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"Retry-After":  []string{"5"},
			},
			wantStatus:       http.StatusServiceUnavailable,
			wantContentType:  "application/json",
			wantRetryAfter:   "5",
			wantErrorType:    "server_error",
			wantErrorMessage: "capacity",
		},
		{
			name: "empty code with rate_limit_error still translates",
			body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-rate-limit-type\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}}\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:       http.StatusTooManyRequests,
			wantContentType:  "application/json",
			wantErrorType:    "rate_limit_error",
			wantErrorMessage: "slow down",
		},
		{
			name: "unknown failure fails open",
			body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-unknown\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"too long\"}}}\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "text/event-stream",
			wantRawBody:     "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-unknown\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"too long\"}}}\n\n",
		},
		{
			name: "non failed first event stays passthrough",
			body: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "text/event-stream",
			wantRawBody:     "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n",
		},
		{
			name: "comments and bom preserve raw bytes",
			body: "\xEF\xBB\xBF: keepalive\n\nevent: response.created\ndata: first snowman ☃\ndata: second line\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "text/event-stream",
			wantRawBody:     "\xEF\xBB\xBF: keepalive\n\nevent: response.created\ndata: first snowman ☃\ndata: second line\n\n",
		},
		{
			name: "control only message does not consume first semantic event",
			body: "id: msg-1\nretry: 1000\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-control\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"back off\"}}}\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:       http.StatusTooManyRequests,
			wantContentType:  "application/json",
			wantErrorType:    "rate_limit_error",
			wantErrorMessage: "back off",
		},
		{
			name: "crlf framing works",
			body: "event: response.failed\r\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-crlf\",\"error\":{\"type\":\"server_error\",\"code\":\"bad_gateway\",\"message\":\"gateway\"}}}\r\n\r\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:       http.StatusBadGateway,
			wantContentType:  "application/json",
			wantErrorType:    "server_error",
			wantErrorMessage: "gateway",
		},
		{
			name: "unnamed failed event translates",
			body: "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-unnamed\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"later\"}}}\n\n",
			headers: http.Header{
				"Content-Type":   []string{"text/event-stream"},
				"retry-after-ms": []string{"500"},
			},
			wantStatus:       http.StatusTooManyRequests,
			wantContentType:  "application/json",
			wantRetryAfter:   "1",
			wantErrorType:    "rate_limit_error",
			wantErrorMessage: "later",
		},
		{
			name: "unnamed invalid payload fails open",
			body: "data: not-json\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "text/event-stream",
			wantRawBody:     "data: not-json\n\n",
		},
		{
			name: "malformed failed json fails open",
			body: "event: response.failed\ndata: {bad json}\n\n",
			headers: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "text/event-stream",
			wantRawBody:     "event: response.failed\ndata: {bad json}\n\n",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     cloneHeader(tt.headers),
					Body:       io.NopCloser(strings.NewReader(tt.body)),
				}, nil
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"Hello","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.HandleResponses(w, req)

			resp := w.Result()
			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d, body=%s", resp.StatusCode, tt.wantStatus, body)
			}
			if got := resp.Header.Get("Content-Type"); got != tt.wantContentType {
				t.Fatalf("Content-Type = %q, want %q", got, tt.wantContentType)
			}
			if got := resp.Header.Get("Retry-After"); got != tt.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			if tt.assertHeaders != nil {
				tt.assertHeaders(t, resp.Header)
			}

			body, _ := io.ReadAll(resp.Body)
			if tt.wantRawBody != "" {
				if string(body) != tt.wantRawBody {
					t.Fatalf("body = %q, want %q", string(body), tt.wantRawBody)
				}
				return
			}

			var envelope struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body=%s", err, body)
			}
			if envelope.Error.Type != tt.wantErrorType {
				t.Fatalf("error.type = %q, want %q", envelope.Error.Type, tt.wantErrorType)
			}
			if envelope.Error.Message != tt.wantErrorMessage {
				t.Fatalf("error.message = %q, want %q", envelope.Error.Message, tt.wantErrorMessage)
			}
		})
	}
}

func TestPeekAndForwardResponses_CompleteFailurePrecedesMaxPeekBytes(t *testing.T) {
	t.Parallel()

	raw := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"too_many_requests\",\"message\":\"" + strings.Repeat("x", 128) + "\"}}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(raw)),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
	w := httptest.NewRecorder()
	h := &ProxyHandler{log: logger.New(logger.LevelInfo)}

	peekAndForwardResponsesWithConfig(h, w, req, resp, context.Background(), func() {}, "gpt-5.4", 50*time.Millisecond, 32, "")

	result := w.Result()
	if result.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(result.Body)
		t.Fatalf("status = %d, want 429: %s", result.StatusCode, body)
	}
	body, _ := io.ReadAll(result.Body)
	if !bytes.Contains(body, []byte(`"type":"rate_limit_error"`)) {
		t.Fatalf("translated body = %q, want rate_limit_error", body)
	}
}

func TestPeekAndForwardResponses_FailsOpenOnPeekTimeout(t *testing.T) {
	t.Parallel()

	body := &timeoutReadCloser{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
	w := httptest.NewRecorder()
	h := &ProxyHandler{log: logger.New(logger.LevelInfo)}

	peekAndForwardResponsesWithConfig(h, w, req, resp, context.Background(), func() {}, "gpt-5.4", 20*time.Millisecond, 1024, "")

	result := w.Result()
	if result.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(result.Body)
		t.Fatalf("status = %d, want 200: %s", result.StatusCode, raw)
	}
	raw, _ := io.ReadAll(result.Body)
	want := ": keepalive\n\nevent: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-timeout\"}}\n\n"
	if string(raw) != want {
		t.Fatalf("body = %q, want %q", string(raw), want)
	}
}

func TestPeekAndForwardResponses_ClientDisconnectDuringPeekClosesUpstream(t *testing.T) {
	t.Parallel()

	body := newBlockingReadCloser()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h := &ProxyHandler{log: logger.New(logger.LevelInfo)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		peekAndForwardResponsesWithConfig(h, w, req, resp, context.Background(), func() {}, "gpt-5.4", time.Second, 1024, "")
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler did not return after client disconnect")
	}

	select {
	case <-body.closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("upstream body was not closed")
	}
}

func TestResponsesPreparedBodyCloseUnblocksCompletedPumpWithQueuedTrailingData(t *testing.T) {
	prefix := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	trailing := append([]byte("data: "), bytes.Repeat([]byte("x"), 1<<20)...)
	trailing = append(trailing, '\n', '\n')

	pr, pw := io.Pipe()
	chunkCh := make(chan responsesPeekChunk, 1)
	chunkCh <- responsesPeekChunk{data: trailing}
	abortCh := make(chan struct{})
	var abortOnce sync.Once
	body := &responsesPreparedBody{
		reader: pr,
		closeFn: func() {
			abortOnce.Do(func() { close(abortCh) })
		},
	}

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		writePrefixAndDrainResponsesStream(pw, prefix, chunkCh, abortCh, false, nil)
	}()

	err := consumeResponsesSSEData(body, func(data string) error {
		if strings.Contains(data, `"type":"response.completed"`) {
			return errResponsesWebSocketStreamTerminal
		}
		return nil
	})
	if !errors.Is(err, errResponsesWebSocketStreamTerminal) {
		t.Fatalf("consumeResponsesSSEData error = %v, want terminal sentinel", err)
	}

	deadline := time.Now().Add(time.Second)
	for len(chunkCh) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("pump did not dequeue separately queued trailing data")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-pumpDone:
		t.Fatal("pump unexpectedly exited before the prepared body was closed")
	default:
	}

	if err := body.Close(); err != nil {
		t.Fatalf("prepared body Close() error = %v", err)
	}
	select {
	case <-pumpDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("closing prepared body did not unblock pump blocked in pipe write")
	}
}

func TestRunResponsesPeekPump_CompletedWithQueuedTrailingDataAlwaysExits(t *testing.T) {
	for iteration := range 50 {
		upstream := newCompletedThenTrailingReadCloser()
		pr, pw := io.Pipe()
		peekDone := make(chan peekResult, 1)
		commitCh := make(chan struct{})
		abortCh := make(chan struct{})
		var abortOnce sync.Once
		abort := func() {
			abortOnce.Do(func() {
				close(abortCh)
				_ = upstream.Close()
			})
		}
		body := &responsesPreparedBody{reader: pr, closeFn: abort}
		pumpDone := make(chan struct{})
		go func() {
			defer close(pumpDone)
			runResponsesPeekPump(upstream, pw, http.Header{}, peekDone, newResponsesPeekState(), commitCh, abortCh, make(chan struct{}), responsesPrecommitMaxPeekBytes)
		}()

		select {
		case result := <-peekDone:
			if result.decision != responsesPeekDecisionPassthrough {
				t.Fatalf("iteration %d peek decision = %v, want passthrough", iteration, result.decision)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d timed out waiting for peek decision", iteration)
		}
		select {
		case <-upstream.trailingQueued:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d trailing data was not separately queued", iteration)
		}
		close(commitCh)

		err := consumeResponsesSSEData(body, func(data string) error {
			if strings.Contains(data, `"type":"response.completed"`) {
				return errResponsesWebSocketStreamTerminal
			}
			return nil
		})
		if !errors.Is(err, errResponsesWebSocketStreamTerminal) {
			t.Fatalf("iteration %d consume error = %v, want terminal sentinel", iteration, err)
		}
		if err := body.Close(); err != nil {
			t.Fatalf("iteration %d prepared body Close() error = %v", iteration, err)
		}
		select {
		case <-pumpDone:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("iteration %d prepared-stream pump did not exit", iteration)
		}
		select {
		case <-upstream.readerDone:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("iteration %d upstream reader goroutine did not exit", iteration)
		}
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

type timeoutReadCloser struct {
	mu     sync.Mutex
	step   int
	closed chan struct{}
	once   sync.Once
}

func (r *timeoutReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	step := r.step
	r.step++
	if r.closed == nil {
		r.closed = make(chan struct{})
	}
	closed := r.closed
	r.mu.Unlock()

	switch step {
	case 0:
		return copy(p, []byte(": keepalive\n\n")), nil
	case 1:
		timer := time.NewTimer(60 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return copy(p, []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-timeout\"}}\n\n")), nil
		case <-closed:
			return 0, io.EOF
		}
	default:
		return 0, io.EOF
	}
}

func (r *timeoutReadCloser) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		if r.closed == nil {
			r.closed = make(chan struct{})
		}
		close(r.closed)
		r.mu.Unlock()
	})
	return nil
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

type completedThenTrailingReadCloser struct {
	mu             sync.Mutex
	step           int
	closed         chan struct{}
	closeOnce      sync.Once
	trailingQueued chan struct{}
	trailingOnce   sync.Once
	readerDone     chan struct{}
	readerDoneOnce sync.Once
}

func newCompletedThenTrailingReadCloser() *completedThenTrailingReadCloser {
	return &completedThenTrailingReadCloser{
		closed:         make(chan struct{}),
		trailingQueued: make(chan struct{}),
		readerDone:     make(chan struct{}),
	}
}

func (r *completedThenTrailingReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	step := r.step
	r.step++
	r.mu.Unlock()

	switch step {
	case 0:
		return copy(p, []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-queued\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-queued\"}}\n\n")), nil
	case 1:
		return copy(p, []byte("data: trailing-after-completed\n\n")), nil
	default:
		r.trailingOnce.Do(func() { close(r.trailingQueued) })
		<-r.closed
		r.readerDoneOnce.Do(func() { close(r.readerDone) })
		return 0, io.EOF
	}
}

func (r *completedThenTrailingReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read(p []byte) (int, error) {
	<-r.closed
	return 0, io.EOF
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() {
		close(r.closed)
	})
	return nil
}

// TestStreamResponsesPipeRecordsUsage verifies the streaming POST /v1/responses
// path observes token usage from the response.completed SSE event into the
// per-request RequestSummary, while forwarding the stream bytes unchanged.
func TestStreamResponsesPipeRecordsUsage(t *testing.T) {
	sse := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"," +
		"\"usage\":{\"input_tokens\":210,\"output_tokens\":75,\"total_tokens\":285," +
		"\"input_tokens_details\":{\"cached_tokens\":40}," +
		"\"output_tokens_details\":{\"reasoning_tokens\":18}}}}\n\n"

	ctx, summary := WithRequestSummary(context.Background())
	rec := httptest.NewRecorder()
	h := &ProxyHandler{log: logger.New(logger.LevelInfo)}

	streamResponsesPipeWithFailureLog(ctx, h, rec, strings.NewReader(sse), http.Header{}, nil, "")

	d := readSummaryForStats(summary)
	if d.prompt != 210 || d.completion != 75 || d.total != 285 {
		t.Fatalf("streamed usage not recorded: prompt=%d completion=%d total=%d", d.prompt, d.completion, d.total)
	}
	if d.cached != 40 || d.reasoning != 18 {
		t.Fatalf("streamed detail usage not recorded: cached=%d reasoning=%d", d.cached, d.reasoning)
	}
	// The stream bytes must be forwarded to the client unchanged.
	if got := rec.Body.String(); got != sse {
		t.Fatalf("stream body altered.\n got: %q\nwant: %q", got, sse)
	}
}

func TestStreamResponsesPipeRecordsUsageFromFailedAndIncomplete(t *testing.T) {
	for _, tt := range []struct {
		name       string
		event      string
		wantStatus int
	}{
		{
			name:       "failed",
			event:      `{"type":"response.failed","response":{"id":"resp-failed-usage","error":{"type":"server_error","code":"too_many_requests","message":"slow down"},"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}`,
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "incomplete",
			event:      `{"type":"response.incomplete","response":{"id":"resp-incomplete-usage","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":13,"output_tokens":5,"total_tokens":18}}}`,
			wantStatus: http.StatusConflict,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sse := "event: response." + tt.name + "\ndata: " + tt.event + "\n\n"
			ctx, summary := WithRequestSummary(context.Background())
			rec := httptest.NewRecorder()
			streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.New(logger.LevelError)}, rec, strings.NewReader(sse), nil, nil, "")

			usage := readSummaryForStats(summary)
			if usage.prompt == 0 || usage.completion == 0 || usage.total == 0 {
				t.Fatalf("terminal usage was not recorded: prompt=%d completion=%d total=%d", usage.prompt, usage.completion, usage.total)
			}
			if got := summary.FailureStatus(); got != tt.wantStatus {
				t.Fatalf("FailureStatus = %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestPeekAndForwardResponses_ChunkingIndependentNormalWireFormatStress(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-normal-split\"}}\n\n"
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-normal-split\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n"
	wantBody := created + failed
	deliveries := []struct {
		name string
		body func() io.ReadCloser
	}{
		{name: "coalesced", body: func() io.ReadCloser { return io.NopCloser(strings.NewReader(wantBody)) }},
		{name: "split", body: func() io.ReadCloser { return newSplitChunkEOFReadCloser(created, failed) }},
	}
	for _, delivery := range deliveries {
		t.Run(delivery.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       delivery.body(),
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
				w := httptest.NewRecorder()
				h := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}

				peekAndForwardResponsesWithConfig(h, w, req, resp, context.Background(), func() {}, "gpt-5.4", 50*time.Millisecond, responsesPrecommitMaxPeekBytes, "")

				result := w.Result()
				body, _ := io.ReadAll(result.Body)
				_ = result.Body.Close()
				if result.StatusCode != http.StatusOK || string(body) != wantBody {
					t.Fatalf("iteration %d produced status=%d body=%q, want SSE 200 body=%q", i, result.StatusCode, body, wantBody)
				}
			}
		})
	}
}

type splitChunkEOFReadCloser struct {
	chunks [][]byte
	index  int
	offset int
}

func newSplitChunkEOFReadCloser(chunks ...string) *splitChunkEOFReadCloser {
	r := &splitChunkEOFReadCloser{}
	for _, chunk := range chunks {
		r.chunks = append(r.chunks, []byte(chunk))
	}
	return r
}

func (r *splitChunkEOFReadCloser) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	n := copy(p, chunk[r.offset:])
	r.offset += n
	if r.offset == len(chunk) {
		r.index++
		r.offset = 0
	}
	return n, nil
}

func (r *splitChunkEOFReadCloser) Close() error { return nil }

func TestPeekAndForwardResponses_LifecycleCancellationWakesPrecommitAwait(t *testing.T) {
	run := func(t *testing.T, prefix string, cancelNearTimeout bool) {
		t.Helper()
		upstreamCtx, cancelUpstream := context.WithCancelCause(context.Background())
		defer cancelUpstream(context.Canceled)
		body := newLifecyclePeekBlockBody(upstreamCtx, prefix)
		h := &ProxyHandler{}
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
		ctx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		if !cancelNearTimeout {
			body.onBlocked = func() { cancelUpstream(errProxyLifecycleShutdown) }
		}
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			peekAndForwardResponsesWithConfig(
				h,
				w,
				req,
				&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: newLifecycleAwareReadCloser(body, upstreamCtx)},
				upstreamCtx,
				func() {},
				"gpt-5.4",
				100*time.Millisecond,
				responsesPrecommitMaxPeekBytes,
				"",
				streamLifecycleHooks{
					// Deliberately false: the HTTP path must wake from upstreamCtx and
					// must not depend on a later reader-side cancellation sample.
					transportCanceled: func() bool { return false },
					suppressStats:     func() { suppressRequestStats(ctx) },
					writePrecommitShutdown: func() {
						h.WriteShutdownServiceUnavailable(w, req)
					},
				},
			)
		}()
		<-body.blocked
		if cancelNearTimeout {
			time.Sleep(80 * time.Millisecond)
			cancelStarted := time.Now()
			cancelUpstream(errProxyLifecycleShutdown)
			select {
			case <-done:
				if elapsed := time.Since(cancelStarted); elapsed > 75*time.Millisecond {
					t.Fatalf("cancellation wake elapsed = %v, want prompt return before timeout path", elapsed)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for precommit cancellation")
			}
		} else {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for initial-event cancellation")
			}
		}
		if w.Result().StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", w.Result().StatusCode, w.Body.String())
		}
		if !summary.StatsSuppressed() {
			t.Fatal("precommit lifecycle cancellation was not stats-suppressed")
		}
		if cancelNearTimeout && time.Since(start) >= 200*time.Millisecond {
			t.Fatalf("total elapsed = %v, cancellation did not wake precommit await", time.Since(start))
		}
	}

	t.Run("cancellation just before peek timeout", func(t *testing.T) {
		run(t, "", true)
	})
	t.Run("initial event then blocked read", func(t *testing.T) {
		run(t, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-initial-only\"}}\n\n", false)
	})
}

func TestPeekAndForwardResponses_PrecommitAwaitTerminationOverHTTP(t *testing.T) {
	type handlerResult struct {
		status  int
		summary *RequestSummary
	}
	type requestControl struct {
		body          *precommitBlockingReadCloser
		cancel        context.CancelFunc
		cancelInbound context.CancelFunc
	}

	run := func(t *testing.T, mode string) (*http.Response, error, handlerResult, statsSnapshot, string) {
		t.Helper()

		var logs bytes.Buffer
		h := &ProxyHandler{
			log:   logger.NewWithWriter(logger.LevelDebug, &logs),
			stats: newStatsCollector(),
		}
		controlCh := make(chan requestControl, 1)
		resultCh := make(chan handlerResult, 1)
		downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			timeout := time.Hour
			if mode == "deadline" {
				timeout = 50 * time.Millisecond
			}
			upstreamCtx, upstreamCancel := h.newLifecycleUpstreamContext(timeout)
			body := newPrecommitBlockingReadCloser()
			inboundCtx, cancelInbound := context.WithCancel(r.Context())
			defer cancelInbound()
			controlCh <- requestControl{body: body, cancel: upstreamCancel, cancelInbound: cancelInbound}

			ctx, summary := WithRequestSummary(inboundCtx)
			summary.setRoute("responses", "gpt-5.4", true)
			summary.setProvider("copilot", "copilot")
			r = r.WithContext(ctx)
			tracked := &precommitStatusResponseWriter{ResponseWriter: w}
			lifecycleBody := newLifecycleAwareReadCloser(body, upstreamCtx)
			peekAndForwardResponsesWithConfig(
				h,
				tracked,
				r,
				&http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       lifecycleBody,
				},
				upstreamCtx,
				upstreamCancel,
				"gpt-5.4",
				time.Second,
				responsesPrecommitMaxPeekBytes,
				"",
				h.lifecycleStreamHooks(ctx, lifecycleBody.canceledAtFailure, func() {
					h.WriteShutdownServiceUnavailable(tracked, r)
				}),
			)

			status := tracked.status
			if status == 0 {
				status = http.StatusOK
			}
			statsStatus := status
			if failure := summary.FailureStatus(); failure != 0 {
				statsStatus = failure
			}
			h.RecordRequest(summary, statsStatus, "responses-precommit-test", time.Millisecond)
			resultCh <- handlerResult{status: status, summary: summary}
		}))
		t.Cleanup(downstream.Close)

		requestCtx, cancelRequest := context.WithCancel(context.Background())
		t.Cleanup(cancelRequest)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, downstream.URL, strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		type clientResult struct {
			resp *http.Response
			err  error
		}
		clientDone := make(chan clientResult, 1)
		go func() {
			resp, err := downstream.Client().Do(req)
			clientDone <- clientResult{resp: resp, err: err}
		}()

		var control requestControl
		select {
		case control = <-controlCh:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for precommit request setup")
		}
		select {
		case <-control.body.readStarted:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for prepared stream read")
		}

		var client clientResult
		clientFinished := false
		switch mode {
		case "deadline":
			// The lifecycle-rooted upstream timeout fires on its own.
		case "upstream_cancel":
			control.cancel()
		case "shutdown":
			h.BeginShutdown()
		case "client_cancel":
			// First wait for the actual HTTP client to observe its cancellation,
			// then release the server-side derivative. Returning the handler before
			// Client.Do has processed cancellation can race net/http's implicit empty
			// 200 response and make this transport assertion flaky.
			cancelRequest()
			select {
			case client = <-clientDone:
				clientFinished = true
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for HTTP client cancellation")
			}
			control.cancelInbound()
		default:
			t.Fatalf("unknown mode %q", mode)
		}

		if !clientFinished {
			select {
			case client = <-clientDone:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for HTTP client result")
			}
		}
		var result handlerResult
		select {
		case result = <-resultCh:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for HTTP handler result")
		}
		return client.resp, client.err, result, h.stats.snapshot(), logs.String()
	}

	t.Run("upstream deadline", func(t *testing.T) {
		resp, err, result, stats, logs := run(t, "deadline")
		if err != nil {
			t.Fatalf("HTTP request error = %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusGatewayTimeout || result.status != http.StatusGatewayTimeout {
			t.Fatalf("status = client:%d handler:%d, want 504; body=%s", resp.StatusCode, result.status, body)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") || !bytes.Contains(body, []byte(`"type":"server_error"`)) {
			t.Fatalf("gateway-timeout response is not an OpenAI JSON error: headers=%v body=%s", resp.Header, body)
		}
		if result.summary.StatsSuppressed() || result.summary.FailureStatus() != http.StatusGatewayTimeout {
			t.Fatalf("deadline accounting = suppressed:%v failure:%d, want false/504", result.summary.StatsSuppressed(), result.summary.FailureStatus())
		}
		if stats.Totals.Requests != 1 || stats.Totals.Errors != 1 || !statsHasStatusCode(stats, http.StatusGatewayTimeout, 1) {
			t.Fatalf("deadline stats = requests:%d errors:%d status_codes:%v, want 1/1 with 504", stats.Totals.Requests, stats.Totals.Errors, stats.StatusCodes)
		}
		if !strings.Contains(logs, `"msg":"upstream request failed"`) || !strings.Contains(logs, `"status":504`) || !strings.Contains(logs, `"endpoint":"responses_precommit"`) {
			t.Fatalf("deadline failure log missing status/endpoint: %s", logs)
		}
	})

	t.Run("ordinary upstream cancellation", func(t *testing.T) {
		resp, err, result, stats, logs := run(t, "upstream_cancel")
		if err != nil {
			t.Fatalf("HTTP request error = %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || result.status != http.StatusBadGateway {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = client:%d handler:%d, want 502; body=%s", resp.StatusCode, result.status, body)
		}
		if result.summary.StatsSuppressed() || result.summary.FailureStatus() != http.StatusBadGateway {
			t.Fatalf("upstream-cancel accounting = suppressed:%v failure:%d, want false/502", result.summary.StatsSuppressed(), result.summary.FailureStatus())
		}
		if stats.Totals.Requests != 1 || stats.Totals.Errors != 1 || !statsHasStatusCode(stats, http.StatusBadGateway, 1) {
			t.Fatalf("upstream-cancel stats = requests:%d errors:%d status_codes:%v, want 1/1 with 502", stats.Totals.Requests, stats.Totals.Errors, stats.StatusCodes)
		}
		if !strings.Contains(logs, `"msg":"upstream request failed"`) || !strings.Contains(logs, `"status":502`) {
			t.Fatalf("upstream-cancel failure log missing 502: %s", logs)
		}
	})

	t.Run("lifecycle shutdown", func(t *testing.T) {
		resp, err, result, stats, logs := run(t, "shutdown")
		if err != nil {
			t.Fatalf("HTTP request error = %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || result.status != http.StatusServiceUnavailable {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = client:%d handler:%d, want 503; body=%s", resp.StatusCode, result.status, body)
		}
		if !result.summary.StatsSuppressed() || result.summary.FailureStatus() != 0 {
			t.Fatalf("shutdown accounting = suppressed:%v failure:%d, want true/0", result.summary.StatsSuppressed(), result.summary.FailureStatus())
		}
		if stats.Totals.Requests != 0 || stats.Totals.Errors != 0 {
			t.Fatalf("shutdown stats = requests:%d errors:%d, want 0/0", stats.Totals.Requests, stats.Totals.Errors)
		}
		if strings.Contains(logs, `"msg":"upstream request failed"`) {
			t.Fatalf("shutdown emitted provider failure log: %s", logs)
		}
	})

	t.Run("inbound client cancellation", func(t *testing.T) {
		resp, err, result, stats, logs := run(t, "client_cancel")
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatalf("client cancellation unexpectedly received HTTP %d", resp.StatusCode)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("HTTP request error = %v, want context.Canceled", err)
		}
		if result.summary.StatsSuppressed() || result.summary.FailureStatus() != 0 {
			t.Fatalf("client-cancel accounting = suppressed:%v failure:%d, want false/0", result.summary.StatsSuppressed(), result.summary.FailureStatus())
		}
		if stats.Totals.Errors != 0 {
			t.Fatalf("client cancellation provider errors = %d, want 0", stats.Totals.Errors)
		}
		if strings.Contains(logs, `"msg":"upstream request failed"`) {
			t.Fatalf("client cancellation emitted provider failure log: %s", logs)
		}
	})
}

type precommitStatusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *precommitStatusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *precommitStatusResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *precommitStatusResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

type precommitBlockingReadCloser struct {
	readStarted chan struct{}
	closed      chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

func newPrecommitBlockingReadCloser() *precommitBlockingReadCloser {
	return &precommitBlockingReadCloser{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *precommitBlockingReadCloser) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.readStarted) })
	<-b.closed
	return 0, context.Canceled
}

func (b *precommitBlockingReadCloser) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func statsHasStatusCode(stats statsSnapshot, status int, count int64) bool {
	want := strconv.Itoa(status)
	for _, row := range stats.StatusCodes {
		if row.Label == want && row.Count == count {
			return true
		}
	}
	return false
}

type lifecyclePeekBlockBody struct {
	ctx       context.Context
	prefix    *strings.Reader
	blocked   chan struct{}
	blockOnce sync.Once
	onBlocked func()
}

func newLifecyclePeekBlockBody(ctx context.Context, prefix string) *lifecyclePeekBlockBody {
	return &lifecyclePeekBlockBody{ctx: ctx, prefix: strings.NewReader(prefix), blocked: make(chan struct{})}
}

func (b *lifecyclePeekBlockBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.blockOnce.Do(func() { close(b.blocked) })
	if b.onBlocked != nil {
		b.onBlocked()
	}
	<-b.ctx.Done()
	return 0, context.Canceled
}

func (b *lifecyclePeekBlockBody) Close() error { return nil }

func TestResponsesPeekPumpObservesTerminalAfterCommittedWriterBlocks(t *testing.T) {
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-observe-only\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1,\"total_tokens\":5}}}\n\n"
	body := newGatedResponsesFailureBody(completed, nil)
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	peekDone := make(chan peekResult, 1)
	peekState := newResponsesPeekState()
	commitCh := make(chan struct{})
	abortCh := make(chan struct{})
	observeOnlyCh := make(chan struct{})
	go runResponsesPeekPump(body, pw, http.Header{}, peekDone, peekState, commitCh, abortCh, observeOnlyCh, responsesPrecommitMaxPeekBytes)

	select {
	case result := <-peekDone:
		if result.terminal != nil {
			t.Fatalf("initial peek unexpectedly terminal: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial response.created peek")
	}
	close(commitCh)
	close(observeOnlyCh)
	body.releaseSecond()

	select {
	case <-peekState.terminalDone:
		terminal, ok := peekState.terminalResult()
		if !ok || terminal.terminal == nil || terminal.terminal.Type != "response.completed" {
			t.Fatalf("terminal result = %+v, want response.completed", terminal)
		}
		if got := terminal.terminal.Response.Usage.TotalTokens; got != 5 {
			t.Fatalf("terminal usage total = %d, want 5", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal was not observed after committed pipe writer blocked")
	}
	close(abortCh)
}

func TestResponsesTerminalObserverHandlesBoundedCompletedAndRejectsOversizedFragments(t *testing.T) {
	for _, tt := range []struct {
		name         string
		outputSize   int
		total        int
		wantTerminal bool
	}{
		{name: "large valid terminal", outputSize: 2 << 20, total: 17, wantTerminal: true},
		{name: "oversized fragment reconstruction is not authoritative", outputSize: openAIStreamScannerMaxBuffer + 4096, total: 23},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := newResponsesPeekState()
			observer := newResponsesTerminalObserver(nil, state)
			payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-large","output":"%s","usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d}}}`, strings.Repeat("x", tt.outputSize), tt.total-1, tt.total)
			stream := []byte("event: response.completed\ndata: " + payload + "\n\n")
			for len(stream) > 0 {
				n := min(len(stream), responsesPeekReadChunkSize)
				observer.Write(stream[:n])
				stream = stream[n:]
			}

			terminal, ok := state.terminalResult()
			if !tt.wantTerminal {
				if ok {
					t.Fatalf("terminal = %+v, want no authoritative oversized fragment result", terminal)
				}
				return
			}
			if !ok || terminal.terminal == nil || terminal.terminal.Type != "response.completed" {
				t.Fatalf("terminal = %+v, want response.completed", terminal)
			}
			if got := terminal.terminal.Response.ID; got != "resp-large" {
				t.Fatalf("terminal response id = %q, want resp-large", got)
			}
			if got := terminal.terminal.Response.Usage.TotalTokens; got != tt.total {
				t.Fatalf("terminal total tokens = %d, want %d", got, tt.total)
			}
		})
	}
}

func TestResponsesTerminalObserverFinalizesTerminalAtEOFWithoutDelimiter(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	observer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-eof\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}"))
	observer.FinalizeEOF()

	terminal, ok := state.terminalResult()
	if !ok || terminal.terminal == nil || terminal.terminal.Type != "response.completed" {
		t.Fatalf("terminal = %+v, want response.completed", terminal)
	}
	if got := terminal.terminal.Response.Usage.TotalTokens; got != 3 {
		t.Fatalf("terminal total tokens = %d, want 3", got)
	}
}

func TestResponsesTerminalObserverFirstTerminalWinsAndInfersEventType(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	observer.Write([]byte("event: response.completed\ndata: {\"response\":{\"id\":\"resp-first\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"))
	observer.Write([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"too_many_requests\",\"message\":\"late\"}}}\n\n"))

	terminal, ok := state.terminalResult()
	if !ok || terminal.terminal == nil {
		t.Fatal("terminal result missing")
	}
	if terminal.terminal.Type != "response.completed" {
		t.Fatalf("terminal type = %q, want inferred response.completed", terminal.terminal.Type)
	}
	if terminal.decision != responsesPeekDecisionPassthrough || terminal.terminal.Response.Usage.TotalTokens != 4 {
		t.Fatalf("terminal result = %+v, want first completed outcome", terminal)
	}
}

func TestPrepareResponsesStreamPreservesTerminalWhenInboundCancellationWins(t *testing.T) {
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-cancel-race\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	for i := 0; i < 20; i++ {
		waitCtx, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		resp, result, _, err := prepareResponsesStreamAttemptWithGrace(waitCtx, context.Background(), 100*time.Millisecond, func() (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(stream)),
			}, nil
		})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil || result == nil || result.terminal == nil || result.terminal.Type != "response.completed" {
			t.Fatalf("iteration %d result = resp:%v result:%+v err:%v, want completed terminal", i, resp != nil, result, err)
		}
	}
}

func TestResponsesTerminalObserverDoesNotClassifyOversizedFailureFragments(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	payload := fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp-large-failure","output":"%s","error":{"type":"server_error","code":"too_many_requests","message":"slow down"},"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}}`, strings.Repeat("x", openAIStreamScannerMaxBuffer+4096))
	stream := []byte("event: response.failed\r\ndata: " + payload + "\r\n\r\n")
	for len(stream) > 0 {
		n := min(len(stream), responsesPeekReadChunkSize)
		observer.Write(stream[:n])
		stream = stream[n:]
	}
	if terminal, ok := state.terminalResult(); ok {
		t.Fatalf("terminal = %+v, want no authoritative oversized fragment result", terminal)
	}
}

func TestPrepareResponsesStreamPropagatesSalvagedTerminalWithoutResponse(t *testing.T) {
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-wrapper-cancel\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	sawNilResponse := false
	for i := 0; i < 20; i++ {
		waitCtx, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		h := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
		resp, result, _, err := h.prepareResponsesStreamWithGrace(waitCtx, context.Background(), "gpt-5.4", 100*time.Millisecond, func() (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(stream)),
			}, nil
		})
		if resp == nil {
			sawNilResponse = true
		} else if resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil || result == nil || result.terminal == nil || result.terminal.Type != "response.completed" {
			t.Fatalf("iteration %d result = resp:%v result:%+v err:%v, want propagated completed terminal", i, resp != nil, result, err)
		}
	}
	if !sawNilResponse {
		t.Fatal("test never exercised the salvaged-terminal path with a nil response")
	}
}

type delayedResponsesOutcomeReadCloser struct {
	steps []responsesOutcomeReadStep
	index int
	delay time.Duration
}

type responsesOutcomeReadStep struct {
	data string
	err  error
}

func (r *delayedResponsesOutcomeReadCloser) Read(p []byte) (int, error) {
	if r.index >= len(r.steps) {
		return 0, io.EOF
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	step := r.steps[r.index]
	r.index++
	return copy(p, step.data), step.err
}

func (r *delayedResponsesOutcomeReadCloser) Close() error { return nil }

func TestPrepareResponsesStreamAttemptPreservesIndependentReadOutcomeOverCancellation(t *testing.T) {
	resetErr := errors.New("upstream reset")
	for _, tt := range []struct {
		name        string
		steps       []responsesOutcomeReadStep
		wantBody    string
		wantReadErr error
	}{
		{name: "EOF without bytes", steps: []responsesOutcomeReadStep{{err: io.EOF}}},
		{name: "EOF with same-read bytes", steps: []responsesOutcomeReadStep{{data: ": keepalive\n\n", err: io.EOF}}, wantBody: ": keepalive\n\n"},
		{name: "reset without bytes", steps: []responsesOutcomeReadStep{{err: resetErr}}, wantReadErr: resetErr},
		{name: "reset after separate bytes", steps: []responsesOutcomeReadStep{{data: ": keepalive\n\n"}, {err: resetErr}}, wantBody: ": keepalive\n\n", wantReadErr: resetErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			waitCtx, cancelWait := context.WithCancel(context.Background())
			cancelWait()
			resp, result, _, err := prepareResponsesStreamAttempt(waitCtx, context.Background(), func() (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: &delayedResponsesOutcomeReadCloser{
						steps: tt.steps,
						delay: time.Millisecond,
					},
				}, nil
			})
			if err != nil {
				t.Fatalf("prepareResponsesStreamAttempt() error = %v, want preserved response", err)
			}
			if result != nil {
				t.Fatalf("result = %+v, want no terminal result", result)
			}
			if resp == nil || resp.Body == nil {
				t.Fatal("response body missing after independent read outcome")
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != tt.wantBody {
				t.Fatalf("body = %q, want %q", body, tt.wantBody)
			}
			if !errors.Is(readErr, tt.wantReadErr) {
				t.Fatalf("read error = %v, want %v", readErr, tt.wantReadErr)
			}
		})
	}
}

func TestResponsesTerminalObserverDoesNotFinalizeTruncatedOversizedTerminalAtEOF(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-truncated-large","output":"%s","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`, strings.Repeat("x", openAIStreamScannerMaxBuffer+4096))
	observer.Write([]byte("event: response.completed\ndata: " + payload + "\n"))
	observer.FinalizeEOF()

	if terminal, ok := state.terminalResult(); ok {
		t.Fatalf("terminal = %+v, want no authoritative terminal without explicit SSE boundary", terminal)
	}
	if usage, ok := state.recoveredUsageResult(); !ok || usage.TotalTokens != 9 {
		t.Fatalf("recovered usage = %+v ok=%v, want terminal usage 9 at EOF", usage, ok)
	}
	if len(observer.line) > openAIStreamScannerMaxBuffer || len(observer.data) > openAIStreamScannerMaxBuffer || len(observer.usageTail) > responsesFailureTapOverflowTail {
		t.Fatalf("observer retained unbounded state: line=%d data=%d tail=%d", len(observer.line), len(observer.data), len(observer.usageTail))
	}
}

func TestResponsesTerminalObserverDoesNotClassifyOversizedDataOnlyFragments(t *testing.T) {
	for _, typeName := range []string{"response.completed", "response.failed", "response.incomplete"} {
		t.Run(typeName, func(t *testing.T) {
			state := newResponsesPeekState()
			observer := newResponsesTerminalObserver(nil, state)
			payload := fmt.Sprintf(`{"type":%q,"response":{"id":"resp-data-only","output":"%s","usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`, typeName, strings.Repeat("x", openAIStreamScannerMaxBuffer+4096))
			stream := []byte("data: " + payload + "\r\n\r\n")
			for len(stream) > 0 {
				n := min(len(stream), responsesPeekReadChunkSize)
				observer.Write(stream[:n])
				stream = stream[n:]
			}
			if terminal, ok := state.terminalResult(); ok {
				t.Fatalf("terminal = %+v, want no authoritative oversized fragment result", terminal)
			}
		})
	}
}

func TestResponsesSSEBoundaryEndAcrossAllChunkSplits(t *testing.T) {
	for _, boundary := range []string{"\n\n", "\n\r\n", "\r\n\n", "\r\n\r\n"} {
		for split := 0; split <= len(boundary); split++ {
			t.Run(fmt.Sprintf("%q/split-%d", boundary, split), func(t *testing.T) {
				tail := []byte("payload")
				seen := false
				for _, chunk := range [][]byte{[]byte(boundary[:split]), []byte(boundary[split:])} {
					if len(chunk) == 0 {
						continue
					}
					combined := append(append([]byte(nil), trailingBytes(tail, 3)...), chunk...)
					if responsesSSEBoundaryEnd(combined) > 0 {
						seen = true
						break
					}
					tail = trailingBytes(combined, 3)
				}
				if !seen {
					t.Fatalf("boundary %q was not detected across split %d", boundary, split)
				}
			})
		}
	}
}

func TestExtractResponsesTerminalTypeIgnoresEscapedNestedText(t *testing.T) {
	buf := []byte(`data: {"type":"response.output_item.done","item":{"text":"quoted \\"type\\":\\"response.failed\\" text"}}`)
	if got := extractResponsesTerminalType(buf); got != "" {
		t.Fatalf("terminal type = %q, want none from escaped output text", got)
	}
	buf = []byte("data: {\"response\":{}, \"type\" : \"response.incomplete\"}")
	if got := extractResponsesTerminalType(buf); got != "response.incomplete" {
		t.Fatalf("terminal type = %q, want response.incomplete", got)
	}
}

func TestStreamResponsesPipeHandlesOversizedTerminalFraming(t *testing.T) {
	payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-http-truncated","output":"%s","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`, strings.Repeat("x", responsesFailureTapMaxBuffer+4096))
	for _, tt := range []struct {
		name        string
		delimiter   string
		wantFailure int
	}{
		{name: "EOF without boundary", delimiter: "\n", wantFailure: http.StatusBadGateway},
		{name: "explicit SSE boundary", delimiter: "\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire := "data: " + payload + tt.delimiter
			ctx, summary := WithRequestSummary(context.Background())
			recorder := httptest.NewRecorder()
			streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}, recorder, strings.NewReader(wire), nil, nil, "")

			if got := summary.FailureStatus(); got != tt.wantFailure {
				t.Fatalf("FailureStatus = %d, want %d", got, tt.wantFailure)
			}
			usage := readSummaryForStats(summary)
			if usage.prompt != 7 || usage.completion != 2 || usage.total != 9 {
				t.Fatalf("usage = prompt:%d completion:%d total:%d, want retained 7/2/9", usage.prompt, usage.completion, usage.total)
			}
			if got := recorder.Body.String(); got != wire {
				t.Fatalf("forwarded body changed: got %d bytes, want %d", len(got), len(wire))
			}
		})
	}
}

func TestResponsesPreparedBodyTerminalResultReturnsWhenReadOutcomeEnds(t *testing.T) {
	state := newResponsesPeekState()
	state.publishOutcome(responsesPeekReadOutcome{err: io.EOF})
	observed := false
	body := &responsesPreparedBody{
		peekState: state,
		observeOnlyFn: func() {
			observed = true
		},
	}
	start := time.Now()
	if terminal, ok := body.terminalResultWithin(500 * time.Millisecond); ok {
		t.Fatalf("unexpected terminal result: %+v", terminal)
	}
	if !observed {
		t.Fatal("terminal observation did not switch the reader to observe-only mode")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("known EOF waited for terminal grace: %v", elapsed)
	}
}

func TestPrepareResponsesStreamAttemptCommitsUpstreamCanceledTerminal(t *testing.T) {

	streamCtx, cancelStream := context.WithCancel(context.Background())
	cancelStream()
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-upstream-cancel\",\"output\":[{\"type\":\"message\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	resp, result, _, err := prepareResponsesStreamAttemptWithGrace(context.Background(), streamCtx, 100*time.Millisecond, func() (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &delayedResponsesOutcomeReadCloser{
				steps: []responsesOutcomeReadStep{{data: stream, err: io.EOF}},
				delay: 50 * time.Millisecond,
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("prepareResponsesStreamAttempt error = %v", err)
	}
	if resp == nil || resp.Body == nil {
		t.Fatal("upstream-canceled terminal did not retain the prepared response")
	}
	defer func() { _ = resp.Body.Close() }()
	if result == nil || result.terminal == nil || result.terminal.Type != "response.completed" {
		t.Fatalf("terminal result = %+v, want response.completed", result)
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read committed terminal body: %v", readErr)
	}
	if string(body) != stream {
		t.Fatalf("committed terminal body changed: got %q want %q", body, stream)
	}
}

func TestResponsesFailureTapResumesAfterOversizedNonterminalEvent(t *testing.T) {
	oversized := fmt.Sprintf("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"text\":\"%s\"}}\n\n", strings.Repeat("x", responsesFailureTapMaxBuffer+4096))
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-after-oversize\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	wire := oversized + completed
	ctx, summary := WithRequestSummary(context.Background())
	recorder := httptest.NewRecorder()
	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}, recorder, strings.NewReader(wire), nil, nil, "")

	if got := summary.FailureStatus(); got != 0 {
		t.Fatalf("FailureStatus = %d, want successful terminal after oversized nonterminal", got)
	}
	usage := readSummaryForStats(summary)
	if usage.total != 3 {
		t.Fatalf("total usage = %d, want 3 from final completed event", usage.total)
	}
	if recorder.Body.String() != wire {
		t.Fatal("forwarded stream changed")
	}
}

func TestResponsesFailureTapParsesNearLimitTerminalBeforeFollowingEvent(t *testing.T) {
	prefix := `event: response.completed
data: {"type":"response.completed","response":{"id":"resp-near-limit","output":"`
	suffix := `","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}

`
	outputBytes := responsesFailureTapMaxBuffer - len(prefix) - len(suffix) - 1
	if outputBytes <= 0 {
		t.Fatal("invalid near-limit test sizing")
	}
	completed := prefix + strings.Repeat("x", outputBytes) + suffix
	if len(completed) >= responsesFailureTapMaxBuffer {
		t.Fatalf("completed event bytes = %d, want below %d", len(completed), responsesFailureTapMaxBuffer)
	}
	wire := completed + "data: [DONE]\n\n"
	if len(wire) <= responsesFailureTapMaxBuffer {
		t.Fatalf("combined wire bytes = %d, want above %d", len(wire), responsesFailureTapMaxBuffer)
	}

	ctx, summary := WithRequestSummary(context.Background())
	recorder := httptest.NewRecorder()
	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}, recorder, strings.NewReader(wire), nil, nil, "")
	if got := summary.FailureStatus(); got != 0 {
		t.Fatalf("FailureStatus = %d, want near-limit completed event to remain successful", got)
	}
	usage := readSummaryForStats(summary)
	if usage.prompt != 5 || usage.completion != 2 || usage.total != 7 {
		t.Fatalf("usage = prompt:%d completion:%d total:%d, want 5/2/7", usage.prompt, usage.completion, usage.total)
	}
	if recorder.Body.String() != wire {
		t.Fatal("forwarded near-limit stream changed")
	}
}

func TestResponsesTerminalObserverIgnoresOversizedNonterminalUsage(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	payload := fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","output":"%s","usage":{"input_tokens":900,"output_tokens":80,"total_tokens":980}}}`, strings.Repeat("x", openAIStreamScannerMaxBuffer+4096))
	stream := []byte("event: response.output_item.done\ndata: " + payload + "\n\n")
	for len(stream) > 0 {
		n := min(len(stream), responsesPeekReadChunkSize)
		observer.Write(stream[:n])
		stream = stream[n:]
	}
	if terminal, ok := state.terminalResult(); ok {
		t.Fatalf("terminal = %+v, want no terminal from output_item.done", terminal)
	}
	if usage, ok := state.recoveredUsageResult(); ok {
		t.Fatalf("recovered usage = %+v, want none from nonterminal event", usage)
	}
}

func TestResponsesTerminalObserverDoesNotRecoverUsageFromNonterminalOverflow(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	payload := fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","content":"%s","usage":{"input_tokens":99,"output_tokens":1,"total_tokens":100}}}`, strings.Repeat("x", openAIStreamScannerMaxBuffer+4096))
	stream := []byte("event: response.output_item.done\ndata: " + payload + "\n\n")
	for len(stream) > 0 {
		n := min(len(stream), responsesPeekReadChunkSize)
		observer.Write(stream[:n])
		stream = stream[n:]
	}
	if usage, ok := state.recoveredUsageResult(); ok {
		t.Fatalf("nonterminal overflow recovered usage %+v", usage)
	}
	if terminal, ok := state.terminalResult(); ok {
		t.Fatalf("nonterminal overflow published terminal %+v", terminal)
	}
}

func TestResponsesPreparedStreamCanDisableTerminalObservation(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\"}}\n\n")),
	}
	prepared := newResponsesPreparedStream(resp, responsesPrecommitMaxPeekBytes, false)
	if prepared.peekState.observeTerminal {
		t.Fatal("HTTP prepared stream unexpectedly enabled terminal observation")
	}
	result, _, _, err := prepared.await(context.Background(), context.Background(), time.Second)
	if err != nil {
		t.Fatalf("prepared await error: %v", err)
	}
	if result.terminal == nil || result.terminal.Type != "response.completed" {
		t.Fatalf("precommit parser result = %+v, want completed decision", result)
	}
	committed := prepared.commitResponse()
	_, _ = io.Copy(io.Discard, committed.Body)
	_ = committed.Body.Close()
}

func TestResponsesTerminalObserverStopsAtDoneMarker(t *testing.T) {
	state := newResponsesPeekState()
	observer := newResponsesTerminalObserver(nil, state)
	observer.Write([]byte("data: [DONE]\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-after-done\",\"usage\":{\"input_tokens\":9,\"output_tokens\":1,\"total_tokens\":10}}}\n\n"))
	if terminal, ok := state.terminalResult(); ok {
		t.Fatalf("terminal = %+v, want [DONE] to stop observation", terminal)
	}
	if usage, ok := state.recoveredUsageResult(); ok {
		t.Fatalf("recovered usage = %+v, want none after [DONE]", usage)
	}
}
