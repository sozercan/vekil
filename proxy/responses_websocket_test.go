package proxy

import (
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

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestHandleResponsesWebSocket_UpgradeRequiredWithoutUpgradeHeaders(t *testing.T) {
	handler := &ProxyHandler{}
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
		if got := r.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
			t.Fatalf("expected turn metadata header to be forwarded, got %q", got)
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
		"ws_request_header_traceparent": "00-11111111111111111111111111111111-2222222222222222-01",
		"x-codex-turn-metadata":         `{"turn_id":"turn-1"}`,
	}
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
			_, _ = fmt.Fprint(w, `{"id":"comp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}]}`)
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
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-2\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
		default:
			t.Fatalf("unexpected normal upstream request count %d", requestNumber)
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		AutoCompactMaxItems: 3,
		AutoCompactMaxBytes: 1 << 20,
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
			_, _ = fmt.Fprint(w, `{"id":"comp-413","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary after 413"}]}]}`)
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

func TestHandleResponsesWebSocket_LaterTransientResponseFailedStillRelaysEventAndMaps429(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-later-rate-limit\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-later-rate-limit\",\"error\":{\"type\":\"server_error\",\"code\":\"too_many_requests\",\"message\":\"Too Many Requests\"}}}\n\n")
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
	var gotSessionID, gotClientRequestID string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotSessionID = r.Header.Get("session_id")
		gotClientRequestID = r.Header.Get("X-Client-Request-Id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-sess\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-sess\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, http.Header{
		"session_id":          []string{"conv-123"},
		"X-Client-Request-Id": []string{"req-456"},
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

	if gotSessionID != "conv-123" {
		t.Fatalf("expected session_id to be forwarded upstream, got %q", gotSessionID)
	}
	if gotClientRequestID != "req-456" {
		t.Fatalf("expected X-Client-Request-Id to be forwarded upstream, got %q", gotClientRequestID)
	}
}

func startResponsesWebSocketProxyServer(t *testing.T, handler *ProxyHandler) *httptest.Server {
	t.Helper()
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
