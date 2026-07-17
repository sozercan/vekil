package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var _ = flag.String("settings", "", "test-only Claude settings argument")

type fakeProxy struct {
	server *httptest.Server
	done   chan error

	mu      sync.Mutex
	started bool
	stopped bool
}

func newFakeProxy(handler http.Handler) *fakeProxy {
	return &fakeProxy{
		server: httptest.NewUnstartedServer(handler),
		done:   make(chan error, 1),
	}
}

func (p *fakeProxy) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.server.Start()
	p.started = true
	return nil
}

func (p *fakeProxy) Addr() string { return p.server.Listener.Addr().String() }

func (p *fakeProxy) Done() <-chan error { return p.done }

func (p *fakeProxy) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return nil
	}
	p.stopped = true
	p.server.Close()
	close(p.done)
	return nil
}

func (p *fakeProxy) state() (started, stopped bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started, p.stopped
}

type delayedAdapter struct {
	delay  time.Duration
	binary string
	args   []string
}

func (delayedAdapter) Name() string { return "delayed" }

func (a delayedAdapter) Prepare(PrepareInput) (PreparedProcess, error) {
	time.Sleep(a.delay)
	return PreparedProcess{Path: a.binary, Args: append([]string(nil), a.args...)}, nil
}

func TestRunDoesNotStartAgentAfterCancellationDuringPrepare(t *testing.T) {
	capturePath := t.TempDir() + "/child.json"
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	signals := make(chan os.Signal, 1)
	time.AfterFunc(50*time.Millisecond, func() { signals <- os.Interrupt })
	result, err := Run(context.Background(), launcherTestProxy(), delayedAdapter{
		delay:  150 * time.Millisecond,
		binary: binary,
		args:   []string{"-test.run=TestLaunchHelperProcess"},
	}, Options{
		Model:      "claude-public",
		LocalToken: "test-token-placeholder",
		Signals:    signals,
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 130 {
		t.Fatalf("ExitCode = %d, want 130", result.ExitCode)
	}
	if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
		t.Fatalf("agent started after cancellation; capture stat error = %v", err)
	}
}

func TestRunDoesNotStartAgentAfterProxyFailureDuringPrepare(t *testing.T) {
	capturePath := t.TempDir() + "/child.json"
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	proxy := launcherTestProxy()
	time.AfterFunc(50*time.Millisecond, func() { proxy.done <- errors.New("proxy failed during prepare") })
	_, err = Run(context.Background(), proxy, delayedAdapter{
		delay:  150 * time.Millisecond,
		binary: binary,
		args:   []string{"-test.run=TestLaunchHelperProcess"},
	}, Options{
		Model:      "claude-public",
		LocalToken: "test-token-placeholder",
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "proxy failed during prepare") {
		t.Fatalf("Run() error = %v, want proxy failure", err)
	}
	if _, err := os.Stat(capturePath); !os.IsNotExist(err) {
		t.Fatalf("agent started after proxy failure; capture stat error = %v", err)
	}
}

func TestRunLaunchesClaudeWithSanitizedEnvironmentAndPreservesExitCode(t *testing.T) {
	capturePath := t.TempDir() + "/child.json"
	proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"claude-public","name":"Claude Public","supported_endpoints":["/chat/completions"]}]}`))
		case "/stats.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"totals":{"requests":2,"errors":0,"prompt_tokens":11,"completion_tokens":7,"cached_tokens":3},"by_model":[{"model":"claude-public","requests":2,"tokens":18,"errors":0}]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	var stderr bytes.Buffer
	result, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		ForwardedArgs: []string{"-test.run=TestLaunchHelperProcess", "--", "forwarded"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_EXIT=7",
			"UPSTREAM_SECRET=redacted",
			"ANTHROPIC_API_KEY=decoy-token",
			"HTTP_PROXY=http://proxy.example",
			"NO_PROXY=internal.example",
		},
		SensitiveEnv: []string{"UPSTREAM_SECRET"},
		Stderr:       &stderr,
		Stdout:       &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	started, stopped := proxy.state()
	if !started || !stopped {
		t.Fatalf("proxy state started=%v stopped=%v, want both true", started, stopped)
	}

	body, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read helper capture: %v", err)
	}
	var captured map[string]interface{}
	if err := json.Unmarshal(body, &captured); err != nil {
		t.Fatalf("decode helper capture: %v", err)
	}
	if got := captured["api_value"]; got != "" {
		t.Fatalf("child ANTHROPIC_API_KEY = %#v, want empty", got)
	}
	if got := captured["auth_value"]; got != "test-token-placeholder" {
		t.Fatalf("child ANTHROPIC_AUTH_TOKEN = %#v, want local placeholder", got)
	}
	if got := captured["anthropic_model"]; got != "claude-public" {
		t.Fatalf("child ANTHROPIC_MODEL = %#v", got)
	}
	if got := captured["removed_value"]; got != "" {
		t.Fatalf("child received sensitive environment value %#v", got)
	}
	if got := captured["http_proxy"]; got != "http://proxy.example" {
		t.Fatalf("child HTTP_PROXY = %#v", got)
	}
	for _, key := range []string{"no_proxy_upper", "no_proxy_lower"} {
		got, _ := captured[key].(string)
		for _, want := range []string{"internal.example", "127.0.0.1", "localhost", "::1"} {
			if !strings.Contains(got, want) {
				t.Fatalf("child %s = %q, missing %q", key, got, want)
			}
		}
	}
	if !strings.Contains(stderr.String(), "vekil ready: claude -> claude-public") {
		t.Fatalf("missing ready banner: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "vekil session summary") {
		t.Fatalf("missing session summary: %s", stderr.String())
	}
}

type blockingStartProxy struct {
	stopped bool
	done    chan error
}

func (p *blockingStartProxy) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (*blockingStartProxy) Addr() string         { return "127.0.0.1:0" }
func (p *blockingStartProxy) Done() <-chan error { return p.done }
func (p *blockingStartProxy) Stop(context.Context) error {
	p.stopped = true
	return nil
}

func TestRunStartupTimeoutCoversProxyAuthentication(t *testing.T) {
	proxy := &blockingStartProxy{done: make(chan error)}
	started := time.Now()
	_, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:          "claude-public",
		LocalToken:     "test-token-placeholder",
		StartupTimeout: 50 * time.Millisecond,
		Stderr:         &bytes.Buffer{},
		Stdout:         &bytes.Buffer{},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("startup timeout took %s", elapsed)
	}
	if proxy.stopped {
		t.Fatal("Run called Stop after Start returned an error")
	}
}

func TestRunReadinessTimeoutStopsProxy(t *testing.T) {
	proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	_, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:          "claude-public",
		LocalToken:     "test-token-placeholder",
		StartupTimeout: 100 * time.Millisecond,
		Stderr:         &bytes.Buffer{},
		Stdout:         &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Run() error = %v, want readiness timeout", err)
	}
	_, stopped := proxy.state()
	if !stopped {
		t.Fatal("proxy was not stopped after readiness timeout")
	}
}

func TestRunProxyFailureDuringReadinessReturnsImmediately(t *testing.T) {
	proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		<-r.Context().Done()
	}))
	time.AfterFunc(50*time.Millisecond, func() {
		proxy.done <- errors.New("listener failed during startup")
	})
	started := time.Now()
	_, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:          "claude-public",
		LocalToken:     "test-token-placeholder",
		StartupTimeout: 5 * time.Second,
		Stderr:         &bytes.Buffer{},
		Stdout:         &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "listener failed during startup") {
		t.Fatalf("Run() error = %v, want startup proxy failure", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("proxy startup failure took %s to surface", elapsed)
	}
}

func TestRunSignalDuringReadinessReturnsCancellationCode(t *testing.T) {
	proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	signals := make(chan os.Signal, 1)
	time.AfterFunc(50*time.Millisecond, func() { signals <- os.Interrupt })
	result, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:          "claude-public",
		LocalToken:     "test-token-placeholder",
		StartupTimeout: 5 * time.Second,
		Signals:        signals,
		Stderr:         &bytes.Buffer{},
		Stdout:         &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 130 {
		t.Fatalf("ExitCode = %d, want 130", result.ExitCode)
	}
	_, stopped := proxy.state()
	if !stopped {
		t.Fatal("proxy was not stopped after startup cancellation")
	}
}

func TestRunCancellationStopsChildAndProxy(t *testing.T) {
	capturePath := t.TempDir() + "/child.json"
	proxy := launcherTestProxy()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	result, err := Run(ctx, proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		ForwardedArgs: []string{"-test.run=TestLaunchHelperProcess"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_SLEEP_MS=10000",
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 130 {
		t.Fatalf("ExitCode = %d, want 130", result.ExitCode)
	}
	_, stopped := proxy.state()
	if !stopped {
		t.Fatal("proxy was not stopped after cancellation")
	}
}

func TestRunProxyFailureStopsChild(t *testing.T) {
	capturePath := t.TempDir() + "/child.json"
	proxy := launcherTestProxy()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	time.AfterFunc(150*time.Millisecond, func() {
		proxy.done <- errors.New("listener failed")
	})
	_, err = Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		ForwardedArgs: []string{"-test.run=TestLaunchHelperProcess"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_SLEEP_MS=10000",
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "listener failed") {
		t.Fatalf("Run() error = %v, want proxy failure", err)
	}
	_, stopped := proxy.state()
	if !stopped {
		t.Fatal("proxy was not stopped after listener failure")
	}
}

func TestPollProxyFailureReportsReadyFailure(t *testing.T) {
	want := errors.New("listener failed after child exit")
	done := make(chan error, 1)
	done <- want
	err := pollProxyFailure(done)
	if !errors.Is(err, want) {
		t.Fatalf("pollProxyFailure() error = %v, want %v", err, want)
	}
}

func TestPollProxyFailureIgnoresRunningProxy(t *testing.T) {
	if err := pollProxyFailure(make(chan error)); err != nil {
		t.Fatalf("pollProxyFailure() error = %v, want nil", err)
	}
}

func TestStopCommandBoundsWaitAfterFailedKill(t *testing.T) {
	killErr := errors.New("forced termination failed")
	controller := &failedStartProcessController{killErr: killErr}
	started := time.Now()
	outcome := stopCommand(controller, make(chan commandOutcome), 20*time.Millisecond, os.Interrupt, false)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("stopCommand() took %s, want bounded post-kill wait", elapsed)
	}
	if !controller.killCalled {
		t.Fatal("stopCommand() did not attempt forced termination")
	}
	if !errors.Is(outcome.err, killErr) || !strings.Contains(outcome.err.Error(), "did not exit") {
		t.Fatalf("stopCommand() error = %v, want kill and timeout errors", outcome.err)
	}
}

func launcherTestProxy() *fakeProxy {
	return newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-public","supported_endpoints":["/chat/completions"]}]}`))
		case "/stats.json":
			_, _ = w.Write([]byte(`{"totals":{"requests":0}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRunDryRunDoesNotStartProxy(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	proxy := newFakeProxy(http.NotFoundHandler())
	var stderr bytes.Buffer
	result, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		DryRun:        true,
		DryRunBaseURL: "http://127.0.0.1:4242",
		SensitiveEnv:  []string{"OPENAI_API_KEY"},
		Stderr:        &stderr,
		Stdout:        &bytes.Buffer{},
		NoSummary:     true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	started, stopped := proxy.state()
	if started || stopped {
		t.Fatalf("dry-run touched proxy: started=%v stopped=%v", started, stopped)
	}
	if !strings.Contains(stderr.String(), "vekil launch dry-run") {
		t.Fatalf("missing dry-run output: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "proxy:  http://127.0.0.1:4242") {
		t.Fatalf("dry-run ignored configured port: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "unset:  OPENAI_API_KEY") {
		t.Fatalf("dry-run omitted launch-wide environment sanitization: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "set:    ANTHROPIC_API_KEY=[empty]") {
		t.Fatalf("dry-run did not report the empty Claude API key: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "set:    ANTHROPIC_AUTH_TOKEN=[local placeholder]") {
		t.Fatalf("dry-run did not redact the local Claude auth token: %s", stderr.String())
	}
}

func TestRunRejectsUnknownModelAndStopsProxy(t *testing.T) {
	proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"other-model","supported_endpoints":["/chat/completions"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	_, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:          "missing-model",
		LocalToken:     "test-token-placeholder",
		StartupTimeout: time.Second,
		Stderr:         &bytes.Buffer{},
		Stdout:         &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "not served") {
		t.Fatalf("Run() error = %v, want missing-model error", err)
	}
	_, stopped := proxy.state()
	if !stopped {
		t.Fatal("proxy was not stopped after model validation failure")
	}
}

func TestLaunchHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LAUNCH_HELPER") != "1" {
		return
	}
	capture := map[string]interface{}{
		"api_value":       os.Getenv("ANTHROPIC_API_KEY"),
		"auth_value":      os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		"anthropic_model": os.Getenv("ANTHROPIC_MODEL"),
		"removed_value":   os.Getenv("UPSTREAM_SECRET"),
		"http_proxy":      os.Getenv("HTTP_PROXY"),
		"no_proxy_upper":  os.Getenv("NO_PROXY"),
		"no_proxy_lower":  os.Getenv("no_proxy"),
		"args":            os.Args,
	}
	body, err := json.Marshal(capture)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(98)
	}
	if err := os.WriteFile(os.Getenv("LAUNCH_HELPER_CAPTURE"), body, 0o600); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	if os.Getenv("LAUNCH_HELPER_SPAWN_GRANDCHILD") == "1" {
		grandchild := exec.Command(os.Args[0], "-test.run=TestLaunchGrandchildProcess")
		grandchild.Env = append(os.Environ(), "GO_WANT_LAUNCH_GRANDCHILD=1")
		if os.Getenv("LAUNCH_HELPER_GRANDCHILD_INHERIT_STDIO") == "1" {
			grandchild.Stdout = os.Stdout
			grandchild.Stderr = os.Stderr
		}
		if err := grandchild.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		if os.Getenv("LAUNCH_HELPER_WAIT_GRANDCHILD_READY") == "1" {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(os.Getenv("LAUNCH_GRANDCHILD_PID_FILE")); err == nil {
					time.Sleep(150 * time.Millisecond)
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	if sleepMS, _ := strconv.Atoi(os.Getenv("LAUNCH_HELPER_SLEEP_MS")); sleepMS > 0 {
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
	}
	code, _ := strconv.Atoi(os.Getenv("LAUNCH_HELPER_EXIT"))
	os.Exit(code)
}
