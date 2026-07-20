package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestRuntimeSnapshotPinsGenerationAndCaches(t *testing.T) {
	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com", chatRoutes: newChatRouteDiscoveryCache()}
	g1 := &runtimeSnapshot{
		generation: 1,
		revision:   "cfg_one",
		config:     ProvidersConfig{},
		providers:  defaultProviderSetup(h),
		policy:     &policyBinding{},
		caches:     newRuntimeCaches(),
	}
	h.publishRuntime(g1)
	pinned := h.PinRuntimeContext(context.Background())

	g2 := &runtimeSnapshot{
		generation: 2,
		revision:   "cfg_two",
		config: ProvidersConfig{Providers: []ProviderConfig{{
			ID:      "other",
			Type:    string(providerTypeOpenAICompatible),
			Default: true,
			BaseURL: "https://example.invalid/v1",
		}}},
		providers: &providerSetup{providers: map[string]*providerRuntime{"other": {id: "other"}}, providerOrder: []string{"other"}, defaultProviderID: "other", models: map[string]providerModel{}},
		policy:    &policyBinding{},
		caches:    newRuntimeCaches(),
	}
	h.publishRuntime(g2)

	if got := h.runtimeForContext(pinned); got != g1 {
		t.Fatalf("pinned runtime = %p, want G1 %p", got, g1)
	}
	if got := h.runtimeForContext(context.Background()); got != g2 {
		t.Fatalf("current runtime = %p, want G2 %p", got, g2)
	}
	if got := h.providerSetupForContext(pinned); got != g1.providers {
		t.Fatal("pinned provider setup switched generations")
	}
	if got := h.modelsCacheForContext(pinned); got != g1.caches.models {
		t.Fatal("pinned models cache switched generations")
	}
	if g1.caches.models == g2.caches.models || g1.caches.chatRoutes == g2.caches.chatRoutes {
		t.Fatal("runtime generations unexpectedly share discovery caches")
	}
}

func TestRuntimeRevisionDoesNotExposeSecrets(t *testing.T) {
	secret := "super-secret-dashboard-key"
	revision := runtimeRevisionFromConfig(ProvidersConfig{Providers: []ProviderConfig{{ID: "p", Type: string(providerTypeOpenAICompatible), APIKey: secret}}})
	if !strings.HasPrefix(revision, "cfg_") {
		t.Fatalf("revision = %q", revision)
	}
	if strings.Contains(revision, secret) {
		t.Fatal("runtime revision exposed a secret")
	}
}

func TestPinnedHTTPChatRequestFinishesOnOldRuntime(t *testing.T) {
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	var callsA, callsB atomic.Int32
	response := `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA.Add(1)
		select {
		case <-startedA:
		default:
			close(startedA)
		}
		<-releaseA
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsB.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer serverB.Close()

	providerConfig := func(baseURL string) ProvidersConfig {
		return ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "local",
			Type:           string(providerTypeOpenAICompatible),
			Default:        true,
			BaseURL:        baseURL,
			AuthType:       string(providerAuthTypeNone),
			ModelDiscovery: string(providerModelDiscoveryStatic),
			Models: []ProviderModelConfig{{
				PublicID:   "demo",
				Deployment: "demo-upstream",
				Endpoints:  []string{providerEndpointChatCompletions},
			}},
		}}}
	}

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(providerConfig(serverA.URL)),
	)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"demo","messages":[{"role":"user","content":"hi"}]}`
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq = h.PinRuntimeRequest(firstReq)
	firstWriter := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.HandleOpenAIChatCompletions(firstWriter, firstReq)
	}()
	select {
	case <-startedA:
	case <-time.After(time.Second):
		t.Fatal("G1 request did not reach provider A")
	}

	g2, err := h.buildRuntimeSnapshot(context.Background(), providerConfig(serverB.URL), 2, "cfg_g2", false)
	if err != nil {
		t.Fatal(err)
	}
	h.publishRuntime(g2)
	close(releaseA)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("G1 request did not finish")
	}
	if firstWriter.Code != http.StatusOK || callsA.Load() != 1 || callsB.Load() != 0 {
		t.Fatalf("G1 result status=%d callsA=%d callsB=%d body=%s", firstWriter.Code, callsA.Load(), callsB.Load(), firstWriter.Body.String())
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq = h.PinRuntimeRequest(secondReq)
	secondWriter := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(secondWriter, secondReq)
	if secondWriter.Code != http.StatusOK || callsB.Load() != 1 {
		t.Fatalf("G2 result status=%d callsA=%d callsB=%d body=%s", secondWriter.Code, callsA.Load(), callsB.Load(), secondWriter.Body.String())
	}
}

type runtimeSnapshotTestAzureTokenSource struct{ id int }

func (s *runtimeSnapshotTestAzureTokenSource) AccessToken(context.Context) (string, error) {
	return "token", nil
}

func TestRuntimeCandidateReusesOnlyCompatibleAuthResources(t *testing.T) {
	var created []*runtimeSnapshotTestAzureTokenSource
	factory := func(string, string) (azureTokenSource, error) {
		source := &runtimeSnapshotTestAzureTokenSource{id: len(created) + 1}
		created = append(created, source)
		return source, nil
	}
	config := func(scope string) ProvidersConfig {
		return ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "azure",
			Type:           string(providerTypeAzureOpenAI),
			Default:        true,
			BaseURL:        "https://example.openai.azure.com/openai/v1",
			AuthMode:       string(providerAuthModeAzureIdentity),
			TokenScope:     scope,
			ModelDiscovery: string(providerModelDiscoveryStatic),
			Models: []ProviderModelConfig{{
				PublicID:   "demo",
				Deployment: "deployment",
				Endpoints:  []string{providerEndpointResponses},
			}},
		}}}
	}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(config("scope-a")),
		func(h *ProxyHandler) { h.azureIdentityTokenSourceFactory = factory },
	)
	if err != nil {
		t.Fatal(err)
	}
	initial := h.currentRuntime().providers.providerByID("azure").azureToken
	if len(created) != 1 || initial != created[0] {
		t.Fatalf("initial auth resource = %v, created=%v", initial, created)
	}

	compatible, err := h.buildRuntimeSnapshot(context.Background(), config("scope-a"), 2, "cfg_compatible", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := compatible.providers.providerByID("azure").azureToken; got != initial {
		t.Fatal("compatible candidate did not reuse Azure identity resource")
	}

	incompatible, err := h.buildRuntimeSnapshot(context.Background(), config("scope-b"), 2, "cfg_incompatible", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := incompatible.providers.providerByID("azure").azureToken; got == initial {
		t.Fatal("changed token scope incorrectly reused Azure identity resource")
	}
}
