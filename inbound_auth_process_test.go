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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	inboundProviderSecret = "synthetic-provider-secret"
	inboundCallerSecret   = "synthetic-caller-secret"
)

type inboundAuthServer struct {
	url      string
	upstream *atomic.Int32
	headers  func() []http.Header
	logs     func() string
}

// startInboundAuthServer runs the real binary against a local static provider.
// Only the supplied environment and an isolated token dir are visible to it.
func startInboundAuthServer(t *testing.T, binary string, env map[string]string, envFile string) inboundAuthServer {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex
	var seen []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"auth-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	config := filepath.Join(dir, "providers.yaml")
	body := fmt.Sprintf(`providers:
  - id: local-static
    type: openai-compatible
    base_url: %s/v1
    auth_type: bearer
    api_key_env: INBOUND_TEST_PROVIDER_SECRET
    model_discovery: static
    models:
      - public_id: auth-test
        name: Auth Test
        endpoints:
          - /chat/completions
`, upstream.URL)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--host", "127.0.0.1", "--port", "0", "--providers-config", config, "--token-dir", filepath.Join(dir, "tokens")}
	if envFile != "" {
		path := filepath.Join(dir, "serve.env")
		if err := os.WriteFile(path, []byte(envFile), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--env-file", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "INBOUND_TEST_PROVIDER_SECRET=" + inboundProviderSecret}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var logMu sync.Mutex
	var logs bytes.Buffer
	ready := make(chan string, 1)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logMu.Lock()
			logs.Write(scanner.Bytes())
			logs.WriteByte('\n')
			logMu.Unlock()
			var entry struct {
				Message string `json:"msg"`
				Addr    string `json:"addr"`
			}
			if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Message == "vekil listening" {
				select {
				case ready <- "http://" + entry.Addr:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = cmd.Wait(); <-scanned })
	var url string
	select {
	case url = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	return inboundAuthServer{url: url, upstream: &calls, logs: func() string { logMu.Lock(); defer logMu.Unlock(); return logs.String() },
		headers: func() []http.Header { mu.Lock(); defer mu.Unlock(); return append([]http.Header(nil), seen...) }}
}

func inboundDo(t *testing.T, method, url string, header map[string]string) int {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"auth-test","messages":[{"role":"user","content":"hi"}]}`)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestServeInboundAuthTokenFromEnvironment(t *testing.T) {
	name := "vekil"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	help, _ := exec.Command(binary, "--help").CombinedOutput()
	cmdHelp := exec.Command(binary, "--help")
	cmdHelp.Env = []string{"PATH=" + os.Getenv("PATH"), "INBOUND_AUTH_TOKEN=" + inboundCallerSecret}
	envHelp, _ := cmdHelp.CombinedOutput()
	if bytes.Contains(help, []byte("INBOUND_AUTH_TOKEN")) || bytes.Contains(help, []byte("-auth-token")) || bytes.Contains(help, []byte("-inbound-")) || bytes.Contains(envHelp, []byte(inboundCallerSecret)) {
		t.Fatal("help exposes an inbound token flag or value")
	}

	type route struct{ method, path string }
	routes := []route{
		{"POST", "/v1/messages"}, {"POST", "/v1/messages/count_tokens"}, {"POST", "/v1/chat/completions"},
		{"POST", "/v1beta/models/auth-test:generateContent"}, {"POST", "/v1/models/auth-test:generateContent"}, {"POST", "/models/auth-test:generateContent"},
		{"POST", "/v1/responses"}, {"POST", "/v1/responses/compact"}, {"GET", "/v1/responses"}, {"POST", "/v1/memories/trace_summarize"},
		{"POST", "/mcp/web-search"}, {"GET", "/v1/models"}, {"GET", "/dashboard"}, {"GET", "/dashboard/uPlot.min.js"},
		{"POST", "/dashboard/insight"}, {"GET", "/stats.json"}, {"GET", "/favicon.ico"}, {"GET", "/not-a-route"},
	}
	chat := "/v1/chat/completions"
	for _, tc := range []struct {
		name    string
		env     map[string]string
		envFile string
		token   string // token clients must present; empty means public
		reject  string // inherited value that must not work
	}{
		{name: "unset"},
		{name: "whitespace", env: map[string]string{"INBOUND_AUTH_TOKEN": " \t "}},
		{name: "environment", env: map[string]string{"INBOUND_AUTH_TOKEN": inboundCallerSecret}, token: inboundCallerSecret},
		{name: "env-file overrides", env: map[string]string{"INBOUND_AUTH_TOKEN": "inherited-secret"}, envFile: "INBOUND_AUTH_TOKEN=" + inboundCallerSecret + "\n", token: inboundCallerSecret, reject: "inherited-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startInboundAuthServer(t, binary, tc.env, tc.envFile)
			for _, p := range []string{"/healthz", "/readyz"} {
				if got := inboundDo(t, "GET", srv.url+p, nil); got != 200 {
					t.Fatalf("%s status = %d, want public 200", p, got)
				}
			}
			if tc.token == "" {
				if got := inboundDo(t, "POST", srv.url+chat, nil); got != 200 || srv.upstream.Load() != 1 {
					t.Fatalf("unauthenticated compatibility status=%d upstream=%d", got, srv.upstream.Load())
				}
				return
			}
			wrong := []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"x-api-key": "wrong"}}
			if tc.reject != "" {
				wrong = append(wrong, map[string]string{"Authorization": "Bearer " + tc.reject})
			}
			for _, r := range routes {
				for _, h := range wrong {
					if got := inboundDo(t, r.method, srv.url+r.path, h); got != 401 {
						t.Fatalf("%s %s headers=%v status = %d, want 401", r.method, r.path, h, got)
					}
				}
			}
			if got := srv.upstream.Load(); got != 0 {
				t.Fatalf("upstream calls after rejected requests = %d, want 0", got)
			}
			for _, h := range []map[string]string{{"Authorization": "Bearer " + tc.token}, {"x-api-key": tc.token}} {
				if got := inboundDo(t, "POST", srv.url+chat, h); got != 200 {
					t.Fatalf("valid %v status = %d, want 200", h, got)
				}
			}
			if got := srv.upstream.Load(); got != 2 {
				t.Fatalf("upstream calls after valid requests = %d, want 2", got)
			}
			for _, h := range srv.headers() {
				if got := h.Get("Authorization"); got != "Bearer "+inboundProviderSecret {
					t.Fatalf("upstream Authorization = %q, want provider credential", got)
				}
				for k, v := range h {
					if strings.Contains(strings.Join(v, ","), tc.token) || strings.EqualFold(k, "x-api-key") {
						t.Fatalf("caller credential reached upstream in %s", k)
					}
				}
			}
			if logs := srv.logs(); strings.Contains(logs, tc.token) || strings.Contains(logs, "inherited-secret") {
				t.Fatalf("server logs expose a caller secret:\n%s", logs)
			}
		})
	}
}
