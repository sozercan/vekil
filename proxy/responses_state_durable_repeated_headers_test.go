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
)

func TestDurableRepeatedFinalHeaders(t *testing.T) {
	const token = "repeated-turn-fixture"
	for _, values := range [][]string{{token}, {token, token}, {"", token}, {token, ""}, {token + ", " + token}} {
		for _, surface := range []string{"responses", "chat", "websocket", "websocket-trigger", "websocket-progress", "websocket-override"} {
			for _, memory := range []bool{false, true} {
				t.Run(fmt.Sprintf("%q/%s/memory=%t", values, surface, memory), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						sends.Add(1)
						for _, value := range values {
							w.Header().Add("X-Codex-Turn-State", value)
						}
						if surface == "websocket-progress" || surface == "websocket-override" {
							w.Header().Set("Content-Type", "text/event-stream")
							failure := `{"type":"error","error":{"code":"rate_limit_exceeded","message":"fixture unavailable"}}`
							if surface == "websocket-override" {
								failure = durableFailureEvent("error", `"override-turn-fixture"`)
							}
							_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"repeated header progress"}`)+responsesRouteSSE("error", failure))
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":{"message":"fixture unavailable"}}`)
					}))
					defer upstream.Close()
					s, config := newDurableStoreFixture(t, 16)
					if memory {
						closeDurableStoreFixture(t, s)
						var err error
						s, err = newStateBindingStore(stateBindingStoreConfig{})
						if err != nil {
							t.Fatal(err)
						}
					}
					h, route := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
					isWS := strings.HasPrefix(surface, "websocket")
					reject := !memory && isWS && len(values) > 1 && surface != "websocket-override"
					var exposed string
					var status int
					var projected []string
					if isWS {
						server := startResponsesWebSocketProxyServer(t, h)
						conn := mustDialResponsesWebSocket(t, server, nil)
						defer func() { _ = conn.Close() }()
						input := []any{map[string]string{"role": "user", "content": "fixture"}}
						if surface == "websocket-trigger" {
							input = append(input, map[string]string{"type": "compaction_trigger"})
						}
						if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": input}); err != nil {
							t.Fatal(err)
						}
						for range 8 {
							payload := mustReadWebSocketJSON(t, conn)
							encoded, _ := json.Marshal(payload)
							exposed += string(encoded)
							if code, ok := payload["status_code"].(float64); ok {
								status = int(code)
								if headers, ok := payload["headers"].(map[string]any); ok {
									for name, value := range headers {
										if strings.EqualFold(name, "X-Codex-Turn-State") {
											projected = append(projected, value.(string))
										}
									}
								}
								break
							}
						}
						_ = conn.Close()
					} else {
						mux := http.NewServeMux()
						mux.HandleFunc("POST /v1/responses", h.HandleResponses)
						mux.HandleFunc("POST /v1/chat/completions", h.HandleOpenAIChatCompletions)
						server := httptest.NewServer(mux)
						defer server.Close()
						path, body := "/v1/responses", `{"model":"public-model","input":"fixture"}`
						if surface == "chat" {
							path, body = "/v1/chat/completions", `{"model":"public-model","messages":[{"role":"user","content":"fixture"}]}`
						}
						resp, err := server.Client().Post(server.URL+path, "application/json", strings.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						data, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						status, exposed, projected = resp.StatusCode, string(data), resp.Header.Values("X-Codex-Turn-State")
					}
					wantStatus := http.StatusBadRequest
					if surface == "websocket-progress" || surface == "websocket-override" {
						wantStatus = http.StatusTooManyRequests
						if !strings.Contains(exposed, "repeated header progress") {
							t.Error("lost already committed stream progress")
						}
					}
					if reject {
						wantStatus = http.StatusBadGateway
					}
					if status != wantStatus || sends.Load() != 1 {
						t.Errorf("status=%d want=%d sends=%d exposed=%s", status, wantStatus, sends.Load(), exposed)
					}
					if reject {
						if len(projected) != 0 || strings.Contains(exposed, token) {
							t.Errorf("ambiguous WebSocket state escaped: %s", exposed)
						}
					} else {
						want := values
						if isWS {
							want = []string{strings.Join(values, ", ")}
						}
						if surface == "websocket-override" {
							want = []string{"override-turn-fixture"}
						}
						if fmt.Sprint(projected) != fmt.Sprint(want) {
							t.Errorf("header projection=%q want=%q", projected, want)
						}
					}
					h.BeginShutdown()
					if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
						t.Fatal(err)
					}
					if memory {
						return
					}
					reopened, err := newDurableStateBindingStore(config)
					if err != nil {
						t.Fatal(err)
					}
					defer closeDurableStoreFixture(t, reopened)
					// Accepted streams bind their HTTP headers before semantic data.
					// Withholding a later error must not remove that earlier proof.
					wantKnown := !reject || surface == "websocket-progress"
					original := token
					if len(values) == 1 {
						original = values[0]
					}
					got := reopened.lookup(stateBindingTypeTurnState, original)
					if got.err != nil || (got.outcome == stateBindingLookupKnown) != wantKnown || (wantKnown && reopened.ownerTarget(got.owner, route) != route.targets[0].id) {
						t.Errorf("reopened original proof=%+v want known=%t", got, wantKnown)
					}
					if reject && reopened.lookup(stateBindingTypeTurnState, strings.Join(values, ", ")).outcome != stateBindingLookupUnknown {
						t.Error("invented joined token was recorded")
					}
					if surface == "websocket-override" {
						got := reopened.lookup(stateBindingTypeTurnState, "override-turn-fixture")
						if got.err != nil || got.outcome != stateBindingLookupKnown || reopened.ownerTarget(got.owner, route) != route.targets[0].id {
							t.Errorf("reopened overriding event proof=%+v", got)
						}
					}
				})
			}
		}
	}
}
