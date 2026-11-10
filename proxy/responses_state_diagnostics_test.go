package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/logger"
)

type responsesStateDiagnosticCase struct {
	name       string
	tokens     []string
	wantCode   string
	wantDetail string
}

func responsesStateDiagnosticCases() []responsesStateDiagnosticCase {
	const unavailable = "provider_state_unavailable"
	const missingDetail = "one or more state values have no live binding in this Vekil process"
	const conflictDetail = "conflicting provider-bound state for explicit model route"
	return []responsesStateDiagnosticCase{
		{"known then missing", []string{"opaque-known-a", "opaque-missing"}, unavailable, missingDetail},
		{"missing then known", []string{"opaque-missing", "opaque-known-a"}, unavailable, missingDetail},
		{"all missing", []string{"opaque-missing", "opaque-missing-b"}, unavailable, missingDetail},
		{"different owners", []string{"opaque-known-a", "opaque-known-b"}, "", conflictDetail},
		{"missing before different owners", []string{"opaque-missing", "opaque-known-a", "opaque-known-b"}, "", conflictDetail},
		{"missing between different owners", []string{"opaque-known-a", "opaque-missing", "opaque-known-b"}, "", conflictDetail},
		{"missing after different owners", []string{"opaque-known-a", "opaque-known-b", "opaque-missing"}, "", conflictDetail},
		{"tombstone", []string{"opaque-tombstone"}, "", conflictDetail},
		{"missing before tombstone", []string{"opaque-missing", "opaque-tombstone"}, "", conflictDetail},
		{"tombstone before missing", []string{"opaque-tombstone", "opaque-missing"}, "", conflictDetail},
		{"cross route", []string{"opaque-cross-route"}, "", conflictDetail},
		{"missing before cross route", []string{"opaque-missing", "opaque-cross-route"}, "", conflictDetail},
		{"cross route before missing", []string{"opaque-cross-route", "opaque-missing"}, "", conflictDetail},
		{"malformed", []string{""}, "", "encrypted_content"},
		{"missing before malformed", []string{"opaque-missing", ""}, "", "encrypted_content"},
	}
}

// Provider configuration, request handling, transport and serialization are
// real. Only prior ownership observations are seeded directly: these tests
// exercise the diagnostic after proof is missing, not persistence or exposure.
func newResponsesStateDiagnosticHandler(t *testing.T) (*ProxyHandler, *explicitResponsesStateTarget, *explicitResponsesStateTarget) {
	t.Helper()
	primary, secondary := &explicitResponsesStateTarget{}, &explicitResponsesStateTarget{}
	primaryServer := httptest.NewServer(http.HandlerFunc(primary.serveHTTP))
	secondaryServer := httptest.NewServer(http.HandlerFunc(secondary.serveHTTP))
	t.Cleanup(primaryServer.Close)
	t.Cleanup(secondaryServer.Close)
	h := newExplicitRouteResponsesWebSocketHandler(t, primaryServer.URL, secondaryServer.URL)
	store, err := h.ensureStateBindingStore()
	if err != nil {
		t.Fatal(err)
	}
	ownerA := stateBindingOwner{routeID: "public-ws-route", targetID: "primary"}
	ownerB := stateBindingOwner{routeID: "public-ws-route", targetID: "secondary"}
	for _, stateType := range []stateBindingType{stateBindingTypeEncryptedContent, stateBindingTypeTurnState, stateBindingTypeResponseID} {
		store.bind(stateType, "opaque-known-a", ownerA)
		store.bind(stateType, "opaque-known-b", ownerB)
		store.bind(stateType, "opaque-cross-route", stateBindingOwner{routeID: "other-private-route", targetID: "primary"})
		store.bind(stateType, "opaque-tombstone", ownerA)
		store.bind(stateType, "opaque-tombstone", ownerB)
	}
	return h, primary, secondary
}

func assertResponsesStateDiagnostic(t *testing.T, raw []byte, wantCode, wantDetail string, websocket bool) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("invalid error JSON: %s: %v", raw, err)
	}
	var detail struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    *string `json:"code"`
	}
	if err := json.Unmarshal(envelope["error"], &detail); err != nil {
		t.Fatalf("invalid error object: %s: %v", raw, err)
	}
	if !strings.Contains(detail.Message, wantDetail) {
		t.Errorf("message = %q, want detail %q", detail.Message, wantDetail)
	}
	if websocket {
		if string(envelope["type"]) != `"error"` || string(envelope["status_code"]) != "400" {
			t.Errorf("expected WebSocket 400 error frame: %s", raw)
		}
		if wantCode == "" {
			wantCode = "invalid_request_error"
		}
		if detail.Type != "" {
			t.Errorf("WebSocket error type changed: %s", raw)
		}
	} else if detail.Type != "invalid_request_error" {
		t.Errorf("HTTP error type = %q", detail.Type)
	}
	if wantCode == "" {
		if detail.Code != nil {
			t.Errorf("error code = %q, want null", *detail.Code)
		}
	} else if detail.Code == nil || *detail.Code != wantCode {
		t.Errorf("error code = %s, want %q", envelope["error"], wantCode)
	}
	assertResponsesStateDiagnosticPrivacy(t, string(raw))
}

