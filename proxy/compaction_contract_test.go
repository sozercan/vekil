package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCompactionContract_CompactEndpointReturnsCodexCompatibleHistory(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("expected compact fallback to call /responses, got %q", r.URL.Path)
		}
		if got := r.Header.Get("session-id"); got != "sess-compact" {
			t.Fatalf("expected compact request to forward session-id, got %q", got)
		}
		if got := r.Header.Get("thread-id"); got != "thread-compact" {
			t.Fatalf("expected compact request to forward thread-id, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Installation-Id"); got != "install-compact" {
			t.Fatalf("expected compact request to forward installation id, got %q", got)
		}
		body := decodeJSONBodyForContract(t, r.Body)
		if !strings.Contains(rawJSONToStringForContract(t, body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
			t.Fatalf("expected compact prompt instructions, got %s", body["instructions"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary from upstream"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItemForContract("system", "system compact context"),
			messageItemForContract("user", "hello remote compact"),
			messageItemForContract("assistant", "FIRST_REMOTE_REPLY"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "sess-compact")
	req.Header.Set("thread-id", "thread-compact")
	req.Header.Set("X-Codex-Installation-Id", "install-compact")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	body := decodeJSONBodyForContract(t, resp.Body)
	output := rawJSONArrayForContract(t, body["output"])
	if len(output) != 3 {
		t.Fatalf("expected retained system/user messages plus compaction item, got %d output items: %s", len(output), body["output"])
	}
	if got := contractMessageText(t, output[0], "system"); got != "system compact context" {
		t.Fatalf("expected compact response to retain original system history, got %q", got)
	}
	if got := contractMessageText(t, output[1], "user"); got != "hello remote compact" {
		t.Fatalf("expected compact response to retain original user history, got %q", got)
	}
	if contractItemType(t, output[2]) != "compaction" {
		t.Fatalf("expected final compact response item to be compaction, got %s", output[2])
	}
	for _, item := range output {
		if contractItemRole(t, item) == "assistant" {
			t.Fatalf("compact response must not include assistant summary messages that duplicate the checkpoint: %s", item)
		}
	}
}

func TestCompactionContract_UnknownCompactionTokenIsPreserved(t *testing.T) {
	const upstreamOpaqueToken = "opaque+server/token/with/slashes=="
	var upstreamInput []json.RawMessage
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		upstreamInput = rawJSONArrayForContract(t, body["input"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-replay","object":"response","status":"completed","output":[]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": upstreamOpaqueToken,
			},
			messageItemForContract("user", "new request wins"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if len(upstreamInput) != 2 {
		t.Fatalf("expected two upstream input items, got %d: %s", len(upstreamInput), upstreamInput)
	}
	first := rawJSONObjectForContract(t, upstreamInput[0])
	if rawJSONToStringForContract(t, first["type"]) != "compaction" {
		t.Fatalf("expected unknown token to remain a compaction item, got %s", upstreamInput[0])
	}
	if rawJSONToStringForContract(t, first["encrypted_content"]) != upstreamOpaqueToken {
		t.Fatalf("expected unknown token to be preserved, got %s", first["encrypted_content"])
	}
}

func TestCompactionContract_RemoteCompactionV2TriggerProducesCompactionItem(t *testing.T) {
	var mu sync.Mutex
	var upstreamRequests []map[string]json.RawMessage
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		mu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		mu.Unlock()

		if _, ok := body["instructions"]; !ok {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-normal\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"normal response\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-normal\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
			return
		}

		input := rawJSONArrayForContract(t, body["input"])
		for _, item := range input {
			if contractItemType(t, item) == "compaction_trigger" {
				t.Fatalf("compact fallback request must not forward compaction_trigger: %s", body["input"])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-v2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"remote v2 checkpoint"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItemForContract("user", "USER_ONE"),
			messageItemForContract("assistant", "FIRST_REMOTE_REPLY"),
			map[string]interface{}{"type": "compaction_trigger"},
		},
		"tools":               []interface{}{},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"stream":              true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE response, got Content-Type %q", got)
	}
	events := parseSSEDataForContract(t, resp.Body)
	if !sseHasCompactionOutputForContract(t, events) {
		t.Fatalf("expected remote compaction v2 SSE to contain a compaction output item, got %#v", events)
	}
	mu.Lock()
	gotRequests := len(upstreamRequests)
	mu.Unlock()
	if gotRequests != 1 {
		t.Fatalf("expected only the compact fallback upstream request, got %d upstream requests", gotRequests)
	}
}

func TestCompactionContract_RemoteCompactionV2PreservesPreviousResponseID(t *testing.T) {
	var upstreamRequest map[string]json.RawMessage
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		upstreamRequest = body
		if !strings.Contains(rawJSONToStringForContract(t, body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
			t.Fatalf("expected compact prompt instructions, got %s", body["instructions"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-v2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"server-side checkpoint"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev",
		"input":                []interface{}{map[string]interface{}{"type": "compaction_trigger"}},
		"stream":               true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := rawJSONToStringForContract(t, upstreamRequest["previous_response_id"]); got != "resp-prev" {
		t.Fatalf("expected compact fallback to preserve previous_response_id, got %q", got)
	}
	input := rawJSONArrayForContract(t, upstreamRequest["input"])
	if len(input) != 0 {
		t.Fatalf("expected compact fallback to strip only the trigger from delta input, got %s", upstreamRequest["input"])
	}
}

func TestCompactionContract_WebSocketResponseProcessedIsControlOnly(t *testing.T) {
	var upstreamRequests int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-%d\"}}\n\n", upstreamRequests)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-%d\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n", upstreamRequests)
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "hello")})); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if err := conn.WriteJSON(map[string]interface{}{"type": "response.processed", "response_id": "resp-1"}); err != nil {
		t.Fatalf("failed to write response.processed frame: %v", err)
	}
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "next")})); err != nil {
		t.Fatalf("failed to write second websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if upstreamRequests != 2 {
		t.Fatalf("expected response.processed not to create an upstream request, got %d", upstreamRequests)
	}
}

func TestCompactionContract_WebSocketRemoteCompactionFollowUpUsesCompactionHistory(t *testing.T) {
	var mu sync.Mutex
	var upstreamRequests []map[string]interface{}
	var normalRequests int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		mu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		mu.Unlock()

		if strings.Contains(fmt.Sprint(body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
			input := body["input"].([]interface{})
			if !strings.Contains(fmt.Sprint(input), "first") {
				t.Fatalf("compact upstream request must use full websocket history, got %v", input)
			}
			for _, item := range input {
				itemObject := item.(map[string]interface{})
				if itemObject["type"] == "compaction_trigger" {
					t.Fatalf("compact upstream request must not forward compaction_trigger: %v", body["input"])
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"resp-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}]}`)
			return
		}

		normalRequests++
		if normalRequests == 2 && r.Header.Get("X-Codex-Turn-State") != "" {
			t.Fatalf("follow-up after synthetic compaction must not use stale turn state, got %q", r.Header.Get("X-Codex-Turn-State"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if normalRequests == 1 {
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
		}
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-normal-%d\"}}\n\n", normalRequests)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-normal-%d\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n", normalRequests)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "first")})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}
	firstCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	firstID := websocketResponseID(t, firstCreated)

	compact := newResponsesWebSocketCreateRequest([]interface{}{map[string]interface{}{"type": "compaction_trigger"}})
	compact["previous_response_id"] = firstID
	if err := conn.WriteJSON(compact); err != nil {
		t.Fatalf("failed to write websocket compaction request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	compactionOutput := mustReadWebSocketJSON(t, conn)
	if compactionOutput["type"] != "response.output_item.done" {
		t.Fatalf("expected compaction output event, got %v", compactionOutput["type"])
	}
	compactCompleted := mustReadWebSocketJSON(t, conn)
	compactionID := websocketResponseID(t, compactCompleted)

	if err := conn.WriteJSON(map[string]interface{}{"type": "response.processed", "response_id": compactionID}); err != nil {
		t.Fatalf("failed to write response.processed websocket frame: %v", err)
	}
	followUp := newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "after")})
	followUp["previous_response_id"] = compactionID
	if err := conn.WriteJSON(followUp); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}
	followUpCreated := mustReadWebSocketJSON(t, conn)
	if followUpCreated["type"] != "response.created" {
		t.Fatalf("expected normal follow-up response.created, got %v", followUpCreated["type"])
	}
	followUpCompleted := mustReadWebSocketJSON(t, conn)
	if followUpCompleted["type"] != "response.completed" {
		t.Fatalf("expected normal follow-up response.completed, got %v", followUpCompleted["type"])
	}

	mu.Lock()
	requests := append([]map[string]interface{}(nil), upstreamRequests...)
	mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("expected first turn, compaction, and normal follow-up upstream requests; got %d", len(requests))
	}
	if strings.Contains(fmt.Sprint(requests[2]["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("follow-up request should not be another compaction request: %#v", requests[2])
	}
	followUpInput := requests[2]["input"].([]interface{})
	if len(followUpInput) != 2 {
		t.Fatalf("expected follow-up replay to contain compaction plus new user message, got %d: %#v", len(followUpInput), followUpInput)
	}
	checkpoint := followUpInput[0].(map[string]interface{})
	if checkpoint["type"] != "message" || checkpoint["role"] != "developer" || !strings.Contains(fmt.Sprint(checkpoint["content"]), "checkpoint summary") {
		t.Fatalf("expected follow-up replay to start with developer checkpoint, got %#v", followUpInput[0])
	}
	followUpUser := followUpInput[1].(map[string]interface{})
	if followUpUser["type"] == "compaction_trigger" {
		t.Fatalf("follow-up replay must not include stale compaction_trigger: %#v", followUpInput)
	}
	if followUpUser["role"] != "user" || !strings.Contains(fmt.Sprint(followUpUser["content"]), "after") {
		t.Fatalf("expected follow-up replay to include latest user message, got %#v", followUpInput[1])
	}
}

func messageItemForContract(role, text string) map[string]interface{} {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]interface{}{
		"type": "message",
		"role": role,
		"content": []map[string]string{
			{"type": contentType, "text": text},
		},
	}
}

func mustMarshalContractJSON(t testing.TB, value interface{}) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return body
}

func decodeJSONBodyForContract(t testing.TB, r io.Reader) map[string]json.RawMessage {
	t.Helper()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read JSON body: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode JSON body %s: %v", string(body), err)
	}
	return decoded
}

func rawJSONArrayForContract(t testing.TB, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode JSON array %s: %v", string(raw), err)
	}
	return items
}

func rawJSONObjectForContract(t testing.TB, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode JSON object %s: %v", string(raw), err)
	}
	return item
}

func rawJSONToStringForContract(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode JSON string %s: %v", string(raw), err)
	}
	return value
}

