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
	"syscall"
	"testing"
)

func invokeFinalHeaderSurface(t *testing.T, h *ProxyHandler, surface, turn string) (int, string) {
	t.Helper()
	headers := make(http.Header)
	if turn != "" {
		headers.Set("X-Codex-Turn-State", turn)
	}
	if !strings.HasPrefix(surface, "websocket") {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`))
		request.Header = headers
		w := httptest.NewRecorder()
		switch surface {
		case "chat":
			h.HandleOpenAIChatCompletions(w, request)
		case "messages":
			h.HandleAnthropicMessages(w, request)
		case "count-tokens":
			h.HandleAnthropicMessagesCountTokens(w, request)
		}
		return w.Code, fmt.Sprint(w.Header()) + w.Body.String()
	}
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true}
	server := startResponsesWebSocketProxyServer(t, h)
	conn := mustDialResponsesWebSocket(t, server, headers)
	defer func() { _ = conn.Close() }()
	input := []any{map[string]string{"role": "user", "content": "fixture"}}
	if surface == "websocket-trigger" {
		input = append(input, map[string]string{"type": "compaction_trigger"})
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": input}); err != nil {
		t.Fatal(err)
	}
	payload := mustReadWebSocketJSONSkipMetadata(t, conn)
	if payload["type"] != "error" {
		t.Fatalf("expected terminal error: %v", payload)
	}
	encoded, _ := json.Marshal(payload)
	return int(payload["status_code"].(float64)), string(encoded)
}

func continueFinalHeader(h *ProxyHandler, turn string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"continue"}`))
	r.Header.Set("X-Codex-Turn-State", turn)
	w := httptest.NewRecorder()
	h.HandleResponses(w, r)
	return w
}

