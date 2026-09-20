package proxy

import (
	"context"
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

func ambiguousDurableOutputFixtures() []struct{ name, body string } {
	return []struct{ name, body string }{
		{"root-id", `{"id":"ambiguous-first-fixture","id":"ambiguous-last-fixture","output":[]}`},
		{"escaped-id", `{"id":"ambiguous-first-fixture","\u0069d":"ambiguous-last-fixture","output":[]}`},
		{"case-id", `{"id":"ambiguous-first-fixture","ID":"ambiguous-last-fixture","output":[]}`},
		{"case-response", `{"type":"response.completed","response":{"id":"ambiguous-first-fixture"},"Response":{"id":"ambiguous-last-fixture"}}`},
		{"case-type", `{"type":"vendor.fixture","Type":"response.completed","response":{"id":"ambiguous-last-fixture"}}`},
		{"case-output", `{"output":[{"type":"reasoning","encrypted_content":"ambiguous-first-fixture"}],"Output":[{"type":"reasoning","encrypted_content":"ambiguous-last-fixture"}]}`},
		{"case-conversation", `{"conversation":{"id":"ambiguous-first-fixture"},"Conversation":{"id":"ambiguous-last-fixture"}}`},
		{"case-conversation-id", `{"conversation":{"id":"ambiguous-first-fixture","ID":"ambiguous-last-fixture"},"output":[]}`},
		{"case-item", `{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"ambiguous-first-fixture"},"Item":{"type":"reasoning","encrypted_content":"ambiguous-last-fixture"}}`},
		{"case-item-type", `{"type":"response.output_item.done","item":{"type":"message","Type":"reasoning","encrypted_content":"ambiguous-last-fixture"}}`},
		{"case-encrypted-content", `{"output":[{"type":"reasoning","encrypted_content":"ambiguous-first-fixture","Encrypted_Content":"ambiguous-last-fixture"}]}`},
		{"encrypted-content", `{"output":[{"type":"reasoning","encrypted_content":"ambiguous-first-fixture","encrypted_content":"ambiguous-last-fixture"}]}`},
		{"output-item", `{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"ambiguous-first-fixture","encrypted_content":"ambiguous-last-fixture"}}`},
		{"conversation", `{"conversation":{"id":"ambiguous-first-fixture","id":"ambiguous-last-fixture"},"output":[]}`},
		{"response-envelope", `{"type":"vendor.fixture","response":{"id":"ambiguous-first-fixture","id":"ambiguous-last-fixture"}}`},
		{"error-header", `{"type":"error","error":{"headers":{"X-Codex-Turn-State":"ambiguous-first-fixture","X-Codex-Turn-State":"ambiguous-last-fixture"}}}`},
		{"error-header-block", `{"type":"error","headers":{"X-Codex-Turn-State":"ambiguous-first-fixture"},"headers":{"X-Codex-Turn-State":"ambiguous-last-fixture"}}`},
	}
}

func assertNoAmbiguousDurableProof(t *testing.T, s *stateBindingStore) {
	t.Helper()
	for _, kind := range []stateBindingType{stateBindingTypeResponseID, stateBindingTypeConversationID, stateBindingTypeEncryptedContent, stateBindingTypeTurnState} {
		for _, value := range []string{"ambiguous-first-fixture", "ambiguous-last-fixture"} {
			if got := s.lookup(kind, value); got.err != nil || got.outcome != stateBindingLookupUnknown {
				t.Fatalf("ambiguous state acquired proof: %s/%s: %+v", kind, value, got)
			}
		}
	}
}

func TestDurableResponsesDistinctObjectsAreUnambiguous(t *testing.T) {
	const body = `{"id":"valid-response-fixture","output":[{"type":"reasoning","encrypted_content":"valid-first-fixture"},{"type":"reasoning","encrypted_content":"valid-last-fixture"},{"type":"message","content":[{"type":"output_text","text":"{\"id\":1,\"id\":2}"}]}]}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+body+`}`))
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			s, _ := newDurableStoreFixture(t, 8)
			h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
			w := durableWireRequest(h, `{"model":"public-model","input":"fixture"}`, stream, false)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "valid-first-fixture") || !strings.Contains(w.Body.String(), "valid-last-fixture") {
				t.Fatalf("unambiguous response rejected: %d %s", w.Code, w.Body.String())
			}
			for _, value := range []string{"valid-first-fixture", "valid-last-fixture"} {
				if got := s.lookup(stateBindingTypeEncryptedContent, value); got.err != nil || got.outcome != stateBindingLookupKnown {
					t.Fatalf("valid state was not bound: %+v", got)
				}
			}
			if got := s.lookup(stateBindingTypeResponseID, "valid-response-fixture"); got.err != nil || got.outcome != stateBindingLookupKnown {
				t.Fatalf("valid response ID was not bound: %+v", got)
			}
		})
	}
}

func durableNestedResponseFixture(depth int, leaf string) string {
	return `{"id":"nested-response-fixture","status":"completed","output":[],"extension":` + strings.Repeat("[", depth) + leaf + strings.Repeat("]", depth) + `}`
}

func TestDurableResponsesNestedJSON(t *testing.T) {
	for _, depth := range []int{1000, 2000, 4000} {
		for _, stream := range []bool{false, true} {
			for _, kind := range []string{"valid", "duplicate", "trailing"} {
				t.Run(fmt.Sprintf("depth=%d/stream=%t/%s", depth, stream, kind), func(t *testing.T) {
					leaf := `0`
					if kind == "duplicate" {
						leaf = `{"id":"first","\u0069d":"last"}`
					}
					body := durableNestedResponseFixture(depth, leaf)
					if stream {
						body = `{"type":"response.completed","response":` + body + `}`
					}
					if kind == "trailing" {
						body += ` {"id":"trailing-fixture"}`
					}
					s, _ := newDurableStoreFixture(t, 4)
					h := &ProxyHandler{stateBindings: s}
					h.stateBindingsOnce.Do(func() {})
					info := explicitRouteResponseInfo{routeID: "route", targetID: "target", publicID: "public", stateIdentity: [32]byte{1}}
					var err error
					var exposed []byte
					if stream {
						reader := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(responsesRouteSSE("response.completed", body))), info)
						defer func() { _ = reader.Close() }()
						exposed, err = io.ReadAll(reader)
					} else {
						resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
						w := httptest.NewRecorder()
						err = writeExplicitResponsesResponse(context.Background(), h, w, resp, info, nil, "")
						exposed = w.Body.Bytes()
					}
					if kind == "valid" {
						if err != nil || !strings.Contains(string(exposed), "nested-response-fixture") || s.lookup(stateBindingTypeResponseID, "nested-response-fixture").outcome != stateBindingLookupKnown {
							t.Fatalf("valid nested response rejected or unbound: %v", err)
						}
					} else if err == nil || len(exposed) != 0 || s.stats().entries != 0 {
						t.Fatalf("invalid nested document exposed/bound: err=%v bytes=%d records=%d", err, len(exposed), s.stats().entries)
					}
				})
			}
		}
	}
}

func TestDurableResponsesRejectAmbiguousJSON(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, surface := range []string{"responses", "compact", "memory"} {
			for _, status := range []int{http.StatusCreated, http.StatusBadRequest} {
				for _, fixture := range ambiguousDurableOutputFixtures() {
					t.Run(fmt.Sprintf("durable=%t/%s/%d/%s", durable, surface, status, fixture.name), func(t *testing.T) {
						var sends atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							sends.Add(1)
							w.Header().Set("X-Codex-Turn-State", "batch-header-fixture")
							w.WriteHeader(status)
							_, _ = io.WriteString(w, fixture.body)
						}))
						defer upstream.Close()
						s, config := newDurableStoreFixture(t, 16)
						if !durable {
							closeDurableStoreFixture(t, s)
							var err error
							s, err = newStateBindingStore(stateBindingStoreConfig{})
							if err != nil {
								t.Fatal(err)
							}
						}
						h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
						w := httptest.NewRecorder()
						invokeDurableErrorSurface(h, surface, w)
						want := status
						if durable {
							want = http.StatusBadGateway
						}
						if w.Code != want || sends.Load() != 1 {
							t.Fatalf("status=%d want=%d sends=%d body=%s", w.Code, want, sends.Load(), w.Body.String())
						}
						if !durable {
							if !strings.Contains(w.Body.String(), "ambiguous-last-fixture") {
								t.Fatal("memory-only passthrough changed")
							}
							return
						}
						if strings.Contains(w.Body.String(), "ambiguous-") || w.Header().Get("X-Codex-Turn-State") != "" {
							t.Fatal("ambiguous response or atomic batch header escaped")
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
						assertNoAmbiguousDurableProof(t, reopened)
						if reopened.stats().entries != 0 {
							t.Fatal("rejected JSON partially committed its headers")
						}
					})
				}
			}
		}
	}
}

func TestDurableResponsesRejectAmbiguousStream(t *testing.T) {
	fixtures := ambiguousDurableOutputFixtures()
	fixtures = append(fixtures, struct{ name, body string }{"lifecycle", `{"type":"response.completed","response":{"id":"ambiguous-first-fixture","id":"ambiguous-last-fixture","status":"completed","output":[]}}`})
	for _, durable := range []bool{false, true} {
		for _, websocket := range []bool{false, true} {
			for _, fixture := range fixtures {
				t.Run(fmt.Sprintf("durable=%t/websocket=%t/%s", durable, websocket, fixture.name), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						sends.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("X-Codex-Turn-State", "accepted-header-fixture")
						_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"accepted progress"}`)+
							responsesRouteSSE("vendor.fixture", fixture.body)+
							responsesRouteSSE("response.completed", `{"type":"response.completed","response":{"id":"after-ambiguity-fixture","status":"completed","output":[]}}`))
					}))
					defer upstream.Close()
					s, config := newDurableStoreFixture(t, 16)
					if !durable {
						closeDurableStoreFixture(t, s)
						var err error
						s, err = newStateBindingStore(stateBindingStoreConfig{})
						if err != nil {
							t.Fatal(err)
						}
					}
					h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
					var exposed string
					if websocket {
						h.responsesWS = ResponsesWebSocketConfig{Enabled: true}
						server := startResponsesWebSocketProxyServer(t, h)
						conn := mustDialResponsesWebSocket(t, server, nil)
						defer func() { _ = conn.Close() }()
						if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": "fixture"}); err != nil {
							t.Fatal(err)
						}
						if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
							t.Fatal(err)
						}
						for range 8 {
							_, raw, err := conn.ReadMessage()
							if err != nil {
								t.Fatal(err)
							}
							// Assert the actual frame bytes: decoding into a test map
							// would hide the same duplicate-key bug as production.
							exposed += string(raw)
							var envelope struct {
								Type string `json:"type"`
							}
							if err := json.Unmarshal(raw, &envelope); err != nil {
								t.Fatal(err)
							}
							if envelope.Type == "error" || envelope.Type == "response.failed" || envelope.Type == "response.completed" {
								break
							}
						}
						_ = conn.Close()
					} else {
						w := durableWireRequest(h, `{"model":"public-model","input":"fixture"}`, true, false)
						if w.Code != http.StatusOK {
							t.Fatalf("progress was not committed: %d %s", w.Code, w.Body.String())
						}
						exposed = w.Body.String()
					}
					if !strings.Contains(exposed, "accepted progress") || sends.Load() != 1 {
						t.Fatalf("wrong committed stream/send count: %d %s", sends.Load(), exposed)
					}
					if !durable {
						if !strings.Contains(exposed, "ambiguous-last-fixture") {
							t.Fatalf("memory-only event transparency changed: %s", exposed)
						}
						return
					}
					if strings.Contains(exposed, "ambiguous-") || strings.Contains(exposed, "after-ambiguity-fixture") {
						t.Fatalf("ambiguous stream event or later completion escaped: %s", exposed)
					}
					if websocket && !strings.Contains(exposed, `"type":"error"`) {
						t.Fatalf("websocket lacked its terminal error: %s", exposed)
					}
					if !websocket && exposed != responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"accepted progress"}`) {
						t.Fatalf("HTTP stream must end after the committed progress, without a false completion: %s", exposed)
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
					assertNoAmbiguousDurableProof(t, reopened)
					if got := reopened.lookup(stateBindingTypeTurnState, "accepted-header-fixture"); got.err != nil || got.outcome != stateBindingLookupKnown {
						t.Fatal("previously committed header proof was lost")
					}
					if got := reopened.lookup(stateBindingTypeResponseID, "after-ambiguity-fixture"); got.err != nil || got.outcome != stateBindingLookupUnknown {
						t.Fatal("stream continued binding after rejection")
					}
				})
			}
		}
	}
}
