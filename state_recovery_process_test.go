package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type durableProcessFixture struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	url  string
}

func durableProcessEnvironment() []string {
	// Do not inherit provider credentials, proxy settings, or the user's model
	// configuration. All providers and token storage are isolated fixtures.
	return []string{"PATH=" + os.Getenv("PATH"), "GOMAXPROCS=2", "LIVE_COPILOT_DIRECT_BEARER_TEST=0"}
}

func startDurableProcess(t *testing.T, binary, config, store, tokenDir string) *durableProcessFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "--host", "127.0.0.1", "--port", "0", "--providers-config", config, "--state-bindings-file", store, "--token-dir", tokenDir)
	cmd.Env = durableProcessEnvironment()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			var entry struct {
				Message string `json:"msg"`
				Addr    string `json:"addr"`
			}
			if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Message == "vekil listening" && entry.Addr != "" {
				select {
				case ready <- "http://" + entry.Addr:
				default:
				}
			}
		}
	}()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &durableProcessFixture{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("owned child did not exit")
		}
		select {
		case <-scanned:
		case <-time.After(time.Second):
			t.Error("child log reader did not exit")
		}
	})
	select {
	case p.url = <-ready:
	case <-p.done:
		t.Fatalf("owned server exited before listening: %v", p.err)
	case <-time.After(10 * time.Second):
		t.Fatal("owned server startup deadline exceeded")
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(p.url + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("owned server readiness deadline exceeded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-p.done:
		t.Fatal("owned server died after readiness")
	default:
	}
	return p
}

func killDurableProcess(t *testing.T, p *durableProcessFixture) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned SIGKILL child did not exit")
	}
	if p.err == nil {
		t.Fatal("SIGKILL unexpectedly looked like graceful shutdown")
	}
}

