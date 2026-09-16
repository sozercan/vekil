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
)

type durableFixtureTokenSource struct{ token atomic.Value }

func (s *durableFixtureTokenSource) AccessToken(context.Context) (string, error) {
	return s.token.Load().(string), nil
}

func TestDurableResponsesActualCredentialSnapshot(t *testing.T) {
	for _, kind := range []string{"codex", "entra"} {
		t.Run(kind, func(t *testing.T) {
			arrived, release := make(chan string, 1), make(chan struct{})
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					arrived <- r.Header.Get("Authorization")
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, durableWireFixture)
			}))
			defer upstream.Close()
			s, config := newDurableStoreFixture(t, 32)
			provider := explicitRouteTestProvider("primary", upstream.URL, "fixture-key")
			authDir := t.TempDir()
			source := &durableFixtureTokenSource{}
			setCredential := func(principal, refresh string) string {
				claims := map[string]interface{}{"iss": "https://issuer.example.test", "sub": principal, "tid": "tenant-fixture", "oid": principal, "exp": time.Now().Add(time.Hour).Unix(), "jti": refresh}
				token := testOpenAICodexJWT(t, claims)
				if kind == "codex" {
					tokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "account-fixture", false, "unused-refresh")
					tokens.AccessToken = token
					path := writeTestOpenAICodexAuth(t, authDir, tokens)
					if provider.codexAuth == nil {
						provider.codexAuth = &openAICodexAuth{path: path}
					}
				} else {
					source.token.Store(token)
				}
				return token
			}
			if kind == "codex" {
				provider.kind = providerTypeOpenAICodex
			} else {
				provider.authMode = providerAuthModeAzureIdentity
				provider.azureToken = source
				provider.tokenScope = defaultAzureIdentityTokenScope
			}
			oldToken := setCredential("principal-a", "first")
			h, _ := durableWireHandler(t, s, provider)
			h.streamingUpstreamTimeout = 5 * time.Second
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- durableWireRequest(h, `{"model":"public-model","input":"start"}`, false, false) }()
			select {
			case bearer := <-arrived:
				if bearer != "Bearer "+oldToken {
					t.Error("wrong actual request credential")
				}
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("request did not reach fixture")
			}
			// Switch the real credential source while the authenticated request
			// is in flight. Response ownership must remain principal A's snapshot.
			setCredential("principal-b", "replacement")
			close(release)
			select {
			case first := <-result:
				if first.Code != http.StatusOK {
					t.Fatalf("first = %d %s", first.Code, first.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("first request hung")
			}
			continuation := `{"model":"public-model","previous_response_id":"resp-durable-fixture","input":"continue"}`
			if rejected := durableWireRequest(h, continuation, false, false); rejected.Code != http.StatusBadRequest || calls.Load() != 1 {
				t.Fatalf("principal switch accepted or dispatched: %d calls=%d", rejected.Code, calls.Load())
			}
			setCredential("principal-a", "refreshed")
			if refreshed := durableWireRequest(h, continuation, false, false); refreshed.Code != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("stable refresh rejected: %d %s", refreshed.Code, refreshed.Body.String())
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
			h, _ = durableWireHandler(t, reopened, provider)
			setCredential("principal-a", "refreshed-after-restart")
			if refreshed := durableWireRequest(h, continuation, false, false); refreshed.Code != http.StatusOK || calls.Load() != 3 {
				t.Fatalf("restart refresh rejected: %d %s", refreshed.Code, refreshed.Body.String())
			}
		})
	}
}

func TestDurableBearerClaimsRejectAmbiguousIdentity(t *testing.T) {
	for _, token := range []string{"opaque", "Bearer opaque", "Bearer a.b.c", "bearer a.b.c", "Bearer " + testOpenAICodexJWT(t, map[string]interface{}{"sub": 42}), "Bearer " + testOpenAICodexJWT(t, map[string]interface{}{"iss": " "})} {
		if _, err := durableBearerClaims(token); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}

func TestDurableResponsesCopilotSourceAndRefresh(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, durableWireFixture)
	}))
	defer upstream.Close()
	s, config := newDurableStoreFixture(t, 32)
	p := explicitRouteTestProvider("primary", upstream.URL, "")
	p.kind = providerTypeCopilot
	h, _ := durableWireHandler(t, s, p)
	h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_source-a", "first-bearer")
	if first := durableWireRequest(h, `{"model":"public-model","input":"start"}`, false, false); first.Code != 200 {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
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
	h, _ = durableWireHandler(t, reopened, p)
	h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_source-a", "refreshed-bearer")
	continuation := `{"model":"public-model","previous_response_id":"resp-durable-fixture","input":"continue"}`
	if response := durableWireRequest(h, continuation, false, false); response.Code != 200 || calls.Load() != 2 {
		t.Fatalf("refresh = %d %s", response.Code, response.Body.String())
	}
	h.auth = auth.NewTestAuthenticatorWithResponsesToken("ghu_source-b", "replacement-bearer")
	if response := durableWireRequest(h, continuation, false, false); response.Code != 400 || calls.Load() != 2 {
		t.Fatalf("changed source = %d sends=%d", response.Code, calls.Load())
	}
}

func TestDurableNativeChatHeaderHasActualIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Codex-Turn-State", "chat-turn-fixture")
		_, _ = io.WriteString(w, `{"id":"chat-fixture","object":"chat.completion","model":"physical","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	s, _ := newDurableStoreFixture(t, 4)
	h, _ := durableWireHandler(t, s, explicitRouteTestProvider("primary", upstream.URL, "fixture-key"))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"start"}]}`))
	result := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(result, request)
	if result.Code != 200 || result.Header().Get("X-Codex-Turn-State") != "chat-turn-fixture" {
		t.Fatalf("Chat state = %d %s", result.Code, result.Body.String())
	}
	if r := s.lookup(stateBindingTypeTurnState, "chat-turn-fixture"); r.err != nil || r.outcome != stateBindingLookupKnown || r.owner.identity == [32]byte{} {
		t.Fatal("native Chat exposed state without actual identity")
	}
}