func contractItemType(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	item := rawJSONObjectForContract(t, raw)
	return rawJSONToStringForContract(t, item["type"])
}

func contractItemRole(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	item := rawJSONObjectForContract(t, raw)
	if len(item["role"]) == 0 {
		return ""
	}
	return rawJSONToStringForContract(t, item["role"])
}

func contractMessageText(t testing.TB, raw json.RawMessage, wantRole string) string {
	t.Helper()
	item := rawJSONObjectForContract(t, raw)
	if gotType := rawJSONToStringForContract(t, item["type"]); gotType != "message" {
		t.Fatalf("expected message item, got %q in %s", gotType, raw)
	}
	if gotRole := rawJSONToStringForContract(t, item["role"]); gotRole != wantRole {
		t.Fatalf("expected role %q, got %q in %s", wantRole, gotRole, raw)
	}
	content := rawJSONArrayForContract(t, item["content"])
	var pieces []string
	for _, rawPart := range content {
		part := rawJSONObjectForContract(t, rawPart)
		if len(part["text"]) == 0 {
			continue
		}
		pieces = append(pieces, rawJSONToStringForContract(t, part["text"]))
	}
	return strings.Join(pieces, "\n")
}

func parseSSEDataForContract(t testing.TB, r io.Reader) []map[string]interface{} {
	t.Helper()
	scanner := bufio.NewScanner(r)
	var dataLines []string
	var events []map[string]interface{}
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("decode SSE data %s: %v", data, err)
		}
		events = append(events, event)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	flush()
	return events
}

func sseHasCompactionOutputForContract(t testing.TB, events []map[string]interface{}) bool {
	t.Helper()
	for _, event := range events {
		if event["type"] != "response.output_item.done" {
			continue
		}
		item, ok := event["item"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected output_item.done item object, got %#v", event["item"])
		}
		if item["type"] == "compaction" && strings.TrimSpace(fmt.Sprint(item["encrypted_content"])) != "" {
			return true
		}
	}
	return false
}
