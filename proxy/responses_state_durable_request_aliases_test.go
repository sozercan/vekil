package proxy

import (
	"context"
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
)

func durableRequestStateAliasFixtures() []struct{ name, fields string } {
	return []struct{ name, fields string }{
		{"previous-id", `"Previous_Response_ID":"unknown-response"`},
		{"escaped-previous-id", `"\u0050revious_Response_ID":"unknown-response"`},
		{"unicode-previous-id", `"previous_re\u017fpon\u017fe_id":"unknown-response"`},
		{"previous-id-alias-last", `"previous_response_id":"resp-durable-fixture","Previous_Response_ID":"unknown-response"`},
		{"previous-id-alias-first", `"Previous_Response_ID":"unknown-response","previous_response_id":"resp-durable-fixture"`},
		{"previous-id-two-aliases", `"Previous_Response_ID":"unknown-response","PREVIOUS_RESPONSE_ID":"resp-durable-fixture"`},
		{"previous-id-duplicate", `"previous_response_id":"resp-durable-fixture","previous_response_id":"unknown-response"`},
		{"previous-id-escaped-duplicate", `"previous_response_id":"resp-durable-fixture","\u0070revious_response_id":"unknown-response"`},
		{"conversation", `"Conversation":"unknown-conversation"`},
		{"conversation-alias-last", `"conversation":"conv-durable-fixture","Conversation":"unknown-conversation"`},
		{"conversation-id", `"conversation":{"ID":"unknown-conversation"}`},
		{"conversation-id-alias-last", `"conversation":{"id":"conv-durable-fixture","ID":"unknown-conversation"}`},
		{"input", `"Input":[{"type":"reasoning","encrypted_content":"unknown-reasoning"}]`},
		{"input-alias-last", `"input":[],"Input":[{"type":"reasoning","encrypted_content":"unknown-reasoning"}]`},
		{"item-type", `"input":[{"Type":"reasoning","encrypted_content":"unknown-reasoning"}]`},
		{"item-type-alias-last", `"input":[{"type":"message","Type":"reasoning","encrypted_content":"unknown-reasoning"}]`},
		{"item-type-alias-first", `"input":[{"Type":"reasoning","type":"message","encrypted_content":"unknown-reasoning"}]`},
		{"reasoning-content", `"input":[{"type":"reasoning","Encrypted_Content":"unknown-reasoning"}]`},
		{"compaction-content", `"input":[{"type":"compaction","Encrypted_Content":"unknown-compaction"}]`},
		{"context-compaction-content", `"input":[{"type":"context_compaction","Encrypted_Content":"unknown-compaction"}]`},
		{"content-alias-last", `"input":[{"type":"reasoning","encrypted_content":"reasoning-durable-fixture","Encrypted_Content":"unknown-reasoning"}]`},
		{"content-alias-first", `"input":[{"type":"reasoning","Encrypted_Content":"unknown-reasoning","encrypted_content":"reasoning-durable-fixture"}]`},
		{"content-duplicate", `"input":[{"type":"reasoning","encrypted_content":"reasoning-durable-fixture","encrypted_content":"unknown-reasoning"}]`},
	}
}

