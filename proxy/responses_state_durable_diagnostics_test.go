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
)

// Prior observations are seeded, not inferred from configuration. Reopening
// exercises the persisted keyed owners; these tests prove rejection diagnostics,
// while the durable wire/restart suite proves actual issuance and continuation.
func newDurableDiagnosticStore(t *testing.T) *stateBindingStore {
	t.Helper()
	s, config := newDurableStoreFixture(t, 32)
	a := stateBindingOwner{routeID: "public-ws-route", targetID: "primary", identity: [32]byte{1}}
	b := stateBindingOwner{routeID: "public-ws-route", targetID: "secondary", identity: [32]byte{2}}
	for _, kind := range []stateBindingType{stateBindingTypeEncryptedContent, stateBindingTypeTurnState, stateBindingTypeResponseID} {
		for _, seed := range []struct {
			token string
			owner stateBindingOwner
		}{
			{"opaque-known-a", a}, {"opaque-known-b", b},
			{"opaque-cross-route", stateBindingOwner{routeID: "other-private-route", targetID: "primary", identity: [32]byte{1}}},
			{"opaque-tombstone", a}, {"opaque-tombstone", b},
		} {
			if result := s.bind(kind, seed.token, seed.owner); result.err != nil {
				t.Fatal(result.err)
			}
		}
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDurableStoreFixture(t, reopened) })
	return reopened
}

func newDurableDiagnosticHandler(t *testing.T) (*ProxyHandler, *explicitResponsesStateTarget, *explicitResponsesStateTarget) {
	t.Helper()
	h, primary, secondary := newResponsesStateDiagnosticHandler(t)
	h.stateBindings = newDurableDiagnosticStore(t)
	return h, primary, secondary
}

func durableDiagnosticDetail(tc responsesStateDiagnosticCase) string {
	if tc.wantCode == "provider_state_unavailable" {
		return errDurableStateUnknown.Error()
	}
	return tc.wantDetail
}

func TestDurableStateDiagnosticsOrdering(t *testing.T) {
	s := newDurableDiagnosticStore(t)
	for _, tc := range responsesStateDiagnosticCases() {
		t.Run(tc.name, func(t *testing.T) {
			var tokens []stateBindingToken
			for _, token := range tc.tokens {
				tokens = append(tokens, stateBindingToken{stateBindingTypeEncryptedContent, token})
			}
			want := stateBindingLookupConflict
			if tc.wantCode != "" {
				want = stateBindingLookupUnknown
			}
			if result := s.resolveForRoute("public-ws-route", "", tokens); result.err != nil || result.outcome != want {
				t.Fatalf("resolve = %+v, want %v", result, want)
			}
		})
	}
	for _, known := range []string{"opaque-known-a", "opaque-known-b"} {
		for _, reverse := range []bool{false, true} {
			tokens := []stateBindingToken{{stateBindingTypeEncryptedContent, known}, {stateBindingTypeEncryptedContent, "opaque-missing"}}
			if reverse {
				tokens[0], tokens[1] = tokens[1], tokens[0]
			}
			want := stateBindingLookupUnknown
			if known == "opaque-known-b" {
				want = stateBindingLookupConflict
			}
			if result := s.resolveForRoute("public-ws-route", "primary", tokens); result.err != nil || result.outcome != want {
				t.Fatalf("pinned %s reverse=%t: %+v, want %v", known, reverse, result, want)
			}
		}
	}
	// Valid input cannot disguise a failed store as a missing/conflicting token.
	s.durable.mu.Lock()
	s.durable.failed = errDurableStateIO
	s.durable.mu.Unlock()
	for _, token := range []string{"opaque-known-a", "opaque-missing", "opaque-tombstone"} {
		if result := s.resolveForRoute("public-ws-route", "primary", []stateBindingToken{{stateBindingTypeEncryptedContent, token}}); result.err != errDurableStateIO {
			t.Fatalf("failed store %s = %+v", token, result)
		}
	}
}

func TestDurableResponsesStateDiagnostics(t *testing.T) {
	for _, surface := range []string{"json", "stream", "compact", "websocket"} {
		for _, tc := range responsesStateDiagnosticCases() {
			t.Run(surface+"/"+tc.name, func(t *testing.T) {
				h, primary, secondary := newDurableDiagnosticHandler(t)
				input := make([]any, 0, len(tc.tokens))
				for _, token := range tc.tokens {
					input = append(input, map[string]any{"type": "reasoning", "encrypted_content": token})
				}
				var raw []byte
				if surface == "websocket" {
					server := startResponsesWebSocketProxyServer(t, h)
					conn := mustDialResponsesWebSocket(t, server, nil)
					defer func() { _ = conn.Close() }()
					request := newResponsesWebSocketCreateRequest(input)
					request["model"] = "public-ws-model"
					if err := conn.WriteJSON(request); err != nil {
						t.Fatal(err)
					}
					raw, _ = json.Marshal(mustReadWebSocketJSON(t, conn))
				} else {
					body, _ := json.Marshal(map[string]any{"model": "public-ws-model", "input": input, "stream": surface == "stream"})
					path, handle := "/v1/responses", h.HandleResponses
					if surface == "compact" {
						path, handle = "/v1/responses/compact", h.HandleCompact
					}
					w := httptest.NewRecorder()
					handle(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
					if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") != "application/json" {
						t.Fatalf("response = %d %v %s", w.Code, w.Header(), w.Body.String())
					}
					raw = w.Body.Bytes()
				}
				assertResponsesStateDiagnostic(t, raw, tc.wantCode, durableDiagnosticDetail(tc), surface == "websocket")
				if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
					t.Fatal("diagnostic made an upstream send")
				}
			})
		}
	}
}

