package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

const durableErrorFixture = `{"error":{"code":"fixture_error","message":"upstream fixture failure"},"id":"resp-error-fixture","conversation":"conv-error-fixture","output":[{"type":"reasoning","encrypted_content":"reasoning-error-fixture","summary":[]}]}`

func invokeDurableErrorSurface(h *ProxyHandler, surface string, w http.ResponseWriter) {
	body := `{"model":"public-model","input":"fixture"}`
	switch surface {
	case "memory":
		body = `{"model":"public-model","traces":[{"items":[]}]}`
	case "trigger", "trigger-stream":
		body = fmt.Sprintf(`{"model":"public-model","stream":%t,"input":[{"role":"user","content":"fixture"},{"type":"compaction_trigger"}]}`, surface == "trigger-stream")
	}
	r := httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body))
	switch surface {
	case "responses", "trigger", "trigger-stream":
		h.HandleResponses(w, r)
	case "compact":
		h.HandleCompact(w, r)
	case "memory":
		h.HandleMemorySummarize(w, r)
	}
}

func TestDurableResponsesFinalErrorFailoverOwnership(t *testing.T) {
	for _, surface := range []string{"responses", "compact", "memory", "trigger", "trigger-stream"} {
		for _, secondStatus := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", surface, secondStatus), func(t *testing.T) {
				var firstSends, secondSends atomic.Int32
				first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					firstSends.Add(1)
					w.Header().Set("X-Codex-Turn-State", "first-turn-fixture")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, strings.ReplaceAll(durableErrorFixture, "error-fixture", "first-error-fixture"))
				}))
				defer first.Close()
				second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					secondSends.Add(1)
					w.Header().Set("X-Codex-Turn-State", "turn-error-fixture")
					w.WriteHeader(secondStatus)
					_, _ = io.WriteString(w, durableErrorFixture)
				}))
				defer second.Close()
				s, config := newDurableStoreFixture(t, 32)
				providers := []*providerRuntime{explicitRouteTestProvider("first", first.URL, "first-key"), explicitRouteTestProvider("second", second.URL, "second-key")}
				h, route := durableWireHandler(t, s, providers...)
				w := httptest.NewRecorder()
				invokeDurableErrorSurface(h, surface, w)
				wantStatus, wantSecondSends := secondStatus, int32(1)
				if surface == "compact" || surface == "trigger" || surface == "trigger-stream" {
					// Compaction attempts cannot switch targets, even on 429.
					wantStatus, wantSecondSends = http.StatusTooManyRequests, 0
				}
				if w.Code != wantStatus || firstSends.Load() != 1 || secondSends.Load() != wantSecondSends {
					t.Fatalf("final status=%d sends=%d/%d body=%s", w.Code, firstSends.Load(), secondSends.Load(), w.Body.String())
				}
				selected, abandoned, target := "resp-error-fixture", "resp-first-error-fixture", route.targets[1].id
				if wantStatus == http.StatusTooManyRequests {
					// Equal exhausted failures preserve the first error, not the last
					// attempt. Its captured response retains the original issuer.
					selected, abandoned, target = abandoned, selected, route.targets[0].id
				}
				if !strings.Contains(w.Body.String(), selected) {
					t.Fatal("wrong final response exposed")
				}
				if got := s.lookup(stateBindingTypeResponseID, selected); got.err != nil || got.outcome != stateBindingLookupKnown || s.ownerTarget(got.owner, route) != target {
					t.Fatal("exposed failure not bound to its exact issuer")
				}
				if got := s.lookup(stateBindingTypeResponseID, abandoned); got.err != nil || got.outcome != stateBindingLookupUnknown {
					t.Fatal("unexposed failover attempt was persisted")
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
				continued := durableWireRequest(h, fmt.Sprintf(`{"model":"public-model","previous_response_id":%q,"input":"continue"}`, selected), false, false)
				wantFirst, wantSecond := int32(1), wantSecondSends+1
				if wantStatus == http.StatusTooManyRequests {
					wantFirst, wantSecond = 2, wantSecondSends
				}
				if continued.Code != wantStatus || firstSends.Load() != wantFirst || secondSends.Load() != wantSecond {
					t.Fatalf("reopened issuer: status=%d sends=%d/%d", continued.Code, firstSends.Load(), secondSends.Load())
				}
			})
		}
	}
}

func TestDurableResponsesErrorMemoryModeUnchanged(t *testing.T) {
	for _, surface := range []string{"responses", "compact", "memory", "trigger", "trigger-stream"} {
		t.Run(surface, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, durableErrorFixture)
			}))
			defer upstream.Close()
			s, err := newStateBindingStore(stateBindingStoreConfig{})
			if err != nil {
				t.Fatal(err)
			}
			h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
			w := httptest.NewRecorder()
			invokeDurableErrorSurface(h, surface, w)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "reasoning-error-fixture") || s.lookup(stateBindingTypeResponseID, "resp-error-fixture").outcome != stateBindingLookupUnknown {
				t.Fatal("memory-only error passthrough changed")
			}
		})
	}
}

