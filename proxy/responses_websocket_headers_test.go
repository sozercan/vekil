package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestResponsesWebSocketPerTurnHeadersAndStringInput(t *testing.T) {
	captured := make(chan *http.Request, 2)
	bodies := make(chan map[string]json.RawMessage, 2)
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		captured <- r
		bodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-headers\"}}\n\n")
	})
	h.responsesWS.DisableAutoCompact = true
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), http.Header{
		"X-Initiator": {"user"}, "X-Client-Session-Id": {"session-original"},
	})
	defer func() { _ = conn.Close() }()
	create := newResponsesWebSocketCreateRequest(nil)
	create["input"] = "hello"
	for _, interaction := range []string{"turn-one", "turn-two"} {
		create["headers"] = map[string]string{
			"X-Initiator": "agent", "X-Interaction-Id": interaction,
			"Authorization": "untrusted", "X-Codex-Turn-State": "untrusted-state",
		}
		if err := conn.WriteJSON(create); err != nil {
			t.Fatal(err)
		}
		if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
			t.Fatalf("turn response = %#v", frame)
		}
		r := <-captured
		if r.Header.Get("X-Initiator") != "agent" || r.Header.Get("X-Interaction-Id") != interaction || r.Header.Get("X-Client-Session-Id") != "session-original" {
			t.Fatalf("request attribution = %+v", r.Header)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Codex-Turn-State") != "" {
			t.Fatal("per-turn headers replaced provider credentials or trusted state")
		}
		body := <-bodies
		if _, ok := body["headers"]; ok {
			t.Fatal("per-turn headers leaked into HTTP JSON")
		}
		var input []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(body["input"], &input) != nil || len(input) == 0 || input[len(input)-1].Role != "user" || input[len(input)-1].Content != "hello" {
			t.Fatalf("string input translation = %s", body["input"])
		}
		create["previous_response_id"] = "resp-headers"
		delete(create, "stream") // Omitted and true have identical streaming semantics.
	}
}

func TestResponsesWebSocketRejectsUnsupportedEnvelope(t *testing.T) {
	for _, fields := range []string{
		`"stream":false`, `"stream":null`, `"stream":"true"`,
		`"Stream":false`, `"STREAM_ID":"parallel"`,
		`"stream_id":"parallel"`, `"stream_id":null`, `"input":42`,
		`"headers":{"X-Interaction-Id":"one\r\nInjected: value"}`,
		`"headers":{"Bad Header":"value"}`,
		`"headers":{"X-Interaction-Id":"one","x-interaction-id":"two"}`,
		`"headers":{"X-Interaction-Id":["one","two"]}`,
		`"Headers":{},"headers":{}`,
		`"model":"another-model"`,
		`"headers":{},"headers":{"X-Interaction-Id":"two"}`,
		`"headers":{"X-Interaction-Id":"one","X-Interaction-Id":"two"}`,
		`"headers":{"X-Interaction-Id":"one","X-Interaction-\u0049d":"two"}`,
		`"input":"first","input":"second"`,
		`"type":"response.create"`,
	} {
		t.Run(fields, func(t *testing.T) {
			if _, err := parseResponsesWebSocketCreateRequest([]byte(`{"type":"response.create","model":"gpt-5.4",` + fields + `}`)); err == nil {
				t.Fatalf("unsupported envelope accepted: %s", fields)
			}
		})
	}
	if err := validateResponsesWebSocketHeaders(map[string]string{"X-Interaction-Id": strings.Repeat("x", (8<<10)+1)}); err == nil {
		t.Fatal("oversized header accepted")
	}
}

func TestResponsesWebSocketDuplicateKeysRejectBeforeDispatch(t *testing.T) {
	var sends atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp-valid"}}`+"\n\n")
	})
	h.stats = newStatsCollector()
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	for _, fields := range []string{
		`"model":"another-model"`,
		`"headers":{"X-Initiator":"user"},"headers":{"X-Initiator":"agent"}`,
		`"headers":{"X-Interaction-Id":"one","X-Interaction-Id":"two"}`,
		`"headers":{"X-Interaction-Id":"one","X-Interaction-\u0049d":"two"}`,
	} {
		payload := `{"type":"response.create","model":"gpt-5.4",` + fields + `}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] != "error" || frame["status_code"] != float64(http.StatusBadRequest) {
			t.Fatalf("duplicate keys accepted: %s: %#v", payload, frame)
		}
	}
	if sends.Load() != 0 || h.stats.taskUsage.snapshot().Totals.Sends != 0 {
		t.Fatalf("ambiguous envelope dispatched: sends=%d task=%+v", sends.Load(), h.stats.taskUsage.snapshot())
	}
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" || sends.Load() != 1 {
		t.Fatalf("valid turn after local rejection = %#v, sends=%d", frame, sends.Load())
	}
}
