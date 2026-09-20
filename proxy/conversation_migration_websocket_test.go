package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func conversationWebSocketTurn(t *testing.T, conn *websocket.Conn, fields map[string]any) map[string]any {
	t.Helper()
	fields["type"], fields["model"], fields["store"] = "response.create", "coding", false
	if err := conn.WriteJSON(fields); err != nil {
		t.Fatal(err)
	}
	for {
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] == "response.created" {
			continue
		}
		if frame["type"] != "response.completed" {
			t.Fatalf("websocket turn failed: %v", frame)
		}
		response, ok := frame["response"].(map[string]any)
		if !ok {
			t.Fatalf("missing response: %v", frame)
		}
		diagnostic, ok := response["vekil"].(map[string]any)
		if !ok || diagnostic["history"] != "saved" {
			t.Fatalf("websocket completion was not saved: %v", frame)
		}
		return response
	}
}

func TestConversationMigrationWebSocketContinuationsAndReconnect(t *testing.T) {
	var outage atomic.Bool
	var eastSends, westSends atomic.Int32
	var mu sync.Mutex
	var westBodies []string
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		if strings.HasPrefix(req.URL.Host, "east.") {
			eastSends.Add(1)
			if outage.Load() {
				return nil, errors.New("east connection refused before write")
			}
			return conversationResponse(t, req, "ws-east-1",
				map[string]any{"type": "reasoning", "id": "ws-reason-east-1", "encrypted_content": "ws-private-east-1", "summary": []any{}},
				conversationText("The original answer.")), nil
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		westBodies = append(westBodies, string(body))
		mu.Unlock()
		if bytes.Contains(body, []byte("ws-private-east")) || bytes.Contains(body, []byte("previous_response_id")) || req.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("websocket sent provider-owned state while reconstructing store:false history")
		}
		id := fmt.Sprintf("ws-west-%d", westSends.Add(1))
		return conversationResponse(t, req, id, conversationText("Answer for "+id)), nil
	})
	h, cfg := newConversationAPIHandler(t, transport, nil)
	server := startResponsesWebSocketProxyServer(t, h)
	conn := mustDialResponsesWebSocket(t, server, http.Header{"Session_id": {"ws-client"}})
	defer func() { _ = conn.Close() }()
	input := func(text string) []any { return []any{map[string]any{"role": "user", "content": text}} }
	first := conversationWebSocketTurn(t, conn, map[string]any{"instructions": "Keep the initial instruction.", "input": input("Original question.")})
	if first["id"] != "ws-east-1" {
		t.Fatalf("first target: %v", first)
	}
	outage.Store(true)
	migrated := conversationWebSocketTurn(t, conn, map[string]any{"previous_response_id": first["id"], "input": input("Continue after outage.")})
	if migrated["id"] != "ws-west-1" || migrated["vekil"].(map[string]any)["migration"] != "completed" {
		t.Fatalf("migration was not completed: %v", migrated)
	}
	conversationWebSocketTurn(t, conn, map[string]any{"previous_response_id": migrated["id"], "input": input("Next west turn.")})
	_ = conn.Close()
	stopConversationAPIHandler(t, h)
	server.Close()
	h, _ = newConversationAPIHandler(t, transport, &cfg)
	server = startResponsesWebSocketProxyServer(t, h)
	conn = mustDialResponsesWebSocket(t, server, nil)
	conversationWebSocketTurn(t, conn, map[string]any{"previous_response_id": "ws-west-2", "input": input("Reconnect after restart.")})
	conversationWebSocketTurn(t, conn, map[string]any{"previous_response_id": "ws-east-1", "input": input("Branch from original east answer.")})
	mu.Lock()
	defer mu.Unlock()
	if eastSends.Load() != 3 || westSends.Load() != 4 || len(westBodies) != 4 {
		t.Fatalf("unexpected physical sends east=%d west=%d", eastSends.Load(), westSends.Load())
	}
	for _, text := range []string{"Keep the initial instruction.", "Original question.", "The original answer.", "Continue after outage."} {
		if !strings.Contains(westBodies[0], text) {
			t.Errorf("migration lost %q", text)
		}
	}
	if !strings.Contains(westBodies[2], "Next west turn.") || !strings.Contains(westBodies[2], "Answer for ws-west-2") {
		t.Fatal("reconnect lost saved west history")
	}
	if strings.Contains(westBodies[3], "Next west turn.") || strings.Contains(westBodies[3], "Answer for ws-west") {
		t.Fatal("older branch acquired future west history")
	}
	route, _ := h.resolveModelRouteForRequest("coding", providerEndpointResponses)
	owner := h.stateBindings.lookup(stateBindingTypeResponseID, "ws-east-1")
	if h.stateBindings.ownerTarget(owner.owner, route) != "east" {
		t.Fatal("old response ownership changed")
	}
}

