package proxy

import (
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

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestHandleResponsesWebSocket_UpgradeRequiredWithoutUpgradeHeaders(t *testing.T) {
	handler := &ProxyHandler{responsesWS: ResponsesWebSocketConfig{Enabled: true}}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	w := httptest.NewRecorder()

	handler.HandleResponsesWebSocket(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("expected 426, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("expected Upgrade header to be websocket, got %q", resp.Header.Get("Upgrade"))
	}
}

func TestHandleResponsesWebSocket_DisabledByDefaultReturnsUpgradeRequired(t *testing.T) {
	handler := &ProxyHandler{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/responses", handler.HandleResponsesWebSocket)
	server := httptest.NewServer(mux)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected websocket dial to fail when bridge is disabled")
	}
	if resp == nil {
		t.Fatalf("expected HTTP response for disabled websocket bridge: %v", err)
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("expected 426, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("expected Upgrade header to be websocket, got %q", resp.Header.Get("Upgrade"))
	}
}

func TestResponsesWebSocketCreateRequest_IgnoresInitiatorForSignatureAndUpstream(t *testing.T) {
	base := map[string]interface{}{
		"type":                 "response.create",
		"model":                "gpt-5.4",
		"instructions":         "You are helpful",
		"input":                []interface{}{},
		"previous_response_id": "resp-previous",
		"generate":             true,
		"client_metadata": map[string]string{
			"x-codex-turn-metadata": `{"turn_id":"turn-1"}`,
		},
		"initiator": "user",
		"stream":    true,
	}

	withUserInitiator, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	base["initiator"] = "agent"
	withAgentInitiator, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	userRequest, err := parseResponsesWebSocketCreateRequest(withUserInitiator)
	if err != nil {
		t.Fatalf("failed to parse user-initiated request: %v", err)
	}
	agentRequest, err := parseResponsesWebSocketCreateRequest(withAgentInitiator)
	if err != nil {
		t.Fatalf("failed to parse agent-initiated request: %v", err)
	}

	if userRequest.signature() != agentRequest.signature() {
		t.Fatalf("expected initiator not to affect websocket request signature, got %q vs %q", userRequest.signature(), agentRequest.signature())
	}

	body, err := userRequest.upstreamBody(nil)
	if err != nil {
		t.Fatalf("failed to build upstream body: %v", err)
	}
	var upstream map[string]json.RawMessage
	if err := json.Unmarshal(body, &upstream); err != nil {
		t.Fatalf("failed to decode upstream body: %v", err)
	}
	for _, key := range []string{"initiator", "client_metadata", "previous_response_id", "generate", "type"} {
		if _, ok := upstream[key]; ok {
			t.Fatalf("upstream request should not include websocket field %q", key)
		}
	}
}

func TestResponsesWebSocketClientWriteErrorIsClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !isResponsesWebSocketClientDisconnect(context.Background(), &responsesWebSocketClientWriteError{err: io.ErrClosedPipe}) {
		t.Fatal("wrapped websocket write errors should be classified as client disconnects")
	}
	if !isResponsesWebSocketClientDisconnect(ctx, context.Canceled) {
		t.Fatal("canceled session context should be classified as client disconnect")
	}
}

func TestResponsesWebSocketStreamUpstreamResponseWrapsClientWriteError(t *testing.T) {
	connCh := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		connCh <- conn
		<-done
		_ = conn.Close()
	}))
	defer server.Close()
	defer close(done)

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverConn := <-connCh
	_ = client.Close()
	_ = serverConn.Close()

	session := &responsesWebSocketSession{conn: serverConn, ctx: context.Background()}
	stream := strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\"}}\n\n")
	_, err = session.streamUpstreamResponse(nil, stream, nil, nil)
	if !errors.Is(err, errResponsesWebSocketClientWrite) {
		t.Fatalf("streamUpstreamResponse error = %v, want client write sentinel", err)
	}
}

func TestHandleResponsesWebSocket_BridgesStreamingResponse(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/responses" {
			t.Fatalf("expected path /responses, got %q", r.URL.Path)
		}
		if got := r.Header.Get("Traceparent"); got != "00-11111111111111111111111111111111-2222222222222222-01" {
			t.Fatalf("expected traceparent header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Custom-Test-Telemetry"); got != "custom-value" {
			t.Fatalf("expected custom telemetry header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Turn-State"); got != "" {
			t.Fatalf("expected client metadata turn state not to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
			t.Fatalf("expected turn metadata header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Installation-Id"); got != "install-123" {
			t.Fatalf("expected installation id header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Window-Id"); got != "thread-1:3" {
			t.Fatalf("expected window id header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Parent-Thread-Id"); got != "parent-thread-1" {
			t.Fatalf("expected parent thread id header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-OpenAI-Subagent"); got != "collab_spawn" {
			t.Fatalf("expected subagent header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-OpenAI-Memgen-Request"); got != "true" {
			t.Fatalf("expected memgen header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-ResponsesAPI-Include-Timing-Metrics"); got != "true" {
			t.Fatalf("expected timing metrics header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-WS-Stream-Request-Start-Ms"); got != "1738888888123" {
			t.Fatalf("expected websocket stream start header to be forwarded, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Beta-Features"); got != "responses_websockets_v2" {
			t.Fatalf("expected beta features header to be forwarded, got %q", got)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		if _, ok := body["type"]; ok {
			t.Fatalf("upstream request should not include websocket type field")
		}
		if _, ok := body["client_metadata"]; ok {
			t.Fatalf("upstream request should not include websocket client metadata")
		}
		if _, ok := body["initiator"]; ok {
			t.Fatalf("upstream request should not include websocket initiator field")
		}
		if _, ok := body["previous_response_id"]; ok {
			t.Fatalf("upstream request should not include websocket previous_response_id")
		}
		var serviceTier string
		if err := json.Unmarshal(body["service_tier"], &serviceTier); err != nil {
			t.Fatalf("upstream request should preserve service_tier: %v", err)
		}
		if serviceTier != "auto" {
			t.Fatalf("expected upstream service_tier auto, got %q", serviceTier)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{
		"X-Codex-Beta-Features": []string{"responses_websockets_v2"},
	})
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	request["client_metadata"] = map[string]string{
		"ws_request_header_traceparent":             "00-11111111111111111111111111111111-2222222222222222-01",
		"x-codex-turn-metadata":                     `{"turn_id":"turn-1"}`,
		"x-codex-installation-id":                   "install-123",
		"x-codex-window-id":                         "thread-1:3",
		"x-codex-parent-thread-id":                  "parent-thread-1",
		"x-openai-subagent":                         "collab_spawn",
		"x-openai-memgen-request":                   "true",
		"x-responsesapi-include-timing-metrics":     "true",
		"x-codex-ws-stream-request-start-ms":        "1738888888123",
		"ws_request_header_x-codex-turn-state":      "client-state-should-not-forward",
		"ws_request_header_x-custom-test-telemetry": "custom-value",
	}
	request["initiator"] = "user"
	request["service_tier"] = "auto"

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected first event to be response.created, got %v", created["type"])
	}
	output := mustReadWebSocketJSON(t, conn)
	if output["type"] != "response.output_item.done" {
		t.Fatalf("expected second event to be response.output_item.done, got %v", output["type"])
	}
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected third event to be response.completed, got %v", completed["type"])
	}

	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request, got %d", got)
	}
}

func TestHandleResponsesWebSocket_ForwardsCustomTurnMetadataFields(t *testing.T) {
	turnMetadata := `{"turn_id":"turn-123","fiber_run_id":"fiber-123","origin":"app-server"}`
	var gotTurnMetadata string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotTurnMetadata = r.Header.Get("X-Codex-Turn-Metadata")

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	request["client_metadata"] = map[string]string{
		"x-codex-turn-metadata": turnMetadata,
	}

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	var parsed map[string]string
	if err := json.Unmarshal([]byte(gotTurnMetadata), &parsed); err != nil {
		t.Fatalf("expected forwarded turn metadata to be valid JSON, got %q: %v", gotTurnMetadata, err)
	}
	if parsed["turn_id"] != "turn-123" {
		t.Fatalf("expected turn_id to be preserved, got %q", parsed["turn_id"])
	}
	if parsed["fiber_run_id"] != "fiber-123" {
		t.Fatalf("expected custom fiber_run_id to be preserved, got %q", parsed["fiber_run_id"])
	}
	if parsed["origin"] != "app-server" {
		t.Fatalf("expected custom origin to be preserved, got %q", parsed["origin"])
	}
}

func TestHandleResponsesWebSocket_ForwardsCompletedUsage(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-usage\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-usage\",\"usage\":{\"input_tokens\":1234,\"input_tokens_details\":{\"cached_tokens\":456},\"output_tokens\":78,\"output_tokens_details\":{\"reasoning_tokens\":9},\"total_tokens\":1312}}}\n\n")
	})
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected first event to be response.created, got %v", created["type"])
	}
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected second event to be response.completed, got %v", completed["type"])
	}

	response, ok := completed["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("response payload missing or wrong type: %#v", completed["response"])
	}
	usage, ok := response["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage payload missing or wrong type: %#v", response["usage"])
	}
	assertUsageNumber := func(field string, want float64) {
		t.Helper()
		got, ok := usage[field].(float64)
		if !ok || got != want {
			t.Fatalf("%s = %#v, want %.0f", field, usage[field], want)
		}
	}
	assertUsageNumber("input_tokens", 1234)
	assertUsageNumber("output_tokens", 78)
	assertUsageNumber("total_tokens", 1312)

	inputDetails, ok := usage["input_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("input_tokens_details missing or wrong type: %#v", usage["input_tokens_details"])
	}
	if got, ok := inputDetails["cached_tokens"].(float64); !ok || got != 456 {
		t.Fatalf("cached_tokens = %#v, want 456", inputDetails["cached_tokens"])
	}
	outputDetails, ok := usage["output_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("output_tokens_details missing or wrong type: %#v", usage["output_tokens_details"])
	}
	if got, ok := outputDetails["reasoning_tokens"].(float64); !ok || got != 9 {
		t.Fatalf("reasoning_tokens = %#v, want 9", outputDetails["reasoning_tokens"])
	}

	deadline := time.Now().Add(time.Second)
	for {
		snap := handler.stats.snapshot()
		if snap.Totals.Requests >= 1 {
			if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
				t.Fatalf("no-compaction success stats = requests:%d errors:%d, want 1/0", snap.Totals.Requests, snap.Totals.Errors)
			}
			if snap.Totals.PromptTokens != 1234 || snap.Totals.CompletionTokens != 78 || snap.Totals.TotalTokens != 1312 {
				t.Fatalf("no-compaction success usage = prompt:%d completion:%d total:%d, want 1234/78/1312", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
			}
			if snap.Totals.CachedTokens != 456 || snap.Totals.ReasoningTokens != 9 {
				t.Fatalf("no-compaction success detail usage = cached:%d reasoning:%d, want 456/9", snap.Totals.CachedTokens, snap.Totals.ReasoningTokens)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no-compaction success stats were not recorded: %+v", snap.Totals)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHandleResponsesWebSocket_IgnoresResponseProcessedControlFrame(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-%d\"}}\n\n", requestNumber)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-%d\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n", requestNumber)
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if err := conn.WriteJSON(map[string]interface{}{
		"type":        "response.processed",
		"response_id": "resp-1",
	}); err != nil {
		t.Fatalf("failed to write response.processed websocket request: %v", err)
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "second"},
			},
		},
	})
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second websocket request after response.processed: %v", err)
	}
	secondCreated := mustReadWebSocketJSON(t, conn)
	if secondCreated["type"] != "response.created" {
		t.Fatalf("expected response.created after response.processed, got %v", secondCreated["type"])
	}
	secondCompleted := mustReadWebSocketJSON(t, conn)
	if secondCompleted["type"] != "response.completed" {
		t.Fatalf("expected response.completed after response.processed, got %v", secondCompleted["type"])
	}

	if got := upstreamRequests.Load(); got != 2 {
		t.Fatalf("expected response.processed not to create upstream request; got %d upstream requests", got)
	}
}

func TestHandleResponsesWebSocket_RoutesConfiguredAzureModelAndPreservesPriorityServiceTier(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	var upstreamRequests atomic.Int32
	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if got := r.URL.Path; got != "/openai/v1/responses" {
			t.Fatalf("expected Azure path /openai/v1/responses, got %s", got)
		}
		if got := r.URL.RawQuery; got != "" {
			t.Fatalf("expected no Azure query params for /openai/v1 base URL, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "azure-test-key" {
			t.Fatalf("expected api-key header, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected no Copilot Authorization header, got %q", got)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		var model string
		if err := json.Unmarshal(body["model"], &model); err != nil {
			t.Fatalf("failed to decode upstream model: %v", err)
		}
		if model != "gpt-5-4-prod" {
			t.Fatalf("expected Azure deployment model gpt-5-4-prod, got %q", model)
		}

		var serviceTier string
		if err := json.Unmarshal(body["service_tier"], &serviceTier); err != nil {
			t.Fatalf("failed to decode upstream service_tier: %v", err)
		}
		if serviceTier != "priority" {
			t.Fatalf("expected upstream service_tier priority, got %q", serviceTier)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-azure-ws\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-azure-ws\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    azureServer.URL + "/openai/v1",
				APIKeyEnv:  "TEST_AZURE_API_KEY",
				APIVersion: "preview",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{"/responses"},
					Name:       "GPT-5 Public",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	request["model"] = "gpt-5-public"
	request["service_tier"] = "priority"

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected first event to be response.created, got %v", created["type"])
	}
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected second event to be response.completed, got %v", completed["type"])
	}

	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request, got %d", got)
	}
}

func TestHandleResponsesWebSocket_RoutesConfiguredAzureIdentityProvider(t *testing.T) {
	tokenSource := &staticAzureTokenSource{token: "ws-entra-token"}
	var upstreamRequests atomic.Int32

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if got := r.URL.Path; got != "/openai/v1/responses" {
			t.Fatalf("expected Azure path /openai/v1/responses, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ws-entra-token" {
			t.Fatalf("expected Azure identity Authorization header, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		var model string
		if err := json.Unmarshal(body["model"], &model); err != nil {
			t.Fatalf("failed to decode upstream model: %v", err)
		}
		if model != "gpt-5-4-prod" {
			t.Fatalf("expected Azure deployment model gpt-5-4-prod, got %q", model)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-azure-identity-ws\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-azure-identity-ws\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "foundry",
			Type:     "azure-openai",
			Default:  true,
			BaseURL:  azureServer.URL + "/openai/v1",
			AuthMode: "azure_identity",
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-5-public",
				Deployment: "gpt-5-4-prod",
				Endpoints:  []string{"/responses"},
			}},
		}}}),
		withAzureIdentityTokenSourceFactoryForTest(func(string, string) (azureTokenSource, error) {
			return tokenSource, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	request["model"] = "gpt-5-public"

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected first event to be response.created, got %v", created["type"])
	}
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected second event to be response.completed, got %v", completed["type"])
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request, got %d", got)
	}
}

func TestHandleResponsesWebSocket_ExplicitRouteFirstTurnFailsOverThenPinsSession(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read primary body: %v", err)
		}
		if !strings.Contains(string(body), `"model":"physical-primary"`) {
			t.Errorf("primary body did not use physical model: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Openai-Model", "physical-primary")
		w.Header().Set("X-Codex-Turn-State", "primary-hidden-state")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary_hidden\",\"model\":\"physical-primary\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_primary_hidden\",\"model\":\"physical-primary\",\"error\":{\"type\":\"server_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"primary overloaded\"}}}\n\n")
	}))
	defer primary.Close()

	var secondaryCalls atomic.Int32
	secondTurnState := make(chan string, 1)
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := secondaryCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read secondary body: %v", err)
		}
		if !strings.Contains(string(body), `"model":"physical-secondary"`) {
			t.Errorf("secondary body did not use physical model: %s", body)
		}
		if strings.Contains(string(body), "resp_primary_hidden") || strings.Contains(string(body), "primary-hidden-state") {
			t.Errorf("failed primary leaked into secondary request: %s", body)
		}
		if call == 2 {
			secondTurnState <- r.Header.Get("X-Codex-Turn-State")
		}

		responseID := fmt.Sprintf("resp_secondary_%d", call)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Openai-Model", "physical-secondary")
		w.Header().Set("X-Codex-Turn-State", fmt.Sprintf("secondary-state-%d", call))
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"model\":\"physical-secondary\"}}\n\n", responseID)
		if call == 1 {
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_secondary\",\"content\":[{\"type\":\"output_text\",\"text\":\"secondary only\"}]}}\n\n")
		}
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"model\":\"physical-secondary\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n", responseID)
	}))
	defer secondary.Close()

	handler := newExplicitRouteResponsesWebSocketHandler(t, primary.URL, secondary.URL)
	handler.responsesWS.TurnStateDelta = true
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "first"}},
		},
	})
	first["model"] = "public-ws-model"
	first["client_metadata"] = map[string]string{"x-codex-turn-metadata": `{"turn_id":"route-turn"}`}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("write first request: %v", err)
	}

	firstMetadata := mustReadWebSocketJSON(t, conn)
	firstOperationID := explicitResponsesWebSocketOperationID(t, firstMetadata)
	if got := explicitResponsesWebSocketMetadataHeader(t, firstMetadata, "openai-model"); got != "public-ws-model" {
		t.Fatalf("first metadata model = %q, want public-ws-model", got)
	}
	firstCreated := mustReadWebSocketJSON(t, conn)
	if firstCreated["type"] != "response.created" {
		t.Fatalf("first visible event = %v, want response.created", firstCreated["type"])
	}
	if responseID := websocketResponseID(t, firstCreated); responseID != "resp_secondary_1" {
		t.Fatalf("first visible response id = %q, want secondary response", responseID)
	}
	firstResponse, ok := firstCreated["response"].(map[string]interface{})
	if !ok || firstResponse["model"] != "public-ws-model" {
		t.Fatalf("first visible response model = %+v, want public model", firstCreated["response"])
	}
	firstOutput := mustReadWebSocketJSON(t, conn)
	if firstOutput["type"] != "response.output_item.done" {
		t.Fatalf("first output event = %v", firstOutput["type"])
	}
	firstCompleted := mustReadWebSocketJSON(t, conn)
	if firstCompleted["type"] != "response.completed" {
		t.Fatalf("first terminal event = %v", firstCompleted["type"])
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "second"}},
		},
	})
	second["model"] = "public-ws-model"
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	second["client_metadata"] = map[string]string{"x-codex-turn-metadata": `{"turn_id":"route-turn"}`}
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("write second request: %v", err)
	}

	secondMetadata := mustReadWebSocketJSON(t, conn)
	secondOperationID := explicitResponsesWebSocketOperationID(t, secondMetadata)
	secondCreated := mustReadWebSocketJSON(t, conn)
	if secondCreated["type"] != "response.created" || websocketResponseID(t, secondCreated) != "resp_secondary_2" {
		t.Fatalf("second created event = %+v", secondCreated)
	}
	secondCompleted := mustReadWebSocketJSON(t, conn)
	if secondCompleted["type"] != "response.completed" {
		t.Fatalf("second terminal event = %v", secondCompleted["type"])
	}

	if got := primaryCalls.Load(); got != 1 {
		t.Fatalf("primary calls = %d, want only failed first-turn attempt", got)
	}
	if got := secondaryCalls.Load(); got != 2 {
		t.Fatalf("secondary calls = %d, want first-turn success plus pinned follow-up", got)
	}
	if got := <-secondTurnState; got != "secondary-state-1" {
		t.Fatalf("second turn state = %q, want committed secondary state", got)
	}
	assertResponsesWebSocketOperationSequence(t, firstOperationID, secondOperationID)
}

func TestHandleResponsesWebSocket_ExplicitRoutePinnedTargetFailureDoesNotMigrate(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := primaryCalls.Add(1)
		if call == 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"primary unavailable","code":"rate_limit_exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Openai-Model", "physical-primary")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary_1\",\"model\":\"physical-primary\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_primary_1\",\"model\":\"physical-primary\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer primary.Close()

	var secondaryCalls atomic.Int32
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_secondary_unexpected\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_secondary_unexpected\"}}\n\n")
	}))
	defer secondary.Close()

	handler := newExplicitRouteResponsesWebSocketHandler(t, primary.URL, secondary.URL)
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{})
	first["model"] = "public-ws-model"
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	firstMetadata := mustReadWebSocketJSON(t, conn)
	firstOperationID := explicitResponsesWebSocketOperationID(t, firstMetadata)
	firstCreated := mustReadWebSocketJSON(t, conn)
	if firstCreated["type"] != "response.created" {
		t.Fatalf("first created event = %v", firstCreated["type"])
	}
	if completed := mustReadWebSocketJSON(t, conn); completed["type"] != "response.completed" {
		t.Fatalf("first terminal event = %v", completed["type"])
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{})
	second["model"] = "public-ws-model"
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	errorFrame := mustReadWebSocketJSON(t, conn)
	if errorFrame["type"] != "error" || int(errorFrame["status_code"].(float64)) != http.StatusTooManyRequests {
		t.Fatalf("pinned failure frame = %+v", errorFrame)
	}
	secondOperationID := explicitResponsesWebSocketErrorHeader(t, errorFrame, responsesWebSocketOperationHeader)

	if got := primaryCalls.Load(); got != 2 {
		t.Fatalf("primary calls = %d, want one call per turn with no automatic same-target retry", got)
	}
	if got := secondaryCalls.Load(); got != 0 {
		t.Fatalf("secondary calls = %d, pinned session must fail closed", got)
	}
	assertResponsesWebSocketOperationSequence(t, firstOperationID, secondOperationID)
}

