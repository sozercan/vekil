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
	nativeFrames              chan nativeGateFrame
	// turnState, when set, is returned as X-Codex-Turn-State on HTTP turns.
	turnState string
}

// nativeGateFrame is what one native response.create carried.
type nativeGateFrame struct {
	inputItems int
	previousID string
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
				var create struct {
					Input              []json.RawMessage `json:"input"`
					PreviousResponseID string            `json:"previous_response_id"`
				}
				if err := conn.ReadJSON(&create); err != nil {
					return
				}
				if u.nativeFrames != nil {
					u.nativeFrames <- nativeGateFrame{len(create.Input), create.PreviousResponseID}
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
			if u.turnState != "" {
				w.Header().Set("X-Codex-Turn-State", u.turnState)
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

func TestResponsesNativeWebSocketHTTPStartedSessionSwitchesWithFullHistory(t *testing.T) {
	upstream := &nativeGateTestUpstream{postInputItems: make(chan int, 1), nativeFrames: make(chan nativeGateFrame, 2)}
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

	if upstream.posts.Load() != 1 || upstream.upgrades.Load() != 1 {
		t.Fatalf("transport: posts=%d upgrades=%d, want one HTTP turn then one native connection", upstream.posts.Load(), upstream.upgrades.Load())
	}
	if got := <-upstream.postInputItems; got != 1 {
		t.Fatalf("HTTP turn input items = %d, want 1", got)
	}
	// The first native turn replays the proxy's history (user, assistant,
	// user); the next continues the connection with only the new input.
	if got := <-upstream.nativeFrames; got != (nativeGateFrame{inputItems: 3}) {
		t.Fatalf("first native turn = %+v, want full history without a previous response", got)
	}
	if got := <-upstream.nativeFrames; got != (nativeGateFrame{inputItems: 1, previousID: "resp-native-1"}) {
		t.Fatalf("second native turn = %+v, want a one-item continuation", got)
	}
}

func TestResponsesNativeWebSocketOversizedFullHistoryUsesHTTP(t *testing.T) {
	upstream := &nativeGateTestUpstream{}
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}
	advertiseNativeWebSocketForTest(h)

	input := make([]any, responsesNativeMaxPendingItems+1)
	for i := range input {
		input[i] = map[string]string{"role": "user", "content": "x"}
	}
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(input)); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
		t.Fatalf("oversized turn = %#v", frame)
	}
	if upstream.upgrades.Load() != 0 || upstream.posts.Load() != 1 {
		t.Fatalf("transport: upgrades=%d posts=%d, want HTTP for history beyond native limits", upstream.upgrades.Load(), upstream.posts.Load())
	}
}

func TestResponsesNativeWebSocketTurnStateDeltaStaysOnHTTP(t *testing.T) {
	upstream := &nativeGateTestUpstream{postInputItems: make(chan int, 2), turnState: "opaque-turn-state"}
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true, TurnStateDelta: true}
	h.copilotNativeWS.record("copilot", []providerModel{nativeWebSocketTestModel("gpt-5.4", false)}, true)

	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]any{map[string]string{"role": "user", "content": "first"}})
	request["model"] = "gpt-5.4"
	for turn := 1; turn <= 2; turn++ {
		if turn == 2 {
			// The model becomes native-eligible while the session holds HTTP
			// turn state.
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
	// The second body is a one-item turn-state delta: it must stay on HTTP,
	// where the turn state carries the earlier context.
	if upstream.upgrades.Load() != 0 || upstream.posts.Load() != 2 {
		t.Fatalf("transport: upgrades=%d posts=%d, want the turn-state delta on HTTP", upstream.upgrades.Load(), upstream.posts.Load())
	}
	if first, second := <-upstream.postInputItems, <-upstream.postInputItems; first != 1 || second != 1 {
		t.Fatalf("HTTP input items = %d then %d, want a turn-state delta", first, second)
	}
}
