package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestColdChatRouteDiscoversResponsesOnlyCopilotModel(t *testing.T) {
	var modelHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != providerEndpointModels {
			t.Fatalf("upstream path = %q, want %q", r.URL.Path, providerEndpointModels)
		}
		modelHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-responses","object":"model","owned_by":"copilot","supported_endpoints":["/responses"]}]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	route, err := handler.resolveChatRoute(context.Background(), "gpt-responses")
	if err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	if route.backend != chatBackendResponses || route.nativeEndpoint != providerEndpointResponses {
		t.Fatalf("route = backend %v endpoint %q, want Responses", route.backend, route.nativeEndpoint)
	}
	if !route.known {
		t.Fatal("route.known = false, want discovered ownership")
	}
	if got := modelHits.Load(); got != 1 {
		t.Fatalf("/models hits = %d, want 1", got)
	}
}

func TestColdChatRouteTimeoutFallsBackWithinBound(t *testing.T) {
	var modelHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits.Add(1)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.chatRoutes.timeout = 50 * time.Millisecond

	started := time.Now()
	route, err := handler.resolveChatRoute(context.Background(), "still-unknown")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	if route.backend != chatBackendNativeChat {
		t.Fatalf("route.backend = %v, want legacy native Chat fallback", route.backend)
	}
	if elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("resolve elapsed = %v, want bounded near 50ms", elapsed)
	}
	if got := modelHits.Load(); got != 1 {
		t.Fatalf("/models hits = %d, want 1", got)
	}
}

func TestColdChatRouteSuccessTTLNegativeCachesRandomUnknownModels(t *testing.T) {
	var modelHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	handler.chatRoutes.now = func() time.Time { return now }

	for _, model := range []string{"random-one", "random-two"} {
		route, err := handler.resolveChatRoute(context.Background(), model)
		if err != nil {
			t.Fatalf("resolveChatRoute(%q) error = %v", model, err)
		}
		if route.backend != chatBackendNativeChat {
			t.Fatalf("resolveChatRoute(%q) backend = %v, want native fallback", model, route.backend)
		}
	}
	if got := modelHits.Load(); got != 1 {
		t.Fatalf("/models hits = %d, want 1 within success TTL", got)
	}
}

func TestColdChatRouteFailureBackoffNegativeCachesRandomUnknownModels(t *testing.T) {
	var modelHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits.Add(1)
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	now := time.Unix(1_700_000_000, 0)
	handler.chatRoutes.now = func() time.Time { return now }

	for _, model := range []string{"random-failure-one", "random-failure-two"} {
		route, err := handler.resolveChatRoute(context.Background(), model)
		if err != nil {
			t.Fatalf("resolveChatRoute(%q) error = %v", model, err)
		}
		if route.backend != chatBackendNativeChat {
			t.Fatalf("resolveChatRoute(%q) backend = %v, want native fallback", model, route.backend)
		}
	}
	if got := modelHits.Load(); got != 1 {
		t.Fatalf("/models hits = %d, want 1 within failure backoff", got)
	}
}

func TestColdChatRouteCoalescesConcurrentProviderRefresh(t *testing.T) {
	const callers = 12

	var modelHits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if modelHits.Add(1) == 1 {
			close(started)
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1

	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			route, err := handler.resolveChatRoute(context.Background(), fmt.Sprintf("random-%d", i))
			if err == nil && route.backend != chatBackendNativeChat {
				err = fmt.Errorf("backend = %v, want native fallback", route.backend)
			}
			errs <- err
		}(i)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("timed out waiting for provider refresh")
	}
	time.Sleep(100 * time.Millisecond)
	if got := modelHits.Load(); got != 1 {
		close(release)
		wg.Wait()
		t.Fatalf("concurrent /models hits = %d, want 1 coalesced refresh", got)
	}

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("resolveChatRoute() error = %v", err)
		}
	}
}