func TestHandleResponsesWebSocket_ExplicitRouteRejectsUnknownStateBeforeDispatch(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()

	handler := newExplicitRouteResponsesWebSocketHandler(t, upstream.URL, upstream.URL)
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":              "reasoning",
			"encrypted_content": "unknown-provider-state",
		},
	})
	request["model"] = "public-ws-model"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	errorFrame := mustReadWebSocketJSON(t, conn)
	if errorFrame["type"] != "error" || int(errorFrame["status_code"].(float64)) != http.StatusBadRequest {
		t.Fatalf("unknown-state frame = %+v", errorFrame)
	}
	if got := explicitResponsesWebSocketErrorHeader(t, errorFrame, responsesWebSocketOperationHeader); !strings.HasSuffix(got, ":1") {
		t.Fatalf("operation id = %q, want first turn suffix", got)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, unknown state must fail before dispatch", got)
	}
}

func TestHandleResponsesWebSocket_ExplicitRoutePreservesLocalWarmupReplay(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read primary body: %v", err)
		}
		if !strings.Contains(string(body), "local warmup") || !strings.Contains(string(body), "provider turn") {
			t.Errorf("provider replay body = %s, want local warmup plus current input", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary_warmup\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_primary_warmup\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer primary.Close()

	var secondaryCalls atomic.Int32
	secondary := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondaryCalls.Add(1)
	}))
	defer secondary.Close()

	handler := newExplicitRouteResponsesWebSocketHandler(t, primary.URL, secondary.URL)
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	warmup := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "local warmup"}},
		},
	})
	warmup["model"] = "public-ws-model"
	warmup["generate"] = false
	if err := conn.WriteJSON(warmup); err != nil {
		t.Fatalf("write warmup request: %v", err)
	}
	warmupCreated := mustReadWebSocketJSON(t, conn)
	if warmupCreated["type"] != "response.created" {
		t.Fatalf("warmup created event = %v", warmupCreated["type"])
	}
	if completed := mustReadWebSocketJSON(t, conn); completed["type"] != "response.completed" {
		t.Fatalf("warmup terminal event = %v", completed["type"])
	}

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "provider turn"}},
		},
	})
	request["model"] = "public-ws-model"
	request["previous_response_id"] = websocketResponseID(t, warmupCreated)
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("write provider request: %v", err)
	}
	if metadata := mustReadWebSocketJSON(t, conn); !strings.HasSuffix(explicitResponsesWebSocketOperationID(t, metadata), ":1") {
		t.Fatalf("provider operation metadata = %+v", metadata)
	}
	if created := mustReadWebSocketJSON(t, conn); created["type"] != "response.created" {
		t.Fatalf("provider created event = %v", created["type"])
	}
	if completed := mustReadWebSocketJSON(t, conn); completed["type"] != "response.completed" {
		t.Fatalf("provider terminal event = %v", completed["type"])
	}
	if got := primaryCalls.Load(); got != 1 {
		t.Fatalf("primary calls = %d, want one provider-backed turn", got)
	}
	if got := secondaryCalls.Load(); got != 0 {
		t.Fatalf("secondary calls = %d, warmup replay should use primary", got)
	}
}

func TestHandleResponsesWebSocket_CreateRequestUsesStreamingUpstreamTimeout(t *testing.T) {
	deadlineCh := make(chan time.Duration, 1)
	handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("expected upstream request deadline")
		}
		deadlineCh <- time.Until(deadline)

		return sseHTTPResponse(
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-deadline\"}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-deadline\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n",
		), nil
	}))

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected first event to be response.created, got %v", created["type"])
	}
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected second event to be response.completed, got %v", completed["type"])
	}

	assertDeadlineApprox(t, <-deadlineCh, streamingUpstreamTimeout)
}

func TestHandleResponsesWebSocket_WarmupStaysLocalAndNextRequestExpandsState(t *testing.T) {
	var upstreamRequests atomic.Int32
	var upstreamBodyMu sync.Mutex
	var upstreamBody map[string]interface{}
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamBodyMu.Lock()
		upstreamBody = body
		upstreamBodyMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	warmup := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "warm me up"},
			},
		},
	})
	warmup["generate"] = false

	if err := conn.WriteJSON(warmup); err != nil {
		t.Fatalf("failed to write warmup request: %v", err)
	}

	warmupCreated := mustReadWebSocketJSON(t, conn)
	warmupCompleted := mustReadWebSocketJSON(t, conn)
	if warmupCreated["type"] != "response.created" {
		t.Fatalf("expected warmup response.created event, got %v", warmupCreated["type"])
	}
	if warmupCompleted["type"] != "response.completed" {
		t.Fatalf("expected warmup response.completed event, got %v", warmupCompleted["type"])
	}
	if got := upstreamRequests.Load(); got != 0 {
		t.Fatalf("expected warmup request to avoid upstream call, got %d requests", got)
	}

	warmupID := websocketResponseID(t, warmupCreated)
	request := newResponsesWebSocketCreateRequest([]interface{}{})
	request["previous_response_id"] = warmupID

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write expanded request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request after warmup, got %d", got)
	}

	upstreamBodyMu.Lock()
	body := upstreamBody
	upstreamBodyMu.Unlock()

	input := upstreamInputItems(t, body)
	if len(input) != 1 {
		t.Fatalf("expected expanded upstream input length 1, got %d", len(input))
	}
	if got := inputTextFromMessage(t, input[0]); got != "warm me up" {
		t.Fatalf("expected expanded upstream input text to be preserved, got %q", got)
	}
}

func TestHandleResponsesWebSocket_ExpandsPreviousOutputItemsIntoNextRequest(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 2)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		requestCount := len(upstreamRequests)
		upstreamRequestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestCount {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"shell_command\",\"arguments\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestCount)
		}
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "run something"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call-1",
			"output":  "command complete",
		},
	})
	second["previous_response_id"] = "resp-1"
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}

	firstInput := upstreamInputItems(t, requests[0])
	if len(firstInput) != 1 {
		t.Fatalf("expected first upstream input length 1, got %d", len(firstInput))
	}

	secondInput := upstreamInputItems(t, requests[1])
	if len(secondInput) != 3 {
		t.Fatalf("expected second upstream input length 3, got %d", len(secondInput))
	}
	if secondInput[0]["type"] != "message" {
		t.Fatalf("expected first expanded item to be original message, got %v", secondInput[0]["type"])
	}
	if secondInput[1]["type"] != "function_call" {
		t.Fatalf("expected second expanded item to be previous output function_call, got %v", secondInput[1]["type"])
	}
	if secondInput[2]["type"] != "function_call_output" {
		t.Fatalf("expected third expanded item to be current function_call_output, got %v", secondInput[2]["type"])
	}
}

func TestHandleResponsesWebSocket_AutoCompactsLongHistory(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 3)
	var normalRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}

		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamRequestsMu.Unlock()

		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"comp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}],"usage":{"input_tokens":1000,"output_tokens":200,"total_tokens":1200}}`)
			return
		}

		requestNumber := normalRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"first\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-2\",\"content\":[{\"type\":\"output_text\",\"text\":\"second\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-3\",\"content\":[{\"type\":\"output_text\",\"text\":\"third\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-4\",\"content\":[{\"type\":\"output_text\",\"text\":\"fourth\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":12}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":6},\"output_tokens\":3,\"output_tokens_details\":{\"reasoning_tokens\":2},\"total_tokens\":23}}}\n\n")
		default:
			t.Fatalf("unexpected normal upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 4,
		AutoCompactMaxBytes: 1 << 20,
		AutoCompactKeepTail: 2,
	}
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{"User-Agent": []string{"Codex CLI accounting-test"}})
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	firstCompleted := mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "second turn"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	_ = firstCompleted
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	deadline := time.Now().Add(2 * time.Second)
	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	for len(requests) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("expected at least 3 upstream requests (turn + compaction + turn), got %d", len(requests))
		}
		time.Sleep(10 * time.Millisecond)
		requests = snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	}

	secondTurnInput := upstreamInputItems(t, requests[2])
	if len(secondTurnInput) != 4 {
		t.Fatalf("expected compacted second upstream input length 4, got %d", len(secondTurnInput))
	}
	if got := requireMessageTextWithRole(t, secondTurnInput[0], "developer"); !strings.Contains(got, "checkpoint summary") {
		t.Fatalf("expected compacted checkpoint summary in first input item, got %q", got)
	}
	if got := inputTextFromMessage(t, secondTurnInput[3]); got != "second turn" {
		t.Fatalf("expected latest user turn to be preserved, got %q", got)
	}

	// One websocket response.create is one dashboard request even when the proxy
	// performs internal auto-compaction. The two client turns report 10+2 and
	// 20+3 tokens; compaction reports 1000+200 and must be folded into those two
	// turns without creating a third request.
	deadlineStats := time.Now().Add(2 * time.Second)
	for {
		snap := handler.stats.snapshot()
		if snap.Totals.TotalTokens >= 1235 {
			if snap.Totals.Requests != 2 || snap.Totals.Errors != 0 {
				t.Fatalf("auto-compaction stats = requests:%d errors:%d, want 2/0", snap.Totals.Requests, snap.Totals.Errors)
			}
			if snap.Totals.PromptTokens != 1030 || snap.Totals.CompletionTokens != 205 || snap.Totals.TotalTokens != 1235 {
				t.Fatalf("auto-compaction usage = prompt:%d completion:%d total:%d, want 1030/205/1235", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
			}
			if snap.Totals.CachedTokens != 10 || snap.Totals.ReasoningTokens != 3 {
				t.Fatalf("auto-compaction detail usage = cached:%d reasoning:%d, want 10/3", snap.Totals.CachedTokens, snap.Totals.ReasoningTokens)
			}
			if len(snap.ByModel) != 1 || snap.ByModel[0].Model != "gpt-5.4" || snap.ByModel[0].Requests != 2 || snap.ByModel[0].Tokens != 1235 {
				t.Fatalf("auto-compaction model attribution = %+v, want gpt-5.4 requests=2 tokens=1235", snap.ByModel)
			}
			if len(snap.ByProvider) != 1 || snap.ByProvider[0].Provider != "copilot" || snap.ByProvider[0].Requests != 2 || snap.ByProvider[0].Tokens != 1235 {
				t.Fatalf("auto-compaction provider attribution = %+v, want copilot requests=2 tokens=1235", snap.ByProvider)
			}
			if len(snap.ByAgent) != 1 || snap.ByAgent[0].Agent != "Codex CLI" || snap.ByAgent[0].Requests != 2 || snap.ByAgent[0].Tokens != 1235 {
				t.Fatalf("auto-compaction agent attribution = %+v, want Codex CLI requests=2 tokens=1235", snap.ByAgent)
			}
			break
		}
		if time.Now().After(deadlineStats) {
			t.Fatalf("auto-compaction stats never reached expected usage: %+v", snap.Totals)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResponsesWebSocketDefaultAutoCompactCoversObservedPressure(t *testing.T) {
	cfg := DefaultResponsesWebSocketConfig()
	if cfg.AutoCompactKeepTail >= 10 {
		t.Fatalf("default keep-tail %d would block compaction for the observed 10-item pressure case", cfg.AutoCompactKeepTail)
	}

	items := make([]json.RawMessage, 0, 10)
	for i := 0; i < 10; i++ {
		raw, err := json.Marshal(map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{{
				"type": "input_text",
				"text": fmt.Sprintf("pressure item %02d %s", i, strings.Repeat("x", 3500)),
			}},
		})
		if err != nil {
			t.Fatalf("failed to marshal history item: %v", err)
		}
		items = append(items, raw)
	}

	if got := rawMessagesSize(items); got <= 34275 {
		t.Fatalf("test fixture should exceed the observed 34,275 byte history pressure case, got %d bytes", got)
	}
	if !responsesWebSocketHistoryExceedsThreshold(items, cfg) {
		t.Fatalf("default websocket auto-compaction should trigger for 10 items / %d raw bytes: %#v", rawMessagesSize(items), cfg)
	}
}

func TestResponsesWebSocketHistoryItemThresholdRequiresReducibleHistory(t *testing.T) {
	items := make([]json.RawMessage, 0, 5)
	for i := 0; i < 5; i++ {
		raw, err := json.Marshal(map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{{
				"type": "input_text",
				"text": fmt.Sprintf("item %d", i),
			}},
		})
		if err != nil {
			t.Fatalf("failed to marshal history item: %v", err)
		}
		items = append(items, raw)
	}

	cfg := ResponsesWebSocketConfig{
		AutoCompactMaxItems: 3,
		AutoCompactMaxBytes: 1 << 20,
		AutoCompactKeepTail: 4,
	}
	if responsesWebSocketHistoryExceedsThreshold(items, cfg) {
		t.Fatal("item-count threshold should not trigger when compaction cannot reduce history item count")
	}

	cfg.AutoCompactMaxBytes = 1
	if !responsesWebSocketHistoryExceedsThreshold(items, cfg) {
		t.Fatal("byte threshold should still trigger for short histories that cannot reduce item count")
	}

	cfg.AutoCompactMaxBytes = 1 << 20
	cfg.AutoCompactKeepTail = 2
	if !responsesWebSocketHistoryExceedsThreshold(items, cfg) {
		t.Fatal("item-count threshold should trigger when compaction can reduce history item count")
	}
}

func TestResponsesWebSocketAutoCompactsShortBytePressureHistory(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}
	msg := func(role, text string) json.RawMessage {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return raw(map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": []map[string]string{{"type": contentType, "text": text}},
		})
	}

	var compactInput []map[string]interface{}
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		compactInput = upstreamInputItems(t, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"comp-short-byte-pressure","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"short byte pressure checkpoint"}]}]}`)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 99,
		AutoCompactMaxBytes: 1024,
		AutoCompactKeepTail: 4,
	}

	session := &responsesWebSocketSession{
		ctx: context.Background(),
		historyItems: []json.RawMessage{
			msg("user", strings.Repeat("x", 2048)),
			msg("assistant", "latest answer"),
		},
	}
	request := &responsesWebSocketCreateRequest{Model: "gpt-5.4"}

	if !responsesWebSocketHistoryExceedsThreshold(session.historyItems, handler.responsesWS) {
		t.Fatalf("expected short history to exceed byte threshold despite keep-tail being larger than item count")
	}
	_, compacted, err := session.compactHistory(handler, context.Background(), request, false, nil)
	if err != nil {
		t.Fatalf("compactHistory failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected websocket history to compact under byte pressure")
	}
	if len(compactInput) != 1 {
		t.Fatalf("expected compaction request to summarize the oversized prefix, got %d items: %#v", len(compactInput), compactInput)
	}

	compactedItems := decodeRawMessagesForTest(t, session.historyItems)
	if len(compactedItems) != 2 {
		t.Fatalf("expected checkpoint plus latest answer, got %d items: %#v", len(compactedItems), compactedItems)
	}
	if got := requireMessageTextWithRole(t, compactedItems[0], "developer"); !strings.Contains(got, "short byte pressure checkpoint") {
		t.Fatalf("expected checkpoint summary in compacted history, got %q", got)
	}
	if compactedItems[1]["type"] != "message" || compactedItems[1]["role"] != "assistant" {
		t.Fatalf("expected latest assistant answer tail to be retained, got %#v", compactedItems[1])
	}
	if got := inputTextFromMessage(t, compactedItems[1]); got != "latest answer" {
		t.Fatalf("expected latest answer tail to be retained, got %q", got)
	}
}

func TestResponsesWebSocketCompactHistoryStripsInternalChatMetadataBeforeUpstream(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}

	var compactInput []map[string]interface{}
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", r.URL.Path)
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		if strings.Contains(string(bodyBytes), responsesInternalChatMessageMetadataPassthroughField) {
			t.Fatalf("compact upstream body retained internal chat metadata passthrough: %s", bodyBytes)
		}

		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		compactInput = upstreamInputItems(t, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"comp-strip-metadata","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"metadata-free checkpoint"}]}]}`)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxBytes: 1,
		AutoCompactKeepTail: 1,
	}

	session := &responsesWebSocketSession{
		ctx: context.Background(),
		historyItems: []json.RawMessage{
			raw(map[string]interface{}{
				"type":    "message",
				"role":    "user",
				"content": []map[string]string{{"type": "input_text", "text": "old question"}},
				responsesInternalChatMessageMetadataPassthroughField: map[string]string{"turn_id": "turn-old"},
				"nested": map[string]interface{}{responsesInternalChatMessageMetadataPassthroughField: map[string]string{"turn_id": "nested-old"}},
			}),
			raw(map[string]interface{}{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]string{{"type": "output_text", "text": "latest answer"}},
				responsesInternalChatMessageMetadataPassthroughField: map[string]string{"turn_id": "turn-tail"},
			}),
		},
	}
	request := &responsesWebSocketCreateRequest{Model: "gpt-5.4"}

	_, compacted, err := session.compactHistory(handler, context.Background(), request, false, nil)
	if err != nil {
		t.Fatalf("compactHistory failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected websocket history to compact")
	}
	if len(compactInput) != 1 {
		t.Fatalf("expected one compacted prefix item, got %d items: %#v", len(compactInput), compactInput)
	}
	if got := inputTextFromMessage(t, compactInput[0]); got != "old question" {
		t.Fatalf("expected compacted prefix content to survive, got %q", got)
	}
}

func TestResponsesWebSocketCompactHistoryRewritesPriorSyntheticCheckpoint(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}
	msg := func(role, text string) json.RawMessage {
		return raw(map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": []map[string]string{{"type": "input_text", "text": text}},
		})
	}

	priorSummary := "prior proxy-owned checkpoint"
	var compactInput []map[string]interface{}
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		compactInput = upstreamInputItems(t, body)
		for _, item := range compactInput {
			if item["type"] == "compaction" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":{"message":"encrypted content could not be verified","code":"invalid_request_body"}}`)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"comp-prior-checkpoint","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"merged checkpoint summary"}]}]}`)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 2,
		AutoCompactMaxBytes: 1 << 20,
		AutoCompactKeepTail: 1,
	}

	session := &responsesWebSocketSession{
		ctx: context.Background(),
		historyItems: []json.RawMessage{
			raw(map[string]interface{}{"type": "compaction", "encrypted_content": encodeSyntheticCompaction(priorSummary)}),
			msg("user", "after checkpoint"),
			msg("assistant", "latest answer"),
		},
	}
	request := &responsesWebSocketCreateRequest{Model: "gpt-5.4"}

	_, compacted, err := session.compactHistory(handler, context.Background(), request, false, nil)
	if err != nil {
		t.Fatalf("compactHistory failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected websocket history to compact")
	}

	if len(compactInput) != 2 {
		t.Fatalf("expected prior checkpoint plus pre-tail message in compact request, got %d items: %#v", len(compactInput), compactInput)
	}
	if got := requireMessageTextWithRole(t, compactInput[0], "developer"); !strings.Contains(got, priorSummary) {
		t.Fatalf("expected prior synthetic checkpoint to be rewritten before upstream compaction, got %q", got)
	}
	if got := inputTextFromMessage(t, compactInput[1]); got != "after checkpoint" {
		t.Fatalf("expected pre-tail user message to be preserved, got %q", got)
	}
}

func TestResponsesWebSocketCompactHistoryKeepsToolPairsAcrossBoundary(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}
	msg := func(role, text string) json.RawMessage {
		return raw(map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": []map[string]string{{"type": "input_text", "text": text}},
		})
	}

	var compactInput []map[string]interface{}
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		compactInput = upstreamInputItems(t, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"comp-tool-pair","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}]}`)
	})
	historyItems := []json.RawMessage{
		msg("user", "run a command"),
		raw(map[string]interface{}{"type": "function_call", "call_id": "call-1", "name": "shell", "arguments": "{}"}),
		msg("assistant", "thinking between call and output"),
		raw(map[string]interface{}{"type": "function_call_output", "call_id": "call-1", "output": "done"}),
		msg("assistant", "command finished"),
	}
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 3,
		AutoCompactMaxBytes: rawMessagesSize(historyItems) - 1,
		AutoCompactKeepTail: 2,
	}
	session := &responsesWebSocketSession{
		ctx:          context.Background(),
		historyItems: historyItems,
	}
	request := &responsesWebSocketCreateRequest{Model: "gpt-5.4"}

	_, compacted, err := session.compactHistory(handler, context.Background(), request, false, nil)
	if err != nil {
		t.Fatalf("compactHistory failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected websocket history to compact")
	}

	if len(compactInput) != 1 {
		t.Fatalf("expected only pre-pair history to be compacted, got %d items: %#v", len(compactInput), compactInput)
	}
	if got := inputTextFromMessage(t, compactInput[0]); got != "run a command" {
		t.Fatalf("expected compacted prefix to contain first user message, got %q", got)
	}

	compactedItems := decodeRawMessagesForTest(t, session.historyItems)
	if len(compactedItems) != 5 {
		t.Fatalf("expected checkpoint plus intact call/output tail, got %d items: %#v", len(compactedItems), compactedItems)
	}
	if got := requireMessageTextWithRole(t, compactedItems[0], "developer"); !strings.Contains(got, "checkpoint summary") {
		t.Fatalf("expected checkpoint summary in compacted history, got %q", got)
	}
	if compactedItems[1]["type"] != "function_call" || compactedItems[1]["call_id"] != "call-1" {
		t.Fatalf("expected retained function_call for call-1, got %#v", compactedItems[1])
	}
	if compactedItems[3]["type"] != "function_call_output" || compactedItems[3]["call_id"] != "call-1" {
		t.Fatalf("expected retained function_call_output for call-1, got %#v", compactedItems[3])
	}
}

