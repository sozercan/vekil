//go:build linux

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

type durableDrainLogBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *durableDrainLogBarrier) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"msg":"request completed"`) {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return len(p), nil
}

func TestDurableStateStopTimeoutEventuallyReleasesLock(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) { testDurableStateStopTimeoutEventuallyReleasesLock(t, transport) })
	}
}

func testDurableStateStopTimeoutEventuallyReleasesLock(t *testing.T, transport string) {
	t.Helper()
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Error("unexpected provider dispatch")
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-before-stop\",\"object\":\"response\",\"model\":\"physical-model\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-before-stop","object":"response","model":"physical-model","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	cfg := proxy.ProvidersConfig{
		SchemaVersion: 2,
		Providers:     []proxy.ProviderConfig{{ID: "issuer", Type: "openai-compatible", BaseURL: upstream.URL, APIKey: "synthetic-key"}},
		ModelRoutes: []proxy.ModelRouteConfig{{ID: "route", PublicID: "public-model", Endpoints: []string{"/responses"},
			Targets: []proxy.ModelRouteTargetConfig{{ID: "target", Provider: "issuer", UpstreamModel: "physical-model"}}}},
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := WithProxyOptions(proxy.WithProvidersConfig(cfg), proxy.WithDurableStateBindings(proxy.DurableStateBindingsConfig{Path: filepath.Join(dir, "state.db")}), proxy.WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{Enabled: true}))
	barrier := &durableDrainLogBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer release()
	newServer := func(log *logger.Logger) (*Server, error) {
		return New(auth.NewTestAuthenticator("synthetic-token"), log, "127.0.0.1", "0", opts)
	}
	quietLog := logger.NewWithWriter(logger.LevelError, io.Discard)
	srv, err := newServer(logger.NewWithWriter(logger.LevelInfo, barrier))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		_ = srv.httpServer.Close()
		srv.proxyHandler.BeginShutdown()
		_ = srv.proxyHandler.WaitLifecycleWorkers(context.Background())
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	requestDone := make(chan struct{})
	if transport == "http" {
		go func() {
			defer close(requestDone)
			resp, requestErr := client.Post("http://"+srv.Addr()+"/v1/responses", "application/json", strings.NewReader(`{"model":"public-model","input":"fixture"}`))
			if requestErr == nil {
				_ = resp.Body.Close()
			}
		}()
	} else {
		dialer := websocket.Dialer{HandshakeTimeout: time.Second}
		conn, response, dialErr := dialer.Dial("ws://"+srv.Addr()+"/v1/responses", nil)
		if response != nil {
			_ = response.Body.Close()
		}
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		defer func() { _ = conn.Close() }()
		// The bridge obtains and binds real upstream HTTP Responses state;
		// this case specifically covers the hijacked client handler's teardown.
		if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "public-model", "input": "fixture"}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for i := 0; ; i++ {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 && strings.Contains(string(frame), `"type":"codex.response.metadata"`) {
				continue
			}
			if !strings.Contains(string(frame), `"type":"response.completed"`) || !strings.Contains(string(frame), `"id":"resp-before-stop"`) {
				t.Fatalf("websocket completion = %s", frame)
			}
			break
		}
		_ = conn.Close()
		close(requestDone)
	}
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("real request did not reach its handler-teardown barrier")
	}
	// This barrier is inside the real request-log middleware, after the actual
	// Responses writer committed ownership. Closing the HTTP connection (or
	// hijacking it for a websocket) cannot prove return from a blocked log sink.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = srv.Stop(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want deadline", err)
	}
	if second, openErr := newServer(quietLog); openErr == nil {
		_ = second.Stop(context.Background())
		t.Fatal("lock released while old HTTP handler still running")
	} else if !strings.Contains(openErr.Error(), "already in use") {
		t.Fatalf("replacement before drain = %v", openErr)
	}
	// All later callers keep the original Stop result; no retry may be required
	// to release resources once the old generation actually finishes.
	var waiters sync.WaitGroup
	for range 8 {
		waiters.Go(func() {
			if stopErr := srv.Stop(context.Background()); !errors.Is(stopErr, context.DeadlineExceeded) {
				t.Errorf("cached Stop = %v", stopErr)
			}
		})
	}
	waiters.Wait()
	release()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("force-closed client did not finish")
	}
	var replacement *Server
	deadline := time.Now().Add(2 * time.Second)
	for {
		replacement, err = newServer(quietLog)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "already in use") || time.Now().After(deadline) {
			t.Fatalf("replacement after handler drain = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() { _ = replacement.Stop(context.Background()) })
	if err := replacement.Start(); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post("http://"+replacement.Addr()+"/v1/responses", "application/json", strings.NewReader(`{"model":"public-model","previous_response_id":"resp-before-stop","input":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"resp-before-stop"`) || sends.Load() != 2 {
		t.Fatalf("continuation status=%d sends=%d body=%s err=%v", resp.StatusCode, sends.Load(), body, err)
	}
	late := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(late, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"late"}`)))
	if late.Code != http.StatusServiceUnavailable || sends.Load() != 2 {
		t.Fatalf("old generation admitted work: status=%d sends=%d", late.Code, sends.Load())
	}
}

