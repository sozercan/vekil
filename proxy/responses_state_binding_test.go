package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/logger"
)

type explicitResponsesStateTarget struct {
	calls atomic.Int32

	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

func (target *explicitResponsesStateTarget) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target.calls.Add(1)
	target.mu.Lock()
	target.bodies = append(target.bodies, append([]byte(nil), body...))
	target.headers = append(target.headers, r.Header.Clone())
	target.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"resp-state-binding-test","object":"response","model":"physical-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compacted summary"}]}]}`)
}

func (target *explicitResponsesStateTarget) onlyRequest(t *testing.T) ([]byte, http.Header) {
	t.Helper()
	target.mu.Lock()
	defer target.mu.Unlock()
	if len(target.bodies) != 1 || len(target.headers) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(target.bodies))
	}
	return append([]byte(nil), target.bodies[0]...), target.headers[0].Clone()
}

func newExplicitResponsesStateBindingHandler(t *testing.T) (*ProxyHandler, *modelRoute, *explicitResponsesStateTarget, *explicitResponsesStateTarget) {
	t.Helper()
	primaryTarget := &explicitResponsesStateTarget{}
	primary := httptest.NewServer(http.HandlerFunc(primaryTarget.serveHTTP))
	t.Cleanup(primary.Close)
	secondaryTarget := &explicitResponsesStateTarget{}
	secondary := httptest.NewServer(http.HandlerFunc(secondaryTarget.serveHTTP))
	t.Cleanup(secondary.Close)

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "primary-key"),
		explicitRouteTestProvider("secondary", secondary.URL, "secondary-key"),
	)
	h.log = logger.New(logger.LevelFatal)
	t.Cleanup(h.BeginShutdown)
	return h, route, primaryTarget, secondaryTarget
}

func bindExplicitEncryptedContentForTest(t *testing.T, h *ProxyHandler, route *modelRoute, targetID, token string) {
	t.Helper()
	if err := h.bindExplicitStateTokens(explicitRouteResponseInfo{
		routeID:  route.public.routeID,
		targetID: targetID,
	}, []stateBindingToken{{stateType: stateBindingTypeEncryptedContent, value: token}}); err != nil {
		t.Fatalf("bindExplicitStateTokens() error = %v", err)
	}
}

func contextCompactionStateRequestBody(t *testing.T, tokens ...string) []byte {
	t.Helper()
	input := make([]any, 0, len(tokens)+1)
	for _, token := range tokens {
		input = append(input, map[string]any{"type": "context_compaction", "encrypted_content": token})
	}
	input = append(input, map[string]any{"type": "message", "role": "user", "content": "continue"})
	body, err := json.Marshal(map[string]any{
		"model": "public-model",
		"input": input,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return body
}

func proxyCompactionStateRequestBody(t *testing.T, token string, includeSyntheticLineage bool) []byte {
	t.Helper()
	body := map[string]any{
		"model": "public-model",
		"input": []any{
			map[string]any{"type": "compaction", "encrypted_content": token},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	}
	if includeSyntheticLineage {
		body["previous_response_id"] = syntheticCompactionResponseIDPrefix + "state-binding-test"
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return encoded
}

func TestExplicitResponsesRoutesRejectMalformedProxyCheckpointsBeforeDispatch(t *testing.T) {
	endpoints := []struct {
		name   string
		path   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{name: "responses", path: "/v1/responses", handle: (*ProxyHandler).HandleResponses},
		{name: "compact", path: "/v1/responses/compact", handle: (*ProxyHandler).HandleCompact},
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, tc := range malformedSyntheticCheckpointTokensForTest(t) {
				t.Run(tc.name, func(t *testing.T) {
					h, _, primary, secondary := newExplicitResponsesStateBindingHandler(t)
					body := proxyCompactionStateRequestBody(t, tc.token, false)
					w := httptest.NewRecorder()
					endpoint.handle(h, w, httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewReader(body)))

					if w.Code != http.StatusBadRequest {
						t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
					}
					if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
						t.Fatalf("malformed checkpoint reached upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
					}
				})
			}
		})
	}
}

func TestHandleResponsesValidatesContextCompactionStateBeforeSanitizing(t *testing.T) {
	const knownToken = "provider encrypted checkpoint owned by secondary"

	t.Run("unknown state fails closed without dispatch", func(t *testing.T) {
		h, route, primary, secondary := newExplicitResponsesStateBindingHandler(t)
		ctx, summary := WithRequestSummary(context.Background())
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(contextCompactionStateRequestBody(t, "unknown provider encrypted checkpoint"))).WithContext(ctx)
		w := httptest.NewRecorder()
		h.HandleResponses(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
			t.Fatalf("unknown state reached upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
		}
		assertExplicitAdmissionOperationID(t, w, summary, route.public.routeID)
	})

	t.Run("conflicting state fails closed without dispatch", func(t *testing.T) {
		const primaryToken = "provider encrypted checkpoint owned by primary"
		const secondaryToken = "provider encrypted checkpoint owned by secondary"
		h, route, primary, secondary := newExplicitResponsesStateBindingHandler(t)
		bindExplicitEncryptedContentForTest(t, h, route, route.targets[0].id, primaryToken)
		bindExplicitEncryptedContentForTest(t, h, route, route.targets[1].id, secondaryToken)

		ctx, summary := WithRequestSummary(context.Background())
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(contextCompactionStateRequestBody(t, primaryToken, secondaryToken))).WithContext(ctx)
		w := httptest.NewRecorder()
		h.HandleResponses(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "conflicting provider-bound state") {
			t.Fatalf("body = %s, want conflicting state detail", w.Body.String())
		}
		if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
			t.Fatalf("conflicting state reached upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
		}
		assertExplicitAdmissionOperationID(t, w, summary, route.public.routeID)
	})

	t.Run("known state pins exact owner", func(t *testing.T) {
		h, route, primary, secondary := newExplicitResponsesStateBindingHandler(t)
		bindExplicitEncryptedContentForTest(t, h, route, route.targets[1].id, knownToken)

		w := httptest.NewRecorder()
		h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(contextCompactionStateRequestBody(t, knownToken))))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
		}
		if primary.calls.Load() != 0 || secondary.calls.Load() != 1 {
			t.Fatalf("state-pinned calls primary=%d secondary=%d, want 0/1", primary.calls.Load(), secondary.calls.Load())
		}
	})

	for _, checkpoint := range []struct {
		name  string
		token string
	}{
		{name: "current", token: encodeSyntheticCompaction("current proxy checkpoint summary for state binding")},
		{name: "legacy", token: syntheticCheckpointTokenForTest(t, legacySyntheticCompactionPrefix, "legacy proxy checkpoint summary for state binding")},
	} {
		t.Run("proxy "+checkpoint.name+" checkpoint keeps synthetic lineage compatibility", func(t *testing.T) {
			h, _, primary, secondary := newExplicitResponsesStateBindingHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(proxyCompactionStateRequestBody(t, checkpoint.token, true)))
			req.Header.Set("X-Codex-Turn-State", "stale synthetic turn state")
			w := httptest.NewRecorder()
			h.HandleResponses(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
			}
			if primary.calls.Load() != 1 || secondary.calls.Load() != 0 {
				t.Fatalf("proxy checkpoint calls primary=%d secondary=%d, want 1/0", primary.calls.Load(), secondary.calls.Load())
			}
			body, headers := primary.onlyRequest(t)
			if bytes.Contains(body, []byte(syntheticCompactionResponseIDPrefix)) {
				t.Fatalf("upstream body retained synthetic response lineage: %s", body)
			}
			if bytes.Contains(body, []byte(checkpoint.token)) || !bytes.Contains(body, []byte("proxy checkpoint summary for state binding")) {
				t.Fatalf("upstream body did not expand proxy checkpoint: %s", body)
			}
			if got := headers.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("upstream X-Codex-Turn-State = %q, want empty", got)
			}
		})
	}
}

func TestHandleCompactValidatesContextCompactionStateBeforeSanitizing(t *testing.T) {
	const knownToken = "provider encrypted compact checkpoint owned by secondary"

	t.Run("unknown state fails closed without dispatch", func(t *testing.T) {
		h, route, primary, secondary := newExplicitResponsesStateBindingHandler(t)
		ctx, summary := WithRequestSummary(context.Background())
		req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(contextCompactionStateRequestBody(t, "unknown provider compact checkpoint"))).WithContext(ctx)
		w := httptest.NewRecorder()
		h.HandleCompact(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
			t.Fatalf("unknown compact state reached upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
		}
		assertExplicitAdmissionOperationID(t, w, summary, route.public.routeID)
	})

	t.Run("known state pins exact owner", func(t *testing.T) {
		h, route, primary, secondary := newExplicitResponsesStateBindingHandler(t)
		bindExplicitEncryptedContentForTest(t, h, route, route.targets[1].id, knownToken)

		w := httptest.NewRecorder()
		h.HandleCompact(w, httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(contextCompactionStateRequestBody(t, knownToken))))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
		}
		if primary.calls.Load() != 0 || secondary.calls.Load() != 1 {
			t.Fatalf("state-pinned compact calls primary=%d secondary=%d, want 0/1", primary.calls.Load(), secondary.calls.Load())
		}
	})

	for _, checkpoint := range []struct {
		name    string
		summary string
		token   string
	}{
		{
			name:    "current",
			summary: "current proxy compact checkpoint summary for state binding",
			token:   encodeSyntheticCompaction("current proxy compact checkpoint summary for state binding"),
		},
		{
			name:    "legacy",
			summary: "legacy proxy compact checkpoint summary for state binding",
			token:   syntheticCheckpointTokenForTest(t, legacySyntheticCompactionPrefix, "legacy proxy compact checkpoint summary for state binding"),
		},
	} {
		t.Run("proxy "+checkpoint.name+" checkpoint resets synthetic lineage before binding and dispatch", func(t *testing.T) {
			h, _, primary, secondary := newExplicitResponsesStateBindingHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(proxyCompactionStateRequestBody(t, checkpoint.token, true)))
			req.Header.Set("X-Codex-Turn-State", "stale synthetic compact turn state")
			w := httptest.NewRecorder()
			h.HandleCompact(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
			}
			if primary.calls.Load() != 1 || secondary.calls.Load() != 0 {
				t.Fatalf("proxy compact checkpoint calls primary=%d secondary=%d, want 1/0", primary.calls.Load(), secondary.calls.Load())
			}
			body, headers := primary.onlyRequest(t)
			if bytes.Contains(body, []byte(syntheticCompactionResponseIDPrefix)) {
				t.Fatalf("upstream compact body retained synthetic response lineage: %s", body)
			}
			if bytes.Contains(body, []byte(checkpoint.token)) || !strings.Contains(string(body), checkpoint.summary) {
				t.Fatalf("upstream compact body did not expand proxy checkpoint: %s", body)
			}
			if got := headers.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("upstream compact X-Codex-Turn-State = %q, want empty", got)
			}
		})
	}
}