func TestResponsesWebSocketAutoCompactReducesTailWhenToolOutputDominatesBytes(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}
	msg := func(role, text string) json.RawMessage {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return raw(map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": []map[string]string{{"type": contentType, "text": text}},
		})
	}

	var compactInput []map[string]interface{}
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		compactInput = upstreamInputItems(t, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"comp-tool-output-pressure","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tool output checkpoint"}]}]}`)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 99,
		AutoCompactMaxBytes: 32 << 10,
		AutoCompactKeepTail: 4,
	}

	session := &responsesWebSocketSession{
		ctx: context.Background(),
		historyItems: []json.RawMessage{
			msg("user", "seed"),
			raw(map[string]interface{}{"type": "function_call", "call_id": "call-pressure-1", "name": "shell_command", "arguments": `{"command":"cat huge.log"}`}),
			raw(map[string]interface{}{"type": "function_call_output", "call_id": "call-pressure-1", "output": strings.Repeat("tool-output-line\n", 5000)}),
			msg("assistant", "tool output captured"),
			msg("user", "latest question"),
			msg("assistant", "latest answer"),
		},
	}
	originalBytes := rawMessagesSize(session.historyItems)
	request := &responsesWebSocketCreateRequest{Model: "gpt-5.4"}

	_, compacted, err := session.compactHistory(handler, context.Background(), request, false, nil)
	if err != nil {
		t.Fatalf("compactHistory failed: %v", err)
	}
	if !compacted {
		t.Fatal("expected websocket history to compact")
	}
	if len(compactInput) != 4 {
		t.Fatalf("expected auto-compaction to shrink tail and summarize the tool-output pair, got %d compact input items: %#v", len(compactInput), compactInput)
	}
	if compactInput[1]["type"] != "function_call" || compactInput[2]["type"] != "function_call_output" {
		t.Fatalf("expected compact input to include intact tool call/output pair, got %#v", compactInput)
	}
	if got := rawMessagesSize(session.historyItems); got >= originalBytes {
		t.Fatalf("expected compacted history to reduce bytes below %d, got %d", originalBytes, got)
	}

	compactedItems := decodeRawMessagesForTest(t, session.historyItems)
	if len(compactedItems) != 3 {
		t.Fatalf("expected checkpoint plus latest user/assistant tail, got %d items: %#v", len(compactedItems), compactedItems)
	}
	if got := requireMessageTextWithRole(t, compactedItems[0], "developer"); !strings.Contains(got, "tool output checkpoint") {
		t.Fatalf("expected checkpoint summary in compacted history, got %q", got)
	}
	if got := inputTextFromMessage(t, compactedItems[1]); got != "latest question" {
		t.Fatalf("expected latest question tail to be retained, got %q", got)
	}
	if got := inputTextFromMessage(t, compactedItems[2]); got != "latest answer" {
		t.Fatalf("expected latest answer tail to be retained, got %q", got)
	}
}

func TestHandleResponsesWebSocket_CompactsOversizedReplayAndRetries(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 4)
	var normalRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}

		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamRequestsMu.Unlock()

		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"comp-413","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary after 413"}]}],"usage":{"input_tokens":700,"output_tokens":90,"total_tokens":790}}`)
			return
		}

		requestNumber := normalRequests.Add(1)
		switch requestNumber {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"first\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-2\",\"content\":[{\"type\":\"output_text\",\"text\":\"second\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-3\",\"content\":[{\"type\":\"output_text\",\"text\":\"third\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens\":1,\"output_tokens_details\":null,\"total_tokens\":6}}}\n\n")
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, `{"error":{"message":"failed to parse request","code":"payload_too_large"}}`)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":8,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":10}}}\n\n")
		default:
			t.Fatalf("unexpected normal upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "second turn"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	completed := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected retried response.created event, got %v", created["type"])
	}
	if completed["type"] != "response.completed" {
		t.Fatalf("expected retried response.completed event, got %v", completed["type"])
	}

	deadline := time.Now().Add(2 * time.Second)
	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	for len(requests) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("expected 4 upstream requests (turn + 413 + compaction + retry), got %d", len(requests))
		}
		time.Sleep(10 * time.Millisecond)
		requests = snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	}

	initialReplayInput := upstreamInputItems(t, requests[1])
	if len(initialReplayInput) != 5 {
		t.Fatalf("expected oversized replay to include full history plus latest input, got %d items", len(initialReplayInput))
	}
	if got := inputTextFromMessage(t, initialReplayInput[0]); got != "first turn" {
		t.Fatalf("expected oversized replay to start with original user turn, got %q", got)
	}

	compactionInput := upstreamInputItems(t, requests[2])
	if len(compactionInput) != 2 {
		t.Fatalf("expected 413 compaction request to summarize only the old prefix, got %d items", len(compactionInput))
	}
	if got := inputTextFromMessage(t, compactionInput[0]); got != "first turn" {
		t.Fatalf("expected compaction request to preserve the oldest user turn, got %q", got)
	}

	retriedInput := upstreamInputItems(t, requests[3])
	if len(retriedInput) != 4 {
		t.Fatalf("expected retried request to use compacted history plus latest input, got %d items", len(retriedInput))
	}
	if got := requireMessageTextWithRole(t, retriedInput[0], "developer"); !strings.Contains(got, "checkpoint summary after 413") {
		t.Fatalf("expected retried request to start with compacted checkpoint, got %q", got)
	}
	if got := inputTextFromMessage(t, retriedInput[3]); got != "second turn" {
		t.Fatalf("expected retried request to keep latest user turn, got %q", got)
	}

	// The initial and retried client turns report 5+1 and 8+2 tokens. The
	// internal 413 compaction reports 700+90 and must be added to the second
	// client turn without becoming a third dashboard request.
	deadlineStats := time.Now().Add(2 * time.Second)
	for {
		snap := handler.stats.snapshot()
		if snap.Totals.TotalTokens >= 806 {
			if snap.Totals.Requests != 2 || snap.Totals.Errors != 0 {
				t.Fatalf("413 fallback stats = requests:%d errors:%d, want 2/0", snap.Totals.Requests, snap.Totals.Errors)
			}
			if snap.Totals.PromptTokens != 713 || snap.Totals.CompletionTokens != 93 || snap.Totals.TotalTokens != 806 {
				t.Fatalf("413 fallback usage = prompt:%d completion:%d total:%d, want 713/93/806", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
			}
			if snap.Totals.CachedTokens != 5 || snap.Totals.ReasoningTokens != 1 {
				t.Fatalf("413 fallback detail usage = cached:%d reasoning:%d, want 5/1", snap.Totals.CachedTokens, snap.Totals.ReasoningTokens)
			}
			break
		}
		if time.Now().After(deadlineStats) {
			t.Fatalf("413 fallback stats never reached expected usage: %+v", snap.Totals)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHandleResponsesWebSocket_CompactionErrorAfterUsageCountsFailedCreateOnce(t *testing.T) {
	var normalRequests atomic.Int32
	var compactionRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			compactionRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"comp-invalid","object":"response","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}`)
			return
		}

		switch requestNumber := normalRequests.Add(1); requestNumber {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-before-compact-error\"}}\n\n")
			for i := 1; i <= 3; i++ {
				_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-%d\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer-%d\"}]}}\n\n", i, i)
			}
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-before-compact-error\",\"usage\":{\"input_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens\":1,\"output_tokens_details\":null,\"total_tokens\":6}}}\n\n")
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, `{"error":{"message":"request too large","code":"payload_too_large"}}`)
		default:
			t.Fatalf("unexpected normal upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "seed history"}},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}
	firstCreated := mustReadWebSocketJSON(t, conn)
	for range 4 {
		_ = mustReadWebSocketJSON(t, conn)
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "trigger oversized replay"}},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("second frame type = %v, want error", errFrame["type"])
	}
	if got, _ := errFrame["status_code"].(float64); got != float64(http.StatusRequestEntityTooLarge) {
		t.Fatalf("error status = %v, want 413", errFrame["status_code"])
	}
	if got := normalRequests.Load(); got != 2 {
		t.Fatalf("normal upstream requests = %d, want 2", got)
	}
	if got := compactionRequests.Load(); got != 1 {
		t.Fatalf("compaction upstream requests = %d, want 1", got)
	}

	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 2 || snap.Totals.Errors != 1 {
		t.Fatalf("compaction-error stats = requests:%d errors:%d, want 2/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 105 || snap.Totals.CompletionTokens != 21 || snap.Totals.TotalTokens != 126 {
		t.Fatalf("compaction-error usage = prompt:%d completion:%d total:%d, want 105/21/126", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
	}
	if snap.Totals.CachedTokens != 2 || snap.Totals.ReasoningTokens != 0 {
		t.Fatalf("compaction-error detail usage = cached:%d reasoning:%d, want 2/0", snap.Totals.CachedTokens, snap.Totals.ReasoningTokens)
	}
}

func TestHandleResponsesWebSocket_CompactedRetryFailureCombinesTerminalAndInternalUsage(t *testing.T) {
	var normalRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"comp-before-failure","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint before failed retry"}]}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}`)
			return
		}

		switch requestNumber := normalRequests.Add(1); requestNumber {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-before-failed-retry\"}}\n\n")
			for i := 1; i <= 3; i++ {
				_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-%d\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer-%d\"}]}}\n\n", i, i)
			}
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-before-failed-retry\",\"usage\":{\"input_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens\":1,\"output_tokens_details\":null,\"total_tokens\":6}}}\n\n")
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, `{"error":{"message":"request too large","code":"payload_too_large"}}`)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-failed-after-compact\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-failed-after-compact\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"},\"usage\":{\"input_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":11}}}\n\n")
		default:
			t.Fatalf("unexpected normal upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "seed history"}},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}
	firstCreated := mustReadWebSocketJSON(t, conn)
	for range 4 {
		_ = mustReadWebSocketJSON(t, conn)
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "trigger failed compact retry"}},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	var errFrame map[string]interface{}
	for _, wantType := range []string{"response.created", "response.failed", "error"} {
		frame := mustReadWebSocketJSON(t, conn)
		if frame["type"] != wantType {
			t.Fatalf("frame type = %v, want %s", frame["type"], wantType)
		}
		if wantType == "error" {
			errFrame = frame
		}
	}
	if got, _ := errFrame["status_code"].(float64); got != float64(http.StatusTooManyRequests) {
		t.Fatalf("failed compact retry status = %v, want 429", errFrame["status_code"])
	}

	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 2 || snap.Totals.Errors != 1 {
		t.Fatalf("failed compact retry stats = requests:%d errors:%d, want 2/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 114 || snap.Totals.CompletionTokens != 23 || snap.Totals.TotalTokens != 137 {
		t.Fatalf("failed compact retry usage = prompt:%d completion:%d total:%d, want 114/23/137", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
	}
	if snap.Totals.CachedTokens != 5 || snap.Totals.ReasoningTokens != 1 {
		t.Fatalf("failed compact retry detail usage = cached:%d reasoning:%d, want 5/1", snap.Totals.CachedTokens, snap.Totals.ReasoningTokens)
	}
	if len(snap.StatusCodes) != 1 || snap.StatusCodes[0].Label != "429" || snap.StatusCodes[0].Count != 1 {
		t.Fatalf("failed compact retry status accounting = %+v, want one 429", snap.StatusCodes)
	}
}

func TestResponsesWebSocketMaybeRetryCompactedCreateRequestCapsOriginal413Body(t *testing.T) {
	var upstreamRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"compaction failed"}}`)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}

	largeBody := strings.Repeat("x", compactUpstreamErrorBodySize+1024)
	resp := &http.Response{
		StatusCode:    http.StatusRequestEntityTooLarge,
		Header:        http.Header{"Content-Length": []string{fmt.Sprintf("%d", len(largeBody))}},
		Body:          io.NopCloser(strings.NewReader(largeBody)),
		ContentLength: int64(len(largeBody)),
	}
	session := &responsesWebSocketSession{
		ctx: context.Background(),
		historyItems: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`),
			json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}`),
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}`),
		},
	}
	request := &responsesWebSocketCreateRequest{
		Model:              "gpt-5.4",
		PreviousResponseID: "resp-prev",
		Input: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest"}]}`),
		},
	}

	got, err := session.maybeRetryCompactedCreateRequest(handler, context.Background(), request, resp, true, nil)
	if err != nil {
		t.Fatalf("maybeRetryCompactedCreateRequest returned error: %v", err)
	}
	if got == nil {
		t.Fatal("expected original response clone, got nil")
	}
	if got.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 response, got %d", got.StatusCode)
	}
	if upstreamRequests.Load() == 0 {
		t.Fatal("expected compaction attempt after initial 413")
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("failed to read cloned response body: %v", err)
	}
	if len(body) != compactUpstreamErrorBodySize {
		t.Fatalf("expected cloned 413 body capped at %d bytes, got %d", compactUpstreamErrorBodySize, len(body))
	}
	if got.ContentLength != int64(compactUpstreamErrorBodySize) {
		t.Fatalf("expected cloned ContentLength %d, got %d", compactUpstreamErrorBodySize, got.ContentLength)
	}
	if got.Header.Get("Content-Length") != "" {
		t.Fatalf("expected stale Content-Length header to be removed, got %q", got.Header.Get("Content-Length"))
	}
}

func TestHandleResponsesWebSocket_Compacted413BodyCancellationPreservesStatus(t *testing.T) {
	for _, stage := range []string{"initial", "retry"} {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			stalledBodyCh := make(chan *stalledWebSocket413Body, 1)
			var normalCalls atomic.Int32
			handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
				bodyBytes, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var payload map[string]interface{}
				if err := json.Unmarshal(bodyBytes, &payload); err != nil {
					return nil, err
				}
				if instructions, _ := payload["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(
							`{"id":"comp-race","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint"}]}]}`)),
						Request: req,
					}, nil
				}

				call := normalCalls.Add(1)
				if stage == "retry" && call == 1 {
					return &http.Response{
						StatusCode: http.StatusRequestEntityTooLarge,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"initial replay too large"}}`)),
						Request:    req,
					}, nil
				}

				body := newStalledWebSocket413Body(req.Context(), `{"error":{"message":"partial 413 detail`)
				stalledBodyCh <- body
				return &http.Response{
					StatusCode: http.StatusRequestEntityTooLarge,
					Header: http.Header{
						"Content-Type":   []string{"application/problem+json"},
						"Content-Length": []string{"999"},
						"X-413-Stage":    []string{stage},
					},
					Body:          body,
					ContentLength: 999,
					Request:       req,
				}, nil
			})
			handler.maxRetries = 1
			handler.stats = newStatsCollector()
			handler.responsesWS = ResponsesWebSocketConfig{
				DisableAutoCompact:  true,
				AutoCompactKeepTail: 2,
			}

			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			defer func() { _ = clientConn.Close() }()
			defer func() { _ = serverConn.Close() }()
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			session.historyItems = []json.RawMessage{
				json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`),
				json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
				json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}`),
			}
			session.historyBytes = rawMessagesSize(session.historyItems)
			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest([]interface{}{
				map[string]interface{}{
					"type": "message", "role": "user",
					"content": []map[string]string{{"type": "input_text", "text": "latest"}},
				},
			}))
			request.PreviousResponseID = "resp-prev"
			session.lastResponseID = request.PreviousResponseID
			session.lastSignature = request.signature()

			done := make(chan error, 1)
			go func() { done <- session.handleCreateRequest(handler, request) }()
			var stalledBody *stalledWebSocket413Body
			select {
			case stalledBody = <-stalledBodyCh:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for stalled 413 response")
			}
			waitForLifecycleSignal(t, stalledBody.blocked, "stalled websocket 413 body read")
			handler.BeginShutdown()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("handleCreateRequest error = %v, want graceful shutdown cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for websocket 413 shutdown")
			}
			assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusRequestEntityTooLarge)
		})
	}
}

