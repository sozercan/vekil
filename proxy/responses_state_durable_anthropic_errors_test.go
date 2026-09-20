package proxy

import (
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

func TestDurableAnthropicStorageFailureEnvelope(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
			for _, failure := range []string{"capacity", "io"} {
				t.Run(fmt.Sprintf("%s/%d/%s", path, status, failure), func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sends.Add(1)
						if r.URL.Path != path {
							t.Errorf("upstream path=%s want=%s", r.URL.Path, path)
						}
						w.Header().Set("X-Codex-Turn-State", "withheld-anthropic-fixture")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"private-provider-fixture"}}`)
					}))
					defer upstream.Close()
					s, _ := newDurableStoreFixture(t, 1)
					cause := errDurableStateCapacity
					if failure == "capacity" {
						if got := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "existing-fixture"}}, durableFixtureOwner()); got.err != nil {
							t.Fatal(got.err)
						}
					} else {
						cause = errDurableStateIO
						s.durable.beforeCommit = func() error { return syscall.EIO }
					}
					h := newOperationAdmissionTestHandler(t, providerTypeAnthropicCompatible, []string{providerEndpointMessages}, upstream.URL)
					h.stateBindings = s
					h.stateBindingsOnce.Do(func() {})
					invoke := func() *httptest.ResponseRecorder {
						r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"public-model","max_tokens":16,"messages":[{"role":"user","content":"fixture"}]}`))
						w := httptest.NewRecorder()
						if path == "/v1/messages" {
							h.HandleAnthropicMessages(w, r)
						} else {
							h.HandleAnthropicMessagesCountTokens(w, r)
						}
						return w
					}
					w := invoke()
					if w.Code != http.StatusServiceUnavailable || sends.Load() != 1 || w.Header().Get("Content-Type") != "application/json" {
						t.Fatalf("status=%d sends=%d headers=%v body=%s", w.Code, sends.Load(), w.Header(), w.Body.String())
					}
					var body struct {
						Type  string            `json:"type"`
						Error map[string]string `json:"error"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					message, _, _ := durableStateFailureDetails(cause)
					if body.Type != "error" || body.Error["type"] != "overloaded_error" || body.Error["message"] != message || len(body.Error) != 2 {
						t.Fatalf("wrong Anthropic envelope: %s", w.Body.String())
					}
					if w.Header().Get("X-Codex-Turn-State") != "" || strings.Contains(w.Body.String(), "fixture") || s.lookup(stateBindingTypeTurnState, "withheld-anthropic-fixture").outcome != stateBindingLookupUnknown {
						t.Fatal("uncommitted provider state escaped or was recorded")
					}
					if failure == "io" {
						// The first real request froze the store at the commit seam.
						// Subsequent requests fail before dispatch, with the same
						// bounded native envelope, including after the store closes.
						for _, phase := range []string{"frozen", "closed"} {
							if phase == "closed" {
								closeDurableStoreFixture(t, s)
							}
							followup := invoke()
							if followup.Code != http.StatusServiceUnavailable || followup.Body.String() != w.Body.String() || sends.Load() != 1 || followup.Header().Get("X-Codex-Turn-State") != "" {
								t.Fatalf("%s pre-dispatch failure: status=%d sends=%d body=%s", phase, followup.Code, sends.Load(), followup.Body.String())
							}
						}
					}
				})
			}
		}
	}
}