func TestDurableStateProcessCrashReopen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("durable runtime support is Linux-only")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "vekil")
	buildCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOMAXPROCS=2")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			fixtureDir := t.TempDir()
			if err := os.Chmod(fixtureDir, 0o700); err != nil {
				t.Fatal(err)
			}
			var primaryCalls, secondaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				var request struct {
					Stream   bool            `json:"stream"`
					Previous string          `json:"previous_response_id"`
					Input    json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Previous != "" && (!strings.Contains(string(request.Input), "encrypted-process-fixture") || request.Previous != "resp-process-fixture") {
					t.Error("continuation context was altered")
				}
				const response = `{"id":"resp-process-fixture","model":"physical-fixture","status":"completed","output":[{"type":"reasoning","encrypted_content":"encrypted-process-fixture","summary":[]}]}`
				w.Header().Set("X-Codex-Turn-State", "turn-process-fixture")
				if request.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+response+"}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, response)
				}
			}))
			defer secondary.Close()
			config := filepath.Join(fixtureDir, "providers.json")
			cfg := fmt.Sprintf(`{"schema_version":2,"providers":[{"id":"primary","type":"openai-compatible","base_url":%q,"auth_type":"bearer","api_key":"synthetic-primary"},{"id":"secondary","type":"openai-compatible","base_url":%q,"auth_type":"bearer","api_key":"synthetic-secondary"}],"model_routes":[{"id":"fixture-route","public_id":"public-fixture","endpoints":["/responses"],"targets":[{"id":"first","provider":"primary","upstream_model":"physical-fixture"},{"id":"second","provider":"secondary","upstream_model":"physical-fixture"}],"routing":{"mode":"priority_failover","max_target_attempts":2,"max_upstream_sends":2}}]}`, primary.URL, secondary.URL)
			if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(fixtureDir, "bindings.db")
			tokenDir := filepath.Join(fixtureDir, "unused-tokens")
			first := startDurableProcess(t, binary, config, store, tokenDir)
			request := func(base, body string, turn bool) string {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, base+"/v1/responses", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if turn {
					req.Header.Set("X-Codex-Turn-State", "turn-process-fixture")
				}
				resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("response = %d %s", resp.StatusCode, data)
				}
				if resp.Header.Get("X-Codex-Turn-State") != "turn-process-fixture" {
					t.Fatal("state header missing")
				}
				return string(data)
			}
			initial := request(first.url, fmt.Sprintf(`{"model":"public-fixture","stream":%t,"input":"start"}`, stream), false)
			if !strings.Contains(initial, "encrypted-process-fixture") || !strings.Contains(initial, "resp-process-fixture") {
				t.Fatal("provider state not observed by client")
			}
			// A real second process must refuse the live database, not wait or
			// start an independent store. It contacts no provider.
			lockCtx, lockCancel := context.WithTimeout(context.Background(), 5*time.Second)
			second := exec.CommandContext(lockCtx, binary, "--host", "127.0.0.1", "--port", "0", "--providers-config", config, "--state-bindings-file", store, "--token-dir", tokenDir)
			second.Env = durableProcessEnvironment()
			out, err := second.CombinedOutput()
			lockCancel()
			if err == nil || !strings.Contains(string(out), "already in use") {
				t.Fatalf("second writer did not refuse: %v %s", err, out)
			}
			killDurableProcess(t, first)
			restarted := startDurableProcess(t, binary, config, store, tokenDir)
			beforePrimary, beforeSecondary := primaryCalls.Load(), secondaryCalls.Load()
			continued := request(restarted.url, fmt.Sprintf(`{"model":"public-fixture","stream":%t,"previous_response_id":"resp-process-fixture","input":[{"type":"reasoning","encrypted_content":"encrypted-process-fixture","summary":[]},{"role":"user","content":"continue"}]}`, stream), true)
			if !strings.Contains(continued, "encrypted-process-fixture") {
				t.Fatal("restarted response lost state")
			}
			if primaryCalls.Load() != beforePrimary || secondaryCalls.Load() != beforeSecondary+1 {
				t.Fatal("reopened continuation switched issuer or retried")
			}
			killDurableProcess(t, restarted)
			beforePrune, err := os.ReadFile(store)
			if err != nil {
				t.Fatal(err)
			}
			pruneCtx, pruneCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer pruneCancel()
			prune := exec.CommandContext(pruneCtx, binary, "state", "prune", "--file", store, "--before", "2020-01-01T00:00:00.500Z", "--confirm")
			prune.Env = durableProcessEnvironment()
			out, err = prune.CombinedOutput()
			if err == nil || prune.ProcessState == nil || prune.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "whole-second") {
				t.Fatalf("fractional prune was not rejected: %v %s", err, out)
			}
			afterPrune, err := os.ReadFile(store)
			if err != nil || !bytes.Equal(beforePrune, afterPrune) {
				t.Fatalf("invalid CLI prune changed the store: %v", err)
			}
			prune = exec.CommandContext(pruneCtx, binary, "state", "prune", "--file", store, "--before", "2020-01-01T00:00:00Z", "--confirm")
			prune.Env = durableProcessEnvironment()
			if out, err := prune.CombinedOutput(); err != nil || !strings.Contains(string(out), "Pruned 0 ownership records") {
				t.Fatalf("whole-second CLI prune: %v %s", err, out)
			}
			afterPruning := startDurableProcess(t, binary, config, store, tokenDir)
			continued = request(afterPruning.url, fmt.Sprintf(`{"model":"public-fixture","stream":%t,"previous_response_id":"resp-process-fixture","input":[{"type":"reasoning","encrypted_content":"encrypted-process-fixture","summary":[]},{"role":"user","content":"continue"}]}`, stream), true)
			if !strings.Contains(continued, "encrypted-process-fixture") || primaryCalls.Load() != beforePrimary || secondaryCalls.Load() != beforeSecondary+2 {
				t.Fatal("CLI pruning lost retained proof or changed its exact issuer")
			}
			killDurableProcess(t, afterPruning)
		})
	}
}