func TestHandleResponsesWebSocket_ReducesKeepTailWhenCompactedReplayStill413s(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 6)
	var normalRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}

		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamRequestsMu.Unlock()

		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			input := upstreamInputItems(t, body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"comp-dynamic-tail-ws","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ws summary for %d items"}]}]}`, len(input))
			return
		}

		requestNumber := normalRequests.Add(1)
		switch requestNumber {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"first\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-2\",\"content\":[{\"type\":\"output_text\",\"text\":\"second\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-3\",\"content\":[{\"type\":\"output_text\",\"text\":\"third\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, `{"error":{"message":"failed to parse request","code":"payload_too_large"}}`)
		case 3:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, `{"error":{"message":"compacted replay still too large","code":"payload_too_large"}}`)
		case 4:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected normal upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "second turn"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	completed := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected retried response.created event, got %v", created["type"])
	}
	if completed["type"] != "response.completed" {
		t.Fatalf("expected retried response.completed event, got %v", completed["type"])
	}

	deadline := time.Now().Add(2 * time.Second)
	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	for len(requests) < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("expected 6 upstream requests (turn + 413 + compact + 413 + compact + retry), got %d", len(requests))
		}
		time.Sleep(10 * time.Millisecond)
		requests = snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	}
	if len(requests) != 6 {
		t.Fatalf("expected exactly 6 upstream requests (turn + 413 + compact + 413 + compact + retry), got %d", len(requests))
	}

	initialReplayInput := upstreamInputItems(t, requests[1])
	if len(initialReplayInput) != 5 {
		t.Fatalf("expected oversized replay to include full history plus latest input, got %d items", len(initialReplayInput))
	}
	if got := inputTextFromMessage(t, initialReplayInput[0]); got != "first turn" {
		t.Fatalf("expected oversized replay to start with original user turn, got %q", got)
	}

	firstCompactionInput := upstreamInputItems(t, requests[2])
	if len(firstCompactionInput) != 2 {
		t.Fatalf("expected first compaction to summarize two items with keep-tail 2, got %d", len(firstCompactionInput))
	}
	firstRetriedInput := upstreamInputItems(t, requests[3])
	if len(firstRetriedInput) != 4 {
		t.Fatalf("expected first retry to keep checkpoint plus 2 history tail items plus latest input, got %d", len(firstRetriedInput))
	}
	if got := requireMessageTextWithRole(t, firstRetriedInput[0], "developer"); !strings.Contains(got, "ws summary for 2 items") {
		t.Fatalf("expected first retry checkpoint for two-item summary, got %q", got)
	}

	secondCompactionInput := upstreamInputItems(t, requests[4])
	if len(secondCompactionInput) != 3 {
		t.Fatalf("expected second compaction to reduce keep-tail to 1 and summarize three items, got %d", len(secondCompactionInput))
	}
	secondRetriedInput := upstreamInputItems(t, requests[5])
	if len(secondRetriedInput) != 3 {
		t.Fatalf("expected second retry to keep checkpoint plus one history tail item plus latest input, got %d", len(secondRetriedInput))
	}
	if got := requireMessageTextWithRole(t, secondRetriedInput[0], "developer"); !strings.Contains(got, "ws summary for 3 items") {
		t.Fatalf("expected second retry checkpoint for three-item summary, got %q", got)
	}
	if secondRetriedInput[1]["role"] != "assistant" {
		t.Fatalf("expected reduced retry to keep assistant tail item, got %#v", secondRetriedInput[1])
	}
	if got := inputTextFromMessage(t, secondRetriedInput[1]); got != "third" {
		t.Fatalf("expected reduced retry to keep newest assistant tail item, got %q", got)
	}
	if got := inputTextFromMessage(t, secondRetriedInput[2]); got != "second turn" {
		t.Fatalf("expected reduced retry to preserve latest user turn, got %q", got)
	}
}

func TestHandleResponsesWebSocket_TurnStateDeltaReplayUsesOnlyCurrentInputAndIgnoresClientTurnStateHeader(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 2)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		requestCount := len(upstreamRequests)
		upstreamRequestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestCount {
		case 1:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("expected first request to omit turn state, got %q", got)
			}
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "turn-state-1" {
				t.Fatalf("expected second request to include turn state, got %q", got)
			}
			w.Header().Set("X-Codex-Turn-State", "turn-state-2")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestCount)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	first["client_metadata"] = map[string]string{
		"ws_request_header_x-codex-turn-state": "client-state-first",
	}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "follow up"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	second["client_metadata"] = map[string]string{
		"ws_request_header_x-codex-turn-state": "client-state-second",
	}
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}

	secondInput := upstreamInputItems(t, requests[1])
	if len(secondInput) != 1 {
		t.Fatalf("expected delta replay to send only latest input, got %d items", len(secondInput))
	}
	if got := inputTextFromMessage(t, secondInput[0]); got != "follow up" {
		t.Fatalf("expected delta replay to forward only latest user turn, got %q", got)
	}
}

func TestHandleResponsesWebSocket_TurnStateDeltaIgnoresUpgradeTurnStateBeforeUpstreamState(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 1)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Codex-Turn-State"); got != "" {
			t.Fatalf("expected stale upgrade turn state not to be forwarded, got %q", got)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamRequestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{
		"X-Codex-Turn-State": []string{"stale-upgrade-turn-state"},
	})
	defer func() { _ = conn.Close() }()

	warmup := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "warmup from this workspace"},
			},
		},
	})
	warmup["generate"] = false
	if err := conn.WriteJSON(warmup); err != nil {
		t.Fatalf("failed to write warmup request: %v", err)
	}

	warmupCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	followUp := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "follow up"},
			},
		},
	})
	followUp["previous_response_id"] = websocketResponseID(t, warmupCreated)
	if err := conn.WriteJSON(followUp); err != nil {
		t.Fatalf("failed to write follow-up request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 1 {
		t.Fatalf("expected exactly one upstream request after local warmup, got %d", len(requests))
	}

	input := upstreamInputItems(t, requests[0])
	if len(input) != 2 {
		t.Fatalf("expected full replay after local warmup, got %d input items", len(input))
	}
	if got := inputTextFromMessage(t, input[0]); got != "warmup from this workspace" {
		t.Fatalf("expected replay to include local warmup from this workspace, got %q", got)
	}
	if got := inputTextFromMessage(t, input[1]); got != "follow up" {
		t.Fatalf("expected replay to include follow-up, got %q", got)
	}
}

func TestHandleResponsesWebSocket_TurnStateDeltaClearsForRootLocalWarmup(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 2)
	upstreamTurnStates := make([]string, 0, 2)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamTurnStates = append(upstreamTurnStates, r.Header.Get("X-Codex-Turn-State"))
		requestCount := len(upstreamRequests)
		upstreamRequestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestCount {
		case 1:
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestCount)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "previous root"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	warmup := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "new root warmup"},
			},
		},
	})
	warmup["generate"] = false
	if err := conn.WriteJSON(warmup); err != nil {
		t.Fatalf("failed to write root warmup request: %v", err)
	}
	warmupCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	followUp := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "follow up"},
			},
		},
	})
	followUp["previous_response_id"] = websocketResponseID(t, warmupCreated)
	if err := conn.WriteJSON(followUp); err != nil {
		t.Fatalf("failed to write follow-up request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}

	upstreamRequestsMu.Lock()
	turnStates := append([]string(nil), upstreamTurnStates...)
	upstreamRequestsMu.Unlock()
	if turnStates[0] != "" {
		t.Fatalf("expected first root request to omit turn state, got %q", turnStates[0])
	}
	if turnStates[1] != "" {
		t.Fatalf("expected follow-up after root warmup to clear previous turn state, got %q", turnStates[1])
	}

	followUpInput := upstreamInputItems(t, requests[1])
	if len(followUpInput) != 2 {
		t.Fatalf("expected full replay after root warmup, got %d input items", len(followUpInput))
	}
	if got := inputTextFromMessage(t, followUpInput[0]); got != "new root warmup" {
		t.Fatalf("expected replay to include root warmup, got %q", got)
	}
	if got := inputTextFromMessage(t, followUpInput[1]); got != "follow up" {
		t.Fatalf("expected replay to include follow-up, got %q", got)
	}
}

func TestHandleResponsesWebSocket_IgnoresUpgradeTurnMetadataWithoutCreateMetadata(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if got := r.Header.Get("X-Codex-Turn-Metadata"); got != "" {
			t.Fatalf("expected stale upgrade turn metadata not to be forwarded, got %q", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{
		"X-Codex-Turn-Metadata": []string{`{"turn_id":"stale-turn","workspaces":{"/wrong/repo":{"has_changes":true}}}`},
	})
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "current workspace request"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request, got %d", got)
	}
}

func TestHandleResponsesWebSocket_TurnStateDeltaClearsWhenTurnMetadataChanges(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 2)
	upstreamTurnStates := make([]string, 0, 2)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamTurnStates = append(upstreamTurnStates, r.Header.Get("X-Codex-Turn-State"))
		requestCount := len(upstreamRequests)
		upstreamRequestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestCount {
		case 1:
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"first output\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestCount)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	first["client_metadata"] = map[string]string{
		"x-codex-turn-metadata": `{"turn_id":"turn-1"}`,
	}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "second turn"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	second["client_metadata"] = map[string]string{
		"x-codex-turn-metadata": `{"turn_id":"turn-2"}`,
	}
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}

	upstreamRequestsMu.Lock()
	turnStates := append([]string(nil), upstreamTurnStates...)
	upstreamRequestsMu.Unlock()
	if len(turnStates) != 2 {
		t.Fatalf("expected 2 recorded turn-state headers, got %d", len(turnStates))
	}
	if turnStates[0] != "" {
		t.Fatalf("expected first request to omit turn state, got %q", turnStates[0])
	}
	if turnStates[1] != "" {
		t.Fatalf("expected second turn to clear previous turn state, got %q", turnStates[1])
	}

	secondInput := upstreamInputItems(t, requests[1])
	if len(secondInput) != 3 {
		t.Fatalf("expected changed turn metadata to force full replay, got %d items", len(secondInput))
	}
	if got := inputTextFromMessage(t, secondInput[0]); got != "first turn" {
		t.Fatalf("expected replay to include first turn input, got %q", got)
	}
	if got := inputTextFromMessage(t, secondInput[1]); got != "first output" {
		t.Fatalf("expected replay to include first turn output, got %q", got)
	}
	if got := inputTextFromMessage(t, secondInput[2]); got != "second turn" {
		t.Fatalf("expected replay to include second turn input, got %q", got)
	}
}

func TestHandleResponsesWebSocket_TurnStateDeltaKeepsStateWhenTurnMetadataEnrichesSameTurn(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 2)
	upstreamTurnStates := make([]string, 0, 2)
	upstreamTurnMetadata := make([]string, 0, 2)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamTurnStates = append(upstreamTurnStates, r.Header.Get("X-Codex-Turn-State"))
		upstreamTurnMetadata = append(upstreamTurnMetadata, r.Header.Get("X-Codex-Turn-Metadata"))
		requestCount := len(upstreamRequests)
		upstreamRequestsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestCount {
		case 1:
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"first output\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "turn-state-1" {
				t.Fatalf("expected enriched same-turn metadata to keep turn state, got %q", got)
			}
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestCount)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	firstMetadata := `{"turn_id":"turn-1","thread_source":"user","sandbox":"workspace-write"}`
	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	first["client_metadata"] = map[string]string{
		"x-codex-turn-metadata": firstMetadata,
	}
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	enrichedMetadata := `{"turn_id":"turn-1","thread_source":"user","sandbox":"workspace-write","workspaces":[{"root_path":"/tmp/repo","latest_git_commit_hash":"abc123","associated_remote_urls":["git@github.com:openai/codex.git"],"has_changes":true}]}`
	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "follow up"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	second["client_metadata"] = map[string]string{
		"x-codex-turn-metadata": enrichedMetadata,
	}
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}

	upstreamRequestsMu.Lock()
	turnStates := append([]string(nil), upstreamTurnStates...)
	turnMetadata := append([]string(nil), upstreamTurnMetadata...)
	upstreamRequestsMu.Unlock()
	if len(turnStates) != 2 {
		t.Fatalf("expected 2 recorded turn-state headers, got %d", len(turnStates))
	}
	if turnStates[0] != "" {
		t.Fatalf("expected first request to omit turn state, got %q", turnStates[0])
	}
	if turnStates[1] != "turn-state-1" {
		t.Fatalf("expected second request to keep turn state, got %q", turnStates[1])
	}
	if turnMetadata[0] != firstMetadata {
		t.Fatalf("expected first request metadata %q, got %q", firstMetadata, turnMetadata[0])
	}
	if turnMetadata[1] != enrichedMetadata {
		t.Fatalf("expected second request metadata %q, got %q", enrichedMetadata, turnMetadata[1])
	}

	secondInput := upstreamInputItems(t, requests[1])
	if len(secondInput) != 1 {
		t.Fatalf("expected same-turn metadata enrichment to keep delta replay, got %d input items", len(secondInput))
	}
	if got := inputTextFromMessage(t, secondInput[0]); got != "follow up" {
		t.Fatalf("expected delta replay to include only follow-up input, got %q", got)
	}
}

func TestHandleResponsesWebSocket_TurnStateDeltaFallsBackToFullReplay(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 3)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		requestCount := len(upstreamRequests)
		upstreamRequestsMu.Unlock()

		switch requestCount {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "turn-state-1" {
				t.Fatalf("expected delta attempt to include prior turn state, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"stale turn state","code":"invalid_turn_state"}}`)
		case 3:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("expected full replay fallback to omit turn state, got %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestCount)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}

	firstCreated := mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "follow up"},
			},
		},
	})
	second["previous_response_id"] = websocketResponseID(t, firstCreated)
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second request: %v", err)
	}

	created := mustReadWebSocketJSONSkipMetadata(t, conn)
	completed := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected fallback response.created event, got %v", created["type"])
	}
	if completed["type"] != "response.completed" {
		t.Fatalf("expected fallback response.completed event, got %v", completed["type"])
	}

	requests := snapshotResponsesWebSocketRequests(&upstreamRequestsMu, upstreamRequests)
	if len(requests) != 3 {
		t.Fatalf("expected 3 upstream requests including fallback, got %d", len(requests))
	}

	fallbackInput := upstreamInputItems(t, requests[2])
	if len(fallbackInput) != 3 {
		t.Fatalf("expected fallback replay to include full history plus latest input, got %d items", len(fallbackInput))
	}
	if got := inputTextFromMessage(t, fallbackInput[0]); got != "first turn" {
		t.Fatalf("expected fallback replay to include first user turn, got %q", got)
	}
	if got := inputTextFromMessage(t, fallbackInput[2]); got != "follow up" {
		t.Fatalf("expected fallback replay to include latest user turn, got %q", got)
	}
}

func TestHandleResponsesWebSocket_ResponseFailedKeepsSessionOpen(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-fail\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-fail\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"context too long\"}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-retry\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-retry\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestNumber)
		}
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	// Should receive the response.created event.
	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}

	// Should receive the response.failed event relayed from upstream.
	failed := mustReadWebSocketJSON(t, conn)
	if failed["type"] != "response.failed" {
		t.Fatalf("expected response.failed, got %v", failed["type"])
	}
	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("expected error frame after response.failed, got %v", errFrame["type"])
	}
	if statusCode, _ := errFrame["status_code"].(float64); statusCode != float64(http.StatusInternalServerError) {
		t.Fatalf("expected error status %d, got %v", http.StatusInternalServerError, errFrame["status_code"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error payload, got %T", errFrame["error"])
	}
	if errPayload["code"] != "context_length_exceeded" {
		t.Fatalf("expected error code context_length_exceeded, got %v", errPayload["code"])
	}
	if errPayload["message"] != "context too long" {
		t.Fatalf("expected error message context too long, got %v", errPayload["message"])
	}

	retry := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "try again"},
			},
		},
	})
	if err := conn.WriteJSON(retry); err != nil {
		t.Fatalf("failed to write retry websocket request: %v", err)
	}

	retryCreated := mustReadWebSocketJSON(t, conn)
	if retryCreated["type"] != "response.created" {
		t.Fatalf("expected retry response.created, got %v", retryCreated["type"])
	}

	retryCompleted := mustReadWebSocketJSON(t, conn)
	if retryCompleted["type"] != "response.completed" {
		t.Fatalf("expected retry response.completed, got %v", retryCompleted["type"])
	}
}

func TestHandleResponsesWebSocket_FirstEventTransientResponseFailedSendsOnlyErrorFrame(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Retry-After", "3")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-rate-limit\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"Too Many Requests\"}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-next\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestNumber)
		}
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("expected error frame for first-event transient failure, got %v", errFrame["type"])
	}
	if statusCode, _ := errFrame["status_code"].(float64); statusCode != float64(http.StatusTooManyRequests) {
		t.Fatalf("expected error status %d, got %v", http.StatusTooManyRequests, errFrame["status_code"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error payload, got %T", errFrame["error"])
	}
	if errPayload["code"] != "too_many_requests" {
		t.Fatalf("expected error code too_many_requests, got %v", errPayload["code"])
	}
	if errPayload["message"] != "Too Many Requests" {
		t.Fatalf("expected error message Too Many Requests, got %v", errPayload["message"])
	}
	headers, ok := errFrame["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected forwarded headers, got %T", errFrame["headers"])
	}
	if headers["Retry-After"] != "3" {
		t.Fatalf("expected Retry-After header 3, got %v", headers["Retry-After"])
	}

	next := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "after rate limit"},
			},
		},
	})
	if err := conn.WriteJSON(next); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}

	nextCreated := mustReadWebSocketJSON(t, conn)
	if nextCreated["type"] != "response.created" {
		t.Fatalf("expected follow-up response.created, got %v", nextCreated["type"])
	}

	nextCompleted := mustReadWebSocketJSON(t, conn)
	if nextCompleted["type"] != "response.completed" {
		t.Fatalf("expected follow-up response.completed, got %v", nextCompleted["type"])
	}
}

func TestHandleResponsesWebSocket_FirstEventTopLevelErrorSendsOnlyErrorFrame(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"too_many_requests\",\"code\":\"no_capacity\",\"message\":\"No capacity is available.\",\"headers\":{\"retry-after-ms\":\"1200\",\"x-request-id\":\"event-ws-req\"}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-next-top-level-error\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next-top-level-error\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestNumber)
		}
	})
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("first frame type = %v, want error", errFrame["type"])
	}
	if statusCode, _ := errFrame["status_code"].(float64); statusCode != float64(http.StatusTooManyRequests) {
		t.Fatalf("status_code = %v, want 429", errFrame["status_code"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error payload type = %T, want object", errFrame["error"])
	}
	if errPayload["type"] != "rate_limit_error" || errPayload["code"] != "no_capacity" || errPayload["message"] != "No capacity is available." {
		t.Fatalf("error payload = %#v, want no_capacity message", errPayload)
	}
	headers, ok := errFrame["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("headers type = %T, want object", errFrame["headers"])
	}
	if headers["Retry-After"] != "2" || headers["X-Request-Id"] != "event-ws-req" {
		t.Fatalf("headers = %#v, want Retry-After=2 and X-Request-Id=event-ws-req", headers)
	}
	assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusTooManyRequests)

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}
	if frame := mustReadWebSocketJSON(t, conn); frame["type"] != "response.created" {
		t.Fatalf("follow-up first frame type = %v, want response.created", frame["type"])
	}
	if frame := mustReadWebSocketJSON(t, conn); frame["type"] != "response.completed" {
		t.Fatalf("follow-up second frame type = %v, want response.completed", frame["type"])
	}
}

func TestHandleResponsesWebSocket_FirstEventRootErrorPreservesDiagnostics(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"invalid_prompt\",\"message\":\"The prompt is invalid.\",\"param\":\"input\",\"sequence_number\":1}\n\n")
	})
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" || errFrame["status_code"] != float64(http.StatusBadRequest) {
		t.Fatalf("error frame = %#v, want status 400", errFrame)
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error payload type = %T, want object", errFrame["error"])
	}
	if errPayload["type"] != "invalid_request_error" || errPayload["code"] != "invalid_prompt" || errPayload["message"] != "The prompt is invalid." || errPayload["param"] != "input" {
		t.Fatalf("error payload = %#v, want canonical root diagnostics", errPayload)
	}
	assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusBadRequest)
}

func TestHandleResponsesWebSocket_FirstEventTransientResponseFailedWithoutUpstreamCodeOmitsErrorCode(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Retry-After", "3")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-rate-limit-no-code\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"Too Many Requests\"}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-next\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestNumber)
		}
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("expected error frame for first-event transient failure, got %v", errFrame["type"])
	}
	if statusCode, _ := errFrame["status_code"].(float64); statusCode != float64(http.StatusTooManyRequests) {
		t.Fatalf("expected error status %d, got %v", http.StatusTooManyRequests, errFrame["status_code"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error payload, got %T", errFrame["error"])
	}
	if _, ok := errPayload["code"]; ok {
		t.Fatalf("expected empty upstream error code to be omitted, got %v", errPayload["code"])
	}
	if errPayload["message"] != "Too Many Requests" {
		t.Fatalf("expected error message Too Many Requests, got %v", errPayload["message"])
	}
	headers, ok := errFrame["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected forwarded headers, got %T", errFrame["headers"])
	}
	if headers["Retry-After"] != "3" {
		t.Fatalf("expected Retry-After header 3, got %v", headers["Retry-After"])
	}

	next := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "after rate limit"},
			},
		},
	})
	if err := conn.WriteJSON(next); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}

	nextCreated := mustReadWebSocketJSON(t, conn)
	if nextCreated["type"] != "response.created" {
		t.Fatalf("expected follow-up response.created, got %v", nextCreated["type"])
	}

	nextCompleted := mustReadWebSocketJSON(t, conn)
	if nextCompleted["type"] != "response.completed" {
		t.Fatalf("expected follow-up response.completed, got %v", nextCompleted["type"])
	}
}

func TestResponsesWebSocketSendWrappedErrorSynthesizesRetryAfterFromQuotaReset(t *testing.T) {
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	headers := http.Header{
		"x-ratelimit-remaining-tokens": []string{"0"},
		"x-ratelimit-reset-tokens":     []string{"62"},
	}
	if err := session.sendWrappedError(http.StatusTooManyRequests, "rate limited", "", headers); err != nil {
		t.Fatalf("sendWrappedError error = %v", err)
	}
	frame := mustReadWebSocketJSON(t, clientConn)
	mapped, ok := frame["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("headers = %T, want object", frame["headers"])
	}
	if got := mapped["Retry-After"]; got != "62" {
		t.Fatalf("Retry-After = %v, want 62", got)
	}
	if got := headers.Get("Retry-After"); got != "" {
		t.Fatalf("source headers mutated with Retry-After = %q", got)
	}
}

func TestResponsesWebSocketErrorHeadersPreservesExistingRetryAfter(t *testing.T) {
	headers := http.Header{
		"Retry-After":                  []string{"3"},
		"x-ratelimit-remaining-tokens": []string{"0"},
		"x-ratelimit-reset-tokens":     []string{"62"},
	}
	got := responsesWebSocketErrorHeaders(http.StatusTooManyRequests, headers)
	if retryAfter := got.Get("Retry-After"); retryAfter != "3" {
		t.Fatalf("Retry-After = %q, want existing value 3", retryAfter)
	}
}

func TestResponsesWebSocketErrorHeadersPreservesMassiveRetryAfter(t *testing.T) {
	headers := http.Header{
		"Retry-After": []string{"10000000000"},
	}
	got := responsesWebSocketErrorHeaders(http.StatusTooManyRequests, headers)
	if retryAfter := got.Get("Retry-After"); retryAfter != "10000000000" {
		t.Fatalf("Retry-After = %q, want preserved value 10000000000", retryAfter)
	}
}

func TestResponsesWebSocketErrorHeadersFallsBackFromInvalidRetryAfter(t *testing.T) {
	headers := http.Header{
		"retry-after":    []string{"not-a-delay"},
		"retry-after-ms": []string{"1200"},
	}

	got := responsesWebSocketErrorHeaders(http.StatusTooManyRequests, headers)
	if retryAfter := got.Get("Retry-After"); retryAfter != "2" {
		t.Fatalf("Retry-After = %q, want fallback value 2; headers=%v", retryAfter, got)
	}
	for key := range got {
		if strings.EqualFold(key, "Retry-After") && key != http.CanonicalHeaderKey("Retry-After") {
			t.Fatalf("non-canonical Retry-After header was not replaced: key=%q headers=%v", key, got)
		}
	}
}

func TestHandleResponsesWebSocket_ResponseIncompleteKeepsSessionOpen(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-inc\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-inc\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-next\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestNumber)
		}
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}

	incomplete := mustReadWebSocketJSON(t, conn)
	if incomplete["type"] != "response.incomplete" {
		t.Fatalf("expected response.incomplete, got %v", incomplete["type"])
	}
	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("expected error frame after response.incomplete, got %v", errFrame["type"])
	}
	if statusCode, _ := errFrame["status_code"].(float64); statusCode != float64(http.StatusConflict) {
		t.Fatalf("expected error status %d, got %v", http.StatusConflict, errFrame["status_code"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error payload, got %T", errFrame["error"])
	}
	if errPayload["code"] != "max_output_tokens" {
		t.Fatalf("expected error code max_output_tokens, got %v", errPayload["code"])
	}
	if errPayload["message"] != "upstream response.incomplete: max_output_tokens" {
		t.Fatalf("expected incomplete error message, got %v", errPayload["message"])
	}

	next := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "continue"},
			},
		},
	})
	if err := conn.WriteJSON(next); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}

	nextCreated := mustReadWebSocketJSON(t, conn)
	if nextCreated["type"] != "response.created" {
		t.Fatalf("expected follow-up response.created, got %v", nextCreated["type"])
	}

	nextCompleted := mustReadWebSocketJSON(t, conn)
	if nextCompleted["type"] != "response.completed" {
		t.Fatalf("expected follow-up response.completed, got %v", nextCompleted["type"])
	}
}