func TestDurableResponsesRequestStateAliasesBeforeDispatch(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%t", compact), func(t *testing.T) {
			var sends atomic.Int32
			response := strings.Replace(durableWireFixture, `"output":[`, `"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixture summary"}]},`, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, response)
			}))
			defer upstream.Close()
			provider := explicitRouteTestProvider("primary", upstream.URL, "fixture-key")
			store, config := newDurableStoreFixture(t, 32)
			h, _ := durableWireHandler(t, store, provider)
			first := durableWireRequest(h, `{"model":"public-model","input":"start"}`, false, false)
			if first.Code != http.StatusOK {
				t.Fatalf("issue original state: %d %s", first.Code, first.Body.String())
			}
			h.BeginShutdown()
			if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := newDurableStateBindingStore(config)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDurableStoreFixture(t, reopened)
			h, _ = durableWireHandler(t, reopened, provider)
			request := func(body string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				if compact {
					h.HandleCompact(w, r)
				} else {
					h.HandleResponses(w, r)
				}
				return w
			}
			for _, fixture := range durableRequestStateAliasFixtures() {
				t.Run(fixture.name, func(t *testing.T) {
					before, entries := sends.Load(), reopened.stats().entries
					// Keep the original bytes: map decoding would erase duplicate keys.
					w := request(`{"model":"public-model",` + fixture.fields + `}`)
					if w.Code != http.StatusBadRequest || sends.Load() != before {
						t.Errorf("ambiguous request reached provider: status=%d sends=%d body=%s", w.Code, sends.Load()-before, w.Body.String())
					}
					if reopened.stats().entries != entries {
						t.Error("rejected request changed durable ownership")
					}
				})
			}
			// Schema-owned state fields are strict; vendor metadata and message
			// text keep their original interpretation, including nested aliases.
			before := sends.Load()
			w := request(`{"model":"public-model","previous_response_id":"resp-durable-fixture","metadata":{"previous_response_id":"first","Previous_Response_ID":"last"},"input":[{"type":"message","role":"user","content":"continue","encrypted_content":"first","Encrypted_Content":"last"}]}`)
			if w.Code != http.StatusOK || sends.Load() != before+1 {
				t.Fatalf("canonical continuation after reopen failed: %d %s, sends=%d", w.Code, w.Body.String(), sends.Load()-before)
			}
		})
	}
}

func TestDurableResponsesRequestStateAliasesWebSocket(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, native := range []bool{false, true} {
		for _, fixture := range durableRequestStateAliasFixtures() {
			t.Run(fmt.Sprintf("native=%t/%s", native, fixture.name), func(t *testing.T) {
				var sends atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sends.Add(1)
					if native {
						conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
						if _, _, err := conn.ReadMessage(); err == nil {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":`+durableWireFixture+`}`))
						}
						_, _, _ = conn.ReadMessage()
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
				}))
				defer upstream.Close()
				store, _ := newDurableStoreFixture(t, 32)
				provider := explicitRouteTestProvider("primary", upstream.URL, "fixture-key")
				if native {
					provider.kind = providerTypeCopilot
				}
				h, _ := durableWireHandler(t, store, provider)
				h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_synthetic-source", "synthetic-responses-token")
				h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: native}
				server := startResponsesWebSocketProxyServer(t, h)
				conn := mustDialResponsesWebSocket(t, server, nil)
				defer func() { _ = conn.Close() }()
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public-model",`+fixture.fields+`}`)); err != nil {
					t.Fatal(err)
				}
				for {
					event := mustReadWebSocketJSON(t, conn)
					if event["type"] == "codex.response.metadata" {
						continue
					}
					if event["type"] != "error" || event["status_code"] != float64(http.StatusBadRequest) || sends.Load() != 0 {
						t.Errorf("ambiguous websocket request reached provider: event=%v sends=%d", event, sends.Load())
					}
					break
				}
				if store.stats().entries != 0 {
					t.Error("rejected websocket request changed durable ownership")
				}
			})
		}
	}
}

func TestDurableResponsesRequestStateAliasesMemoryCompatibility(t *testing.T) {
	for _, fields := range []string{
		`"Previous_Response_ID":"unknown-response"`,
		`"Input":[{"type":"reasoning","encrypted_content":"unknown-reasoning"}]`,
		`"input":[{"type":"reasoning","Encrypted_Content":"unknown-reasoning"}]`,
	} {
		t.Run(fields, func(t *testing.T) {
			var sends atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				_, _ = io.WriteString(w, durableWireFixture)
			}))
			defer upstream.Close()
			store, err := newStateBindingStore(stateBindingStoreConfig{})
			if err != nil {
				t.Fatal(err)
			}
			h, _ := durableWireHandler(t, store, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
			w := httptest.NewRecorder()
			h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model",`+fields+`}`)))
			if w.Code != http.StatusOK || sends.Load() != 1 {
				t.Fatalf("memory-only request handling changed: %d %s, sends=%d", w.Code, w.Body.String(), sends.Load())
			}
		})
	}
}