func TestDurableResponsesCompactionTriggerSyntheticSuccess(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, surface := range []string{"trigger", "trigger-stream"} {
			t.Run(fmt.Sprintf("durable=%t/%s", durable, surface), func(t *testing.T) {
				var sends atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					sends.Add(1)
					w.Header().Set("X-Codex-Turn-State", "hidden-turn-fixture")
					_, _ = io.WriteString(w, `{"id":"hidden-response-fixture","status":"completed","output":[{"type":"reasoning","encrypted_content":"hidden-reasoning-fixture","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary fixture"}]}]}`)
				}))
				defer upstream.Close()
				var s *stateBindingStore
				if durable {
					s, _ = newDurableStoreFixture(t, 1)
					// Synthetic success must not need a durable write at all.
					s.durable.beforeCommit = func() error { return syscall.EIO }
				} else {
					var err error
					s, err = newStateBindingStore(stateBindingStoreConfig{})
					if err != nil {
						t.Fatal(err)
					}
				}
				h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
				w := httptest.NewRecorder()
				invokeDurableErrorSurface(h, surface, w)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"type":"compaction"`) || strings.Contains(w.Body.String(), "hidden-") || w.Header().Get("X-Codex-Turn-State") != "" {
					t.Fatalf("synthetic result: %d %s", w.Code, w.Body.String())
				}
				if surface == "trigger-stream" && !strings.Contains(w.Body.String(), `"type":"response.completed"`) {
					t.Fatal("streaming synthetic response lost its completion event")
				}
				if sends.Load() != 1 || s.stats().entries != 0 {
					t.Fatalf("synthetic path sent=%d bound=%d", sends.Load(), s.stats().entries)
				}
			})
		}
	}
}

func TestDurableResponsesErrorWriterEmptyAndReadFailure(t *testing.T) {
	for _, mode := range []string{"nil", "read-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newDurableStoreFixture(t, 4)
			h := &ProxyHandler{stateBindings: s}
			h.stateBindingsOnce.Do(func() {})
			info := explicitRouteResponseInfo{routeID: "route", targetID: "target", publicID: "public", stateIdentity: [32]byte{1}}
			resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"X-Codex-Turn-State": {"turn-error-fixture"}}}
			if mode == "read-failure" {
				resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(durableErrorFixture), durableErrorFailingReader{}))
			}
			w := &durableShimHeaderRecorder{ResponseRecorder: httptest.NewRecorder()}
			w.onHeader = func(code int) {
				if code == http.StatusBadRequest && s.lookup(stateBindingTypeTurnState, "turn-error-fixture").outcome != stateBindingLookupKnown {
					t.Error("header exposed before binding")
				}
			}
			err := writeExplicitResponsesResponse(context.Background(), h, w, resp, info, nil, "")
			if mode == "nil" {
				if err != nil || w.Code != http.StatusBadRequest || w.Header().Get("X-Codex-Turn-State") != "turn-error-fixture" || w.Body.Len() != 0 {
					t.Fatalf("nil-body passthrough: status=%d err=%v", w.Code, err)
				}
				return
			}
			var bodyErr *responseBodyWriteError
			if !errors.As(err, &bodyErr) || bodyErr.committed || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read failure not withheld: %v", err)
			}
			if h.handleResponseBodyWriteError(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), context.Background(), "responses", err) {
				t.Fatal("uncommitted durable read failure suppressed the terminal gateway error")
			}
			if w.Header().Get("X-Codex-Turn-State") != "" || w.Body.Len() != 0 || s.lookup(stateBindingTypeTurnState, "turn-error-fixture").outcome != stateBindingLookupUnknown {
				t.Fatal("incomplete error response was exposed or bound")
			}
		})
	}
}

type durableErrorFailingReader struct{}

func (durableErrorFailingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDurableResponsesErrorWriterKeepsSuccessSideEffectsSeparate(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, _ := newDurableStoreFixture(t, 4)
			h := &ProxyHandler{stateBindings: s}
			h.stateBindingsOnce.Do(func() {})
			optimizer := &recordingToolOptimizer{}
			configureRecordingToolOptimizer(h, optimizer)
			info := explicitRouteResponseInfo{routeID: "route", targetID: "target", publicID: "public", stateIdentity: [32]byte{1}}
			body := `{"id":"resp-error-fixture","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9},"output":[{"type":"function_call","name":"shell_command","call_id":"call-fixture","arguments":"{\"command\":\"grep foo big.log\"}"}]}`
			resp := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			ctx, summary := WithRequestSummary(context.Background())
			w := httptest.NewRecorder()
			if err := writeExplicitResponsesResponse(ctx, h, w, resp, info, h.toolContexts, "fixture"); err != nil {
				t.Fatal(err)
			}
			wantTotal, wantRewrites := 0, 0
			if status == http.StatusOK {
				wantTotal, wantRewrites = 9, 1 // positive control proves both hooks are wired
			}
			if got := readSummaryForStats(summary); got.total != wantTotal || len(optimizer.snapshotRewriteRequests()) != wantRewrites {
				t.Fatalf("success-only hooks: usage=%d rewrites=%d", got.total, len(optimizer.snapshotRewriteRequests()))
			}
			if status != http.StatusOK && !strings.Contains(w.Body.String(), "grep foo big.log") {
				t.Fatal("error tool command was rewritten")
			}
			if s.lookup(stateBindingTypeResponseID, "resp-error-fixture").outcome != stateBindingLookupKnown {
				t.Fatal("state binding incorrectly depends on success")
			}
		})
	}
}

func TestDurableResponsesTerminalErrorsBindBeforeExposure(t *testing.T) {
	for _, surface := range []string{"responses", "compact", "memory", "trigger", "trigger-stream"} {
		for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
			for _, mode := range []string{"state", "capacity", "io", "plain", "empty", "malformed", "non-object", "conflicting-headers"} {
				t.Run(fmt.Sprintf("%s/%d/%s", surface, status, mode), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						sends.Add(1)
						w.Header().Set("Content-Type", "application/json")
						body := durableErrorFixture
						if mode == "plain" {
							body = `{"error":{"message":"ordinary failure"}}`
						} else {
							w.Header().Set("X-Codex-Turn-State", "turn-error-fixture")
						}
						switch mode {
						case "empty":
							body = ""
						case "malformed":
							body = `{"id":"resp-error-fixture","output":`
						case "non-object":
							body = `["error-fixture"]`
						case "conflicting-headers":
							w.Header().Add("X-Codex-Turn-State", "conflicting-turn-fixture")
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, body)
					}))
					defer upstream.Close()
					limit := 32
					if mode == "capacity" {
						limit = 1
					}
					s, config := newDurableStoreFixture(t, limit)
					if mode == "capacity" {
						if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-fixture"}}, durableFixtureOwner()); r.err != nil {
							t.Fatal(r.err)
						}
					}
					if mode == "io" {
						s.durable.beforeCommit = func() error { return syscall.EIO }
					}
					h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
					w := &durableShimHeaderRecorder{ResponseRecorder: httptest.NewRecorder()}
					w.onHeader = func(code int) {
						if code == status && (mode == "state" || mode == "empty") {
							if got := s.lookup(stateBindingTypeTurnState, "turn-error-fixture"); got.err != nil || got.outcome != stateBindingLookupKnown {
								t.Errorf("error headers exposed before commit: %+v", got)
							}
							if mode == "state" {
								if got := s.lookup(stateBindingTypeEncryptedContent, "reasoning-error-fixture"); got.err != nil || got.outcome != stateBindingLookupKnown {
									t.Errorf("error body exposed before commit: %+v", got)
								}
							}
						}
					}
					invokeDurableErrorSurface(h, surface, w)
					want := status
					switch mode {
					case "capacity", "io":
						want = http.StatusServiceUnavailable
					case "malformed", "non-object", "conflicting-headers":
						want = http.StatusBadGateway
					}
					if w.Code != want || sends.Load() != 1 {
						t.Fatalf("status=%d want=%d sends=%d body=%s", w.Code, want, sends.Load(), w.Body.String())
					}
					if want != status {
						if w.Header().Get("X-Codex-Turn-State") != "" || strings.Contains(w.Body.String(), "error-fixture") {
							t.Fatal("uncommitted error state escaped")
						}
					} else if mode == "plain" {
						if !strings.Contains(w.Body.String(), "ordinary failure") {
							t.Fatal("ordinary provider error lost")
						}
					} else if w.Header().Get("X-Codex-Turn-State") != "turn-error-fixture" {
						t.Fatal("committed header lost")
					}
					if mode == "state" && !strings.Contains(w.Body.String(), "reasoning-error-fixture") {
						t.Fatal("committed body lost")
					}
					closeDurableStoreFixture(t, s)
					reopened, err := newDurableStateBindingStore(config)
					if err != nil {
						t.Fatal(err)
					}
					defer closeDurableStoreFixture(t, reopened)
					wantOutcome := stateBindingLookupUnknown
					if mode == "state" || mode == "empty" {
						wantOutcome = stateBindingLookupKnown
					}
					if got := reopened.lookup(stateBindingTypeTurnState, "turn-error-fixture"); got.err != nil || got.outcome != wantOutcome {
						t.Fatalf("reopened header proof = %+v", got)
					}
				})
			}
		}
	}
}
