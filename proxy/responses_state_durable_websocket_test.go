package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
)

func startDurableWebSocketChild(t *testing.T, config DurableStateBindingsConfig, upstream string, native bool) (string, func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDurableStateBarrierChild$", "-test.timeout=18s")
	cmd.Env = []string{"GOMAXPROCS=2", "VEKIL_TEST_STATE_CHILD=serve", "VEKIL_TEST_STATE_FILE=" + config.Path, "VEKIL_TEST_PROVIDER_URL=" + upstream, fmt.Sprintf("VEKIL_TEST_NATIVE=%t", native)}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	kill := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			select {
			case err := <-done:
				if err == nil {
					t.Error("child exited gracefully, want crash")
				}
			case <-ctx.Done():
				t.Error("child exit deadline")
			}
		})
	}
	t.Cleanup(kill)
	ready := make(chan string, 1)
	go func() {
		defer close(ready)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "ready ") {
				ready <- "ws" + strings.TrimPrefix(line, "ready http")
			}
		}
	}()
	select {
	case base := <-ready:
		if base == "" {
			t.Fatal("child startup failed")
		}
		return base + "/v1/responses", kill
	case <-ctx.Done():
		t.Fatal("child startup deadline")
	}
	return "", kill
}

func TestDurableResponsesWebSocketCrashReconnect(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
			var calls, connections atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				check := func(request map[string]json.RawMessage) {
					if calls.Add(1) > 1 && !strings.Contains(string(request["input"]), "reasoning-durable-fixture") {
						t.Error("reconnected request lost retained input state")
					}
				}
				if native {
					if r.Method != http.MethodGet {
						t.Error("native turn used HTTP POST")
						w.WriteHeader(500)
						return
					}
					conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					connections.Add(1)
					defer func() { _ = conn.Close() }()
					for {
						var request map[string]json.RawMessage
						if conn.ReadJSON(&request) != nil {
							return
						}
						check(request)
						if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":`+durableWireFixture+`}`)); err != nil {
							return
						}
					}
				}
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				check(request)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
			}))
			defer upstream.Close()
			s, config := newDurableStoreFixture(t, 32)
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			dial := func(url string) *websocket.Conn {
				t.Helper()
				conn, _, err := websocket.DefaultDialer.Dial(url, nil)
				if err != nil {
					t.Fatal(err)
				}
				return conn
			}
			firstURL, kill := startDurableWebSocketChild(t, config, upstream.URL, native)
			first := dial(firstURL)
			request := map[string]any{"type": "response.create", "model": "public-model", "input": []any{map[string]string{"role": "user", "content": "start"}}}
			if err := first.WriteJSON(request); err != nil {
				t.Fatal(err)
			}
			completed := mustReadWebSocketJSONSkipMetadata(t, first)
			if completed["type"] != "response.completed" {
				t.Fatalf("initial = %v", completed)
			}
			kill()
			_ = first.Close()
			secondURL, killSecond := startDurableWebSocketChild(t, config, upstream.URL, native)
			defer killSecond()
			// Connection-local delta lineage is deliberately not recovered. A
			// reconnect must retain and resend full input; do not silently drop it.
			invalid := dial(secondURL)
			request["previous_response_id"] = "resp-durable-fixture"
			if err := invalid.WriteJSON(request); err != nil {
				t.Fatal(err)
			}
			if rejected := mustReadWebSocketJSONSkipMetadata(t, invalid); rejected["type"] != "error" {
				t.Fatalf("old session lineage = %v", rejected)
			}
			_ = invalid.Close()
			if calls.Load() != 1 {
				t.Fatal("invalid connection lineage dispatched")
			}
			second := dial(secondURL)
			defer func() { _ = second.Close() }()
			delete(request, "previous_response_id")
			request["input"] = []any{map[string]any{"type": "reasoning", "encrypted_content": "reasoning-durable-fixture", "summary": []any{}}, map[string]string{"role": "user", "content": "continue"}}
			if err := second.WriteJSON(request); err != nil {
				t.Fatal(err)
			}
			if completed := mustReadWebSocketJSONSkipMetadata(t, second); completed["type"] != "response.completed" {
				t.Fatalf("reconnect = %v", completed)
			}
			if calls.Load() != 2 {
				t.Fatalf("physical turns = %d", calls.Load())
			}
			if native && connections.Load() != 2 {
				t.Fatalf("native connections = %d", connections.Load())
			}
		})
	}
}

func TestDurableResponsesWebSocketStorageFailure(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, native := range []bool{false, true} {
		for _, header := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%t/header=%t", native, header), func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					headers := make(http.Header)
					if header {
						headers.Set("X-Codex-Turn-State", "fault-turn-fixture")
					}
					if native {
						conn, err := responsesWebSocketUpgrader.Upgrade(w, r, headers)
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
						calls.Add(1)
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":`+durableWireFixture+`}`))
						_, _, _ = conn.ReadMessage()
						return
					}
					calls.Add(1)
					if header {
						w.Header().Set("X-Codex-Turn-State", "fault-turn-fixture")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+durableWireFixture+`}`))
				}))
				defer upstream.Close()
				s, _ := newDurableStoreFixture(t, 32)
				s.durable.beforeCommit = func() error { return syscall.EIO }
				provider := explicitRouteTestProvider("primary", upstream.URL, "fixture-key")
				if native {
					provider.kind = providerTypeCopilot
				}
				h, _ := durableWireHandler(t, s, provider)
				h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_synthetic-source", "synthetic-responses-token")
				h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: native}
				server := startResponsesWebSocketProxyServer(t, h)
				conn := mustDialResponsesWebSocket(t, server, nil)
				defer func() { _ = conn.Close() }()
				if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": []any{map[string]string{"role": "user", "content": "start"}}}); err != nil {
					t.Fatal(err)
				}
				for {
					payload := mustReadWebSocketJSON(t, conn)
					data, _ := json.Marshal(payload)
					if strings.Contains(string(data), "durable-fixture") || strings.Contains(string(data), "fault-turn-fixture") {
						t.Fatal("unrecorded state escaped over websocket")
					}
					if payload["type"] == "codex.response.metadata" {
						continue
					}
					if payload["type"] != "error" || !strings.Contains(string(data), "state_binding_storage_unavailable") || payload["status_code"] != float64(503) {
						t.Fatalf("websocket local failure = %s", data)
					}
					break
				}
				if calls.Load() != 1 {
					t.Fatalf("storage failure sends = %d", calls.Load())
				}
			})
		}
	}
}
