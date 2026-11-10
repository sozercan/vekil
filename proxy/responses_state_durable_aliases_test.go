package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDurableResponsesJSONDepthLimit(t *testing.T) {
	for _, depth := range []int{1000, 9999, 10000, 250000} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			// The root Response object consumes one of JSON's 10,000 levels.
			body := []byte(durableNestedResponseFixture(depth, "0"))
			err := validateUnambiguousResponsesJSON(body)
			if (err == nil) != (depth < 10000) {
				t.Fatalf("depth %d validation = %v", depth, err)
			}
		})
	}
}

func TestDurableResponsesStateFieldAliases(t *testing.T) {
	fixtures := []struct {
		name, body string
		want       []stateBindingToken
	}{
		{
			name: "response-and-conversation",
			body: `{"Type":"response.completed","Response":{"ID":"response-alias","Conversation":{"Id":"conversation-alias"}}}`,
			want: []stateBindingToken{{stateBindingTypeResponseID, "response-alias"}, {stateBindingTypeConversationID, "conversation-alias"}},
		},
		{
			name: "root-output-items",
			body: `{"Output":[{"Type":"reasoning","Encrypted_Content":"reasoning-alias"},{"TYPE":"compaction","ENCRYPTED_CONTENT":"compaction-alias"},{"type":"context_compaction","encrypted_CONTENT":"context-alias"}]}`,
			want: []stateBindingToken{{stateBindingTypeEncryptedContent, "reasoning-alias"}, {stateBindingTypeEncryptedContent, "compaction-alias"}, {stateBindingTypeEncryptedContent, "context-alias"}},
		},
		{
			name: "item-envelope",
			body: `{"Type":"response.output_item.done","Item":{"Type":"reasoning","Encrypted_Content":"item-alias"}}`,
			want: []stateBindingToken{{stateBindingTypeEncryptedContent, "item-alias"}},
		},
		{
			name: "escaped-field-name",
			body: `{"\u0049d":"response-alias"}`,
			want: []stateBindingToken{{stateBindingTypeResponseID, "response-alias"}},
		},
		{
			name: "vendor-fields-and-text",
			body: `{"Type":"vendor.fixture","ID":"vendor-event","metadata":{"id":"first","ID":"last","Response":{"ID":"metadata-id"}},"item":{"type":"message","encrypted_content":"first","Encrypted_Content":"last","content":[{"type":"output_text","text":"{\"id\":1,\"ID\":2}"}]}}`,
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			body := []byte(fixture.body)
			got, err := extractDurableResponsesOutputState(body)
			if err != nil {
				t.Fatal(err)
			}
			requireStateBindingTokens(t, got, fixture.want)
			if string(body) != fixture.body {
				t.Fatal("state extraction changed the wire representation")
			}
			legacy, err := extractExplicitResponsesOutputState(body)
			if err != nil || len(legacy) != 0 {
				t.Fatalf("memory-only state extraction changed: %v, %v", legacy, err)
			}
		})
	}
}

func TestDurableResponsesIDAliasesExposureAndContinuation(t *testing.T) {
	const responseID = "response-case-fixture"
	const response = `{"id":"response-case-fixture","status":"completed","output":[]}`
	for _, fixture := range []struct {
		name, response, event string
	}{
		{"canonical", response, `{"type":"response.completed","response":` + response + `}`},
		{"uppercase-id", strings.Replace(response, `"id"`, `"ID"`, 1), `{"type":"response.completed","response":` + strings.Replace(response, `"id"`, `"ID"`, 1) + `}`},
		{"escaped-id", strings.Replace(response, `"id"`, `"\u0049d"`, 1), `{"type":"response.completed","response":` + strings.Replace(response, `"id"`, `"\u0049d"`, 1) + `}`},
		{"response-envelope", response, `{"type":"response.completed","Response":` + response + `}`},
		{"type-envelope", response, `{"Type":"response.completed","response":` + response + `}`},
	} {
		for _, surface := range []string{"json", "sse", "websocket"} {
			for _, full := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/full=%t", fixture.name, surface, full), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if sends.Add(1) > 1 {
							var request struct {
								PreviousResponseID string `json:"previous_response_id"`
							}
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.PreviousResponseID != responseID {
								t.Errorf("continuation did not preserve the response ID: %+v, %v", request, err)
							}
							_, _ = io.WriteString(w, response)
							return
						}
						if surface == "json" {
							_, _ = io.WriteString(w, fixture.response)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"visible progress"}`)+responsesRouteSSE("response.completed", fixture.event))
					}))
					defer upstream.Close()
					store, _ := newDurableStoreFixture(t, 1)
					if full {
						if got := store.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-proof"}}, durableFixtureOwner()); got.err != nil {
							t.Fatal(got.err)
						}
					}
					h, _ := durableWireHandler(t, store, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
					var exposed string
					if surface == "websocket" {
						server := startResponsesWebSocketProxyServer(t, h)
						conn := mustDialResponsesWebSocket(t, server, nil)
						defer func() { _ = conn.Close() }()
						if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": "fixture"}); err != nil {
							t.Fatal(err)
						}
						if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
							t.Fatal(err)
						}
						for range 5 {
							_, raw, err := conn.ReadMessage()
							if err != nil {
								t.Fatal(err)
							}
							exposed += string(raw)
							var event struct {
								Type   string `json:"type"`
								Status int    `json:"status_code"`
							}
							if err := json.Unmarshal(raw, &event); err != nil {
								t.Fatal(err)
							}
							if event.Type == "response.completed" || event.Type == "error" {
								if full && event.Status != http.StatusServiceUnavailable {
									t.Fatalf("capacity error status=%d: %s", event.Status, raw)
								}
								break
							}
						}
					} else {
						result := durableWireRequest(h, `{"model":"public-model","input":"fixture"}`, surface == "sse", false)
						wantStatus := http.StatusOK
						if full && surface == "json" {
							wantStatus = http.StatusServiceUnavailable
						}
						if result.Code != wantStatus {
							t.Fatalf("response status=%d: %s", result.Code, result.Body.String())
						}
						exposed = result.Body.String()
					}
					if strings.Contains(exposed, responseID) == full || sends.Load() != 1 || store.stats().entries != 1 {
						t.Fatalf("incorrect state exposure: sends=%d records=%d body=%s", sends.Load(), store.stats().entries, exposed)
					}
					if full && !strings.Contains(exposed, "state_binding_capacity_exceeded") {
						t.Fatalf("missing capacity error: %s", exposed)
					}
					proof := store.lookup(stateBindingTypeResponseID, responseID)
					if proof.err != nil || (proof.outcome == stateBindingLookupKnown) == full {
						t.Fatalf("incorrect ownership proof: %+v", proof)
					}
					continuation := durableWireRequest(h, `{"model":"public-model","previous_response_id":"response-case-fixture","input":"continue"}`, false, false)
					if full {
						if continuation.Code != http.StatusBadRequest || !strings.Contains(continuation.Body.String(), "provider_state_unavailable") || sends.Load() != 1 {
							t.Fatalf("withheld response ID allowed continuation: %d %s, sends=%d", continuation.Code, continuation.Body.String(), sends.Load())
						}
					} else if continuation.Code != http.StatusOK || sends.Load() != 2 {
						t.Fatalf("exposed response ID lost continuity: %d %s, sends=%d", continuation.Code, continuation.Body.String(), sends.Load())
					}
				})
			}
		}
	}
}
