package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestCopilotNativeWebSocketCatalogRecordsAdvertisedModels(t *testing.T) {
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"data":[
			{"id":"gpt-6-sol","supported_endpoints":["/responses","ws:/responses"]},
			{"id":"grok-4.7","supported_endpoints":["/responses"]},
			{"id":"mai-code-2","supported_endpoints":["/chat/completions","/responses"]},
			{"id":"gpt-4o"}
		]}`)
	})
	provider := &providerRuntime{id: "copilot", kind: providerTypeCopilot, baseURL: h.copilotURL, paths: providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths()}
	if _, err := h.fetchProviderModels(context.Background(), provider, "", ""); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{"gpt-6-sol": true, "grok-4.7": false, "mai-code-2": false, "gpt-4o": false, "unlisted": false} {
		advertised, known := h.copilotNativeWS.lookup("copilot", model)
		if !known || advertised != want {
			t.Errorf("%s: advertised=%v known=%v, want advertised=%v", model, advertised, known, want)
		}
	}
	if _, known := h.copilotNativeWS.lookup("other", "gpt-6-sol"); known {
		t.Error("catalog leaked across providers")
	}
}

func TestCopilotNativeWebSocketCatalogPartialFetchMerges(t *testing.T) {
	var c copilotNativeWebSocketCatalog
	c.record("copilot", []providerModel{nativeWebSocketTestModel("a", true), nativeWebSocketTestModel("b", true)}, true)
	c.record("copilot", []providerModel{nativeWebSocketTestModel("b", false)}, false)
	if advertised, _ := c.lookup("copilot", "a"); !advertised {
		t.Error("partial fetch dropped an unlisted model")
	}
	if advertised, _ := c.lookup("copilot", "b"); advertised {
		t.Error("partial fetch did not update a listed model")
	}
	c.record("copilot", []providerModel{nativeWebSocketTestModel("b", true)}, true)
	if advertised, _ := c.lookup("copilot", "a"); advertised {
		t.Error("complete fetch kept a removed model")
	}
}

// nativeGateTestUpstream is a fake Copilot upstream that counts websocket
// upgrades, HTTP /responses posts, and /models fetches.
type nativeGateTestUpstream struct {
	upgrades, posts, catalogs atomic.Int32
	catalog                   string
	catalogStatus             int
	postInputItems            chan int
}

func (u *nativeGateTestUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/models":
			u.catalogs.Add(1)
			if u.catalogStatus != 0 {
				http.Error(w, "catalog unavailable", u.catalogStatus)
				return
			}
			_, _ = io.WriteString(w, u.catalog)
		case websocket.IsWebSocketUpgrade(r):
			conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			u.upgrades.Add(1)
			defer func() { _ = conn.Close() }()
			for turn := 1; ; turn++ {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp-native-%d", turn), "output": []any{}}}); err != nil {
					return
				}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/responses":
			turn := u.posts.Add(1)
			var body struct {
				Input []json.RawMessage `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if u.postInputItems != nil {
				u.postInputItems <- len(body.Input)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http-%d\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n", turn)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}
}

func runNativeGateTurns(t *testing.T, h *ProxyHandler, model string, turns int) {
	t.Helper()
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]any{map[string]string{"role": "user", "content": "first"}})
	request["model"] = model
	for turn := 1; turn <= turns; turn++ {
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] != "response.completed" {
			t.Fatalf("turn %d = %#v", turn, frame)
		}
		request["previous_response_id"] = websocketResponseID(t, frame)
		request["input"] = []any{map[string]string{"role": "user", "content": "next"}}
	}
}

func TestResponsesNativeWebSocketUsesHTTPForUnadvertisedModel(t *testing.T) {
	upstream := &nativeGateTestUpstream{postInputItems: make(chan int, 2)}
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}
	h.copilotNativeWS.record("copilot", []providerModel{nativeWebSocketTestModel("gpt-5.4", false)}, true)

	runNativeGateTurns(t, h, "gpt-5.4", 2)

	if upstream.upgrades.Load() != 0 || upstream.posts.Load() != 2 {
		t.Fatalf("transport: upgrades=%d posts=%d, want HTTP only", upstream.upgrades.Load(), upstream.posts.Load())
	}
	// The HTTP bridge keeps session history, so the second turn replays it.
	if first, second := <-upstream.postInputItems, <-upstream.postInputItems; first != 1 || second != 3 {
		t.Fatalf("HTTP input items = %d then %d, want 1 then 3", first, second)
	}
}

