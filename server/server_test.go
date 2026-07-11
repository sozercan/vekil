package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

func TestStart_ReturnsErrorWhenPortInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("failed to close listener: %v", err)
		}
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}

	srv, err := New(auth.NewTestAuthenticator("test-token"), logger.New(logger.ParseLevel("error")), "127.0.0.1", strconv.Itoa(addr.Port))
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	err = srv.Start()
	if err == nil {
		t.Fatal("expected Start to fail when port is already in use")
	}
	if srv.IsRunning() {
		t.Fatal("expected server to remain stopped after listen failure")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("expected address-in-use error, got %v", err)
	}
}

func TestServerBlocksNonHealthRoutesWhileStartupAuthenticationPending(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	srv.SetStartupAuthenticationPending(true)

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/healthz", want: http.StatusOK},
		{method: http.MethodGet, path: "/readyz", want: http.StatusServiceUnavailable},
		{method: http.MethodGet, path: "/v1/models", want: http.StatusServiceUnavailable},
		{method: http.MethodPost, path: "/v1/chat/completions", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"gpt-5.4"}`))
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.path != "/healthz" {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), "startup authentication pending") {
					t.Fatalf("response missing startup auth pending message: %s", body)
				}
			}
		})
	}
}

func TestServerBlocksNonHealthRoutesWhileProviderValidationPending(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithProxyOptions(
			proxy.WithDeferredDynamicProviderModelValidation(true),
			proxy.WithProvidersConfig(proxy.ProvidersConfig{
				Providers: []proxy.ProviderConfig{
					{ID: "copilot", Type: "copilot", Default: true},
					{
						ID:       "local",
						Type:     "openai-compatible",
						BaseURL:  upstream.URL,
						AuthType: "none",
						Models: []proxy.ProviderModelConfig{{
							PublicID: "local-model",
						}},
					},
				},
			}),
		),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/healthz", want: http.StatusOK},
		{method: http.MethodGet, path: "/readyz", want: http.StatusServiceUnavailable},
		{method: http.MethodGet, path: "/v1/models", want: http.StatusServiceUnavailable},
		{method: http.MethodPost, path: "/v1/chat/completions", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"gpt-5.4"}`))
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.path != "/healthz" {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), "provider model validation pending") {
					t.Fatalf("response missing pending validation message: %s", body)
				}
			}
		})
	}
}

func TestNew_ConfiguresExtendedWriteTimeout(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	if got, want := srv.httpServer.WriteTimeout, 65*time.Minute; got != want {
		t.Fatalf("WriteTimeout = %v, want %v", got, want)
	}
}

func TestNew_DerivesWriteTimeoutFromConfiguredProxyHandler(t *testing.T) {
	const customTimeout = 17 * time.Minute

	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "server wrapper",
			opts: []Option{WithStreamingUpstreamTimeout(customTimeout)},
		},
		{
			name: "proxy option",
			opts: []Option{WithProxyOptions(proxy.WithStreamingUpstreamTimeout(customTimeout))},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(
				auth.NewTestAuthenticator("test-token"),
				logger.New(logger.ParseLevel("error")),
				"127.0.0.1",
				"0",
				tc.opts...,
			)
			if err != nil {
				t.Fatalf("failed to initialize server: %v", err)
			}

			if got, want := srv.httpServer.WriteTimeout, customTimeout+5*time.Minute; got != want {
				t.Fatalf("WriteTimeout = %v, want %v", got, want)
			}
		})
	}
}

func TestRequestLogIncludesSummaryUsageAndUpstreamRequestID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-upstream-123")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), logs.String())
	}
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	want := map[string]interface{}{
		"msg":                 "request completed",
		"method":              "POST",
		"path":                "/v1/chat/completions",
		"endpoint":            "openai_chat",
		"model":               "gpt-5",
		"provider":            "copilot",
		"provider_kind":       "copilot",
		"stream":              false,
		"upstream_request_id": "req-upstream-123",
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("log[%s] = %#v, want %#v in %#v", key, got, expected, entry)
		}
	}
	for key, expected := range map[string]float64{"status": 200, "prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15} {
		if got, ok := entry[key].(float64); !ok || got != expected {
			t.Fatalf("log[%s] = %#v, want %v in %#v", key, entry[key], expected, entry)
		}
	}
}

