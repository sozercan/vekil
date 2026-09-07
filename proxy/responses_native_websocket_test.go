package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestResponsesNativeWebSocketIncrementalTransport(t *testing.T) {
	var connections, frames, httpPosts atomic.Int32
	captured := make(chan map[string]json.RawMessage, 4)
	closed := make(chan struct{})
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/responses" {
			httpPosts.Add(1)
			http.Error(w, "unexpected HTTP dispatch", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Initiator") != "agent" {
			t.Error("native handshake did not retain provider authentication and request attribution")
		}
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connections.Add(1)
		defer func() { _ = conn.Close() }()
		defer close(closed)
		for {
			var request map[string]json.RawMessage
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			captured <- request
			id := fmt.Sprintf("resp-native-%d", frames.Add(1))
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed", "response": map[string]any{
					"id": id, "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "done"}}}},
					"usage": map[string]int{"input_tokens": 7, "output_tokens": 2, "total_tokens": 9},
				},
			}); err != nil {
				return
			}
		}
	})
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, AutoCompactMaxItems: 1, AutoCompactMaxBytes: 1}
	h.stats = newStatsCollector()
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]any{map[string]string{"role": "user", "content": "first"}})
	for turn := 1; turn <= 2; turn++ {
		request["headers"] = map[string]string{"X-Initiator": "agent", "X-Interaction-Id": fmt.Sprintf("interaction-%d", turn)}
		if turn > 1 {
			request["previous_response_id"] = "resp-native-1"
			request["input"] = []any{map[string]string{"role": "user", "content": "second"}}
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
			t.Fatalf("turn %d response = %#v", turn, frame)
		}
		upstream := <-captured
		if string(upstream["type"]) != `"response.create"` || string(upstream["model"]) != `"gpt-5.4"` {
			t.Fatalf("native request fields = %#v", upstream)
		}
		if _, ok := upstream["stream"]; ok {
			t.Fatal("native envelope contains HTTP streaming option")
		}
		var input []json.RawMessage
		_ = json.Unmarshal(upstream["input"], &input)
		if len(input) != 1 {
			t.Fatalf("turn %d uploaded history: %s", turn, upstream["input"])
		}
		var headers map[string]string
		_ = json.Unmarshal(upstream["headers"], &headers)
		if headers["X-Interaction-Id"] != fmt.Sprintf("interaction-%d", turn) || headers["Authorization"] != "" {
			t.Fatalf("native per-turn headers = %+v", headers)
		}
		if turn == 2 && string(upstream["previous_response_id"]) != `"resp-native-1"` {
			t.Fatalf("native continuation = %s", upstream["previous_response_id"])
		}
	}
	// Local staging must preserve the actual upstream parent without uploading
	// completed history or sending a generate:false frame to the provider.
	request["generate"] = false
	request["previous_response_id"] = "resp-native-2"
	request["input"] = "staged"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	warmup := mustReadWebSocketJSONSkipMetadata(t, conn)
	request["previous_response_id"] = websocketResponseID(t, warmup)
	request["generate"] = true
	request["input"] = "resume"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
		t.Fatalf("staged continuation = %#v", frame)
	}
	staged := <-captured
	var stagedInput []json.RawMessage
	_ = json.Unmarshal(staged["input"], &stagedInput)
	if len(stagedInput) != 2 || string(staged["previous_response_id"]) != `"resp-native-2"` {
		t.Fatalf("staged native request = %#v", staged)
	}
	if connections.Load() != 1 || frames.Load() != 3 || httpPosts.Load() != 0 {
		t.Fatalf("transport counts = connections:%d frames:%d HTTP:%d", connections.Load(), frames.Load(), httpPosts.Load())
	}
	stats := h.stats.snapshot()
	if stats.Totals.Requests != 3 || stats.Totals.PromptTokens != 21 || stats.Totals.TotalTokens != 27 {
		t.Fatalf("native turn accounting = %+v", stats.Totals)
	}
	_ = conn.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("client close did not close upstream websocket")
	}
}