func TestResponsesNativeWebSocketHTTPOnlyModelKeepsHTTPInputLimits(t *testing.T) {
	h := &ProxyHandler{}
	h.copilotNativeWS.record("copilot", []providerModel{nativeWebSocketTestModel("gpt-5.4", false)}, true)
	request, err := parseResponsesWebSocketCreateRequest([]byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Input = make([]json.RawMessage, responsesNativeMaxPendingItems+1)
	session := &responsesWebSocketSession{nativeUpstream: newResponsesNativeUpstream(context.Background())}
	if _, err := session.planRequest(h, request); err != nil {
		t.Fatalf("HTTP-only model received native input limits: %v", err)
	}
}

func TestResponsesNativeWebSocketColdCatalogDiscoversCapability(t *testing.T) {
	for _, test := range []struct {
		name          string
		catalog       string
		catalogStatus int
		wantUpgrades  int32
		wantPosts     int32
	}{
		{name: "advertised", catalog: `{"data":[{"id":"gpt-5.4","supported_endpoints":["/responses","ws:/responses"]}]}`, wantUpgrades: 1},
		{name: "not advertised", catalog: `{"data":[{"id":"gpt-5.4","supported_endpoints":["/responses"]}]}`, wantPosts: 2},
		{name: "catalog unavailable", catalogStatus: http.StatusInternalServerError, wantPosts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &nativeGateTestUpstream{catalog: test.catalog, catalogStatus: test.catalogStatus}
			h := newTestProxyHandler(t, upstream.handler(t))
			h.maxRetries = 1
			h.chatRoutes = newChatRouteDiscoveryCache()
			h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}

			runNativeGateTurns(t, h, "gpt-5.4", 2)

			if upstream.catalogs.Load() == 0 {
				t.Fatal("cold catalog was not fetched")
			}
			if upstream.upgrades.Load() != test.wantUpgrades || upstream.posts.Load() != test.wantPosts {
				t.Fatalf("transport: upgrades=%d posts=%d, want upgrades=%d posts=%d", upstream.upgrades.Load(), upstream.posts.Load(), test.wantUpgrades, test.wantPosts)
			}
		})
	}
}

func TestResponsesNativeWebSocketExplicitRouteUsesTargetUpstreamModel(t *testing.T) {
	for _, test := range []struct {
		name         string
		advertised   string
		wantUpgrades int32
		wantPosts    int32
	}{
		// The public route ID is not the capability key; the target's
		// upstream model is.
		{name: "target advertised", advertised: "deployment-a", wantUpgrades: 1},
		{name: "only public ID advertised", advertised: "public-model", wantPosts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &nativeGateTestUpstream{}
			primary := newTestProxyHandler(t, upstream.handler(t))
			provider := explicitRouteTestProvider("primary", primary.copilotURL, "")
			provider.kind = providerTypeCopilot
			provider.paths = providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths()
			h, _ := explicitRouteTestHandler(t, primary.client, routeModePrimaryOnly, 1, 1, provider)
			h.auth = auth.NewTestAuthenticator("test-token")
			h.log = logger.New(logger.LevelError)
			h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}
			h.copilotNativeWS.record("primary", []providerModel{nativeWebSocketTestModel(test.advertised, true)}, true)

			runNativeGateTurns(t, h, "public-model", 2)

			if upstream.upgrades.Load() != test.wantUpgrades || upstream.posts.Load() != test.wantPosts {
				t.Fatalf("transport: upgrades=%d posts=%d, want upgrades=%d posts=%d", upstream.upgrades.Load(), upstream.posts.Load(), test.wantUpgrades, test.wantPosts)
			}
		})
	}
}

func TestResponsesNativeWebSocketHTTPStartedSessionStaysHTTP(t *testing.T) {
	upstream := &nativeGateTestUpstream{postInputItems: make(chan int, 3)}
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true, TurnStateDelta: true}
	h.copilotNativeWS.record("copilot", []providerModel{nativeWebSocketTestModel("gpt-5.4", false)}, true)

	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]any{map[string]string{"role": "user", "content": "first"}})
	request["model"] = "gpt-5.4"
	for turn := 1; turn <= 3; turn++ {
		if turn == 2 {
			// A later catalog refresh advertises the model mid-session.
			h.copilotNativeWS.record("copilot", []providerModel{nativeWebSocketTestModel("gpt-5.4", true)}, true)
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] != "response.completed" {
			t.Fatalf("turn %d = %#v", turn, frame)
		}
		request["previous_response_id"] = websocketResponseID(t, frame)
		request["input"] = []any{map[string]string{"role": "user", "content": "next"}}
	}

	if upstream.upgrades.Load() != 0 || upstream.posts.Load() != 3 {
		t.Fatalf("transport: upgrades=%d posts=%d, want HTTP only", upstream.upgrades.Load(), upstream.posts.Load())
	}
	// Without an upstream turn-state header the HTTP bridge replays the full
	// proxy-held history on every turn.
	for i, want := range []int{1, 3, 5} {
		if got := <-upstream.postInputItems; got != want {
			t.Fatalf("HTTP turn %d input items = %d, want %d", i+1, got, want)
		}
	}
}

func TestResponsesNativeWebSocketHTTPStartedSessionKeepsHTTPInputLimits(t *testing.T) {
	h := &ProxyHandler{}
	h.copilotNativeWS.record("copilot", []providerModel{nativeWebSocketTestModel("gpt-5.4", true)}, true)
	request, err := parseResponsesWebSocketCreateRequest([]byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Input = make([]json.RawMessage, responsesNativeMaxPendingItems+1)
	upstream := newResponsesNativeUpstream(context.Background())
	session := &responsesWebSocketSession{nativeUpstream: upstream}
	if _, err := session.planRequest(h, request); err == nil {
		t.Fatal("advertised model skipped native input limits")
	}
	upstream.keepHTTP()
	if _, err := session.planRequest(h, request); err != nil {
		t.Fatalf("HTTP-started session received native input limits: %v", err)
	}
}
