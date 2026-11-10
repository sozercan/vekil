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
	"time"

	"github.com/sozercan/vekil/logger"
)

const durableWireFixture = `{"id":"resp-durable-fixture","model":"physical-fixture","status":"completed","conversation":{"id":"conv-durable-fixture"},"output":[{"type":"reasoning","encrypted_content":"reasoning-durable-fixture","summary":[]},{"type":"compaction","encrypted_content":"compaction-durable-fixture"},{"type":"context_compaction","encrypted_content":"context-durable-fixture"}]}`

func durableWireHandler(t *testing.T, store *stateBindingStore, providers ...*providerRuntime) (*ProxyHandler, *modelRoute) {
	t.Helper()
	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, len(providers), len(providers), providers...)
	h.log = logger.New(logger.LevelFatal)
	h.stateBindings = store
	h.stateBindingsOnce.Do(func() {})
	t.Cleanup(h.BeginShutdown)
	return h, route
}

func durableWireRequest(h *ProxyHandler, body string, stream, turn bool) *httptest.ResponseRecorder {
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		panic(err)
	}
	payload["stream"] = stream
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(encoded)))
	if turn {
		req.Header.Set("X-Codex-Turn-State", "turn-durable-fixture")
	}
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)
	return w
}

func TestDurableResponsesReopenKeepsExactIssuer(t *testing.T) {
	fields := []struct {
		name, body string
		turn       bool
	}{
		{"response_id", `{"model":"public-model","previous_response_id":"resp-durable-fixture","input":"continue"}`, false},
		{"reasoning", `{"model":"public-model","input":[{"type":"reasoning","encrypted_content":"reasoning-durable-fixture","summary":[]},{"role":"user","content":"continue"}]}`, false},
		{"compaction", `{"model":"public-model","input":[{"type":"compaction","encrypted_content":"compaction-durable-fixture"}]}`, false},
		{"context_compaction", `{"model":"public-model","input":[{"type":"context_compaction","encrypted_content":"context-durable-fixture"}]}`, false},
		{"turn", `{"model":"public-model","input":"continue"}`, true},
		{"conversation", `{"model":"public-model","conversation":"conv-durable-fixture","input":"continue"}`, false},
		{"combined", `{"model":"public-model","previous_response_id":"resp-durable-fixture","input":[{"type":"reasoning","encrypted_content":"reasoning-durable-fixture","summary":[]}]}`, true},
	}
	for _, stream := range []bool{false, true} {
		for _, field := range fields {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, field.name), func(t *testing.T) {
				var primaryCalls, secondaryCalls atomic.Int32
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
				}))
				defer primary.Close()
				secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					secondaryCalls.Add(1)
					w.Header().Set("X-Codex-Turn-State", "turn-durable-fixture")
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, durableWireFixture)
					}
				}))
				defer secondary.Close()
				s, config := newDurableStoreFixture(t, 32)
				providers := []*providerRuntime{explicitRouteTestProvider("primary", primary.URL, "fixture-primary"), explicitRouteTestProvider("secondary", secondary.URL, "fixture-secondary")}
				h, _ := durableWireHandler(t, s, providers...)
				first := durableWireRequest(h, `{"model":"public-model","input":"start"}`, stream, false)
				if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "reasoning-durable-fixture") || first.Header().Get("X-Codex-Turn-State") != "turn-durable-fixture" {
					t.Fatalf("first response = %d %s", first.Code, first.Body.String())
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
				reopened.durable.now = func() time.Time { return time.Now().Add(400 * 24 * time.Hour) }
				h, _ = durableWireHandler(t, reopened, providers...)
				beforePrimary, beforeSecondary := primaryCalls.Load(), secondaryCalls.Load()
				continued := durableWireRequest(h, field.body, stream, field.turn)
				if continued.Code != http.StatusOK || !strings.Contains(continued.Body.String(), "reasoning-durable-fixture") {
					t.Fatalf("continuation = %d %s", continued.Code, continued.Body.String())
				}
				if primaryCalls.Load() != beforePrimary || secondaryCalls.Load() != beforeSecondary+1 {
					t.Fatal("reopened continuation did not go exactly once to its original issuer")
				}
			})
		}
	}
}

