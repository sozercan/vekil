package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
)

// This helper runs only in a dedicated test subprocess. The barriers are not
// configurable in a serving binary and the child inherits no provider secrets.
func TestDurableStateBarrierChild(t *testing.T) {
	mode := os.Getenv("VEKIL_TEST_STATE_CHILD")
	if mode != "barrier" && mode != "serve" {
		return
	}
	s, err := newDurableStateBindingStore(DurableStateBindingsConfig{Path: os.Getenv("VEKIL_TEST_STATE_FILE"), MaxEntries: 32})
	if err != nil {
		t.Fatal(err)
	}
	p := explicitRouteTestProvider("primary", os.Getenv("VEKIL_TEST_PROVIDER_URL"), "fixture-key")
	native := os.Getenv("VEKIL_TEST_NATIVE") == "true"
	if native {
		p.kind = providerTypeCopilot
	}
	h, _ := durableWireHandler(t, s, p)
	h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_synthetic-source", "synthetic-responses-token")
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: native}
	barrier := func() error {
		fmt.Println("barrier")
		select {} // Parent kills this exact child; no commit/exposure can advance.
	}
	if mode == "barrier" {
		if os.Getenv("VEKIL_TEST_STATE_WINDOW") == "after" {
			s.durable.afterCommit = barrier
		} else {
			s.durable.beforeCommit = barrier
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready http://" + listener.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", h.HandleResponses)
	mux.HandleFunc("POST /", h.HandleResponses)
	mux.HandleFunc("GET /v1/responses", h.HandleResponsesWebSocket)
	if err := http.Serve(listener, mux); err != nil {
		t.Fatal(err)
	}
}

func TestDurableStateProcessCommitWindows(t *testing.T) {
	for _, window := range []string{"before", "after"} {
		for _, stage := range []string{"header", "json", "sse"} {
			t.Run(window+"/"+stage, func(t *testing.T) {
				s, config := newDurableStoreFixture(t, 32)
				if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-committed"}}, durableFixtureOwner()); r.err != nil {
					t.Fatal(r.err)
				}
				if err := s.close(); err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if stage == "header" {
						w.Header().Set("X-Codex-Turn-State", "turn-durable-fixture")
					}
					if stage == "sse" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, durableWireFixture)
					}
				}))
				defer upstream.Close()
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDurableStateBarrierChild$", "-test.timeout=12s")
				cmd.Env = []string{"GOMAXPROCS=2", "VEKIL_TEST_STATE_CHILD=barrier", "VEKIL_TEST_STATE_FILE=" + config.Path, "VEKIL_TEST_PROVIDER_URL=" + upstream.URL, "VEKIL_TEST_STATE_WINDOW=" + window}
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				defer func() { _ = cmd.Process.Kill() }()
				lines := make(chan string, 8)
				go func() {
					defer close(lines)
					scanner := bufio.NewScanner(stdout)
					for scanner.Scan() {
						select {
						case lines <- scanner.Text():
						case <-ctx.Done():
							return
						}
					}
				}()
				waitLine := func(prefix string) string {
					t.Helper()
					for {
						select {
						case line, ok := <-lines:
							if !ok {
								t.Fatal("child exited before barrier")
							}
							if strings.HasPrefix(line, prefix) {
								return strings.TrimPrefix(line, prefix)
							}
						case <-ctx.Done():
							t.Fatal("child barrier deadline")
						}
					}
				}
				base := waitLine("ready ")
				result := make(chan string, 1)
				go func() {
					resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(base, "application/json", strings.NewReader(fmt.Sprintf(`{"model":"public-model","input":"start","stream":%t}`, stage == "sse")))
					if err != nil {
						result <- ""
						return
					}
					defer func() { _ = resp.Body.Close() }()
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
					result <- resp.Header.Get("X-Codex-Turn-State") + string(body)
				}()
				waitLine("barrier")
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("child was not killed")
					}
				case <-ctx.Done():
					t.Fatal("child did not exit")
				}
				select {
				case body := <-result:
					if strings.Contains(body, "durable-fixture") {
						t.Fatal("state escaped before barrier")
					}
				case <-ctx.Done():
					t.Fatal("client did not finish")
				}
				if calls.Load() != 1 {
					t.Fatal("wrong physical send count")
				}
				reopened, err := newDurableStateBindingStore(config)
				if err != nil {
					t.Fatal(err)
				}
				defer closeDurableStoreFixture(t, reopened)
				if r := reopened.lookup(stateBindingTypeResponseID, "prior-committed"); r.outcome != stateBindingLookupKnown {
					t.Fatal("crash lost prior proof")
				}
				kind, value := stateBindingTypeResponseID, "resp-durable-fixture"
				if stage == "header" {
					kind, value = stateBindingTypeTurnState, "turn-durable-fixture"
				}
				want := stateBindingLookupUnknown
				if window == "after" {
					want = stateBindingLookupKnown
				}
				if r := reopened.lookup(kind, value); r.err != nil || r.outcome != want {
					t.Fatalf("crash window = %+v, want %s", r, want)
				}
			})
		}
	}
}
