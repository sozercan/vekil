package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

type durableShimHeaderRecorder struct {
	*httptest.ResponseRecorder
	onHeader func(int)
}

func (w *durableShimHeaderRecorder) WriteHeader(code int) {
	w.onHeader(code)
	w.ResponseRecorder.WriteHeader(code)
}

func TestDurableResponsesShimNoContent(t *testing.T) {
	for _, shim := range []string{"compact", "memory"} {
		for _, status := range []int{http.StatusNoContent, http.StatusResetContent} {
			for _, mode := range []string{"stateless", "header", "capacity", "io", "conflicting-headers"} {
				t.Run(fmt.Sprintf("%s/status=%d/%s", shim, status, mode), func(t *testing.T) {
					const turn = "no-content-turn-fixture"
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if mode != "stateless" {
							w.Header().Set("X-Codex-Turn-State", turn)
						}
						if mode == "conflicting-headers" {
							w.Header().Add("X-Codex-Turn-State", "conflicting-turn-fixture")
						}
						w.WriteHeader(status)
					}))
					defer upstream.Close()
					s, config := newDurableStoreFixture(t, 1)
					if mode == "capacity" {
						if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "existing-proof"}}, durableFixtureOwner()); r.err != nil {
							t.Fatal(r.err)
						}
					}
					if mode == "io" {
						s.durable.beforeCommit = func() error { return syscall.EIO }
					}
					h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
					body := `{"model":"public-model","input":"history"}`
					if shim == "memory" {
						body = `{"model":"public-model","traces":[{"items":[]}]}`
					}
					request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body))
					result := &durableShimHeaderRecorder{ResponseRecorder: httptest.NewRecorder()}
					result.onHeader = func(code int) {
						if code == status && mode == "header" {
							if r := s.lookup(stateBindingTypeTurnState, turn); r.err != nil || r.outcome != stateBindingLookupKnown {
								t.Fatalf("header exposed before ownership commit: %+v", r)
							}
						}
					}
					if shim == "compact" {
						h.HandleCompact(result, request)
					} else {
						h.HandleMemorySummarize(result, request)
					}
					if calls.Load() != 1 {
						t.Fatalf("sends = %d", calls.Load())
					}
					want := status
					switch mode {
					case "capacity", "io":
						want = http.StatusServiceUnavailable
					case "conflicting-headers":
						want = http.StatusBadGateway
					}
					if result.Code != want {
						t.Fatalf("status = %d, want %d: %s", result.Code, want, result.Body.String())
					}
					if mode == "capacity" && !strings.Contains(result.Body.String(), `"code":"state_binding_capacity_exceeded"`) {
						t.Fatal("capacity error code lost")
					}
					if mode == "io" && !strings.Contains(result.Body.String(), `"code":"state_binding_storage_unavailable"`) {
						t.Fatal("storage error code lost")
					}
					wantHeader := ""
					if mode == "header" {
						wantHeader = turn
					}
					if result.Header().Get("X-Codex-Turn-State") != wantHeader {
						t.Fatal("header missing or unrecorded state exposed")
					}
					if want == status && result.Body.Len() != 0 {
						t.Fatalf("no-content success returned a body: %s", result.Body.String())
					}
					closeDurableStoreFixture(t, s)
					reopened, err := newDurableStateBindingStore(config)
					if err != nil {
						t.Fatal(err)
					}
					defer closeDurableStoreFixture(t, reopened)
					wantOutcome := stateBindingLookupUnknown
					if mode == "header" {
						wantOutcome = stateBindingLookupKnown
					}
					if r := reopened.lookup(stateBindingTypeTurnState, turn); r.err != nil || r.outcome != wantOutcome {
						t.Fatalf("reopened header proof: %+v, want %v", r, wantOutcome)
					}
					if mode == "capacity" {
						if r := reopened.lookup(stateBindingTypeResponseID, "existing-proof"); r.err != nil || r.outcome != stateBindingLookupKnown {
							t.Fatalf("capacity failure lost prior proof: %+v", r)
						}
					}
				})
			}
		}
	}
}