func TestConversationMigrationWebSocketUnknownReconnect(t *testing.T) {
	var sends atomic.Int32
	h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		sends.Add(1)
		return nil, errors.New("unexpected send")
	}), nil)
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "coding", "previous_response_id": "missing", "input": []any{map[string]any{"role": "user", "content": "continue"}}}); err != nil {
		t.Fatal(err)
	}
	frame := mustReadWebSocketJSONSkipMetadata(t, conn)
	encoded, _ := json.Marshal(frame)
	if frame["type"] != "error" || !bytes.Contains(encoded, []byte("conversation_history_unavailable")) || sends.Load() != 0 {
		t.Fatalf("unknown reconnect: %s sends=%d", encoded, sends.Load())
	}
}

func TestConversationMigrationWebSocketStagingPreservesSavedSource(t *testing.T) {
	var sends atomic.Int32
	h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		number := sends.Add(1)
		if number == 2 && strings.HasPrefix(req.URL.Host, "east.") {
			return nil, errors.New("prewrite outage")
		}
		if number == 3 {
			body, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
			for _, want := range []string{"Original instruction.", "Initial request.", "Initial answer.", "Staged input.", "Generate now.", "current_tool"} {
				if !bytes.Contains(body, []byte(want)) {
					t.Errorf("staging lost %q", want)
				}
			}
			if bytes.Contains(body, []byte("staged_tool")) {
				t.Error("staging retained a replaced tool catalog")
			}
		}
		return conversationResponse(t, req, fmt.Sprintf("staged-%d", number), conversationText("Initial answer.")), nil
	}), nil)
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	first := conversationWebSocketTurn(t, conn, map[string]any{"instructions": "Original instruction.", "input": "Initial request."})
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "coding", "store": false, "generate": false, "previous_response_id": first["id"], "input": []any{conversationToolCatalog("staged_tool"), map[string]any{"role": "user", "content": "Staged input."}}}); err != nil {
		t.Fatal(err)
	}
	var stagedID string
	for stagedID == "" {
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] == "response.completed" {
			stagedID = frame["response"].(map[string]any)["id"].(string)
		} else if frame["type"] != "response.created" {
			t.Fatalf("staging failed: %v", frame)
		}
	}
	if sends.Load() != 1 {
		t.Fatal("staging dispatched inference")
	}
	completed := conversationWebSocketTurn(t, conn, map[string]any{"previous_response_id": stagedID, "input": []any{conversationToolCatalog("current_tool"), map[string]any{"role": "user", "content": "Generate now."}}})
	if completed["vekil"].(map[string]any)["migration"] != "completed" || sends.Load() != 3 {
		t.Fatalf("staged continuation failed to migrate: %v sends=%d", completed, sends.Load())
	}
}

func TestConversationMigrationWebSocketIndependentImport(t *testing.T) {
	var sends atomic.Int32
	h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		sends.Add(1)
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte("previous_response_id")) || !bytes.Contains(body, []byte("Earlier answer.")) {
			t.Error("independent import retained foreign lineage or lost history")
		}
		return conversationResponse(t, req, "imported", conversationText("continued")), nil
	}), nil)
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	conversationWebSocketTurn(t, conn, map[string]any{
		"previous_response_id": "foreign-response",
		"headers":              map[string]string{"X-Vekil-History-Complete": "true"},
		"input":                []any{map[string]any{"role": "user", "content": "Earlier question."}, conversationText("Earlier answer."), map[string]any{"role": "user", "content": "Continue."}},
	})
	if sends.Load() != 1 {
		t.Fatalf("import sends = %d", sends.Load())
	}
}