func TestHandleResponsesWebSocket_ResponseFailedOnStalledUpstreamKeepsSessionOpen(t *testing.T) {
	// Regression test: if upstream emits response.failed and then stalls
	// (keeps the body open), the proxy should exit the SSE loop immediately
	// rather than blocking until EOF or the 60-minute timeout, and the
	// websocket session should remain usable for the next turn.
	stallReleased := make(chan struct{})
	var mu sync.Mutex
	upstreamRequests := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamRequests++
		requestNumber := upstreamRequests
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stall\"}}\n\n")
			flusher.Flush()
			_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-stall\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"context too long\"}}}\n\n")
			flusher.Flush()
			// Stall: keep the body open until the test signals release.
			<-stallReleased
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-after-stall\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-after-stall\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected upstream request count %d", requestNumber)
		}
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() {
		_ = conn.Close()
		close(stallReleased)
	}()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}

	failed := mustReadWebSocketJSON(t, conn)
	if failed["type"] != "response.failed" {
		t.Fatalf("expected response.failed, got %v", failed["type"])
	}
	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("expected error frame after response.failed, got %v", errFrame["type"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error payload, got %T", errFrame["error"])
	}
	if errPayload["message"] != "context too long" {
		t.Fatalf("expected error message context too long, got %v", errPayload["message"])
	}

	next := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "after failure"},
			},
		},
	})
	if err := conn.WriteJSON(next); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}

	nextCreated := mustReadWebSocketJSON(t, conn)
	if nextCreated["type"] != "response.created" {
		t.Fatalf("expected follow-up response.created, got %v", nextCreated["type"])
	}

	nextCompleted := mustReadWebSocketJSON(t, conn)
	if nextCompleted["type"] != "response.completed" {
		t.Fatalf("expected follow-up response.completed, got %v", nextCompleted["type"])
	}
}

func TestHandleResponsesWebSocket_SameChunkCreatedThenFailedRelaysSSE(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-later-rate-limit\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-later-rate-limit\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"Too Many Requests\"},\"usage\":{\"input_tokens\":9,\"output_tokens\":2,\"total_tokens\":11}}}\n\n")
	})
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}
	failed := mustReadWebSocketJSON(t, conn)
	if failed["type"] != "response.failed" {
		t.Fatalf("expected response.failed, got %v", failed["type"])
	}
	errFrame := mustReadWebSocketJSON(t, conn)
	if errFrame["type"] != "error" {
		t.Fatalf("expected error frame after response.failed, got %v", errFrame["type"])
	}
	if statusCode, _ := errFrame["status_code"].(float64); statusCode != float64(http.StatusTooManyRequests) {
		t.Fatalf("expected error status %d, got %v", http.StatusTooManyRequests, errFrame["status_code"])
	}
	errPayload, ok := errFrame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error payload, got %T", errFrame["error"])
	}
	if errPayload["code"] != "too_many_requests" {
		t.Fatalf("expected error code too_many_requests, got %v", errPayload["code"])
	}
	if errPayload["message"] != "Too Many Requests" {
		t.Fatalf("expected error message Too Many Requests, got %v", errPayload["message"])
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("failed turn stats = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 9 || snap.Totals.CompletionTokens != 2 || snap.Totals.TotalTokens != 11 {
		t.Fatalf("failed turn usage = prompt:%d completion:%d total:%d, want 9/2/11", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
	}
}

func TestHandleResponsesWebSocket_TopLevelErrorAfterCreatedRelaysErrorAndWrappedFailure(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-later-top-level-error\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"Request rate limit exceeded.\",\"headers\":{\"retry-after-ms\":\"1200\"}}}\n\n")
	})
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	if frame := mustReadWebSocketJSON(t, conn); frame["type"] != "response.created" {
		t.Fatalf("first frame type = %v, want response.created", frame["type"])
	}
	upstreamError := mustReadWebSocketJSON(t, conn)
	if upstreamError["type"] != "error" {
		t.Fatalf("second frame type = %v, want upstream error event", upstreamError["type"])
	}
	if _, ok := upstreamError["status_code"]; ok {
		t.Fatalf("upstream error event unexpectedly wrapped: %#v", upstreamError)
	}
	wrapped := mustReadWebSocketJSON(t, conn)
	if wrapped["type"] != "error" || wrapped["status_code"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("wrapped error = %#v, want status 429", wrapped)
	}
	wrappedError, ok := wrapped["error"].(map[string]interface{})
	if !ok || wrappedError["type"] != "rate_limit_error" {
		t.Fatalf("wrapped error payload = %#v, want rate_limit_error", wrapped["error"])
	}
	headers, ok := wrapped["headers"].(map[string]interface{})
	if !ok || headers["Retry-After"] != "2" {
		t.Fatalf("wrapped headers = %#v, want Retry-After=2", wrapped["headers"])
	}
	assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusTooManyRequests)
}

func TestHandleResponsesWebSocket_RelaysUpstreamHeadersOnSuccess(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Openai-Model", "gpt-5.4-actual")
		w.Header().Set("X-Reasoning-Included", "true")
		w.Header().Set("X-Models-Etag", `"models-v42"`)
		w.Header().Set("X-Codex-Primary-Used-Percent", "42.5")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-headers\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-headers\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	// First frame: codex.response.metadata with openai-model in lowercase
	// (the only header the Codex CLI parses from metadata frames via
	// response_model() using case-insensitive comparison).
	metadata := mustReadWebSocketJSON(t, conn)
	if metadata["type"] != "codex.response.metadata" {
		t.Fatalf("expected codex.response.metadata, got %v", metadata["type"])
	}
	metaHeaders, ok := metadata["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected headers map in metadata, got %T", metadata["headers"])
	}
	if got := metaHeaders["openai-model"]; got != "gpt-5.4-actual" {
		t.Fatalf("expected openai-model header, got %v", got)
	}
	// X-Reasoning-Included and X-Models-Etag should NOT be in the metadata
	// frame — the Codex CLI only reads them from HTTP upgrade headers.
	if _, found := metaHeaders["X-Reasoning-Included"]; found {
		t.Fatalf("X-Reasoning-Included should not be in metadata frame")
	}
	if _, found := metaHeaders["X-Models-Etag"]; found {
		t.Fatalf("X-Models-Etag should not be in metadata frame")
	}

	// Remaining frames are the normal SSE stream.
	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected response.completed, got %v", completed["type"])
	}
}

func TestHandleResponsesWebSocket_ForwardsOpenAIBetaHeader(t *testing.T) {
	var gotOpenAIBeta string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotOpenAIBeta = r.Header.Get("OpenAI-Beta")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-beta\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-beta\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{
		"OpenAI-Beta": []string{"responses_websockets=2026-02-06"},
	})
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn) // response.created
	_ = mustReadWebSocketJSON(t, conn) // response.completed

	if gotOpenAIBeta != "responses_websockets=2026-02-06" {
		t.Fatalf("expected OpenAI-Beta header to be forwarded upstream, got %q", gotOpenAIBeta)
	}
}

func TestHandleResponsesWebSocket_ForwardsSessionAndClientRequestHeaders(t *testing.T) {
	var gotLegacySessionID, gotSessionID, gotThreadID, gotClientRequestID, gotInstallationID string
	var gotInferenceCallID, gotParentThreadID, gotWindowID, gotSubagent, gotMemgen, gotAttestation, gotTimingMetrics string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotLegacySessionID = r.Header.Get("session_id")
		gotSessionID = r.Header.Get("session-id")
		gotThreadID = r.Header.Get("thread-id")
		gotClientRequestID = r.Header.Get("X-Client-Request-Id")
		gotInstallationID = r.Header.Get("X-Codex-Installation-Id")
		gotInferenceCallID = r.Header.Get("X-Codex-Inference-Call-Id")
		gotParentThreadID = r.Header.Get("X-Codex-Parent-Thread-Id")
		gotWindowID = r.Header.Get("X-Codex-Window-Id")
		gotSubagent = r.Header.Get("X-OpenAI-Subagent")
		gotMemgen = r.Header.Get("X-OpenAI-Memgen-Request")
		gotAttestation = r.Header.Get("X-OAI-Attestation")
		gotTimingMetrics = r.Header.Get("X-ResponsesAPI-Include-Timing-Metrics")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-sess\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-sess\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{
		"session_id":                            []string{"conv-legacy-123"},
		"session-id":                            []string{"conv-123"},
		"thread-id":                             []string{"thread-123"},
		"X-Client-Request-Id":                   []string{"req-456"},
		"X-Codex-Installation-Id":               []string{"install-789"},
		"X-Codex-Inference-Call-Id":             []string{"inference-123"},
		"X-Codex-Parent-Thread-Id":              []string{"parent-123"},
		"X-Codex-Window-Id":                     []string{"thread-123:4"},
		"X-OpenAI-Subagent":                     []string{"collab_spawn"},
		"X-OpenAI-Memgen-Request":               []string{"true"},
		"X-OAI-Attestation":                     []string{"attestation-token"},
		"X-ResponsesAPI-Include-Timing-Metrics": []string{"true"},
	})
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	_ = mustReadWebSocketJSON(t, conn) // response.created
	_ = mustReadWebSocketJSON(t, conn) // response.completed

	if gotLegacySessionID != "conv-legacy-123" {
		t.Fatalf("expected session_id to be forwarded upstream, got %q", gotLegacySessionID)
	}
	if gotSessionID != "conv-123" {
		t.Fatalf("expected session-id to be forwarded upstream, got %q", gotSessionID)
	}
	if gotThreadID != "thread-123" {
		t.Fatalf("expected thread-id to be forwarded upstream, got %q", gotThreadID)
	}
	if gotClientRequestID != "req-456" {
		t.Fatalf("expected X-Client-Request-Id to be forwarded upstream, got %q", gotClientRequestID)
	}
	if gotInstallationID != "install-789" {
		t.Fatalf("expected X-Codex-Installation-Id to be forwarded upstream, got %q", gotInstallationID)
	}
	if gotInferenceCallID != "inference-123" {
		t.Fatalf("expected X-Codex-Inference-Call-Id to be forwarded upstream, got %q", gotInferenceCallID)
	}
	if gotParentThreadID != "parent-123" {
		t.Fatalf("expected X-Codex-Parent-Thread-Id to be forwarded upstream, got %q", gotParentThreadID)
	}
	if gotWindowID != "thread-123:4" {
		t.Fatalf("expected X-Codex-Window-Id to be forwarded upstream, got %q", gotWindowID)
	}
	if gotSubagent != "collab_spawn" {
		t.Fatalf("expected X-OpenAI-Subagent to be forwarded upstream, got %q", gotSubagent)
	}
	if gotMemgen != "true" {
		t.Fatalf("expected X-OpenAI-Memgen-Request to be forwarded upstream, got %q", gotMemgen)
	}
	if gotAttestation != "attestation-token" {
		t.Fatalf("expected X-OAI-Attestation to be forwarded upstream, got %q", gotAttestation)
	}
	if gotTimingMetrics != "true" {
		t.Fatalf("expected X-ResponsesAPI-Include-Timing-Metrics to be forwarded upstream, got %q", gotTimingMetrics)
	}
}

func TestHandleResponsesWebSocket_ClientCloseCancelsStalledInference(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once

	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "stall"},
			},
		},
	})); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stalled upstream request")
	}

	if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to send client close frame: %v", err)
	}

	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("client close frame did not promptly cancel stalled upstream inference")
	}
}

func TestHandleResponsesWebSocket_CompletedTurnClosesHeldOpenBodyAndStartsNextTurn(t *testing.T) {
	var requests atomic.Int32
	firstBodyClosed := make(chan struct{})
	secondStarted := make(chan []map[string]interface{}, 1)
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read upstream request body: %v", err)
			return
		}
		var requestBody struct {
			Input []map[string]interface{} `json:"input"`
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Errorf("failed to decode upstream request body: %v", err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-held\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-held\",\"content\":[{\"type\":\"output_text\",\"text\":\"first output\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-held\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				close(firstBodyClosed)
			case <-releaseFirst:
			}
		case 2:
			secondStarted <- requestBody.Input
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-next\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
		default:
			t.Errorf("unexpected upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS.DisableAutoCompact = true

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "first turn"},
			},
		},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected response.completed, got %v", completed["type"])
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "second turn"},
			},
		},
	})
	second["previous_response_id"] = "resp-held"
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second websocket request: %v", err)
	}

	var replayedInput []map[string]interface{}
	select {
	case replayedInput = <-secondStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("next websocket turn did not start while completed upstream body remained open")
	}
	if len(replayedInput) != 3 {
		t.Fatalf("next turn replay input count = %d, want first input + output + second input", len(replayedInput))
	}
	select {
	case <-firstBodyClosed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("completed turn did not promptly close its held-open upstream body")
	}

	nextCreated := mustReadWebSocketJSON(t, conn)
	if nextCreated["type"] != "response.created" {
		t.Fatalf("expected next response.created, got %v", nextCreated["type"])
	}
	nextCompleted := mustReadWebSocketJSON(t, conn)
	if nextCompleted["type"] != "response.completed" {
		t.Fatalf("expected next response.completed, got %v", nextCompleted["type"])
	}
}

func TestHandleResponsesWebSocket_ClientCloseCancelsAutoCompaction(t *testing.T) {
	compactionStarted := make(chan struct{})
	compactionCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once

	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			return nil, err
		}
		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			startedOnce.Do(func() { close(compactionStarted) })
			<-r.Context().Done()
			canceledOnce.Do(func() { close(compactionCanceled) })
			return nil, r.Context().Err()
		}

		stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-compact-close\"}}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\"}}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-2\"}}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-3\"}}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-compact-close\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
		}, nil
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		Enabled:             true,
		AutoCompactMaxItems: 2,
		AutoCompactMaxBytes: 1 << 20,
		AutoCompactKeepTail: 1,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "trigger compaction"},
			},
		},
	})); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	for range 5 {
		_ = mustReadWebSocketJSON(t, conn)
	}
	select {
	case <-compactionStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket auto-compaction")
	}

	if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to send client close frame: %v", err)
	}
	select {
	case <-compactionCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("client close frame did not promptly cancel websocket auto-compaction")
	}
}

func TestHandleResponsesWebSocket_SerializesOneQueuedTurn(t *testing.T) {
	if responsesWebSocketOutstandingRequestLimit != 2 {
		t.Fatalf("responses websocket outstanding request limit = %d, want active + queued = 2", responsesWebSocketOutstandingRequestLimit)
	}

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releaseFirst) })
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		current := active.Add(1)
		for {
			prior := maxActive.Load()
			if current <= prior || maxActive.CompareAndSwap(prior, current) {
				break
			}
		}
		defer active.Add(-1)

		responseID := "resp-queued-2"
		switch call {
		case 1:
			responseID = "resp-queued-1"
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		case 2:
			close(secondStarted)
		default:
			return nil, fmt.Errorf("unexpected concurrent/extra upstream call %d", call)
		}

		stream := fmt.Sprintf("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n", responseID, responseID)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
		}, nil
	})
	handler.responsesWS.DisableAutoCompact = true

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "first"}}},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first upstream call")
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "second"}}},
	})
	second["previous_response_id"] = "resp-queued-1"
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to queue second request: %v", err)
	}

	select {
	case <-secondStarted:
		t.Fatal("second upstream call started before the first completed")
	case <-time.After(50 * time.Millisecond):
	}
	releaseFirstOnce.Do(func() { close(releaseFirst) })

	firstCreated := mustReadWebSocketJSON(t, conn)
	firstCompleted := mustReadWebSocketJSON(t, conn)
	if firstCreated["type"] != "response.created" || firstCompleted["type"] != "response.completed" {
		t.Fatalf("unexpected first turn frames: created=%v completed=%v", firstCreated["type"], firstCompleted["type"])
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("queued second upstream call did not start after first completion")
	}
	secondCreated := mustReadWebSocketJSON(t, conn)
	secondCompleted := mustReadWebSocketJSON(t, conn)
	if secondCreated["type"] != "response.created" || secondCompleted["type"] != "response.completed" {
		t.Fatalf("unexpected second turn frames: created=%v completed=%v", secondCreated["type"], secondCompleted["type"])
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent upstream calls = %d, want 1", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestResponsesWebSocketReadPump_AcceptsActiveAndQueuedBeforeHandlerScheduling(t *testing.T) {
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	session := &responsesWebSocketSession{
		conn:         serverConn,
		shutdownConn: serverConn,
		ctx:          ctx,
		cancel:       cancel,
		writeWait:    time.Second,
	}
	frames := make(chan responsesWebSocketFrame, responsesWebSocketOutstandingRequestLimit)
	outstanding := make(chan struct{}, responsesWebSocketOutstandingRequestLimit)
	go session.readPump(frames, outstanding)

	for range 3 {
		if err := clientConn.WriteJSON(map[string]interface{}{"type": "response.processed"}); err != nil {
			t.Fatalf("failed to write response.processed: %v", err)
		}
	}
	first := newResponsesWebSocketCreateRequest(nil)
	second := newResponsesWebSocketCreateRequest(nil)
	if err := clientConn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first outstanding frame: %v", err)
	}
	if err := clientConn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write second outstanding frame: %v", err)
	}

	waitForResponsesWebSocketChannelLen(t, frames, responsesWebSocketOutstandingRequestLimit)
	if got := len(outstanding); got != responsesWebSocketOutstandingRequestLimit {
		t.Fatalf("outstanding request tokens = %d, want %d", got, responsesWebSocketOutstandingRequestLimit)
	}
	if got := len(frames); got != 2 {
		t.Fatalf("queued frames without handler scheduling = %d, want 2 accepted", got)
	}

	session.beginClosing()
	session.hardClose()
}

func TestResponsesWebSocketReadPump_ThirdOutstandingFrameGetsPolicyViolation(t *testing.T) {
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	session := &responsesWebSocketSession{
		conn:         serverConn,
		shutdownConn: serverConn,
		ctx:          ctx,
		cancel:       cancel,
		writeWait:    time.Second,
	}
	frames := make(chan responsesWebSocketFrame, responsesWebSocketOutstandingRequestLimit)
	outstanding := make(chan struct{}, responsesWebSocketOutstandingRequestLimit)
	go session.readPump(frames, outstanding)

	request := newResponsesWebSocketCreateRequest(nil)
	if err := clientConn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write first outstanding frame: %v", err)
	}
	if err := clientConn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write second outstanding frame: %v", err)
	}
	waitForResponsesWebSocketChannelLen(t, frames, responsesWebSocketOutstandingRequestLimit)
	if err := clientConn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write overflowing frame: %v", err)
	}

	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	_, _, err := clientConn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("overflow close error = %v, want websocket close %d", err, websocket.ClosePolicyViolation)
	}
}

func TestHandleResponsesWebSocket_RejectsMoreThanOneQueuedTurn(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var calls atomic.Int32
	var startedOnce sync.Once
	var canceledOnce sync.Once
	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest(nil)
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write active request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active upstream request")
	}
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write queued request: %v", err)
	}
	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write overflowing request: %v", err)
	}

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("queue overflow did not terminate the active upstream request")
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("queue overflow read error = %v, want websocket close %d", err, websocket.ClosePolicyViolation)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after queue overflow = %d, want only active call", got)
	}
}

func TestHandleResponsesWebSocket_NoPongCancelsStalledInference(t *testing.T) {
	oldWriteWait := responsesWebSocketWriteWait
	oldPingPeriod := responsesWebSocketPingPeriod
	oldPongWait := responsesWebSocketPongWait
	responsesWebSocketWriteWait = 20 * time.Millisecond
	responsesWebSocketPingPeriod = 10 * time.Millisecond
	responsesWebSocketPongWait = 50 * time.Millisecond
	defer func() {
		responsesWebSocketWriteWait = oldWriteWait
		responsesWebSocketPingPeriod = oldPingPeriod
		responsesWebSocketPongWait = oldPongWait
	}()

	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stalled upstream request")
	}

	// Do not read from the client connection: Gorilla processes server pings and
	// emits pongs only while a reader is active.
	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("missing pong did not cancel stalled websocket inference")
	}
}

func TestHandleResponsesWebSocket_CompletedBeforeTransportResetCommitsReplay(t *testing.T) {
	firstBody := newResetAfterDataReadCloser("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-reset\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-reset\",\"content\":[{\"type\":\"output_text\",\"text\":\"kept output\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-reset\",\"usage\":{\"input_tokens\":11,\"output_tokens\":4,\"total_tokens\":15}}}\n\n")
	secondInput := make(chan []map[string]interface{}, 1)
	var calls atomic.Int32
	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       firstBody,
			}, nil
		case 2:
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			var body struct {
				Input []map[string]interface{} `json:"input"`
			}
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				return nil, err
			}
			secondInput <- body.Input
			stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-after-reset\"}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-after-reset\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(stream)),
			}, nil
		default:
			return nil, fmt.Errorf("unexpected upstream call")
		}
	})
	handler.responsesWS.DisableAutoCompact = true

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	first := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "before reset"}}},
	})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected response.completed, got %v", completed["type"])
	}

	second := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "after reset"}}},
	})
	second["previous_response_id"] = "resp-reset"
	if err := conn.WriteJSON(second); err != nil {
		t.Fatalf("failed to write follow-up request: %v", err)
	}

	next := mustReadWebSocketJSON(t, conn)
	if next["type"] != "response.created" {
		t.Fatalf("expected replay to continue with response.created and no trailing reset error, got %v", next["type"])
	}
	_ = mustReadWebSocketJSON(t, conn)
	select {
	case replayed := <-secondInput:
		if len(replayed) != 3 {
			t.Fatalf("replayed input count = %d, want first input + completed output + second input", len(replayed))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replayed follow-up request")
	}
	select {
	case <-firstBody.closed:
	case <-time.After(time.Second):
		t.Fatal("completed response body was not closed after terminal event")
	}
}