func TestDurableResponsesStateDiagnosticsHeadersAndStorage(t *testing.T) {
	for _, surface := range []string{"responses", "compact", "memory", "websocket"} {
		for _, storageFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/storage=%t", surface, storageFailure), func(t *testing.T) {
				h, primary, secondary := newDurableDiagnosticHandler(t)
				if storageFailure {
					h.stateBindings.durable.failed = errDurableStateIO
				}
				wantStatus, wantCode := http.StatusBadRequest, "provider_state_unavailable"
				if storageFailure {
					wantStatus, wantCode = http.StatusServiceUnavailable, "state_binding_storage_unavailable"
				}
				var raw []byte
				if surface == "websocket" {
					server := startResponsesWebSocketProxyServer(t, h)
					conn := mustDialResponsesWebSocket(t, server, nil)
					defer func() { _ = conn.Close() }()
					// Unknown websocket response IDs are rejected by connection
					// replay validation before the provider-state store is reached.
					request := newResponsesWebSocketCreateRequest([]any{map[string]any{"type": "reasoning", "encrypted_content": "opaque-missing"}})
					request["model"] = "public-ws-model"
					if err := conn.WriteJSON(request); err != nil {
						t.Fatal(err)
					}
					frame := mustReadWebSocketJSON(t, conn)
					if frame["status_code"] != float64(wantStatus) {
						t.Fatalf("frame = %+v", frame)
					}
					raw, _ = json.Marshal(frame)
				} else {
					path, body, handle := "/v1/responses", `{"model":"public-ws-model","input":"continue","previous_response_id":"opaque-known-a"}`, h.HandleResponses
					switch surface {
					case "compact":
						path, handle = "/v1/responses/compact", h.HandleCompact
					case "memory":
						path, body, handle = "/v1/memories/trace_summarize", `{"model":"public-ws-model","traces":[{"text":"synthetic trace"}]}`, h.HandleMemorySummarize
					}
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					req.Header.Set("X-Codex-Turn-State", "opaque-missing")
					w := httptest.NewRecorder()
					handle(w, req)
					if w.Code != wantStatus {
						t.Fatalf("response = %d %s", w.Code, w.Body.String())
					}
					raw = w.Body.Bytes()
				}
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(raw, &response); err != nil || response.Error.Code != wantCode {
					t.Fatalf("error = %s, want %s: %v", raw, wantCode, err)
				}
				assertResponsesStateDiagnosticPrivacy(t, string(raw))
				if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
					t.Fatal("diagnostic made an upstream send")
				}
			})
		}
	}
}

func TestDurableResponsesStateDiagnosticsPinnedWebSocket(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("conflict=%t/reverse=%t", conflicting, reverse), func(t *testing.T) {
				var primaryCalls, secondaryCalls atomic.Int32
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					primaryCalls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":{"id":"resp_diagnostic_pin","model":"physical-primary","status":"completed","output":[]}}`))
				}))
				defer primary.Close()
				secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					secondaryCalls.Add(1)
					http.Error(w, "unexpected send", http.StatusInternalServerError)
				}))
				defer secondary.Close()
				h := newExplicitRouteResponsesWebSocketHandler(t, primary.URL, secondary.URL)
				s, _ := newDurableStoreFixture(t, 8)
				h.stateBindings = s
				h.stateBindingsOnce.Do(func() {})
				server := startResponsesWebSocketProxyServer(t, h)
				conn := mustDialResponsesWebSocket(t, server, nil)
				defer func() { _ = conn.Close() }()
				first := newResponsesWebSocketCreateRequest([]any{})
				first["model"] = "public-ws-model"
				if err := conn.WriteJSON(first); err != nil {
					t.Fatal(err)
				}
				if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
					t.Fatalf("first frame = %+v", frame)
				}
				issued := s.lookup(stateBindingTypeResponseID, "resp_diagnostic_pin")
				if issued.err != nil || issued.outcome != stateBindingLookupKnown {
					t.Fatalf("issued = %+v", issued)
				}
				owner := stateBindingOwner{routeID: "public-ws-route", targetID: "primary", identity: issued.owner.identity}
				if conflicting {
					owner.targetID = "secondary"
				}
				if result := s.bind(stateBindingTypeEncryptedContent, "opaque-known", owner); result.err != nil || result.outcome != stateBindingLookupKnown {
					t.Fatalf("seed = %+v", result)
				}
				tokens := []string{"opaque-known", "opaque-missing"}
				if reverse {
					tokens[0], tokens[1] = tokens[1], tokens[0]
				}
				input := []any{map[string]any{"type": "reasoning", "encrypted_content": tokens[0]}, map[string]any{"type": "reasoning", "encrypted_content": tokens[1]}}
				second := newResponsesWebSocketCreateRequest(input)
				second["model"] = "public-ws-model"
				if err := conn.WriteJSON(second); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(mustReadWebSocketJSON(t, conn))
				code, detail := "provider_state_unavailable", errDurableStateUnknown.Error()
				if conflicting {
					code, detail = "", errDurableStateConflict.Error()
				}
				assertResponsesStateDiagnostic(t, raw, code, detail, true)
				if primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
					t.Fatalf("calls = %d/%d", primaryCalls.Load(), secondaryCalls.Load())
				}
			})
		}
	}
}
