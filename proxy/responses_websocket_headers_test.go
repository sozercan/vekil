package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
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
