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
)

func durableFailureEvent(kind, values string) string {
	if kind == "error-root" {
		return `{"type":"error","code":"rate_limit_exceeded","message":"fixture unavailable","headers":{"X-Codex-Turn-State":` + values + `}}`
	}
	if kind == "error-untyped" {
		return `{"error":{"code":"rate_limit_exceeded","message":"fixture unavailable","headers":{"X-Codex-Turn-State":` + values + `}}}`
	}
	if kind == "error" {
		return `{"type":"error","error":{"code":"rate_limit_exceeded","message":"fixture unavailable","headers":{"X-Codex-Turn-State":` + values + `}}}`
	}
	return `{"type":"response.failed","response":{"id":"failed-event-fixture","status":"failed","error":{"code":"rate_limit_exceeded","message":"fixture unavailable","headers":{"X-Codex-Turn-State":` + values + `}}}}`
}

func TestDurableResponsesErrorHeaderCaseAliases(t *testing.T) {
	const first = `{"X-Codex-Turn-State":"case-first-fixture"}`
	const second = `{"X-Codex-Turn-State":"case-second-fixture"}`
	fixtures := []struct {
		name, body, projected string
		reject                bool
	}{
		{"type", `{"type":"error","Type":"vendor.fixture","headers":` + first + `}`, "", true},
		{"headers-null", `{"type":"error","headers":` + first + `,"Headers":null}`, "", true},
		{"error-null", `{"type":"error","error":{"headers":` + first + `},"Error":{"headers":null}}`, "", true},
		{"response-null", `{"type":"response.failed","response":{"error":{"headers":` + first + `}},"Response":{"error":{"headers":null}}}`, "", true},
		{"headers-both", `{"type":"error","headers":` + first + `,"Headers":` + second + `}`, "case-second-fixture", true},
		{"error-both", `{"type":"error","error":{"headers":` + first + `},"Error":{"Headers":` + second + `}}`, "case-second-fixture", true},
		{"response-both", `{"type":"response.failed","response":{"error":{"headers":` + first + `}},"Response":{"Error":{"Headers":` + second + `}}}`, "case-first-fixture", true},
		{"uppercase-only", `{"Type":"error","Error":{"Headers":` + first + `}}`, "case-first-fixture", false},
		{"escaped-alias", `{"type":"error","headers":` + first + `,"\u0048eaders":` + second + `}`, "case-second-fixture", true},
		{"header-name-case", `{"type":"error","Headers":{"x-codex-turn-state":"case-first-fixture"}}`, "case-first-fixture", false},
		{"repeated-header-value", `{"type":"response.failed","response":{"error":{"headers":{"X-Codex-Turn-State":["case-first-fixture","case-first-fixture"]}}}}`, "", true},
		{"merged-header-case", `{"type":"response.failed","response":{"error":{"headers":` + first + `,"Headers":{"x-codex-turn-state":"case-second-fixture"}}}}`, "", true},
		{"unrelated-metadata", `{"type":"error","headers":` + first + `,"metadata":{"Headers":0,"headers":[]}}`, "case-first-fixture", false},
	}
	for _, fixture := range fixtures {
		for _, surface := range []string{"json", "sse", "websocket"} {
			for _, mode := range []string{"state", "capacity", "io"} {
				t.Run(fixture.name+"/"+surface+"/"+mode, func(t *testing.T) {
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						sends.Add(1)
						if surface == "json" {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, fixture.body)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"case progress"}`)+responsesRouteSSE("error", fixture.body))
					}))
					defer upstream.Close()
					s, config := newDurableStoreFixture(t, 8)
					switch mode {
					case "capacity":
						s.durable.maxEntries = 1
						if got := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-fixture"}}, durableFixtureOwner()); got.err != nil {
							t.Fatal(got.err)
						}
					case "io":
						s.durable.beforeCommit = func() error { return syscall.EIO }
					}
					h, route := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
					var exposed string
					var projected map[string]string
					if surface == "websocket" {
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
							// Keep raw bytes and wait for the separately projected error,
							// not merely the preceding provider error event.
							exposed += string(raw)
							var envelope struct {
								Status  int               `json:"status_code"`
								Headers map[string]string `json:"headers"`
							}
							if json.Unmarshal(raw, &envelope) == nil && envelope.Status != 0 {
								projected = envelope.Headers
								break
							}
						}
						_ = conn.Close()
					} else {
						w := durableWireRequest(h, `{"model":"public-model","input":"fixture"}`, surface == "sse", false)
						wantStatus := http.StatusOK
						if surface == "json" {
							wantStatus = http.StatusBadRequest
							if mode != "state" {
								wantStatus = http.StatusServiceUnavailable
							}
							if fixture.reject {
								wantStatus = http.StatusBadGateway
							}
						}
						if w.Code != wantStatus {
							t.Fatalf("status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
						}
						exposed = w.Body.String()
					}
					if sends.Load() != 1 || (surface != "json" && !strings.Contains(exposed, "case progress")) {
						t.Fatalf("wrong send count or lost progress: %d %s", sends.Load(), exposed)
					}
					if !fixture.reject && mode == "state" && surface == "websocket" && fixture.projected != "" {
						var turnState string
						for name, value := range projected {
							if strings.EqualFold(name, "X-Codex-Turn-State") {
								turnState = value
							}
						}
						if turnState != fixture.projected {
							t.Fatalf("typed projection not exercised: %v", projected)
						}
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
					for i, token := range []string{"case-first-fixture", "case-second-fixture"} {
						want := !fixture.reject && mode == "state" && i == 0
						if strings.Contains(exposed, token) != want {
							t.Fatalf("state exposure mismatch for %s: %s", token, exposed)
						}
						got := reopened.lookup(stateBindingTypeTurnState, token)
						if got.err != nil || (got.outcome == stateBindingLookupKnown) != want || (want && reopened.ownerTarget(got.owner, route) != route.targets[0].id) {
							t.Fatalf("reopened exact-owner proof for %s: %+v", token, got)
						}
					}
				})
			}
		}
	}
}

func TestDurableResponsesCommittedErrorEventHeaders(t *testing.T) {
	for _, websocket := range []bool{false, true} {
		for _, kind := range []string{"error", "error-root", "error-untyped", "response.failed"} {
			for _, mode := range []string{"state", "capacity", "io", "conflict", "memory"} {
				t.Run(fmt.Sprintf("websocket=%t/%s/%s", websocket, kind, mode), func(t *testing.T) {
					values := `"event-turn-fixture"`
					if mode == "conflict" {
						values = `["event-turn-fixture","conflicting-event-turn-fixture"]`
					}
					var sends atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						sends.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						// Actual semantic progress prevents either preparation layer
						// from discarding the failure as a precommit rejection.
						_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","delta":"fixture progress"}`)+responsesRouteSSE(strings.Split(kind, "-")[0], durableFailureEvent(kind, values)))
					}))
					defer upstream.Close()
					s, config := newDurableStoreFixture(t, 16)
					switch mode {
					case "memory":
						closeDurableStoreFixture(t, s)
						var err error
						s, err = newStateBindingStore(stateBindingStoreConfig{})
						if err != nil {
							t.Fatal(err)
						}
					case "capacity":
						s.durable.maxEntries = 1
						if got := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-fixture"}}, durableFixtureOwner()); got.err != nil {
							t.Fatal(got.err)
						}
					case "io":
						s.durable.beforeCommit = func() error { return syscall.EIO }
					}
					h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
					var exposed string
					if websocket {
						h.responsesWS = ResponsesWebSocketConfig{Enabled: true}
						server := startResponsesWebSocketProxyServer(t, h)
						conn := mustDialResponsesWebSocket(t, server, nil)
						if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": "fixture"}); err != nil {
							t.Fatal(err)
						}
						for range 4 {
							payload := mustReadWebSocketJSONSkipMetadata(t, conn)
							encoded, _ := json.Marshal(payload)
							exposed += string(encoded)
							if payload["type"] == "error" || payload["type"] == "response.failed" {
								break
							}
						}
						_ = conn.Close()
					} else {
						w := durableWireRequest(h, `{"model":"public-model","input":"fixture"}`, true, false)
						if w.Code != http.StatusOK {
							t.Fatalf("semantic stream was not committed: %d %s", w.Code, w.Body.String())
						}
						exposed = w.Body.String()
					}
					if !strings.Contains(exposed, "fixture progress") || sends.Load() != 1 {
						t.Fatalf("missing committed progress: %s", exposed)
					}
					wantExposure := mode == "state" || mode == "memory"
					if strings.Contains(exposed, "event-turn-fixture") != wantExposure {
						t.Fatalf("unrecorded event header escaped or bound state missing: %s", exposed)
					}
					if mode == "memory" {
						if s.lookup(stateBindingTypeTurnState, "event-turn-fixture").outcome != stateBindingLookupUnknown {
							t.Fatal("memory-only event-header behavior changed")
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
					if got := reopened.lookup(stateBindingTypeTurnState, "event-turn-fixture"); got.err != nil || (got.outcome == stateBindingLookupKnown) != wantExposure {
						t.Fatalf("reopened event header: %+v", got)
					}
					if kind == "response.failed" && mode != "state" && reopened.lookup(stateBindingTypeResponseID, "failed-event-fixture").outcome != stateBindingLookupUnknown {
						t.Fatal("failed event body/header transaction was not atomic")
					}
				})
			}
		}
	}
}