func assertResponsesStateDiagnosticPrivacy(t *testing.T, text string) {
	t.Helper()
	for _, private := range []string{"opaque-", "other-private-route", "public-ws-route", "ws-primary", "ws-secondary", "physical-primary", "physical-secondary", "primary-key", "secondary-key", "127.0.0.1"} {
		if strings.Contains(text, private) {
			t.Errorf("diagnostic exposed %q: %s", private, text)
		}
	}
}

func TestExplicitResponsesStateDiagnosticsHTTP(t *testing.T) {
	for _, surface := range []string{"json", "stream", "compact"} {
		for _, itemType := range []string{"reasoning", "context_compaction"} {
			for _, tc := range responsesStateDiagnosticCases() {
				t.Run(surface+"/"+itemType+"/"+tc.name, func(t *testing.T) {
					h, primary, secondary := newResponsesStateDiagnosticHandler(t)
					var logs bytes.Buffer
					h.log = logger.NewWithWriter(logger.LevelError, &logs)
					input := make([]map[string]any, 0, len(tc.tokens))
					for _, token := range tc.tokens {
						input = append(input, map[string]any{"type": itemType, "encrypted_content": token})
					}
					body, err := json.Marshal(map[string]any{"model": "public-ws-model", "input": input, "stream": surface == "stream"})
					if err != nil {
						t.Fatal(err)
					}
					path := "/v1/responses"
					handle := h.HandleResponses
					if surface == "compact" {
						path, handle = "/v1/responses/compact", h.HandleCompact
					}
					w := httptest.NewRecorder()
					handle(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
					if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") != "application/json" {
						t.Fatalf("response = %d %v %s, want pre-stream JSON 400", w.Code, w.Header(), w.Body.String())
					}
					assertResponsesStateDiagnostic(t, w.Body.Bytes(), tc.wantCode, tc.wantDetail, false)
					assertResponsesStateDiagnosticPrivacy(t, logs.String())
					if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
						t.Fatalf("rejection sent upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
					}
				})
			}
		}
	}
}

func TestExplicitResponsesStateDiagnosticsWebSocket(t *testing.T) {
	for _, tc := range responsesStateDiagnosticCases() {
		t.Run(tc.name, func(t *testing.T) {
			h, primary, secondary := newResponsesStateDiagnosticHandler(t)
			server := startResponsesWebSocketProxyServer(t, h)
			conn := mustDialResponsesWebSocket(t, server, nil)
			defer func() { _ = conn.Close() }()
			input := make([]any, 0, len(tc.tokens))
			for _, token := range tc.tokens {
				input = append(input, map[string]any{"type": "reasoning", "encrypted_content": token})
			}
			request := newResponsesWebSocketCreateRequest(input)
			request["model"] = "public-ws-model"
			if err := conn.WriteJSON(request); err != nil {
				t.Fatal(err)
			}
			frame := mustReadWebSocketJSON(t, conn)
			raw, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			assertResponsesStateDiagnostic(t, raw, tc.wantCode, tc.wantDetail, true)
			if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
				t.Fatalf("rejection sent upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
			}
		})
	}
}