func TestColdChatRouteRefreshIsProviderLocal(t *testing.T) {
	var failedHits atomic.Int32
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failedHits.Add(1)
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
	}))
	defer failed.Close()

	var healthyHits atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"healthy-model","supported_endpoints":["/responses"]}]}`))
	}))
	defer healthy.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "failed",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        failed.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
			{
				ID:             "healthy",
				Type:           "openai-compatible",
				BaseURL:        healthy.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1

	first, err := handler.resolveChatRoute(context.Background(), "unknown-on-failed")
	if err != nil {
		t.Fatalf("resolveChatRoute(failed) error = %v", err)
	}
	if first.backend != chatBackendNativeChat {
		t.Fatalf("failed provider fallback backend = %v, want native Chat", first.backend)
	}

	handler.providerSetup().defaultProviderID = "healthy"
	second, err := handler.resolveChatRoute(context.Background(), "healthy-model")
	if err != nil {
		t.Fatalf("resolveChatRoute(healthy) error = %v", err)
	}
	if second.backend != chatBackendResponses || second.provider.id != "healthy" {
		t.Fatalf("healthy route = provider %q backend %v, want healthy Responses", second.provider.id, second.backend)
	}
	if failedHits.Load() != 1 || healthyHits.Load() != 1 {
		t.Fatalf("provider-local hits = failed %d healthy %d, want 1 each", failedHits.Load(), healthyHits.Load())
	}
}

func TestColdChatRouteCollisionFailsClosed(t *testing.T) {
	var modelHits atomic.Int32
	dynamic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"shared-model","supported_endpoints":["/chat/completions"]},{"id":"new-model","supported_endpoints":["/responses"]}]}`))
	}))
	defer dynamic.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "dynamic",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        dynamic.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
			{
				ID:      "static",
				Type:    "azure-openai",
				BaseURL: "https://example.openai.azure.com/openai/v1",
				APIKey:  "fixture",
				Models: []ProviderModelConfig{{
					PublicID:  "shared-model",
					Endpoints: []string{providerEndpointChatCompletions},
				}},
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	for _, model := range []string{"new-model", "another-unknown"} {
		_, err := handler.resolveChatRoute(context.Background(), model)
		if err == nil {
			t.Fatalf("resolveChatRoute(%q) error = nil, want collision failure", model)
		}
		if !strings.Contains(err.Error(), `model "shared-model" is exposed by both provider "static" and provider "dynamic"`) {
			t.Fatalf("resolveChatRoute(%q) error = %v, want collision details", model, err)
		}
	}
	if got := modelHits.Load(); got != 1 {
		t.Fatalf("/models hits = %d, want collision cached during failure backoff", got)
	}
	if _, ok := handler.providerSetup().lookupModel("new-model"); ok {
		t.Fatal("colliding refresh partially installed provider models")
	}
}

func TestColdChatRouteDoesNotPopulateMergedModelsCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"responses-model","supported_endpoints":["/responses"]}]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	if _, err := handler.resolveChatRoute(context.Background(), "responses-model"); err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}

	handler.models.mu.RLock()
	entries := len(handler.models.entries)
	failureUntil := handler.models.canonicalFailureUntil
	handler.models.mu.RUnlock()
	if entries != 0 || !failureUntil.IsZero() {
		t.Fatalf("merged models cache changed: entries=%d failureUntil=%v", entries, failureUntil)
	}
}

func TestColdChatRouteMissingModelSkipsDiscovery(t *testing.T) {
	var modelHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits.Add(1)
		http.Error(w, "unexpected discovery", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	route, err := handler.resolveChatRoute(context.Background(), "")
	if err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	if route.backend != chatBackendNativeChat {
		t.Fatalf("route.backend = %v, want legacy native Chat behavior", route.backend)
	}
	if got := modelHits.Load(); got != 0 {
		t.Fatalf("/models hits = %d, want 0 for missing model", got)
	}
}

func TestChatRouteDiscoveryDefaults(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	if handler.chatRoutes.timeout != 2*time.Second {
		t.Fatalf("chat route discovery timeout = %v, want 2s", handler.chatRoutes.timeout)
	}
	if handler.chatRoutes.successTTL != 5*time.Minute {
		t.Fatalf("chat route success TTL = %v, want 5m", handler.chatRoutes.successTTL)
	}
	if handler.chatRoutes.failureBackoff != 5*time.Second {
		t.Fatalf("chat route failure backoff = %v, want 5s", handler.chatRoutes.failureBackoff)
	}
}

func TestColdChatRouteRefreshRegistersLifecycleWorker(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	resolved := make(chan error, 1)
	go func() {
		_, err := handler.resolveChatRoute(context.Background(), "unknown")
		resolved <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("timed out waiting for route refresh")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := handler.WaitLifecycleWorkers(waitCtx); err == nil {
		close(release)
		<-resolved
		t.Fatal("WaitLifecycleWorkers() returned while route refresh was active")
	}

	close(release)
	if err := <-resolved; err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	if err := handler.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatalf("WaitLifecycleWorkers() after refresh = %v", err)
	}
}