func TestResponsesNativeWebSocketFailureDoesNotReconnect(t *testing.T) {
	var connections, frames atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connections.Add(1)
		defer func() { _ = conn.Close() }()
		for {
			var request map[string]any
			if conn.ReadJSON(&request) != nil {
				return
			}
			if frames.Add(1) == 2 {
				return // Ambiguous delivery after the next create was received.
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-native-first","output":[]}}`))
		}
	})
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest(nil)
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	request["previous_response_id"] = "resp-native-first"
	for _, status := range []int{http.StatusBadGateway, http.StatusConflict} {
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] != "error" || frame["status_code"] != float64(status) {
			t.Fatalf("failed native session = %#v, want status %d", frame, status)
		}
	}
	if connections.Load() != 1 || frames.Load() != 2 {
		t.Fatalf("failed session reconnected or replayed: connections:%d frames:%d", connections.Load(), frames.Load())
	}
}

func TestResponsesNativeWebSocketInvalidTerminalRetiresSession(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.incomplete"} {
		t.Run(terminal, func(t *testing.T) {
			var connections, frames atomic.Int32
			closed := make(chan struct{}, 1)
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				connections.Add(1)
				defer func() { _ = conn.Close(); closed <- struct{}{} }()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					frames.Add(1)
					// Valid JSON and terminal framing, but no resumable response ID.
					if err := conn.WriteJSON(map[string]any{"type": terminal, "response": map[string]any{"output": []any{}}}); err != nil {
						return
					}
				}
			})
			h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
			h.stats = newStatsCollector()
			conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
			defer func() { _ = conn.Close() }()
			request := newResponsesWebSocketCreateRequest(nil)
			if err := conn.WriteJSON(request); err != nil {
				t.Fatal(err)
			}
			frame := mustReadWebSocketJSONSkipMetadata(t, conn)
			if frame["type"] == terminal {
				frame = mustReadWebSocketJSONSkipMetadata(t, conn)
			}
			if frame["type"] != "error" || frame["status_code"] != float64(http.StatusBadGateway) {
				t.Fatalf("invalid terminal result = %#v", frame)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("invalid terminal left the upstream connection open")
			}
			if err := conn.WriteJSON(request); err != nil {
				t.Fatal(err)
			}
			frame = mustReadWebSocketJSONSkipMetadata(t, conn)
			if frame["type"] != "error" || frame["status_code"] != float64(http.StatusConflict) {
				t.Fatalf("retired session accepted another turn: %#v", frame)
			}
			if connections.Load() != 1 || frames.Load() != 1 || h.stats.taskUsage.snapshot().Totals.Sends != 1 {
				t.Fatalf("invalid terminal reconnected or resent: connections=%d frames=%d task=%+v", connections.Load(), frames.Load(), h.stats.taskUsage.snapshot())
			}
		})
	}
}