func TestExplicitResponsesStateDiagnosticsPinnedWebSocket(t *testing.T) {
	const unavailable = "provider_state_unavailable"
	const missingDetail = "one or more state values have no live binding in this Vekil process"
	const conflictDetail = "conflicting provider-bound state for explicit model route"
	for _, tc := range []responsesStateDiagnosticCase{
		{"conflicting owner", []string{"opaque-known-b"}, "", conflictDetail},
		{"conflicting owner then missing", []string{"opaque-known-b", "opaque-missing"}, "", conflictDetail},
		{"missing then conflicting owner", []string{"opaque-missing", "opaque-known-b"}, "", conflictDetail},
		{"matching owner then missing", []string{"opaque-known-a", "opaque-missing"}, unavailable, missingDetail},
		{"missing then matching owner", []string{"opaque-missing", "opaque-known-a"}, unavailable, missingDetail},
		{"all missing", []string{"opaque-missing", "opaque-missing-b"}, unavailable, missingDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var primaryCalls, secondaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_primary_pinned\",\"model\":\"physical-primary\"}}\n\n")
				_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_primary_pinned\",\"model\":\"physical-primary\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondaryCalls.Add(1)
				http.Error(w, "unexpected secondary send", http.StatusInternalServerError)
			}))
			defer secondary.Close()
			h := newExplicitRouteResponsesWebSocketHandler(t, primary.URL, secondary.URL)
			route, known := h.resolveModelRouteForRequest("public-ws-model", providerEndpointResponses)
			if !known || route == nil {
				t.Fatal("explicit websocket route was not resolved")
			}
			bindExplicitEncryptedContentForTest(t, h, route, "primary", "opaque-known-a")
			bindExplicitEncryptedContentForTest(t, h, route, "secondary", "opaque-known-b")
			server := startResponsesWebSocketProxyServer(t, h)
			conn := mustDialResponsesWebSocket(t, server, nil)
			defer func() { _ = conn.Close() }()

			first := newResponsesWebSocketCreateRequest([]any{})
			first["model"] = "public-ws-model"
			if err := conn.WriteJSON(first); err != nil {
				t.Fatal(err)
			}
			if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.created" {
				t.Fatalf("first created frame = %+v", frame)
			}
			if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
				t.Fatalf("first completed frame = %+v", frame)
			}

			input := make([]any, 0, len(tc.tokens))
			for _, token := range tc.tokens {
				input = append(input, map[string]any{"type": "reasoning", "encrypted_content": token})
			}
			// Omitting previous_response_id resets replay history, but the
			// established WebSocket session remains pinned to primary.
			second := newResponsesWebSocketCreateRequest(input)
			second["model"] = "public-ws-model"
			if err := conn.WriteJSON(second); err != nil {
				t.Fatal(err)
			}
			frame := mustReadWebSocketJSON(t, conn)
			raw, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			assertResponsesStateDiagnostic(t, raw, tc.wantCode, tc.wantDetail, true)
			if primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
				t.Fatalf("rejected turn sent upstream: primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
			}
		})
	}
}

func TestExplicitResponsesStateDiagnosticsMemory(t *testing.T) {
	// Traces are rendered as text by the shim. Only the trusted turn-state
	// header reaches state extraction, so there is no mixed-token memory case.
	for _, tc := range []responsesStateDiagnosticCase{
		{"missing header", []string{"opaque-missing"}, "provider_state_unavailable", "one or more state values have no live binding in this Vekil process"},
		{"tombstone header", []string{"opaque-tombstone"}, "", "conflicting provider-bound state"},
		{"cross route header", []string{"opaque-cross-route"}, "", "conflicting provider-bound state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, primary, secondary := newResponsesStateDiagnosticHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(`{"model":"public-ws-model","traces":[{"text":"synthetic trace"}]}`))
			req.Header.Set("X-Codex-Turn-State", tc.tokens[0])
			w := httptest.NewRecorder()
			h.HandleMemorySummarize(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			assertResponsesStateDiagnostic(t, w.Body.Bytes(), tc.wantCode, tc.wantDetail, false)
			if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
				t.Fatalf("rejection sent upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
			}
		})
	}
}

func TestExplicitResponsesStateDiagnosticsKnownOwnerControl(t *testing.T) {
	h, primary, secondary := newResponsesStateDiagnosticHandler(t)
	w := httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-ws-model","input":[{"type":"reasoning","encrypted_content":"opaque-known-b"}]}`)))
	if w.Code != http.StatusOK || primary.calls.Load() != 0 || secondary.calls.Load() != 1 {
		t.Fatalf("known-owner control: status=%d primary=%d secondary=%d body=%s", w.Code, primary.calls.Load(), secondary.calls.Load(), w.Body.String())
	}
	body, _ := secondary.onlyRequest(t)
	if !bytes.Contains(body, []byte("opaque-known-b")) {
		t.Fatal("known owner control lost provider state")
	}
}

func TestExplicitResponsesStateDiagnosticsLineage(t *testing.T) {
	for _, headers := range []http.Header{
		{"X-Codex-Turn-State": []string{"opaque-missing"}},
		{"X-Codex-Turn-State": []string{"opaque-known-a"}},
	} {
		previous := "opaque-known-a"
		if headers.Get("X-Codex-Turn-State") == previous {
			previous = "opaque-missing"
		}
		t.Run(previous, func(t *testing.T) {
			h, primary, secondary := newResponsesStateDiagnosticHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-ws-model","previous_response_id":"`+previous+`","input":"continue"}`))
			req.Header = headers
			w := httptest.NewRecorder()
			h.HandleResponses(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			assertResponsesStateDiagnostic(t, w.Body.Bytes(), "provider_state_unavailable", "one or more state values have no live binding in this Vekil process", false)
			if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
				t.Fatalf("rejection sent upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
			}
		})
	}
}