func TestDurableResponsesChangedOwnerRejectedBeforeSend(t *testing.T) {
	for _, change := range []string{"key", "endpoint", "deployment", "provider", "tenant"} {
		t.Run(change, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, durableWireFixture)
			}))
			defer upstream.Close()
			s, config := newDurableStoreFixture(t, 32)
			provider := explicitRouteTestProvider("primary", upstream.URL, "fixture-key")
			h, _ := durableWireHandler(t, s, provider)
			if first := durableWireRequest(h, `{"model":"public-model","input":"start"}`, false, false); first.Code != http.StatusOK {
				t.Fatalf("first = %d %s", first.Code, first.Body.String())
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
			provider = explicitRouteTestProvider("primary", upstream.URL, "fixture-key")
			h, route := durableWireHandler(t, reopened, provider)
			switch change {
			case "key":
				provider.apiKey = "fixture-rotated"
			case "endpoint":
				provider.baseURL += "/different"
			case "deployment":
				route.targets[0].upstreamModel = "new-deployment"
			case "provider":
				provider.id = "replacement-provider"
			case "tenant":
				provider.extraHeaders = http.Header{"Openai-Organization": []string{"new-tenant"}}
			}
			before := calls.Load()
			result := durableWireRequest(h, `{"model":"public-model","previous_response_id":"resp-durable-fixture","input":"continue"}`, false, false)
			if result.Code != http.StatusBadRequest || !strings.Contains(result.Body.String(), "ownership identity changed") {
				t.Fatalf("changed owner = %d %s", result.Code, result.Body.String())
			}
			if calls.Load() != before {
				t.Fatal("changed owner reached inference")
			}
		})
	}
}

func TestDurableResponsesFaultPreventsStateExposure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/after=%t", stream, after), func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("X-Codex-Turn-State", "turn-durable-fixture")
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, durableWireFixture)
					}
				}))
				defer upstream.Close()
				s, _ := newDurableStoreFixture(t, 32)
				fault := func() error { return syscall.ENOSPC }
				if after {
					s.durable.afterCommit = fault
				} else {
					s.durable.beforeCommit = fault
				}
				h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
				result := durableWireRequest(h, `{"model":"public-model","input":"start"}`, stream, false)
				if strings.Contains(result.Body.String(), "durable-fixture") || result.Header().Get("X-Codex-Turn-State") != "" {
					t.Fatal("uncommitted state escaped to client")
				}
				if calls.Load() != 1 {
					t.Fatal("storage failure retried inference")
				}
			})
		}
	}
}

func TestDurableResponsesStorageErrors(t *testing.T) {
	for _, stage := range []string{"header", "json", "sse"} {
		for _, fault := range []string{"io", "capacity"} {
			t.Run(stage+"/"+fault, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if stage == "header" {
						w.Header().Set("X-Codex-Turn-State", "turn-durable-fixture")
					}
					if stage == "sse" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"visible text"}`))
						_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, durableWireFixture)
					}
				}))
				defer upstream.Close()
				s, _ := newDurableStoreFixture(t, 1)
				wantCode := "state_binding_storage_unavailable"
				if fault == "capacity" {
					if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "occupied"}}, durableFixtureOwner()); r.err != nil {
						t.Fatal(r.err)
					}
					wantCode = "state_binding_capacity_exceeded"
				} else {
					s.durable.maxEntries = 32
					s.durable.beforeCommit = func() error { return syscall.ENOSPC }
				}
				h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
				result := durableWireRequest(h, `{"model":"public-model","input":"start"}`, stage == "sse", false)
				wantStatus := http.StatusServiceUnavailable
				if stage == "sse" {
					wantStatus = http.StatusOK // already committed; terminal SSE error
					if !strings.Contains(result.Body.String(), "event: error\n") || !strings.Contains(result.Body.String(), "visible text") {
						t.Fatalf("missing stream prefix/error: %s", result.Body.String())
					}
				}
				if result.Code != wantStatus || !strings.Contains(result.Body.String(), wantCode) {
					t.Fatalf("storage failure = %d %s", result.Code, result.Body.String())
				}
				if strings.Contains(result.Body.String(), "durable-fixture") || result.Header().Get("X-Codex-Turn-State") != "" || strings.Contains(result.Body.String(), "response.completed") {
					t.Fatal("unrecorded state or false completion exposed")
				}
				if calls.Load() != 1 {
					t.Fatal("local storage error retried inference")
				}
			})
		}
	}
}