func TestDurableFinalErrorHeaders(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		for _, surface := range []string{"chat", "messages", "count-tokens", "websocket", "websocket-trigger", "websocket-rejected-stream"} {
			for _, mode := range []string{"state", "capacity", "io", "conflict", "raw-body", "memory", "memory-raw"} {
				t.Run(fmt.Sprintf("%d/%s/%s", upstreamStatus, surface, mode), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						sends.Add(1)
						w.Header().Set("X-Codex-Turn-State", "turn-error-fixture")
						if mode == "conflict" {
							w.Header().Add("X-Codex-Turn-State", "conflicting-turn-fixture")
						}
						if surface == "websocket-rejected-stream" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, responsesRouteSSE("response.failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"fixture unavailable"}}}`))
							return
						}
						w.WriteHeader(upstreamStatus)
						body := durableErrorFixture
						if mode == "raw-body" || mode == "memory-raw" {
							body = durableWireFixture
						}
						_, _ = io.WriteString(w, body)
					}))
					defer upstream.Close()
					s, config := newDurableStoreFixture(t, 32)
					memory := strings.HasPrefix(mode, "memory")
					if memory {
						closeDurableStoreFixture(t, s)
						var err error
						s, err = newStateBindingStore(stateBindingStoreConfig{})
						if err != nil {
							t.Fatal(err)
						}
					}
					switch mode {
					case "capacity":
						s.durable.maxEntries = 1
						if result := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-fixture"}}, durableFixtureOwner()); result.err != nil {
							t.Fatal(result.err)
						}
					case "io":
						s.durable.beforeCommit = func() error { return syscall.EIO }
					}
					provider := explicitRouteTestProvider("issuer", upstream.URL, "fixture-key")
					newHandler := func(store *stateBindingStore) *ProxyHandler {
						if surface == "messages" || surface == "count-tokens" {
							h := newOperationAdmissionTestHandler(t, providerTypeAnthropicCompatible, []string{providerEndpointMessages}, upstream.URL)
							h.stateBindings = store
							h.stateBindingsOnce.Do(func() {})
							return h
						}
						h, _ := durableWireHandler(t, store, provider)
						return h
					}
					h := newHandler(s)
					want := upstreamStatus
					switch mode {
					case "capacity", "io":
						want = http.StatusServiceUnavailable
					case "conflict":
						want = http.StatusBadGateway
					}
					if surface == "websocket-rejected-stream" {
						// The executor reduces this precommit rejection to a typed
						// failure, discarding its headers before the websocket writer.
						want = http.StatusTooManyRequests
					}
					status, exposed := invokeFinalHeaderSurface(t, h, surface, "")
					if status != want || sends.Load() != 1 {
						t.Fatalf("status=%d want=%d sends=%d exposed=%s", status, want, sends.Load(), exposed)
					}
					committed := (mode == "state" || mode == "raw-body") && surface != "websocket-rejected-stream"
					if strings.Contains(exposed, "turn-error-fixture") != (committed || memory && surface != "websocket-rejected-stream") {
						t.Fatalf("uncommitted or missing turn state: %s", exposed)
					}
					if !memory && strings.HasPrefix(surface, "websocket") && strings.Contains(exposed, "reasoning-durable-fixture") {
						t.Fatal("raw error-body state escaped into websocket error message")
					}
					if memory {
						if s.lookup(stateBindingTypeTurnState, "turn-error-fixture").outcome != stateBindingLookupUnknown {
							t.Fatal("memory-only error binding changed")
						}
						if mode == "memory-raw" && surface != "websocket-rejected-stream" && !strings.Contains(exposed, "reasoning-durable-fixture") {
							t.Fatal("memory-only raw error passthrough changed")
						}
						return
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
					got := reopened.lookup(stateBindingTypeTurnState, "turn-error-fixture")
					if (got.outcome == stateBindingLookupKnown) != committed || got.err != nil {
						t.Fatalf("reopened header = %+v", got)
					}
					if committed && got.owner.identity == [32]byte{} {
						t.Fatal("missing actual issuer identity")
					}
					if committed && surface != "messages" && surface != "count-tokens" {
						h = newHandler(reopened)
						continued := continueFinalHeader(h, "turn-error-fixture")
						wantStatus, wantSends := upstreamStatus, int32(2)
						if surface == "chat" {
							// Native Chat headers belong to the Chat endpoint. They
							// cannot authorize cross-endpoint Responses continuation.
							wantStatus, wantSends = http.StatusBadRequest, 1
						}
						if continued.Code != wantStatus || sends.Load() != wantSends {
							t.Fatalf("reopened continuation status=%d sends=%d", continued.Code, sends.Load())
						}
						h.BeginShutdown()
						if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func TestDurableFinalErrorHeadersSelectExactIssuer(t *testing.T) {
	for _, surface := range []string{"chat", "websocket", "websocket-trigger"} {
		for _, secondStatus := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", surface, secondStatus), func(t *testing.T) {
				var firstSends, secondSends atomic.Int32
				first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					firstSends.Add(1)
					w.Header().Set("X-Codex-Turn-State", "first-turn-fixture")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, durableErrorFixture)
				}))
				defer first.Close()
				second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					secondSends.Add(1)
					w.Header().Set("X-Codex-Turn-State", "second-turn-fixture")
					w.WriteHeader(secondStatus)
					_, _ = io.WriteString(w, durableErrorFixture)
				}))
				defer second.Close()
				s, config := newDurableStoreFixture(t, 32)
				providers := []*providerRuntime{explicitRouteTestProvider("first", first.URL, "first-key"), explicitRouteTestProvider("second", second.URL, "second-key")}
				h, route := durableWireHandler(t, s, providers...)
				status, exposed := invokeFinalHeaderSurface(t, h, surface, "")
				wantStatus, wantSecondSends := secondStatus, int32(1)
				if surface == "websocket-trigger" {
					wantStatus, wantSecondSends = http.StatusTooManyRequests, 0
				}
				if status != wantStatus || firstSends.Load() != 1 || secondSends.Load() != wantSecondSends {
					t.Fatalf("final header: status=%d sends=%d/%d exposed=%s", status, firstSends.Load(), secondSends.Load(), exposed)
				}
				if wantStatus == http.StatusTooManyRequests {
					// These consumers reduce exhausted rejections to typed errors,
					// discarding both attempts' headers. Nothing should be bound.
					if strings.Contains(exposed, "turn-fixture") || s.stats().entries != 0 {
						t.Fatal("discarded rejection header was exposed or bound")
					}
					return
				}
				selected, discarded, target := "second-turn-fixture", "first-turn-fixture", route.targets[1].id
				if !strings.Contains(exposed, selected) || strings.Contains(exposed, discarded) {
					t.Fatalf("wrong final header: %s", exposed)
				}
				if got := s.lookup(stateBindingTypeTurnState, selected); got.err != nil || got.outcome != stateBindingLookupKnown || s.ownerTarget(got.owner, route) != target {
					t.Fatal("selected header was not bound to its actual request owner")
				}
				if s.lookup(stateBindingTypeTurnState, discarded).outcome != stateBindingLookupUnknown {
					t.Fatal("discarded error was bound")
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
				h, _ = durableWireHandler(t, reopened, providers...)
				continued := continueFinalHeader(h, selected)
				wantFirst, wantSecond := int32(1), wantSecondSends+1
				if surface == "chat" {
					wantStatus, wantSecond = http.StatusBadRequest, wantSecondSends
				}
				if continued.Code != wantStatus || firstSends.Load() != wantFirst || secondSends.Load() != wantSecond {
					t.Fatalf("reopened continuation: status=%d sends=%d/%d", continued.Code, firstSends.Load(), secondSends.Load())
				}
				h.BeginShutdown()
				if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