func TestDurableStateCanceledStopWithoutRequestsReleasesLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := WithProxyOptions(proxy.WithDurableStateBindings(proxy.DurableStateBindingsConfig{Path: filepath.Join(dir, "state.db")}))
	newServer := func() (*Server, error) {
		return New(auth.NewTestAuthenticator("synthetic-token"), logger.NewWithWriter(logger.LevelError, io.Discard), "127.0.0.1", "0", opts)
	}
	srv, err := newServer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	firstErr := srv.Stop(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for {
		replacement, openErr := newServer()
		if openErr == nil {
			if err := replacement.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			break
		}
		if !strings.Contains(openErr.Error(), "already in use") || time.Now().After(deadline) {
			t.Fatalf("empty server after canceled Stop = %v", openErr)
		}
		time.Sleep(time.Millisecond)
	}
	if again := srv.Stop(context.Background()); again != firstErr {
		t.Fatalf("cached result changed: %v -> %v", firstErr, again)
	}
}

func TestDurableStateConstructorFailureReleasesLock(t *testing.T) {
	t.Setenv("LIGHTWEIGHT_API_KEY", "synthetic-lightweight-key")
	t.Setenv("POWERFUL_API_KEY", "synthetic-powerful-key")
	cfg, err := proxy.LoadProvidersConfigFile("../examples/policy-routing-coding-economy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := WithProxyOptions(proxy.WithProvidersConfig(cfg), proxy.WithDurableStateBindings(proxy.DurableStateBindingsConfig{Path: filepath.Join(dir, "state.db")}))
	_, err = New(auth.NewTestAuthenticator("synthetic-token"), logger.New(logger.LevelFatal), "0.0.0.0", "0", opts)
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("constructor = %v, want late listen-host rejection", err)
	}
	// The second constructor must acquire the same store, without serving or
	// contacting the example providers. A leaked descriptor would hold its lock.
	srv, err := New(auth.NewTestAuthenticator("synthetic-token"), logger.New(logger.LevelFatal), "127.0.0.1", "0", opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.proxyHandler.BeginShutdown()
	if err := srv.proxyHandler.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDurableStateListenFailureReleasesLock(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := WithProxyOptions(proxy.WithDurableStateBindings(proxy.DurableStateBindingsConfig{Path: filepath.Join(dir, "state.db")}))
	newServer := func(port string) (*Server, error) {
		return New(auth.NewTestAuthenticator("synthetic-token"), logger.NewWithWriter(logger.LevelError, io.Discard), "127.0.0.1", port, opts)
	}
	srv, err := newServer(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	assertLocked := func() {
		t.Helper()
		if extra, openErr := newServer("0"); openErr == nil {
			_ = extra.Stop(context.Background())
			t.Fatal("store admitted a second owner")
		} else if !strings.Contains(openErr.Error(), "already in use") {
			t.Fatalf("second owner = %v, want store lock refusal", openErr)
		}
	}
	assertLocked()
	err = srv.Start()
	var listenErr *net.OpError
	if !errors.As(err, &listenErr) || listenErr.Op != "listen" {
		t.Fatalf("Start = %v, want original listener error", err)
	}
	if srv.IsRunning() {
		t.Fatal("failed listener marked server running")
	}
	// No explicit Stop or GC may be needed before the replacement acquires
	// the same existing file. This is a real TCP collision and bbolt lock.
	replacement, err := newServer("0")
	if err != nil {
		t.Fatalf("replacement after listen failure = %v", err)
	}
	t.Cleanup(func() { _ = replacement.Stop(context.Background()) })
	if !srv.proxyHandler.ShuttingDown() {
		t.Error("failed generation still admits upstream work")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err == nil {
		t.Fatal("failed generation restarted after resource finalization")
	}
	if err := replacement.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + replacement.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replacement health = %d", resp.StatusCode)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("repeated cleanup = %v", err)
	}
	assertLocked()
}

func TestDurableStateStopDuringListenRetainsLockUntilCleanup(t *testing.T) {
	for _, name := range []string{"before-bind", "bound-before-handoff", "bind-failure"} {
		t.Run(name, func(t *testing.T) {
			port := "0"
			if name == "bind-failure" {
				reservation, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reservation.Close() })
				port = strconv.Itoa(reservation.Addr().(*net.TCPAddr).Port)
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			opts := WithProxyOptions(proxy.WithDurableStateBindings(proxy.DurableStateBindingsConfig{Path: filepath.Join(dir, "state.db")}))
			newServer := func() (*Server, error) {
				return New(auth.NewTestAuthenticator("synthetic-token"), logger.NewWithWriter(logger.LevelError, io.Discard), "127.0.0.1", port, opts)
			}
			srv, err := newServer()
			if err != nil {
				t.Fatal(err)
			}
			barrier := newStartupListenBarrier(t, name == "before-bind")
			srv.listen = barrier.listen
			t.Cleanup(func() {
				barrier.unblock()
				_ = srv.Stop(context.Background())
			})
			startDone := make(chan error, 1)
			go func() { startDone <- srv.Start() }()
			select {
			case <-barrier.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("Start did not reach listener barrier")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			stopErr := srv.Stop(ctx)
			cancel()
			if !errors.Is(stopErr, context.DeadlineExceeded) {
				t.Errorf("Stop while listener is held = %v, want deadline", stopErr)
			}
			if barrier.ctx.Err() == nil {
				t.Error("Stop did not cancel pending Listen")
			}
			if replacement, openErr := newServer(); openErr == nil {
				_ = replacement.Stop(context.Background())
				t.Error("store released before pending listener cleanup")
			} else if !strings.Contains(openErr.Error(), "already in use") {
				t.Fatalf("replacement before cleanup = %v", openErr)
			}
			if err := srv.Start(); err == nil || barrier.calls.Load() != 1 {
				t.Error("stopping generation admitted another Start")
			}
			barrier.unblock()
			startErr := awaitStartupResult(t, startDone)
			if startErr == nil {
				t.Error("Start succeeded after Stop had claimed the generation")
			}
			// Listen's context governs name resolution, not the returned socket;
			// a numeric address may still bind after cancellation. Preserve an
			// actual listener error, and close every late successful listener.
			if barrier.listenErr != nil && !errors.Is(startErr, barrier.listenErr) {
				t.Errorf("Listen error lost: %v, want %v", startErr, barrier.listenErr)
			}
			if name == "bind-failure" && barrier.listenErr == nil {
				t.Error("occupied-port control unexpectedly succeeded")
			}
			if srv.IsRunning() {
				t.Error("stopped generation was marked running")
			}
			if barrier.addr != "" {
				conn, dialErr := net.DialTimeout("tcp", barrier.addr, time.Second)
				if dialErr == nil {
					_ = conn.Close()
					t.Error("late listener was not closed")
				}
			}
			// The original timed-out Stop owns one background finalizer. It must
			// release the store after real listener cleanup without another Stop.
			deadline := time.Now().Add(2 * time.Second)
			for {
				replacement, openErr := newServer()
				if openErr == nil {
					_ = replacement.Stop(context.Background())
					break
				}
				if !strings.Contains(openErr.Error(), "already in use") || time.Now().After(deadline) {
					t.Fatalf("replacement after listener cleanup = %v", openErr)
				}
				time.Sleep(time.Millisecond)
			}
			if again := srv.Stop(context.Background()); again != stopErr {
				t.Fatalf("cached Stop changed from %v to %v", stopErr, again)
			}
			select {
			case <-srv.Done():
				t.Error("uncommitted startup published a Serve result")
			default:
			}
		})
	}
}