func TestDurableResponsesTranslatedWebSocketErrorHeaders(t *testing.T) {
	for _, mode := range []string{"state", "capacity", "io", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			accepted := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Codex-Turn-State", "accepted-header-fixture")
				w.(http.Flusher).Flush()
				select {
				case <-accepted:
				case <-r.Context().Done():
					return
				}
				values := `"translated-turn-fixture"`
				if mode == "conflict" {
					values = `["translated-turn-fixture","conflicting-turn-fixture"]`
				}
				_, _ = io.WriteString(w, responsesRouteSSE("error", durableFailureEvent("error", values)))
			}))
			defer upstream.Close()
			s, config := newDurableStoreFixture(t, 16)
			if mode == "capacity" {
				s.durable.maxEntries = 1
			}
			var commits int
			s.durable.beforeCommit = func() error {
				commits++
				if commits == 1 {
					// The first route preparation times out without semantic data.
					// Its real header commit releases the upstream, so the second
					// preparation layer sees the failure and projects error headers.
					close(accepted)
					return nil
				}
				if mode == "io" {
					return syscall.EIO
				}
				return nil
			}
			h, _ := durableWireHandler(t, s, explicitRouteTestProvider("issuer", upstream.URL, "fixture-key"))
			h.streamingUpstreamTimeout = 5 * time.Second
			status, exposed := invokeFinalHeaderSurface(t, h, "websocket", "")
			want := http.StatusTooManyRequests
			switch mode {
			case "io", "capacity":
				want = http.StatusServiceUnavailable
			case "conflict":
				want = http.StatusBadGateway
			}
			if status != want || strings.Contains(exposed, "translated-turn-fixture") != (mode == "state") {
				t.Fatalf("translated error status=%d want=%d exposed=%s", status, want, exposed)
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
			if got := reopened.lookup(stateBindingTypeTurnState, "translated-turn-fixture"); got.err != nil || (got.outcome == stateBindingLookupKnown) != (mode == "state") {
				t.Fatalf("reopened translated header: %+v", got)
			}
		})
	}
}