func TestShutdownWebSocketSessions_AlreadyCanceledContextStillClosesSession(t *testing.T) {
	handler := &ProxyHandler{log: logger.New(logger.LevelError), responsesWS: ResponsesWebSocketConfig{Enabled: true}}
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	waitForResponsesWebSocketSessionCount(t, handler, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handler.ShutdownWebSocketSessions(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("ShutdownWebSocketSessions() error = %v, want nil or context canceled", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected already-canceled shutdown to hard-close websocket")
	}
	waitForResponsesWebSocketSessionCount(t, handler, 0)
}

func TestShutdownWebSocketSessionsReportsIncompleteHandlerDrain(t *testing.T) {
	handler := &ProxyHandler{responsesWSSessions: make(map[*responsesWebSocketSession]struct{})}
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	conn := newBlockingResponsesWebSocketShutdownConn()
	session := &responsesWebSocketSession{
		ctx:          sessionCtx,
		cancel:       cancelSession,
		handlerDone:  make(chan struct{}),
		shutdownConn: conn,
		writeWait:    time.Second,
	}
	handler.responsesWSSessions[session] = struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := handler.ShutdownWebSocketSessions(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ShutdownWebSocketSessions() error = %v, want deadline exceeded", err)
	}
	select {
	case <-sessionCtx.Done():
	default:
		t.Fatal("incomplete handler session was not canceled")
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("incomplete handler session was not hard-closed")
	}
}

func TestShutdownWebSocketSessions_SendsGoingAwayToWritableClients(t *testing.T) {
	handler := &ProxyHandler{log: logger.New(logger.LevelError), responsesWS: ResponsesWebSocketConfig{Enabled: true}}
	server := startResponsesWebSocketProxyServer(t, handler)
	connections := make([]*websocket.Conn, 0, 3)
	for range 3 {
		conn := mustDialResponsesWebSocket(t, server, nil)
		connections = append(connections, conn)
		defer func() { _ = conn.Close() }()
	}
	waitForResponsesWebSocketSessionCount(t, handler, len(connections))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.ShutdownWebSocketSessions(ctx); err != nil {
		t.Fatalf("ShutdownWebSocketSessions() error = %v", err)
	}

	for idx, conn := range connections {
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("client %d set read deadline: %v", idx, err)
		}
		_, _, err := conn.ReadMessage()
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseGoingAway {
			t.Fatalf("client %d shutdown close error = %v, want websocket close %d", idx, err, websocket.CloseGoingAway)
		}
	}
	waitForResponsesWebSocketSessionCount(t, handler, 0)
}

func TestShutdownWebSocketSessions_HardClosesAllBackpressuredSessions(t *testing.T) {
	handler := &ProxyHandler{
		responsesWSSessions: make(map[*responsesWebSocketSession]struct{}),
	}
	const sessionCount = 4
	connections := make([]*blockingResponsesWebSocketShutdownConn, 0, sessionCount)
	for range sessionCount {
		ctx, cancel := context.WithCancel(context.Background())
		conn := newBlockingResponsesWebSocketShutdownConn()
		session := &responsesWebSocketSession{
			ctx:          ctx,
			cancel:       cancel,
			handlerDone:  make(chan struct{}),
			shutdownConn: conn,
			writeWait:    time.Second,
		}
		handler.responsesWSSessions[session] = struct{}{}
		connections = append(connections, conn)
		go func() {
			<-conn.closed
			handler.unregisterResponsesWebSocketSession(session)
			session.closeHandlerDone()
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.ShutdownWebSocketSessions(ctx); err != nil {
		t.Fatalf("ShutdownWebSocketSessions() error = %v", err)
	}

	for idx, conn := range connections {
		select {
		case <-conn.closed:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("backpressured session %d was not hard-closed", idx)
		}
	}
	waitForResponsesWebSocketSessionCount(t, handler, 0)
}

func TestBeginShutdownBeforeWebSocketDrainPreservesGoingAwayClose(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active inference")
	}

	type readResult struct {
		messageType int
		payload     []byte
		err         error
	}
	readDone := make(chan readResult, 1)
	go func() {
		messageType, payload, err := conn.ReadMessage()
		readDone <- readResult{messageType: messageType, payload: payload, err: err}
	}()

	// Server.Stop begins lifecycle cancellation before entering the websocket
	// drain. Give the canceled turn time to unwind so this test catches any
	// spurious upstream-error frame emitted in that ordering window.
	handler.BeginShutdown()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("BeginShutdown did not cancel active websocket inference")
	}

	var early *readResult
	select {
	case result := <-readDone:
		early = &result
	case <-time.After(100 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := handler.ShutdownWebSocketSessions(ctx); err != nil {
		t.Fatalf("ShutdownWebSocketSessions() error = %v", err)
	}
	cancel()

	if early != nil {
		if early.err == nil {
			t.Fatalf("received websocket data frame before graceful drain: type=%d payload=%s", early.messageType, early.payload)
		}
		t.Fatalf("websocket closed before graceful drain: %v", early.err)
	}

	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket GoingAway close")
	}
	var closeErr *websocket.CloseError
	if !errors.As(result.err, &closeErr) {
		t.Fatalf("ReadMessage() error = %v, want websocket close error", result.err)
	}
	if closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("websocket close code = %d, want %d", closeErr.Code, websocket.CloseGoingAway)
	}
	waitForResponsesWebSocketSessionCount(t, handler, 0)
}

func TestShutdownWebSocketSessions_CancelsActiveInference(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active inference")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.ShutdownWebSocketSessions(ctx); err != nil {
		t.Fatalf("ShutdownWebSocketSessions() error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown did not cancel active websocket inference")
	}
	waitForResponsesWebSocketSessionCount(t, handler, 0)
}

func TestShutdownWebSocketSessions_ConcurrentClientCloseAndShutdown(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	handler := newRoundTripTestProxyHandler(t, func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
		return nil, r.Context().Err()
	})
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active inference")
	}

	shutdownDone := make(chan struct{})
	shutdownErr := make(chan error, 1)
	closeWriteDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownErr <- handler.ShutdownWebSocketSessions(ctx)
	}()
	go func() {
		defer close(closeWriteDone)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	}()

	for name, done := range map[string]<-chan struct{}{
		"shutdown":     shutdownDone,
		"client close": closeWriteDone,
		"cancellation": canceled,
	} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for concurrent %s", name)
		}
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("ShutdownWebSocketSessions() error = %v", err)
	}
	waitForResponsesWebSocketSessionCount(t, handler, 0)
}

func TestResponsesWebSocketSetInflightCancelAfterClosingCancelsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	session := &responsesWebSocketSession{ctx: ctx, cancel: cancel}
	session.beginClosing()

	canceled := make(chan struct{})
	var once sync.Once
	gen := session.setInflightCancel(func() { once.Do(func() { close(canceled) }) })
	if gen == 0 {
		t.Fatal("expected non-zero inflight generation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("inflight work installed after closing was not canceled immediately")
	}
	session.clearInflightCancel(gen)
}

func TestHandleResponsesWebSocket_FailureStatsSurviveImmediateClientWriteFailure(t *testing.T) {
	tests := []struct {
		name       string
		response   func() *http.Response
		wantStatus int
	}{
		{
			name: "translated precommit failure",
			response: func() *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(
						"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-translated\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n")),
				}
			},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name: "translated uncoded quota failure",
			response: func() *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type":                 []string{"text/event-stream"},
						"retry-after-ms":               []string{"2169"},
						"x-ratelimit-remaining-tokens": []string{"-36161"},
					},
					Body: io.NopCloser(strings.NewReader(
						"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-uncoded-quota\",\"error\":{\"message\":\"Your requests have exceeded rate limit.\"}}}\n\n")),
				}
			},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name: "parsed first response.failed",
			response: func() *http.Response {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(
						"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-failed\",\"error\":{\"type\":\"server_error\",\"code\":\"context_length_exceeded\",\"message\":\"too long\"}}}\n\n")),
				}
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "non-200 response",
			response: func() *http.Response {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad request","code":"bad_request"}}`)),
				}
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newRoundTripTestProxyHandler(t, func(*http.Request) (*http.Response, error) {
				return tt.response(), nil
			})
			handler.stats = newStatsCollector()
			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			_ = clientConn.Close()
			_ = serverConn.Close()

			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
			err := session.handleCreateRequest(handler, request)
			if !errors.Is(err, errResponsesWebSocketClientWrite) {
				t.Fatalf("handleCreateRequest error = %v, want client write failure", err)
			}
			assertSingleResponsesWebSocketFailureStats(t, handler, tt.wantStatus)
		})
	}
}

func TestHandleResponsesWebSocket_StreamFailureStatsSurviveClientCloseAtDelivery(t *testing.T) {
	tests := []struct {
		name       string
		second     string
		secondErr  error
		wantStatus int
	}{
		{
			name:       "later response.failed",
			second:     "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-later-failed\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n",
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "later uncoded quota failure",
			second:     "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-later-uncoded\",\"error\":{\"message\":\"Your requests have exceeded rate limit.\"}}}\n\n",
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "response.incomplete",
			second:     "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
			wantStatus: http.StatusConflict,
		},
		{
			name:       "generic stream reset",
			secondErr:  errors.New("upstream stream reset"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := newGatedResponsesFailureBody(tt.second, tt.secondErr)
			handler := newRoundTripTestProxyHandler(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type":                 []string{"text/event-stream"},
						"retry-after-ms":               []string{"2169"},
						"x-ratelimit-remaining-tokens": []string{"-36161"},
					},
					Body: body,
				}, nil
			})
			handler.stats = newStatsCollector()
			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

			handleDone := make(chan error, 1)
			go func() { handleDone <- session.handleCreateRequest(handler, request) }()
			created := mustReadWebSocketJSON(t, clientConn)
			if created["type"] != "response.created" {
				t.Fatalf("first frame type = %v, want response.created", created["type"])
			}
			_ = clientConn.Close()
			_ = serverConn.Close()
			body.releaseSecond()

			select {
			case err := <-handleDone:
				if err == nil {
					t.Fatal("handleCreateRequest unexpectedly succeeded after client close")
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for failure delivery after client close")
			}
			assertSingleResponsesWebSocketFailureStats(t, handler, tt.wantStatus)
		})
	}
}

func TestHandleResponsesWebSocket_StreamFailureShutdownCausality(t *testing.T) {
	t.Run("lifecycle canceled body is suppressed", func(t *testing.T) {
		var body *lifecycleCanceledResponsesBody
		handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
			body = newLifecycleCanceledResponsesBody(req.Context())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})
		handler.stats = newStatsCollector()
		serverConn, clientConn := newResponsesWebSocketConnPair(t)
		defer func() { _ = clientConn.Close() }()
		defer func() { _ = serverConn.Close() }()
		session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
		request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
		handleDone := make(chan error, 1)
		go func() { handleDone <- session.handleCreateRequest(handler, request) }()

		created := mustReadWebSocketJSON(t, clientConn)
		if created["type"] != "response.created" {
			t.Fatalf("first frame type = %v, want response.created", created["type"])
		}
		waitForLifecycleSignal(t, body.blocked, "websocket lifecycle body read")
		handler.BeginShutdown()
		select {
		case err := <-handleDone:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("handleCreateRequest error = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for lifecycle-canceled websocket stream")
		}
		snap := handler.stats.snapshot()
		if snap.Totals.Requests != 0 || snap.Totals.Errors != 0 {
			t.Fatalf("lifecycle cancellation stats = requests:%d errors:%d, want 0/0", snap.Totals.Requests, snap.Totals.Errors)
		}
	})

	t.Run("independent reset before shutdown records one 502", func(t *testing.T) {
		raceErr := newWebSocketShutdownRaceError("independent upstream reset")
		body := newGatedResponsesFailureBody("", raceErr)
		handler := newRoundTripTestProxyHandler(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})
		handler.stats = newStatsCollector()
		serverConn, clientConn := newResponsesWebSocketConnPair(t)
		defer func() { _ = clientConn.Close() }()
		defer func() { _ = serverConn.Close() }()
		session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
		request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
		handleDone := make(chan error, 1)
		go func() { handleDone <- session.handleCreateRequest(handler, request) }()

		created := mustReadWebSocketJSON(t, clientConn)
		if created["type"] != "response.created" {
			t.Fatalf("first frame type = %v, want response.created", created["type"])
		}
		body.releaseSecond()
		waitForLifecycleSignal(t, raceErr.ready, "independent websocket reset")
		handler.BeginShutdown()
		close(raceErr.release)
		select {
		case err := <-handleDone:
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("handleCreateRequest error = %v, want independent reset", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for independent websocket reset")
		}
		assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusBadGateway)
	})
}

func TestHandleResponsesWebSocket_PreHeaderErrorShutdownCausality(t *testing.T) {
	t.Run("lifecycle cancellation is suppressed", func(t *testing.T) {
		started := make(chan struct{})
		handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		handler.maxRetries = 1
		handler.stats = newStatsCollector()
		serverConn, clientConn := newResponsesWebSocketConnPair(t)
		defer func() { _ = clientConn.Close() }()
		defer func() { _ = serverConn.Close() }()
		session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
		request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
		done := make(chan error, 1)
		go func() { done <- session.handleCreateRequest(handler, request) }()
		waitForLifecycleSignal(t, started, "websocket pre-header request")
		handler.BeginShutdown()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("handleCreateRequest error = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for lifecycle-canceled pre-header request")
		}
		snap := handler.stats.snapshot()
		if snap.Totals.Requests != 0 || snap.Totals.Errors != 0 {
			t.Fatalf("lifecycle cancellation stats = requests:%d errors:%d, want 0/0", snap.Totals.Requests, snap.Totals.Errors)
		}
	})

	for _, tt := range []struct {
		name       string
		cause      error
		wantStatus int
	}{
		{name: "independent reset before shutdown", cause: errors.New("connection reset by peer"), wantStatus: http.StatusBadGateway},
		{name: "independent timeout before shutdown", cause: &upstreamError{statusCode: http.StatusGatewayTimeout}, wantStatus: http.StatusGatewayTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raceErr := newWebSocketShutdownRaceErrorWithCause(tt.name, tt.cause)
			handler := newRoundTripTestProxyHandler(t, func(*http.Request) (*http.Response, error) {
				return nil, raceErr
			})
			handler.maxRetries = 1
			handler.stats = newStatsCollector()
			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			defer func() { _ = clientConn.Close() }()
			defer func() { _ = serverConn.Close() }()
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
			done := make(chan error, 1)
			go func() { done <- session.handleCreateRequest(handler, request) }()

			waitForLifecycleSignal(t, raceErr.ready, "independent pre-header error")
			handler.BeginShutdown()
			close(raceErr.release)
			select {
			case err := <-done:
				if err == nil || errors.Is(err, context.Canceled) {
					t.Fatalf("handleCreateRequest error = %v, want independent provider error", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for independent pre-header failure")
			}
			assertSingleResponsesWebSocketFailureStats(t, handler, tt.wantStatus)
		})
	}
}

func TestHandleResponsesWebSocket_IndependentErrorsRecordBeforeClientClose(t *testing.T) {
	t.Run("pre-header reset", func(t *testing.T) {
		raceErr := newWebSocketShutdownRaceError("independent pre-header reset")
		handler := newRoundTripTestProxyHandler(t, func(*http.Request) (*http.Response, error) {
			return nil, raceErr
		})
		handler.maxRetries = 1
		handler.stats = newStatsCollector()
		serverConn, clientConn := newResponsesWebSocketConnPair(t)
		defer func() { _ = clientConn.Close() }()
		defer func() { _ = serverConn.Close() }()
		session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
		request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
		done := make(chan error, 1)
		go func() { done <- session.handleCreateRequest(handler, request) }()
		waitForLifecycleSignal(t, raceErr.ready, "independent pre-header reset")
		session.beginClosing()
		close(raceErr.release)
		select {
		case err := <-done:
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("handleCreateRequest error = %v, want independent reset", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for pre-header reset after client close")
		}
		assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusBadGateway)
	})

	t.Run("committed stream reset", func(t *testing.T) {
		raceErr := newWebSocketShutdownRaceError("independent committed reset")
		body := newGatedResponsesFailureBody("", raceErr)
		handler := newRoundTripTestProxyHandler(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
		})
		handler.stats = newStatsCollector()
		serverConn, clientConn := newResponsesWebSocketConnPair(t)
		defer func() { _ = clientConn.Close() }()
		defer func() { _ = serverConn.Close() }()
		session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
		request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
		done := make(chan error, 1)
		go func() { done <- session.handleCreateRequest(handler, request) }()
		created := mustReadWebSocketJSON(t, clientConn)
		if created["type"] != "response.created" {
			t.Fatalf("first frame type = %v, want response.created", created["type"])
		}
		body.releaseSecond()
		waitForLifecycleSignal(t, raceErr.ready, "independent committed reset")
		session.beginClosing()
		close(raceErr.release)
		select {
		case err := <-done:
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("handleCreateRequest error = %v, want independent reset", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for committed reset after client close")
		}
		assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusBadGateway)
	})
}

func TestHandleResponsesWebSocket_CompletionAccountsBeforeTerminalClientWrite(t *testing.T) {
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-created\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"
	body := newGatedResponsesFailureBody(completed, nil)
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()

	created := mustReadWebSocketJSON(t, clientConn)
	if created["type"] != "response.created" {
		t.Fatalf("first frame type = %v, want response.created", created["type"])
	}
	_ = serverConn.Close()
	body.releaseSecond()
	select {
	case err := <-done:
		if !errors.Is(err, errResponsesWebSocketClientWrite) {
			t.Fatalf("handleCreateRequest error = %v, want client write error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal client write failure")
	}

	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
		t.Fatalf("completion stats = requests:%d errors:%d, want 1/0", snap.Totals.Requests, snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 7 || snap.Totals.CompletionTokens != 3 || snap.Totals.TotalTokens != 10 {
		t.Fatalf("completion usage = prompt:%d completion:%d total:%d, want 7/3/10", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
	}
}

func TestHandleResponsesWebSocket_BufferedTerminalPrecedesLifecycleCancellation(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-peek-ws\"}}\n\n"
	for _, tt := range []struct {
		name       string
		terminal   string
		wantStatus int
		wantFrames []string
	}{
		{
			name:       "completed",
			terminal:   "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-peek-ws\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n",
			wantStatus: http.StatusOK,
			wantFrames: []string{"response.created", "response.completed"},
		},
		{
			name:       "failed",
			terminal:   "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-peek-ws\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n",
			wantStatus: http.StatusTooManyRequests,
			wantFrames: []string{"response.created", "response.failed", "error"},
		},
		{
			name:       "incomplete",
			terminal:   "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-peek-ws\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
			wantStatus: http.StatusConflict,
			wantFrames: []string{"response.created", "response.incomplete"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bodyCh := make(chan *singleChunkTerminalThenCancelBody, 1)
			var handler *ProxyHandler
			handler = newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
				body := newSingleChunkTerminalThenCancelBody(req.Context(), created+tt.terminal)
				body.onBlocked = handler.BeginShutdown
				bodyCh <- body
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       body,
					Request:    req,
				}, nil
			})
			handler.stats = newStatsCollector()
			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			defer func() { _ = clientConn.Close() }()
			defer func() { _ = serverConn.Close() }()
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
			done := make(chan error, 1)
			go func() { done <- session.handleCreateRequest(handler, request) }()
			body := <-bodyCh
			waitForLifecycleSignal(t, body.blocked, "websocket Responses speculative body read")
			handler.BeginShutdown()

			for _, wantType := range tt.wantFrames {
				frame := mustReadWebSocketJSON(t, clientConn)
				if frame["type"] != wantType {
					t.Fatalf("frame type = %v, want %s", frame["type"], wantType)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("handleCreateRequest error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for buffered terminal websocket turn")
			}

			if tt.wantStatus == http.StatusOK {
				snap := handler.stats.snapshot()
				if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
					t.Fatalf("completed stats = requests:%d errors:%d, want 1/0", snap.Totals.Requests, snap.Totals.Errors)
				}
			} else {
				assertSingleResponsesWebSocketFailureStats(t, handler, tt.wantStatus)
			}
		})
	}
}

func TestHandleResponsesWebSocket_QueuedTerminalPrecedesLifecycleCancellation(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-peek-split-ws\"}}\n\n"
	for _, tt := range []struct {
		name       string
		terminal   string
		wantStatus int
		wantFrames []string
	}{
		{name: "completed", terminal: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-peek-split-ws\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", wantStatus: http.StatusOK, wantFrames: []string{"response.created", "response.completed"}},
		{name: "failed", terminal: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-peek-split-ws\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n", wantStatus: http.StatusTooManyRequests, wantFrames: []string{"response.created", "response.failed", "error"}},
		{name: "incomplete", terminal: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-peek-split-ws\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", wantStatus: http.StatusConflict, wantFrames: []string{"response.created", "response.incomplete"}},
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
			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			defer func() { _ = clientConn.Close() }()
			defer func() { _ = serverConn.Close() }()
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
			done := make(chan error, 1)
			go func() { done <- session.handleCreateRequest(handler, request) }()
			body := <-bodyCh
			waitForLifecycleSignal(t, body.blocked, "queued websocket terminal speculative read")

			for _, wantType := range tt.wantFrames {
				frame := mustReadWebSocketJSON(t, clientConn)
				if frame["type"] != wantType {
					t.Fatalf("frame type = %v, want %s", frame["type"], wantType)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("handleCreateRequest error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for queued terminal websocket turn")
			}

			if tt.wantStatus == http.StatusOK {
				snap := handler.stats.snapshot()
				if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
					t.Fatalf("completed stats = requests:%d errors:%d, want 1/0", snap.Totals.Requests, snap.Totals.Errors)
				}
			} else {
				assertSingleResponsesWebSocketFailureStats(t, handler, tt.wantStatus)
			}
		})
	}
}

func TestHandleResponsesWebSocket_PeekReadOutcomeCausality(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-outcome-ws\"}}\n\n"
	for _, tt := range []struct {
		name        string
		readErr     error
		independent bool
	}{
		{name: "independent EOF", readErr: io.EOF, independent: true},
		{name: "independent reset", readErr: errors.New("independent reset"), independent: true},
		{name: "lifecycle cancellation", readErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var handler *ProxyHandler
			handler = newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
				body := newPeekReadOutcomeRaceBody(created, tt.readErr, handler.BeginShutdown)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
			})
			handler.stats = newStatsCollector()
			serverConn, clientConn := newResponsesWebSocketConnPair(t)
			defer func() { _ = clientConn.Close() }()
			defer func() { _ = serverConn.Close() }()
			session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
			request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
			done := make(chan error, 1)
			go func() { done <- session.handleCreateRequest(handler, request) }()

			if tt.independent {
				frame := mustReadWebSocketJSON(t, clientConn)
				if frame["type"] != "response.created" {
					t.Fatalf("frame type = %v, want response.created", frame["type"])
				}
			}
			select {
			case err := <-done:
				if tt.independent {
					if err == nil || errors.Is(err, context.Canceled) {
						t.Fatalf("handleCreateRequest error = %v, want independent stream failure", err)
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("handleCreateRequest error = %v, want lifecycle cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for peek outcome resolution")
			}
			if tt.independent {
				assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusBadGateway)
			} else {
				snap := handler.stats.snapshot()
				if snap.Totals.Requests != 0 || snap.Totals.Errors != 0 {
					t.Fatalf("lifecycle stats = requests:%d errors:%d, want 0/0", snap.Totals.Requests, snap.Totals.Errors)
				}
			}
		})
	}
}