func TestDurableResponsesShimExposure(t *testing.T) {
	for _, shim := range []string{"compact", "memory"} {
		for _, status := range []int{http.StatusOK, http.StatusCreated} {
			for _, capacity := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/status=%d/capacity=%t", shim, status, capacity), func(t *testing.T) {
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("X-Codex-Turn-State", "hidden-turn-fixture")
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"id":"hidden-response-fixture","status":"completed","output":[{"type":"reasoning","encrypted_content":"hidden-reasoning-fixture","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary fixture"}]}]}`)
					}))
					defer upstream.Close()
					s, _ := newDurableStoreFixture(t, 32)
					if capacity {
						s.durable.maxEntries = 1
					}
					h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
					body := `{"model":"public-model","input":"history"}`
					if shim == "memory" {
						body = `{"model":"public-model","traces":[{"items":[]}]}`
					}
					request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body))
					result := httptest.NewRecorder()
					if shim == "compact" {
						h.HandleCompact(result, request)
					} else {
						h.HandleMemorySummarize(result, request)
					}
					if calls.Load() != 1 {
						t.Fatalf("sends = %d", calls.Load())
					}
					if status == http.StatusOK {
						if result.Code != http.StatusOK || strings.Contains(result.Body.String(), "hidden-") || result.Header().Get("X-Codex-Turn-State") != "" {
							t.Fatalf("synthetic shim result = %d %s", result.Code, result.Body.String())
						}
						if s.stats().entries != 0 {
							t.Fatal("discarded upstream state persisted")
						}
					} else if capacity {
						if result.Code != http.StatusServiceUnavailable || strings.Contains(result.Body.String(), "hidden-") || s.stats().entries != 0 {
							t.Fatalf("passthrough capacity = %d %s", result.Code, result.Body.String())
						}
					} else {
						if result.Code != status || !strings.Contains(result.Body.String(), "hidden-reasoning-fixture") || s.stats().entries != 3 {
							t.Fatalf("passthrough did not bind state: %d %s entries=%d", result.Code, result.Body.String(), s.stats().entries)
						}
					}
				})
			}
		}
	}
}

func TestDurableResponsesShimStorageLookupFailure(t *testing.T) {
	for _, shim := range []string{"compact", "memory"} {
		for _, withState := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/state=%t", shim, withState), func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer upstream.Close()
				s, _ := newDurableStoreFixture(t, 4)
				s.durable.failed = errDurableStateIO
				h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
				body := `{"model":"public-model","input":"history"}`
				if shim == "memory" {
					body = `{"model":"public-model","traces":[{"items":[]}]}`
				}
				request := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body))
				if withState {
					request.Header.Set("X-Codex-Turn-State", "unreadable-fixture")
				}
				result := httptest.NewRecorder()
				if shim == "compact" {
					h.HandleCompact(result, request)
				} else {
					h.HandleMemorySummarize(result, request)
				}
				if result.Code != http.StatusServiceUnavailable || !strings.Contains(result.Body.String(), `"code":"state_binding_storage_unavailable"`) || !strings.Contains(result.Body.String(), `"type":"server_error"`) {
					t.Fatalf("storage lookup failure = %d %s", result.Code, result.Body.String())
				}
				if calls.Load() != 0 {
					t.Fatal("unavailable ownership store reached inference")
				}
			})
		}
	}
}

func TestDurableResponsesAbandonedAttemptStateNotPersisted(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Turn-State", "abandoned-turn")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"abandoned-response"}}`)+responsesRouteSSE("response.failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"rate limited"}}}`))
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
	}))
	defer secondary.Close()
	s, _ := newDurableStoreFixture(t, 32)
	h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", primary.URL, "fixture-primary"), explicitRouteTestProvider("secondary", secondary.URL, "fixture-secondary"))
	result := durableWireRequest(h, `{"model":"public-model","input":"start"}`, true, false)
	if result.Code != 200 || !strings.Contains(result.Body.String(), "resp-durable-fixture") || strings.Contains(result.Body.String(), "abandoned") {
		t.Fatalf("failover = %d %s", result.Code, result.Body.String())
	}
	for _, token := range []stateBindingToken{{stateBindingTypeResponseID, "abandoned-response"}, {stateBindingTypeTurnState, "abandoned-turn"}} {
		if r := s.lookup(token.stateType, token.value); r.outcome != stateBindingLookupUnknown {
			t.Fatal("hidden failed-attempt proof persisted")
		}
	}
}