// TestStreamingChatCompletionsPassthroughThroughServer drives a streaming
// POST /v1/chat/completions request through the full server stack (so
// withRequestLog attaches a RequestSummary and the usage callback is non-nil)
// with tool optimizers disabled. This exercises StreamOpenAIPassthroughWithFinalResponse
// on its default-config path, where onFinalResponse is nil but onUsage is not.
// Before the nil-aggregator guard this panicked on the first SSE chunk; this
// test also asserts the streamed body is forwarded verbatim with a [DONE]
// sentinel and that streaming usage is recorded in the request log.
func TestStreamingChatCompletionsPassthroughThroughServer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-stream-456")
		flusher, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeChunk("data: {\"id\":\"chatcmpl-2\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		writeChunk("data: {\"id\":\"chatcmpl-2\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n")
		writeChunk("data: {\"id\":\"chatcmpl-2\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n")
		writeChunk("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(got))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	streamed, _ := io.ReadAll(resp.Body)
	got := string(streamed)
	// The passthrough must forward every upstream chunk verbatim, including
	// the terminal [DONE] sentinel, without truncation or panic.
	for _, want := range []string{"\"content\":\"Hel\"", "\"content\":\"lo\"", "\"finish_reason\":\"stop\"", "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("streamed body missing %q; got:\n%s", want, got)
		}
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), logs.String())
	}
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if got, ok := entry["stream"].(bool); !ok || !got {
		t.Fatalf("log[stream] = %#v, want true", entry["stream"])
	}
	// Usage must be observed via the streaming onUsage callback (regression:
	// the callback was previously wired but never invoked, so these were absent).
	for key, expected := range map[string]float64{"status": 200, "prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9} {
		if got, ok := entry[key].(float64); !ok || got != expected {
			t.Fatalf("log[%s] = %#v, want %v in %#v", key, entry[key], expected, entry)
		}
	}
	if got := entry["upstream_request_id"]; got != "req-stream-456" {
		t.Fatalf("log[upstream_request_id] = %#v, want req-stream-456", got)
	}
}

func TestStopCancelsActiveResponsesWebSocketInference(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
		WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{Enabled: true, DisableAutoCompact: true}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	conn := dialServerResponsesWebSocket(t, testServer)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(serverResponsesWebSocketRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active websocket inference")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop did not cancel active websocket inference")
	}
}

func TestStopCancelsResponsesWebSocketAutoCompaction(t *testing.T) {
	compactionStarted := make(chan struct{})
	compactionCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			startedOnce.Do(func() { close(compactionStarted) })
			<-r.Context().Done()
			canceledOnce.Do(func() { close(compactionCanceled) })
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stop-compact\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-2\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-3\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stop-compact\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
		WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{
			Enabled:             true,
			AutoCompactMaxItems: 2,
			AutoCompactMaxBytes: 1 << 20,
			AutoCompactKeepTail: 1,
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	conn := dialServerResponsesWebSocket(t, testServer)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(serverResponsesWebSocketRequest([]interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "compact"}}},
	})); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	for range 5 {
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set websocket read deadline: %v", err)
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
	}
	select {
	case <-compactionStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket auto-compaction")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-compactionCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop did not cancel websocket auto-compaction")
	}
}

func dialServerResponsesWebSocket(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket %s: %v", url, err)
	}
	return conn
}

func serverResponsesWebSocketRequest(input []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type":                "response.create",
		"model":               "gpt-5.4",
		"instructions":        "You are helpful",
		"input":               input,
		"tools":               []interface{}{},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"store":               false,
		"stream":              true,
		"include":             []string{},
	}
}
