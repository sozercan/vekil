package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestPeekAndForwardResponses_FailsOpenOnMaxPeekBytes(t *testing.T) {
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

	peekAndForwardResponsesWithConfig(h, w, req, resp, func() {}, "gpt-5.4", 50*time.Millisecond, 32)

	result := w.Result()
	if result.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(result.Body)
		t.Fatalf("status = %d, want 200: %s", result.StatusCode, body)
	}
	body, _ := io.ReadAll(result.Body)
	if string(body) != raw {
		t.Fatalf("body = %q, want %q", string(body), raw)
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

	peekAndForwardResponsesWithConfig(h, w, req, resp, func() {}, "gpt-5.4", 20*time.Millisecond, 1024)

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
		peekAndForwardResponsesWithConfig(h, w, req, resp, func() {}, "gpt-5.4", time.Second, 1024)
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