func TestResponsesNativeWebSocketPinnedRouteAndCancellation(t *testing.T) {
	var primaryFrames, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var frame map[string]json.RawMessage
			if conn.ReadJSON(&frame) != nil {
				return
			}
			if string(frame["model"]) != `"deployment-a"` {
				t.Errorf("upstream model = %s", frame["model"])
			}
			if primaryFrames.Add(1) == 1 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-pinned","model":"deployment-a","output":[]}}`))
			} else {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancelled","response":{"id":"resp-cancelled","model":"deployment-a","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`))
				return
			}
		}
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		http.Error(w, "unexpected failover", http.StatusBadGateway)
	}))
	defer secondary.Close()
	provider := explicitRouteTestProvider("primary", primary.URL, "")
	provider.kind = providerTypeCopilot
	provider.paths = providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths()
	h, _ := explicitRouteTestHandler(t, primary.Client(), routeModePriorityFailover, 2, 2, provider, explicitRouteTestProvider("secondary", secondary.URL, "other-key"))
	h.auth = auth.NewTestAuthenticator("test-token")
	h.log = logger.New(logger.LevelError)
	h.stats = newStatsCollector()
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest(nil)
	request["model"] = "public-model"
	for _, terminal := range []string{"response.completed", "response.cancelled"} {
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] != terminal {
			t.Fatalf("terminal = %#v, want %s", frame, terminal)
		}
		response, _ := frame["response"].(map[string]any)
		if response["model"] != "public-model" {
			t.Fatalf("terminal model leaked: %#v", response)
		}
		request["previous_response_id"] = "resp-pinned"
	}
	if secondaryCalls.Load() != 0 || primaryFrames.Load() != 2 {
		t.Fatalf("pinned sends = primary:%d secondary:%d", primaryFrames.Load(), secondaryCalls.Load())
	}
	stats := h.stats.snapshot()
	if stats.Totals.Requests != 2 || stats.Totals.Errors != 0 || stats.Totals.TotalTokens != 9 {
		t.Fatalf("cancelled route accounting = %+v", stats.Totals)
	}
}

func TestResponsesNativeWebSocketNonCopilotUsesHTTP(t *testing.T) {
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("provider request method = %s", r.Method)
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\"}}\n\n")
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(nil, logger.New(logger.LevelError), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: 1, Providers: []ProviderConfig{{
			ID: "compatible", Type: string(providerTypeOpenAICompatible), BaseURL: upstream.URL, APIKey: "test-key",
			Models: []ProviderModelConfig{{PublicID: "gpt-5.4", Endpoints: []string{providerEndpointResponses}}},
		}},
	}), WithResponsesWebSocketConfig(ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" || posts.Load() != 1 {
		t.Fatalf("HTTP provider result = %#v, sends %d", frame, posts.Load())
	}
}

func TestResponsesNativeWebSocketLifecycleCancellation(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan struct{})
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		defer close(closed)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		close(started)
		_, _, _ = conn.ReadMessage()
	})
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native inference did not start")
	}
	h.BeginShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.ShutdownWebSocketSessions(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("shutdown left native inference running")
	}
}

func TestResponsesNativeWebSocketRejectsConnectionModelChange(t *testing.T) {
	var frames atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			frames.Add(1)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-model","output":[]}}`))
		}
	})
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest(nil)
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	request["model"] = "different-model"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	frame := mustReadWebSocketJSONSkipMetadata(t, conn)
	if frame["status_code"] != float64(http.StatusBadRequest) || frames.Load() != 1 {
		t.Fatalf("model switch was sent: response:%#v sends:%d", frame, frames.Load())
	}
}

func TestResponsesNativeWebSocketBounds(t *testing.T) {
	marker := newResponsesNativeUpstream(context.Background())
	request, err := parseResponsesWebSocketCreateRequest([]byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Input = make([]json.RawMessage, responsesNativeMaxPendingItems+1)
	session := &responsesWebSocketSession{nativeUpstream: marker}
	if _, err := session.planRequest(&ProxyHandler{}, request); err == nil {
		t.Fatal("unbounded native pending input accepted")
	}
	request.Input = []json.RawMessage{json.RawMessage(`{"role":"user","content":"next"}`)}
	request.PreviousResponseID = "vekil-ws-staged"
	session.lastResponseID = request.PreviousResponseID
	session.lastSignature = request.signature()
	session.historyItems = make([]json.RawMessage, responsesNativeMaxPendingItems)
	if _, err := session.planRequest(&ProxyHandler{}, request); err == nil {
		t.Fatal("unbounded local warmups before the first native send accepted")
	}
	_, _, err = buildResponsesNativeCreate([]byte(`{"model":"gpt-5.4","input":[]}`), "", http.Header{"X-Interaction-Id": {strings.Repeat("x", 9<<10)}})
	if err == nil {
		t.Fatal("unbounded native per-turn metadata accepted")
	}
}

func TestResponsesNativeWebSocketTLSUpgradeUsesHTTP1(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("websocket upgrade protocol = %s", r.Proto)
		}
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-tls","output":[]}}`))
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	client := upstream.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	defer transport.CloseIdleConnections()
	client.Transport = transport
	h := &ProxyHandler{
		auth: auth.NewTestAuthenticator("test-token"), client: client, copilotURL: upstream.URL,
		log: logger.New(logger.LevelError), maxRetries: 1,
		responsesWS: ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true},
	}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
		t.Fatalf("TLS native response = %#v", frame)
	}
}