type resetAfterDataReadCloser struct {
	data      []byte
	offset    int
	closed    chan struct{}
	closeOnce sync.Once
}

type gatedResponsesFailureBody struct {
	mu          sync.Mutex
	step        int
	second      string
	secondErr   error
	secondReady chan struct{}
	releaseOnce sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
}

type lifecycleCanceledResponsesBody struct {
	ctx       context.Context
	prefix    *strings.Reader
	blocked   chan struct{}
	blockOnce sync.Once
}

type stalledWebSocket413Body struct {
	ctx       context.Context
	prefix    *strings.Reader
	blocked   chan struct{}
	blockOnce sync.Once
}

func newStalledWebSocket413Body(ctx context.Context, prefix string) *stalledWebSocket413Body {
	return &stalledWebSocket413Body{
		ctx:     ctx,
		prefix:  strings.NewReader(prefix),
		blocked: make(chan struct{}),
	}
}

func (b *stalledWebSocket413Body) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.blockOnce.Do(func() { close(b.blocked) })
	<-b.ctx.Done()
	return 0, context.Canceled
}

func (b *stalledWebSocket413Body) Close() error { return nil }

func newLifecycleCanceledResponsesBody(ctx context.Context) *lifecycleCanceledResponsesBody {
	return &lifecycleCanceledResponsesBody{
		ctx:     ctx,
		prefix:  strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-lifecycle\"}}\n\n"),
		blocked: make(chan struct{}),
	}
}

func (b *lifecycleCanceledResponsesBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.blockOnce.Do(func() { close(b.blocked) })
	<-b.ctx.Done()
	return 0, context.Canceled
}

func (b *lifecycleCanceledResponsesBody) Close() error { return nil }

type webSocketShutdownRaceError struct {
	message string
	cause   error
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func newWebSocketShutdownRaceError(message string) *webSocketShutdownRaceError {
	return newWebSocketShutdownRaceErrorWithCause(message, nil)
}

func newWebSocketShutdownRaceErrorWithCause(message string, cause error) *webSocketShutdownRaceError {
	return &webSocketShutdownRaceError{
		message: message,
		cause:   cause,
		ready:   make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (e *webSocketShutdownRaceError) Error() string {
	e.once.Do(func() { close(e.ready) })
	<-e.release
	return e.message
}

func (e *webSocketShutdownRaceError) Unwrap() error { return e.cause }

func newGatedResponsesFailureBody(second string, secondErr error) *gatedResponsesFailureBody {
	return &gatedResponsesFailureBody{
		second:      second,
		secondErr:   secondErr,
		secondReady: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *gatedResponsesFailureBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	step := b.step
	b.step++
	b.mu.Unlock()
	switch step {
	case 0:
		return copy(p, []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-created\"}}\n\n")), nil
	case 1:
		select {
		case <-b.secondReady:
			if b.secondErr != nil {
				return 0, b.secondErr
			}
			return copy(p, []byte(b.second)), nil
		case <-b.closed:
			return 0, io.EOF
		}
	default:
		return 0, io.EOF
	}
}

func (b *gatedResponsesFailureBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *gatedResponsesFailureBody) releaseSecond() {
	b.releaseOnce.Do(func() { close(b.secondReady) })
}

func newResetAfterDataReadCloser(data string) *resetAfterDataReadCloser {
	return &resetAfterDataReadCloser{
		data:   []byte(data),
		closed: make(chan struct{}),
	}
}

func (r *resetAfterDataReadCloser) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}

func (r *resetAfterDataReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

type blockingResponsesWebSocketShutdownConn struct {
	closed       chan struct{}
	closeOnce    sync.Once
	controlStart chan struct{}
	controlOnce  sync.Once
}

func newBlockingResponsesWebSocketShutdownConn() *blockingResponsesWebSocketShutdownConn {
	return &blockingResponsesWebSocketShutdownConn{
		closed:       make(chan struct{}),
		controlStart: make(chan struct{}),
	}
}

func (c *blockingResponsesWebSocketShutdownConn) WriteControl(int, []byte, time.Time) error {
	c.controlOnce.Do(func() { close(c.controlStart) })
	<-c.closed
	return io.ErrClosedPipe
}

func (c *blockingResponsesWebSocketShutdownConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func waitForResponsesWebSocketSessionCount(t *testing.T, handler *ProxyHandler, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		handler.responsesWSSessionsMu.Lock()
		got := len(handler.responsesWSSessions)
		handler.responsesWSSessionsMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("responses websocket session count = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForResponsesWebSocketChannelLen(t *testing.T, frames chan responsesWebSocketFrame, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(frames) != want {
		if time.Now().After(deadline) {
			t.Fatalf("responses websocket frame channel length = %d, want %d", len(frames), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func newResponsesWebSocketConnPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	serverConnCh := make(chan *websocket.Conn, 1)
	handlerDone := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket pair: %v", err)
			return
		}
		serverConnCh <- conn
		<-handlerDone
		_ = conn.Close()
	}))
	t.Cleanup(func() {
		close(handlerDone)
		server.Close()
	})

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket pair: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	select {
	case serverConn := <-serverConnCh:
		return serverConn, clientConn
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for server websocket pair")
		return nil, nil
	}
}

func newExplicitRouteResponsesWebSocketHandler(t *testing.T, primaryURL, secondaryURL string) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: 2,
			Providers: []ProviderConfig{
				{
					ID:      "ws-primary",
					Type:    string(providerTypeAzureOpenAI),
					Default: true,
					BaseURL: primaryURL + "/openai/v1",
					APIKey:  "primary-key",
				},
				{
					ID:      "ws-secondary",
					Type:    string(providerTypeAzureOpenAI),
					BaseURL: secondaryURL + "/openai/v1",
					APIKey:  "secondary-key",
				},
			},
			ModelRoutes: []ModelRouteConfig{{
				ID:        "public-ws-route",
				PublicID:  "public-ws-model",
				Name:      "Public WebSocket Model",
				Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{
					{ID: "primary", Provider: "ws-primary", UpstreamModel: "physical-primary"},
					{ID: "secondary", Provider: "ws-secondary", UpstreamModel: "physical-secondary"},
				},
				Routing: ModelRouteRoutingConfig{
					Mode:              string(routeModePriorityFailover),
					MaxTargetAttempts: 2,
					MaxUpstreamSends:  2,
				},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler explicit websocket route: %v", err)
	}
	t.Cleanup(handler.BeginShutdown)
	return handler
}

func explicitResponsesWebSocketMetadataHeader(t *testing.T, payload map[string]interface{}, name string) string {
	t.Helper()
	if payload["type"] != "codex.response.metadata" {
		t.Fatalf("frame type = %v, want codex.response.metadata", payload["type"])
	}
	headers, ok := payload["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata headers = %T, want object", payload["headers"])
	}
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			text, ok := value.(string)
			if !ok {
				t.Fatalf("metadata header %q = %T, want string", key, value)
			}
			return text
		}
	}
	t.Fatalf("metadata header %q missing from %+v", name, headers)
	return ""
}

func explicitResponsesWebSocketOperationID(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	return explicitResponsesWebSocketMetadataHeader(t, payload, "x-vekil-request-id")
}

func explicitResponsesWebSocketErrorHeader(t *testing.T, payload map[string]interface{}, name string) string {
	t.Helper()
	headers, ok := payload["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("error headers = %T, want object in %+v", payload["headers"], payload)
	}
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			text, ok := value.(string)
			if !ok {
				t.Fatalf("error header %q = %T, want string", key, value)
			}
			return text
		}
	}
	t.Fatalf("error header %q missing from %+v", name, headers)
	return ""
}

func assertResponsesWebSocketOperationSequence(t *testing.T, first, second string) {
	t.Helper()
	firstPrefix, firstSequence, ok := strings.Cut(first, ":")
	if !ok || firstPrefix == "" || firstSequence != "1" {
		t.Fatalf("first operation id = %q, want <connection>:1", first)
	}
	secondPrefix, secondSequence, ok := strings.Cut(second, ":")
	if !ok || secondPrefix != firstPrefix || secondSequence != "2" {
		t.Fatalf("operation ids = %q, %q, want one connection with turns 1 and 2", first, second)
	}
}

func startResponsesWebSocketProxyServer(t *testing.T, handler *ProxyHandler) *httptest.Server {
	t.Helper()
	handler.responsesWS.Enabled = true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/responses", handler.HandleResponsesWebSocket)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func mustDialResponsesWebSocket(t *testing.T, server *httptest.Server, headers http.Header) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		t.Fatalf("failed to dial websocket endpoint %s: %v", url, err)
	}
	return conn
}

func mustReadWebSocketJSON(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read websocket message: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("failed to decode websocket payload %s: %v", string(data), err)
	}
	return payload
}

// mustReadWebSocketJSONSkipMetadata reads the next WebSocket frame, skipping
// over any synthetic codex.response.metadata frames injected by the proxy.
func mustReadWebSocketJSONSkipMetadata(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	for {
		payload := mustReadWebSocketJSON(t, conn)
		if payload["type"] != "codex.response.metadata" {
			return payload
		}
	}
}

func newResponsesWebSocketCreateRequest(input []interface{}) map[string]interface{} {
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

func mustParseResponsesWebSocketCreateRequest(t *testing.T, payload map[string]interface{}) *responsesWebSocketCreateRequest {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal websocket create request: %v", err)
	}
	request, err := parseResponsesWebSocketCreateRequest(encoded)
	if err != nil {
		t.Fatalf("parse websocket create request: %v", err)
	}
	return request
}

func assertSingleResponsesWebSocketFailureStats(t *testing.T, handler *ProxyHandler, wantStatus int) {
	t.Helper()
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("totals = requests:%d errors:%d, want exactly one failed turn", snap.Totals.Requests, snap.Totals.Errors)
	}
	statusCount := int64(0)
	allStatusErrors := int64(0)
	for _, row := range snap.StatusCodes {
		allStatusErrors += row.Count
		if row.Label == strconv.Itoa(wantStatus) {
			statusCount = row.Count
		}
	}
	if statusCount != 1 || allStatusErrors != 1 {
		t.Fatalf("status codes = %+v, want exactly one status %d failure", snap.StatusCodes, wantStatus)
	}
	providerRequests := int64(0)
	providerErrors := int64(0)
	for _, row := range snap.ByProvider {
		providerRequests += row.Requests
		providerErrors += row.Errors
	}
	if providerRequests != 1 || providerErrors != 1 {
		t.Fatalf("provider stats = %+v, want exactly one failed provider turn", snap.ByProvider)
	}
}

func snapshotResponsesWebSocketRequests(mu *sync.Mutex, requests []map[string]interface{}) []map[string]interface{} {
	mu.Lock()
	defer mu.Unlock()

	snapshot := make([]map[string]interface{}, len(requests))
	copy(snapshot, requests)
	return snapshot
}

func websocketResponseID(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	response, ok := payload["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected response payload, got %v", payload)
	}
	id, ok := response["id"].(string)
	if !ok {
		t.Fatalf("expected response id, got %v", response["id"])
	}
	return id
}

func upstreamInputItems(t *testing.T, body map[string]interface{}) []map[string]interface{} {
	t.Helper()
	rawItems, ok := body["input"].([]interface{})
	if !ok {
		t.Fatalf("expected input array, got %T", body["input"])
	}

	items := make([]map[string]interface{}, len(rawItems))
	for idx, raw := range rawItems {
		item, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("expected input item object, got %T", raw)
		}
		items[idx] = item
	}
	return items
}

func decodeRawMessagesForTest(t *testing.T, items []json.RawMessage) []map[string]interface{} {
	t.Helper()
	decoded := make([]map[string]interface{}, len(items))
	for idx, raw := range items {
		if err := json.Unmarshal(raw, &decoded[idx]); err != nil {
			t.Fatalf("failed to decode raw message %d: %v", idx, err)
		}
	}
	return decoded
}

func inputTextFromMessage(t *testing.T, item map[string]interface{}) string {
	t.Helper()
	content, ok := item["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("expected message content array, got %v", item["content"])
	}
	first, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected first message content item to be object, got %T", content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		t.Fatalf("expected first message content text, got %v", first["text"])
	}
	return text
}

func TestResponsesWebSocketInvalidTurnStateClearsBeforeFailedFullReplay(t *testing.T) {
	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch call := calls.Add(1); call {
		case 1:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "turn-state-1" {
				t.Fatalf("delta turn state = %q, want turn-state-1", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"stale turn state","code":"invalid_turn_state"}}`)
		case 2:
			if got := r.Header.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("full replay turn state = %q, want empty", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"full replay rejected","code":"invalid_request_error"}}`)
		default:
			t.Fatalf("unexpected upstream call %d", call)
		}
	})

	request := &responsesWebSocketCreateRequest{
		Type:               "response.create",
		Model:              "gpt-5.4",
		PreviousResponseID: "resp-1",
		Input:              []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"retry"}]}`)},
		signatureValue:     "sig-1",
		upstreamFields: []responsesWebSocketJSONField{
			{key: "model", value: json.RawMessage(`"gpt-5.4"`)},
		},
	}
	session := &responsesWebSocketSession{
		turnState:      "turn-state-1",
		lastResponseID: "resp-1",
		lastSignature:  "sig-1",
		historyItems:   []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"original"}]}`)},
	}
	plan := responsesWebSocketRequestPlan{
		signature:          "sig-1",
		currentInput:       request.Input,
		fullReplaySegments: [][]json.RawMessage{session.historyItems, request.Input},
		useTurnStateDelta:  true,
		compactionChecked:  true,
	}

	resp, deltaAttempted, deltaFallback, err := session.postCreateRequest(handler, context.Background(), request, plan, &responsesWebSocketRequestMetrics{})
	if err != nil {
		t.Fatalf("postCreateRequest() error = %v", err)
	}
	if resp == nil {
		t.Fatal("postCreateRequest() response = nil")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fallback status = %d, want 400", resp.StatusCode)
	}
	if !deltaAttempted || !deltaFallback {
		t.Fatalf("delta attempted/fallback = %v/%v, want true/true", deltaAttempted, deltaFallback)
	}
	if session.turnState != "" {
		t.Fatalf("turnState = %q after failed full replay, want cleared", session.turnState)
	}

	nextPlan, err := session.planRequest(handler, request)
	if err != nil {
		t.Fatalf("planRequest() error = %v", err)
	}
	if nextPlan.useTurnStateDelta {
		t.Fatal("next retry unexpectedly reused rejected turn state")
	}
}

func TestResponsesWebSocketRememberPlannedResponseAppendsInputAndOutput(t *testing.T) {
	t.Parallel()

	session := &responsesWebSocketSession{
		turnState:    "turn-state-1",
		historyItems: []json.RawMessage{json.RawMessage(`{"type":"message","id":"old"}`)},
	}
	plan := responsesWebSocketRequestPlan{
		signature:    "sig-1",
		currentInput: []json.RawMessage{json.RawMessage(`{"type":"message","id":"new"}`)},
	}
	outputItems := []json.RawMessage{json.RawMessage(`{"type":"message","id":"out"}`)}

	session.rememberPlannedResponse(plan, "resp-1", outputItems)

	if session.turnState != "turn-state-1" {
		t.Fatalf("turnState = %q, want preserved turn-state-1", session.turnState)
	}
	if session.lastResponseID != "resp-1" {
		t.Fatalf("lastResponseID = %q, want resp-1", session.lastResponseID)
	}
	if session.lastSignature != "sig-1" {
		t.Fatalf("lastSignature = %q, want sig-1", session.lastSignature)
	}
	if got, want := len(session.historyItems), 3; got != want {
		t.Fatalf("history item count = %d, want %d", got, want)
	}
	if string(session.historyItems[1]) != string(plan.currentInput[0]) {
		t.Fatalf("history current input = %s, want %s", session.historyItems[1], plan.currentInput[0])
	}
	if string(session.historyItems[2]) != string(outputItems[0]) {
		t.Fatalf("history output = %s, want %s", session.historyItems[2], outputItems[0])
	}
}

func TestResponsesWebSocketRememberPlannedResponseResetsCompactionTrigger(t *testing.T) {
	t.Parallel()

	session := &responsesWebSocketSession{
		turnState:    "turn-state-1",
		historyItems: []json.RawMessage{json.RawMessage(`{"type":"message","id":"old"}`)},
	}
	plan := responsesWebSocketRequestPlan{
		signature:    "sig-compact",
		currentInput: []json.RawMessage{json.RawMessage(`{"type":"compaction_trigger"}`)},
	}

	session.rememberPlannedResponse(plan, "resp-compact", nil)

	if session.turnState != "" {
		t.Fatalf("turnState = %q, want cleared", session.turnState)
	}
	if session.lastResponseID != "resp-compact" {
		t.Fatalf("lastResponseID = %q, want resp-compact", session.lastResponseID)
	}
	if session.lastSignature != "sig-compact" {
		t.Fatalf("lastSignature = %q, want sig-compact", session.lastSignature)
	}
	if len(session.historyItems) != 0 {
		t.Fatalf("historyItems = %s, want reset empty history", session.historyItems)
	}
}

func TestHandleResponsesWebSocket_SendsPingAndAcceptsPong(t *testing.T) {
	oldWriteWait := responsesWebSocketWriteWait
	oldPingPeriod := responsesWebSocketPingPeriod
	responsesWebSocketWriteWait = 50 * time.Millisecond
	responsesWebSocketPingPeriod = 10 * time.Millisecond
	t.Cleanup(func() {
		responsesWebSocketWriteWait = oldWriteWait
		responsesWebSocketPingPeriod = oldPingPeriod
	})

	handler := &ProxyHandler{log: logger.New(logger.LevelError), responsesWS: ResponsesWebSocketConfig{Enabled: true}}
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	pinged := make(chan struct{})
	var once sync.Once
	conn.SetPingHandler(func(appData string) error {
		once.Do(func() { close(pinged) })
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-pinged:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket ping")
	}

	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket reader to finish")
	}
}

func TestHandleResponsesWebSocket_ChunkingIndependentNormalWireFormatStress(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-normal-split-ws\"}}\n\n"
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-normal-split-ws\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}}\n\n"
	deliveries := []struct {
		name string
		body func() io.ReadCloser
	}{
		{name: "coalesced", body: func() io.ReadCloser { return io.NopCloser(strings.NewReader(created + failed)) }},
		{name: "split", body: func() io.ReadCloser { return newSplitChunkEOFReadCloser(created, failed) }},
	}
	for _, delivery := range deliveries {
		t.Run(delivery.name, func(t *testing.T) {
			for i := 0; i < 15; i++ {
				handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       delivery.body(),
						Request:    req,
					}, nil
				})
				handler.stats = newStatsCollector()
				serverConn, clientConn := newResponsesWebSocketConnPair(t)
				session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
				request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
				done := make(chan error, 1)
				go func() { done <- session.handleCreateRequest(handler, request) }()

				for _, wantType := range []string{"response.created", "response.failed", "error"} {
					frame := mustReadWebSocketJSON(t, clientConn)
					if frame["type"] != wantType {
						t.Fatalf("iteration %d frame type = %v, want %s", i, frame["type"], wantType)
					}
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("iteration %d handleCreateRequest error = %v", i, err)
					}
				case <-time.After(time.Second):
					t.Fatalf("iteration %d timed out", i)
				}
				assertSingleResponsesWebSocketFailureStats(t, handler, http.StatusTooManyRequests)
				_ = clientConn.Close()
				_ = serverConn.Close()
			}
		})
	}
}

