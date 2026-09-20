package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

// Characterize the existing recovery boundary with state actually exposed by
// HandleResponses. Every value is synthetic; no client transcript is required.
// A healthy provider remains available throughout the simulated state loss.
func TestExplicitResponsesStateLossRejectsContinuationBeforeDispatch(t *testing.T) {
	fields := []struct {
		name      string
		body      string
		turnState bool
	}{
		{"previous_response_id", `{"model":"public-model","previous_response_id":"resp-recovery-fixture","input":"continue"}`, false},
		{"reasoning", `{"model":"public-model","store":false,"input":[{"type":"reasoning","encrypted_content":"reasoning-recovery-fixture","summary":[]},{"role":"user","content":"continue"}]}`, false},
		{"compaction", `{"model":"public-model","input":[{"type":"compaction","encrypted_content":"compaction-recovery-fixture"},{"role":"user","content":"continue"}]}`, false},
		{"context_compaction", `{"model":"public-model","input":[{"type":"context_compaction","encrypted_content":"context-recovery-fixture"},{"role":"user","content":"continue"}]}`, false},
		{"turn_state", `{"model":"public-model","input":"continue"}`, true},
		{"combined", `{"model":"public-model","previous_response_id":"resp-recovery-fixture","input":[{"type":"reasoning","encrypted_content":"reasoning-recovery-fixture","summary":[]},{"role":"user","content":"continue"}]}`, true},
	}
	topologies := []struct {
		name    string
		mode    routeMode
		targets int
	}{
		{"single_target", routeModePrimaryOnly, 1},
		{"primary_only_pool", routeModePrimaryOnly, 2},
		{"failover_pool", routeModePriorityFailover, 2},
	}
	for _, topology := range topologies {
		for _, stream := range []bool{false, true} {
			for _, loss := range []string{"restart", "second_process", "expiry", "eviction"} {
				for _, field := range fields {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/%s", topology.name, stream, loss, field.name), func(t *testing.T) {
						var primaryCalls, secondaryCalls atomic.Int32
						const response = `{"id":"resp-recovery-fixture","model":"physical-fixture","status":"completed","output":[{"type":"reasoning","encrypted_content":"reasoning-recovery-fixture","summary":[]},{"type":"compaction","encrypted_content":"compaction-recovery-fixture"},{"type":"context_compaction","encrypted_content":"context-recovery-fixture"}]}`
						primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							primaryCalls.Add(1)
							_, _ = io.Copy(io.Discard, r.Body)
							if topology.mode == routeModePriorityFailover {
								w.WriteHeader(http.StatusTooManyRequests)
								_, _ = io.WriteString(w, `{"error":{"message":"fixture capacity","code":"rate_limit_exceeded"}}`)
								return
							}
							writeRecoveryFixtureResponse(w, stream, response)
						}))
						defer primary.Close()
						secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							secondaryCalls.Add(1)
							_, _ = io.Copy(io.Discard, r.Body)
							writeRecoveryFixtureResponse(w, stream, response)
						}))
						defer secondary.Close()

						clock := &stateBindingTestClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
						newHandler := func() (*ProxyHandler, *modelRoute) {
							providers := []*providerRuntime{explicitRouteTestProvider("primary", primary.URL, "fixture-primary-key")}
							if topology.targets == 2 {
								providers = append(providers, explicitRouteTestProvider("secondary", secondary.URL, "fixture-secondary-key"))
							}
							h, route := explicitRouteTestHandler(t, http.DefaultClient, topology.mode, topology.targets, topology.targets, providers...)
							h.log = logger.New(logger.LevelFatal)
							// Small deterministic bounds reproduce production TTL/LRU
							// behavior without waiting or generating 256K tokens.
							store, err := newStateBindingStore(stateBindingStoreConfig{maxEntries: 8, ttl: time.Hour, now: clock.Now})
							if err != nil {
								t.Fatal(err)
							}
							h.stateBindings = store
							h.stateBindingsOnce.Do(func() {})
							t.Cleanup(h.BeginShutdown)
							return h, route
						}
						h, route := newHandler()
						request := func(handler *ProxyHandler, body string, turnState bool) *httptest.ResponseRecorder {
							var payload map[string]any
							if err := json.Unmarshal([]byte(body), &payload); err != nil {
								t.Fatal(err)
							}
							payload["stream"] = stream
							encoded, err := json.Marshal(payload)
							if err != nil {
								t.Fatal(err)
							}
							req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoded))
							if turnState {
								req.Header.Set("X-Codex-Turn-State", "turn-recovery-fixture")
							}
							w := httptest.NewRecorder()
							handler.HandleResponses(w, req)
							return w
						}

						first := request(h, `{"model":"public-model","input":"start"}`, false)
						if first.Code != http.StatusOK {
							t.Fatalf("initial response = %d: %s", first.Code, first.Body.String())
						}
						for _, exposed := range []string{"resp-recovery-fixture", "reasoning-recovery-fixture", "compaction-recovery-fixture", "context-recovery-fixture"} {
							if !strings.Contains(first.Body.String(), exposed) {
								t.Fatalf("initial response did not expose synthetic state %q", exposed)
							}
						}
						if first.Header().Get("X-Codex-Turn-State") != "turn-recovery-fixture" {
							t.Fatal("initial response did not expose synthetic turn state")
						}
						beforePrimary, beforeSecondary := primaryCalls.Load(), secondaryCalls.Load()
						warm := request(h, field.body, field.turnState)
						if warm.Code != http.StatusOK {
							t.Fatalf("same-process continuation = %d: %s", warm.Code, warm.Body.String())
						}
						wantPrimary, wantSecondary := int32(1), int32(0)
						if topology.mode == routeModePriorityFailover {
							wantPrimary, wantSecondary = 0, 1
						}
						if primaryCalls.Load()-beforePrimary != wantPrimary || secondaryCalls.Load()-beforeSecondary != wantSecondary {
							t.Fatal("warm continuation did not dispatch exactly once to its issuing target")
						}

						original := h
						switch loss {
						case "restart":
							h.BeginShutdown()
							h, _ = newHandler()
						case "second_process":
							h, _ = newHandler()
						case "expiry":
							clock.Advance(time.Hour)
						case "eviction":
							owner := stateBindingOwner{routeID: route.public.routeID, targetID: route.targets[0].id}
							for i := 0; i < h.stateBindings.maxEntries; i++ {
								h.stateBindings.bind(stateBindingTypeResponseID, fmt.Sprintf("resp-unrelated-fixture-%d", i), owner)
							}
						}
						beforePrimary, beforeSecondary = primaryCalls.Load(), secondaryCalls.Load()
						failed := request(h, field.body, field.turnState)
						if failed.Code != http.StatusBadRequest || !strings.Contains(failed.Body.String(), "unknown provider-bound state for explicit model route") {
							t.Fatalf("lost-state continuation = %d: %s, want unknown-state 400", failed.Code, failed.Body.String())
						}
						if primaryCalls.Load() != beforePrimary || secondaryCalls.Load() != beforeSecondary {
							t.Fatal("lost-state continuation reached a provider")
						}
						if loss == "second_process" {
							if result := request(original, field.body, field.turnState); result.Code != http.StatusOK {
								t.Fatalf("issuing process stopped accepting its own state: %d", result.Code)
							}
						}
						// New stateless traffic still succeeds: this is a local
						// continuity failure, not an upstream outage or safety denial.
						if fresh := request(h, `{"model":"public-model","input":"fresh request"}`, false); fresh.Code != http.StatusOK {
							t.Fatalf("fresh stateless request = %d: %s", fresh.Code, fresh.Body.String())
						}
					})
				}
			}
		}
	}
}

func writeRecoveryFixtureResponse(w http.ResponseWriter, stream bool, response string) {
	w.Header().Set("X-Codex-Turn-State", "turn-recovery-fixture")
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+response+`}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, response)
}