func TestResponsesNativeWebSocketFirstFrameFailureDoesNotFailover(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		status  int
	}{
		{name: "disconnect", status: http.StatusBadGateway},
		{name: "oversized frame", payload: `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", openAIStreamScannerMaxBuffer) + `"}`, status: http.StatusBadGateway},
		{name: "multiplexed response", payload: `{"type":"response.completed","stream_id":"other","response":{"id":"resp-other"}}`, status: http.StatusBadGateway},
		{name: "rate limit", payload: `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Request rate exceeded"}}}`, status: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			var primaryFrames, secondaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				primaryFrames.Add(1)
				if test.payload != "" {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(test.payload))
				}
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				http.Error(w, "unexpected failover", http.StatusBadGateway)
			}))
			defer secondary.Close()
			provider := explicitRouteTestProvider("primary", primary.URL, "")
			provider.kind = providerTypeCopilot
			provider.paths = providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths()
			h, _ := explicitRouteTestHandler(t, primary.Client(), routeModePriorityFailover, 2, 2, provider, explicitRouteTestProvider("secondary", secondary.URL, "other-key"))
			h.auth = auth.NewTestAuthenticator("test-token")
			h.log = logger.New(logger.LevelError)
			h.stats = newStatsCollector()
			h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
			conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
			defer func() { _ = conn.Close() }()
			request := newResponsesWebSocketCreateRequest(nil)
			request["model"] = "public-model"
			for _, status := range []int{test.status, http.StatusConflict} {
				if err := conn.WriteJSON(request); err != nil {
					t.Fatal(err)
				}
				frame := mustReadWebSocketJSONSkipMetadata(t, conn)
				if frame["type"] != "error" || frame["status_code"] != float64(status) {
					t.Fatalf("native failure = %#v, want status %d", frame, status)
				}
			}
			stats := h.stats.snapshot()
			if primaryFrames.Load() != 1 || secondaryCalls.Load() != 0 || stats.UpstreamAttempts != 1 || stats.TaskUsage.Totals.Sends != 1 {
				t.Fatalf("native failure dispatched another attempt: primary:%d secondary:%d attempts:%d sends:%d", primaryFrames.Load(), secondaryCalls.Load(), stats.UpstreamAttempts, stats.TaskUsage.Totals.Sends)
			}
		})
	}
}

func TestResponsesNativeWebSocketLocalRejectionDoesNotCountSend(t *testing.T) {
	var sends atomic.Int32
	client := &http.Client{Transport: handlerTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return nil, errors.New("unexpected dispatch")
	})}
	provider := explicitRouteTestProvider("primary", "http://127.0.0.1:1", "")
	provider.kind = providerTypeCopilot
	provider.paths = providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths()
	h, _ := explicitRouteTestHandler(t, client, routeModePrimaryOnly, 1, 1, provider)
	h.auth = auth.NewTestAuthenticator("test-token")
	h.log = logger.New(logger.LevelError)
	h.stats = newStatsCollector()
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest(nil)
	request["model"] = "public-model"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	frame := mustReadWebSocketJSONSkipMetadata(t, conn)
	if frame["type"] != "error" || frame["status_code"] != float64(http.StatusBadRequest) {
		t.Fatalf("unsupported native transport = %#v", frame)
	}
	stats := h.stats.snapshot()
	if sends.Load() != 0 || stats.UpstreamAttempts != 0 || stats.TaskUsage.Totals.Sends != 0 {
		t.Fatalf("local validation counted a send: transport:%d attempts:%d task:%d", sends.Load(), stats.UpstreamAttempts, stats.TaskUsage.Totals.Sends)
	}
}

func TestResponsesNativeWebSocketCanceledClosesWithUsage(t *testing.T) {
	closed := make(chan struct{})
	var sends atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		defer close(closed)
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		sends.Add(1)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.canceled","response":{"id":"resp-canceled","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`)); err != nil {
			t.Error(err)
			return
		}
		// Keep the server open so only the proxy can retire the connection.
		_, _, _ = conn.ReadMessage()
	})
	h.stats = newStatsCollector()
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest(nil)
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.canceled" {
		t.Fatalf("alternate cancellation spelling = %#v", frame)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the upstream connection")
	}
	stats := h.stats.snapshot()
	if stats.Totals.Requests != 1 || stats.Totals.Errors != 0 || stats.Totals.TotalTokens != 9 {
		t.Fatalf("alternate cancellation accounting = %+v", stats.Totals)
	}
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "error" || frame["status_code"] != float64(http.StatusConflict) {
		t.Fatalf("retired native connection response = %#v", frame)
	}
	stats = h.stats.snapshot()
	if sends.Load() != 1 || stats.TaskUsage.Totals.Sends != 1 || stats.TaskUsage.Totals.Usage.TotalTokens != 9 {
		t.Fatalf("cancelled native task accounting = sends:%d task:%+v", sends.Load(), stats.TaskUsage)
	}
}