func TestHandleResponsesWebSocket_ClientDisconnectBeforeTerminalRecordsOnce(t *testing.T) {
	second := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n"
	body := newGatedResponsesFailureBody(second, nil)
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()

	created := mustReadWebSocketJSON(t, clientConn)
	if created["type"] != "response.created" {
		t.Fatalf("first frame type = %v, want response.created", created["type"])
	}
	_ = serverConn.Close()
	body.releaseSecond()
	select {
	case err := <-done:
		if !errors.Is(err, errResponsesWebSocketClientWrite) {
			t.Fatalf("handleCreateRequest error = %v, want client write error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client write failure")
	}

	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("client disconnect stats = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if len(snap.StatusCodes) != 1 || snap.StatusCodes[0].Label != "499" || snap.StatusCodes[0].Count != 1 {
		t.Fatalf("client disconnect status codes = %#v, want one 499", snap.StatusCodes)
	}
}

func TestHandleResponsesWebSocket_BufferedTerminalAccountsOnEarlyMetadataWriteFailure(t *testing.T) {
	body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-buffered-write\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-buffered-write\",\"usage\":{\"input_tokens\":8,\"output_tokens\":3,\"total_tokens\":11}}}\n\n"
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"Openai-Model": []string{"gpt-5.4-actual"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	_ = serverConn.Close()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	err := session.handleCreateRequest(handler, request)
	if !errors.Is(err, errResponsesWebSocketClientWrite) {
		t.Fatalf("handleCreateRequest error = %v, want client write error", err)
	}

	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
		t.Fatalf("buffered terminal stats = requests:%d errors:%d, want 1/0", snap.Totals.Requests, snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 8 || snap.Totals.CompletionTokens != 3 || snap.Totals.TotalTokens != 11 {
		t.Fatalf("buffered terminal usage = prompt:%d completion:%d total:%d, want 8/3/11", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
	}
}

type callbackEOFResponsesBody struct {
	reader *strings.Reader
	onEOF  func()
	once   sync.Once
}

func (b *callbackEOFResponsesBody) Read(p []byte) (int, error) {
	if b.reader.Len() > 0 {
		return b.reader.Read(p)
	}
	b.once.Do(func() {
		if b.onEOF != nil {
			b.onEOF()
		}
	})
	return 0, io.EOF
}

func (*callbackEOFResponsesBody) Close() error { return nil }

func TestHandleResponsesWebSocket_BufferedTerminalAccountsDuringShutdownWriteFailure(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-shutdown-buffered\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-shutdown-buffered\",\"usage\":{\"input_tokens\":6,\"output_tokens\":2,\"total_tokens\":8}}}\n\n"
	var handler *ProxyHandler
	handler = newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		body := &callbackEOFResponsesBody{reader: strings.NewReader(stream), onEOF: handler.BeginShutdown}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"Openai-Model": []string{"gpt-5.4-actual"},
			},
			Body:    body,
			Request: req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	_ = serverConn.Close()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	err := session.handleCreateRequest(handler, request)
	if !errors.Is(err, errResponsesWebSocketClientWrite) {
		t.Fatalf("handleCreateRequest error = %v, want client write error", err)
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 || snap.Totals.TotalTokens != 8 {
		t.Fatalf("buffered shutdown terminal stats = requests:%d errors:%d total:%d, want 1/0/8", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}
}

func TestResponsesWebSocketCloseCauseOrdering(t *testing.T) {
	t.Run("client before shutdown", func(t *testing.T) {
		session := &responsesWebSocketSession{}
		handler := &ProxyHandler{}
		session.beginClosing()
		handler.BeginShutdown()
		if !session.clientClosePrecedesShutdown(handler) {
			t.Fatal("client close preceding shutdown was not preserved")
		}
	})

	t.Run("shutdown before client", func(t *testing.T) {
		session := &responsesWebSocketSession{}
		handler := &ProxyHandler{}
		handler.BeginShutdown()
		session.beginClosing()
		if session.clientClosePrecedesShutdown(handler) {
			t.Fatal("client close after shutdown incorrectly won cause ordering")
		}
	})

	t.Run("policy close stamps client cause", func(t *testing.T) {
		session := &responsesWebSocketSession{}
		barrier, owner := session.startControlClose()
		if barrier == nil || !owner {
			t.Fatal("policy close did not acquire close barrier")
		}
		handler := &ProxyHandler{}
		handler.BeginShutdown()
		if !session.clientClosePrecedesShutdown(handler) {
			t.Fatal("policy close did not preserve client cause")
		}
	})

	t.Run("concurrent shutdown publishes one earliest sequence", func(t *testing.T) {
		handler := &ProxyHandler{}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				handler.BeginShutdown()
			}()
		}
		close(start)
		wg.Wait()
		if got := handler.shutdownSequence.Load(); got == 0 {
			t.Fatal("shutdown sequence was not published")
		}
	})
}

func TestHandleResponsesWebSocket_RecordsCompletionBeforeAutoCompactionFinishes(t *testing.T) {
	compactionStarted := make(chan struct{})
	releaseCompaction := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCompaction) }) })

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			close(compactionStarted)
			select {
			case <-releaseCompaction:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"comp-delayed","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint"}]}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-immediate-stats\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-a\",\"content\":[{\"type\":\"output_text\",\"text\":\"a\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-b\",\"content\":[{\"type\":\"output_text\",\"text\":\"b\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-immediate-stats\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"total_tokens\":6}}}\n\n")
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 2,
		AutoCompactMaxBytes: 1 << 20,
		AutoCompactKeepTail: 1,
	}
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	create := newResponsesWebSocketCreateRequest([]interface{}{map[string]interface{}{
		"type":    "message",
		"role":    "user",
		"content": []map[string]string{{"type": "input_text", "text": "trigger compaction"}},
	}})
	if err := conn.WriteJSON(create); err != nil {
		t.Fatalf("failed to write create request: %v", err)
	}
	for i := 0; i < 4; i++ {
		_ = mustReadWebSocketJSON(t, conn)
	}

	select {
	case <-compactionStarted:
	case <-time.After(time.Second):
		t.Fatal("auto-compaction did not start")
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 || snap.Totals.TotalTokens != 6 {
		t.Fatalf("terminal stats while compaction blocked = requests:%d errors:%d total:%d, want 1/0/6", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}

	releaseOnce.Do(func() { close(releaseCompaction) })
	deadline := time.Now().Add(time.Second)
	for {
		snap = handler.stats.snapshot()
		if snap.Totals.TotalTokens == 126 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-compaction usage was not amended: %+v", snap.Totals)
		}
		time.Sleep(time.Millisecond)
	}
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
		t.Fatalf("post-compaction count = requests:%d errors:%d, want 1/0", snap.Totals.Requests, snap.Totals.Errors)
	}
}

type lateTerminalResponsesBody struct {
	first      *strings.Reader
	terminal   *strings.Reader
	release    <-chan struct{}
	secondRead chan struct{}
	secondOnce sync.Once
	closed     chan struct{}
	closeOnce  sync.Once
}

func (b *lateTerminalResponsesBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	b.secondOnce.Do(func() { close(b.secondRead) })
	select {
	case <-b.release:
		return b.terminal.Read(p)
	case <-b.closed:
		return 0, context.Canceled
	}
}

func (b *lateTerminalResponsesBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestHandleResponsesWebSocket_EarlyWriteFailureDrainsLateTerminalBeforeCancel(t *testing.T) {

	releaseTerminal := make(chan struct{})
	upstreamContext := make(chan context.Context, 1)
	largeOutput := strings.Repeat("x", openAIStreamScannerMaxBuffer-(64<<10))
	body := &lateTerminalResponsesBody{
		first:      strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-late-terminal\"}}\n\n"),
		terminal:   strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-late-terminal\",\"output\":\"" + largeOutput + "\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2,\"total_tokens\":11}}}\n\n"),
		release:    releaseTerminal,
		secondRead: make(chan struct{}),
		closed:     make(chan struct{}),
	}
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		upstreamContext <- req.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"Openai-Model": []string{"gpt-5.4-actual"},
			},
			Body:    body,
			Request: req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	_ = serverConn.Close()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	session.terminalObservationWait = 3 * time.Second
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()
	ctx := <-upstreamContext
	select {
	case <-body.secondRead:
	case <-time.After(time.Second):
		t.Fatal("terminal body read did not block")
	}
	select {
	case <-ctx.Done():
		t.Fatalf("client write failure canceled upstream before terminal observation: %v", context.Cause(ctx))
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseTerminal)

	select {
	case err := <-done:
		if !errors.Is(err, errResponsesWebSocketClientWrite) {
			t.Fatalf("handleCreateRequest error = %v, want client write error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleCreateRequest did not finish after terminal release")
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 || snap.Totals.TotalTokens != 11 {
		t.Fatalf("late terminal stats = requests:%d errors:%d total:%d, want 1/0/11", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}
}

func TestHandleResponsesWebSocket_MalformedCompletedRetainsUsageButCountsFailure(t *testing.T) {
	stream := `event: response.created
data: {"type":"response.created","response":{"id":"resp-created-only"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"   ","usage":{"input_tokens":6,"output_tokens":2,"total_tokens":8}}}

`
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"Openai-Model": []string{"gpt-5.4-actual"},
			},
			Body:    io.NopCloser(strings.NewReader(stream)),
			Request: req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()
	_ = mustReadWebSocketJSON(t, clientConn) // metadata
	created := mustReadWebSocketJSON(t, clientConn)
	if created["type"] != "response.created" {
		t.Fatalf("created frame type = %v, want response.created", created["type"])
	}
	completed := mustReadWebSocketJSON(t, clientConn)
	if completed["type"] != "response.completed" {
		t.Fatalf("terminal frame type = %v, want response.completed", completed["type"])
	}
	wrapped := mustReadWebSocketJSON(t, clientConn)
	if got := int(wrapped["status_code"].(float64)); got != http.StatusBadGateway {
		t.Fatalf("wrapped status = %d, want 502", got)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "missing response id") {
			t.Fatalf("handleCreateRequest error = %v, want missing response id", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handleCreateRequest did not finish")
	}

	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 || snap.Totals.TotalTokens != 8 {
		t.Fatalf("malformed completion stats = requests:%d errors:%d total:%d, want 1/1/8", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}
	if snap.Status["5xx"] != 1 {
		t.Fatalf("malformed completion status = %+v, want one 5xx", snap.Status)
	}
}

func TestHandleResponsesWebSocket_StatsKeepDispatchedProviderAcrossCatalogChange(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseTerminal := make(chan struct{})
	providerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-provider-a\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(requestStarted)
		select {
		case <-releaseTerminal:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-provider-a\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1,\"total_tokens\":5}}}\n\n")
	}))
	defer providerA.Close()
	providerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("provider B unexpectedly received request %s", r.URL.Path)
	}))
	defer providerB.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:       "provider-a",
				Type:     "openai-compatible",
				Default:  true,
				BaseURL:  providerA.URL,
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:  "gpt-route-stable",
					Endpoints: []string{"/responses"},
				}},
			},
			{
				ID:       "provider-b",
				Type:     "openai-compatible",
				BaseURL:  providerB.URL,
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:  "provider-b-only",
					Endpoints: []string{"/responses"},
				}},
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	handler.responsesWS.Enabled = true
	handler.responsesWS.DisableAutoCompact = true
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	create := newResponsesWebSocketCreateRequest(nil)
	create["model"] = "gpt-route-stable"
	if err := conn.WriteJSON(create); err != nil {
		t.Fatalf("failed to write create request: %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("provider A request did not start")
	}

	setup := handler.providerSetup()
	setup.modelsMu.Lock()
	owner := setup.models["gpt-route-stable"]
	owner.providerID = "provider-b"
	setup.models["gpt-route-stable"] = owner
	setup.modelsMu.Unlock()
	close(releaseTerminal)

	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	deadline := time.Now().Add(time.Second)
	for {
		snap := handler.stats.snapshot()
		if snap.Totals.Requests == 1 {
			if len(snap.ByProvider) != 1 || snap.ByProvider[0].Provider != "provider-a" || snap.ByProvider[0].Tokens != 5 {
				t.Fatalf("provider attribution = %+v, want provider-a with 5 tokens", snap.ByProvider)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn was not recorded: %+v", snap.Totals)
		}
		time.Sleep(time.Millisecond)
	}
}

type createdThenCanceledResponsesBody struct {
	first      *strings.Reader
	ctx        context.Context
	canceled   chan struct{}
	cancelOnce sync.Once
}

func (b *createdThenCanceledResponsesBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	<-b.ctx.Done()
	b.cancelOnce.Do(func() { close(b.canceled) })
	return 0, b.ctx.Err()
}

func (*createdThenCanceledResponsesBody) Close() error { return nil }

func TestHandleResponsesWebSocket_ClientCloseAfterCreatedCancelsStalledBody(t *testing.T) {
	canceled := make(chan struct{})
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &createdThenCanceledResponsesBody{
				first:    strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stalled-after-created\"}}\n\n"),
				ctx:      req.Context(),
				canceled: canceled,
			},
			Request: req,
		}, nil
	})
	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatalf("failed to write create request: %v", err)
	}
	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("first frame type = %v, want response.created", created["type"])
	}
	if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to send client close: %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("client close after response.created did not cancel stalled upstream body")
	}
	_ = conn.Close()
}

func TestHandleResponsesWebSocket_TransportFailureKeepsDispatchedProvider(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseFailure := make(chan struct{})
	var startedOnce sync.Once

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:       "provider-a",
				Type:     "openai-compatible",
				Default:  true,
				BaseURL:  "http://provider-a.test",
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:  "gpt-route-error",
					Endpoints: []string{"/responses"},
				}},
			},
			{
				ID:       "provider-b",
				Type:     "openai-compatible",
				BaseURL:  "http://provider-b.test",
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:  "provider-b-only",
					Endpoints: []string{"/responses"},
				}},
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	handler.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "provider-a.test" {
			t.Fatalf("request routed to %q, want provider-a.test", req.URL.Host)
		}
		startedOnce.Do(func() { close(requestStarted) })
		<-releaseFailure
		return nil, errors.New("injected transport failure")
	})}
	handler.retryBaseDelay = time.Millisecond
	handler.responsesWS.Enabled = true
	handler.responsesWS.DisableAutoCompact = true
	handler.stats = newStatsCollector()

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	create := newResponsesWebSocketCreateRequest(nil)
	create["model"] = "gpt-route-error"
	if err := conn.WriteJSON(create); err != nil {
		t.Fatalf("failed to write create request: %v", err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("provider A transport request did not start")
	}

	setup := handler.providerSetup()
	setup.modelsMu.Lock()
	owner := setup.models["gpt-route-error"]
	owner.providerID = "provider-b"
	setup.models["gpt-route-error"] = owner
	setup.modelsMu.Unlock()
	close(releaseFailure)

	wrapped := mustReadWebSocketJSON(t, conn)
	if got := int(wrapped["status_code"].(float64)); got != http.StatusBadGateway {
		t.Fatalf("wrapped status = %d, want 502", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		snap := handler.stats.snapshot()
		if snap.Totals.Requests == 1 {
			if snap.Totals.Errors != 1 {
				t.Fatalf("transport failure errors = %d, want 1", snap.Totals.Errors)
			}
			if len(snap.ByProvider) != 1 || snap.ByProvider[0].Provider != "provider-a" {
				t.Fatalf("transport failure provider attribution = %+v, want provider-a", snap.ByProvider)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport failure was not recorded: %+v", snap.Totals)
		}
		time.Sleep(time.Millisecond)
	}
}

type terminalAfterContextDoneBody struct {
	ctx      context.Context
	terminal *strings.Reader
	delay    time.Duration
	once     sync.Once
}

func (b *terminalAfterContextDoneBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		<-b.ctx.Done()
		time.Sleep(b.delay)
	})
	return b.terminal.Read(p)
}

func (*terminalAfterContextDoneBody) Close() error { return nil }

func TestHandleResponsesWebSocket_PreservesUpstreamTimeoutTerminalAndReplayState(t *testing.T) {
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-timeout-race\",\"output\":[{\"type\":\"message\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &terminalAfterContextDoneBody{
				ctx:      req.Context(),
				terminal: strings.NewReader(stream),
				delay:    15 * time.Millisecond,
			},
			Request: req,
		}, nil
	})
	WithStreamingUpstreamTimeout(time.Millisecond)(handler)
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()
	terminal := mustReadWebSocketJSON(t, clientConn)
	if terminal["type"] != "response.completed" {
		t.Fatalf("salvaged frame type = %v, want response.completed", terminal["type"])
	}
	response, _ := terminal["response"].(map[string]interface{})
	if response["id"] != "resp-timeout-race" {
		t.Fatalf("salvaged response = %+v, want id resp-timeout-race", response)
	}
	if output, _ := response["output"].([]interface{}); len(output) != 1 {
		t.Fatalf("salvaged response output = %+v, want original output item", response["output"])
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleCreateRequest error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handleCreateRequest did not finish after salvaged terminal")
	}
	if session.lastResponseID != "resp-timeout-race" {
		t.Fatalf("session last response id = %q, want resp-timeout-race", session.lastResponseID)
	}
	if len(session.historyItems) != 1 {
		t.Fatalf("session history items = %d, want terminal response output preserved", len(session.historyItems))
	}
	var remembered map[string]interface{}
	if err := json.Unmarshal(session.historyItems[0], &remembered); err != nil || remembered["type"] != "message" {
		t.Fatalf("remembered terminal output = %s err=%v, want message item", session.historyItems[0], err)
	}
	followUp := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))
	followUp.PreviousResponseID = "resp-timeout-race"
	if _, err := session.planRequest(handler, followUp); err != nil {
		t.Fatalf("salvaged completion did not preserve replay state: %v", err)
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 || snap.Totals.TotalTokens != 3 {
		t.Fatalf("salvaged terminal stats = requests:%d errors:%d total:%d, want 1/0/3", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}
}

func TestNewProviderJSONRequestPublishesRouteBeforeAuthFailure(t *testing.T) {
	handler := &ProxyHandler{}
	ctx, observer := withProviderRouteObserver(context.Background())
	provider := &providerRuntime{
		id:         "auth-failing-provider",
		kind:       providerTypeOpenAICompatible,
		baseURL:    "http://provider.test",
		authType:   providerAuthTypeBearer,
		authHeader: "Authorization",
	}
	if _, err := handler.newProviderJSONRequest(ctx, provider, http.MethodPost, providerEndpointResponses, []byte(`{"model":"gpt-test"}`), nil, ""); err == nil {
		t.Fatal("newProviderJSONRequest unexpectedly succeeded without an API key")
	}
	route, ok := observer.snapshot()
	if !ok || route.id != "auth-failing-provider" || route.kind != string(providerTypeOpenAICompatible) {
		t.Fatalf("published route = %+v ok=%v, want auth-failing-provider/openai-compatible", route, ok)
	}
}

func TestHandleResponsesWebSocket_CRLFEventNameCompletesConsistently(t *testing.T) {
	stream := "event: response.completed\r\ndata: {\"response\":{\"id\":\"resp-crlf-event\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\r\n\r\n"
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()
	completed := mustReadWebSocketJSON(t, clientConn)
	if completed["type"] != "response.completed" {
		t.Fatalf("completed frame = %+v, want injected response.completed type", completed)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleCreateRequest error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handleCreateRequest did not finish")
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 || snap.Totals.TotalTokens != 4 {
		t.Fatalf("CRLF terminal stats = requests:%d errors:%d total:%d, want 1/0/4", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}
}

func TestHandleResponsesWebSocket_OversizedTerminalFailureRetainsUsage(t *testing.T) {
	payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-overflow-usage","output":"%s","usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}`, strings.Repeat("x", openAIStreamScannerMaxBuffer+4096))
	stream := "event: response.completed\ndata: " + payload + "\n\n"
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"Openai-Model": []string{"gpt-5.4-actual"},
			},
			Body:    io.NopCloser(strings.NewReader(stream)),
			Request: req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()
	_ = mustReadWebSocketJSON(t, clientConn) // metadata
	wrapped := mustReadWebSocketJSON(t, clientConn)
	if got := int(wrapped["status_code"].(float64)); got != http.StatusBadGateway {
		t.Fatalf("wrapped status = %d, want 502", got)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("oversized terminal unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("oversized terminal handler did not finish")
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("oversized terminal count = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 10 || snap.Totals.CompletionTokens != 3 || snap.Totals.TotalTokens != 13 {
		t.Fatalf("oversized terminal usage = prompt:%d completion:%d total:%d, want 10/3/13", snap.Totals.PromptTokens, snap.Totals.CompletionTokens, snap.Totals.TotalTokens)
	}
}

func TestHandleResponsesWebSocket_DoneMarkerPreventsTrailingTerminalAccounting(t *testing.T) {
	stream := "data: [DONE]\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-after-done\",\"usage\":{\"input_tokens\":9,\"output_tokens\":1,\"total_tokens\":10}}}\n\n"
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    req,
		}, nil
	})
	handler.stats = newStatsCollector()
	serverConn, clientConn := newResponsesWebSocketConnPair(t)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	session := newResponsesWebSocketSession(serverConn, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	request := mustParseResponsesWebSocketCreateRequest(t, newResponsesWebSocketCreateRequest(nil))

	done := make(chan error, 1)
	go func() { done <- session.handleCreateRequest(handler, request) }()
	wrapped := mustReadWebSocketJSON(t, clientConn)
	if got := int(wrapped["status_code"].(float64)); got != http.StatusBadGateway {
		t.Fatalf("wrapped status = %d, want 502", got)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "before response.completed") {
			t.Fatalf("handleCreateRequest error = %v, want missing completion", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handleCreateRequest did not finish")
	}
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 || snap.Totals.TotalTokens != 0 {
		t.Fatalf("[DONE] stats = requests:%d errors:%d total:%d, want 1/1/0", snap.Totals.Requests, snap.Totals.Errors, snap.Totals.TotalTokens)
	}
}
