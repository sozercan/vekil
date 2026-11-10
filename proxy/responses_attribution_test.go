package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const responsesAttributionTestResponse = `{"id":"resp-attribution","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`

func TestResponsesAttributionHTTPValidation(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    http.Header
	}{
		{
			name:    "user",
			headers: http.Header{"X-Initiator": {" user "}, "X-Interaction-Id": {" interaction "}, "X-Client-Session-Id": {" session "}},
			want:    http.Header{"X-Initiator": {"user"}, "X-Interaction-Id": {"interaction"}, "X-Client-Session-Id": {"session"}},
		},
		{
			name:    "agent at byte limit",
			headers: http.Header{"X-Initiator": {"agent"}, "X-Interaction-Id": {strings.Repeat("i", 1024)}, "X-Client-Session-Id": {strings.Repeat("s", 1024)}},
			want:    http.Header{"X-Initiator": {"agent"}, "X-Interaction-Id": {strings.Repeat("i", 1024)}, "X-Client-Session-Id": {strings.Repeat("s", 1024)}},
		},
		{
			name:    "invalid values",
			headers: http.Header{"X-Initiator": {"automatic"}, "X-Interaction-Id": {strings.Repeat("i", 1025)}, "X-Client-Session-Id": {strings.Repeat("s", 1025)}},
		},
		{
			name:    "repeated values",
			headers: http.Header{"X-Initiator": {"user", "agent"}, "X-Interaction-Id": {"one", "two"}, "X-Client-Session-Id": {"one", "two"}},
		},
		{
			name: "case aliases",
			headers: http.Header{
				"X-Initiator": {"user"}, "x-initiator": {"agent"},
				"X-Interaction-Id": {"one"}, "x-interaction-id": {"two"},
				"X-Client-Session-Id": {"one"}, "x-client-session-id": {"two"},
			},
		},
		{
			name:    "control bytes",
			headers: http.Header{"X-Initiator": {"us\ter"}, "X-Interaction-Id": {"one\ttwo"}, "X-Client-Session-Id": {"one\ttwo"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan http.Header, 1)
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, responsesAttributionTestResponse)
			})
			defer h.BeginShutdown()
			for _, endpoint := range responsesAttributionTestEndpoints(h) {
				t.Run(strings.TrimPrefix(endpoint.path, "/v1/"), func(t *testing.T) {
					req := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(`{"model":"gpt-5.4","input":"history","traces":[{"id":"trace"}]}`))
					req.Header = tc.headers.Clone()
					req.Header.Set("X-Client-Request-Id", "request-one")
					req.Header.Set("Authorization", "Bearer client-secret")
					req.Header.Set("Copilot-Integration-Id", "client-integration")
					w := httptest.NewRecorder()
					endpoint.handle(w, req)
					if w.Code != http.StatusOK {
						t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
					}
					got := <-captured
					assertResponsesAttribution(t, got, tc.want)
					if got.Get("Authorization") != "Bearer test-token" || got.Get("Copilot-Integration-Id") == "client-integration" || got.Get("X-Client-Request-Id") != "request-one" {
						t.Fatal("attribution handling changed server credentials or shared request metadata")
					}
				})
			}
		})
	}
}

func TestResponsesAttributionProviderIsolation(t *testing.T) {
	for _, kind := range []providerType{providerTypeCopilot, providerTypeOpenAICompatible, providerTypeAzureOpenAI, providerTypeOpenAICodex} {
		t.Run(string(kind), func(t *testing.T) {
			captured := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				var body struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+responsesAttributionTestResponse+"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, responsesAttributionTestResponse)
				}
			}))
			defer upstream.Close()
			provider := explicitRouteTestProvider("provider", upstream.URL, "provider-token")
			provider.kind = kind
			provider.paths = providerEndpointPolicyFor(kind).defaultEndpointPaths()
			provider.authType = "bearer"
			provider.authHeader = "Authorization"
			provider.authPrefix = "Bearer"
			if kind == providerTypeOpenAICodex {
				provider.codexAuth = &openAICodexAuth{path: writeTestOpenAICodexAuth(t, t.TempDir(), testOpenAICodexTokens(t, time.Now().Add(time.Hour), "account", false, "refresh"))}
			}
			h, _ := explicitRouteTestHandler(t, upstream.Client(), routeModePriorityFailover, 1, 1, provider)
			h.auth = auth.NewTestAuthenticator("test-token")
			h.log = logger.New(logger.LevelError)
			h.responsesWS.DisableAutoCompact = true
			defer h.BeginShutdown()
			headers := http.Header{"X-Initiator": {"agent"}, "X-Interaction-Id": {"interaction"}, "X-Client-Session-Id": {"session"}}
			var want http.Header
			if kind == providerTypeCopilot {
				want = headers
			}
			for _, endpoint := range responsesAttributionTestEndpoints(h) {
				t.Run(strings.TrimPrefix(endpoint.path, "/v1/"), func(t *testing.T) {
					req := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(`{"model":"public-model","input":"history","traces":[{"id":"trace"}]}`))
					req.Header = headers.Clone()
					w := httptest.NewRecorder()
					endpoint.handle(w, req)
					if w.Code != http.StatusOK {
						t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
					}
					assertResponsesAttribution(t, <-captured, want)
				})
			}
			conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), headers)
			defer func() { _ = conn.Close() }()
			create := newResponsesWebSocketCreateRequest(nil)
			create["model"] = "public-model"
			create["headers"] = map[string]string{"X-Initiator": "agent", "X-Interaction-Id": "interaction", "X-Client-Session-Id": "session"}
			if err := conn.WriteJSON(create); err != nil {
				t.Fatal(err)
			}
			if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
				t.Fatalf("response = %#v", frame)
			}
			assertResponsesAttribution(t, <-captured, want)
		})
	}
}

func TestResponsesAttributionWebSocketValidation(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "http_bridge"
		if native {
			name = "native_upstream"
		}
		t.Run(name, func(t *testing.T) {
			captured := make(chan http.Header, 1)
			handshakes := make(chan http.Header, 1)
			var frames atomic.Int32
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if !websocket.IsWebSocketUpgrade(r) {
					captured <- r.Header.Clone()
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+responsesAttributionTestResponse+"}\n\n")
					return
				}
				handshakes <- r.Header.Clone()
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					var create struct {
						Headers map[string]string `json:"headers"`
					}
					if err := conn.ReadJSON(&create); err != nil {
						return
					}
					headers := make(http.Header)
					for name, value := range create.Headers {
						headers.Set(name, value)
					}
					captured <- headers
					frames.Add(1)
					if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": json.RawMessage(responsesAttributionTestResponse)}); err != nil {
						return
					}
				}
			})
			h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: native, DisableAutoCompact: true}
			defer h.BeginShutdown()
			upgradeHeaders := http.Header{"X-Initiator": {"user"}, "X-Interaction-Id": {"upgrade-interaction"}, "X-Client-Session-Id": {"upgrade-session"}}
			conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), upgradeHeaders)
			defer func() { _ = conn.Close() }()
			for _, source := range []string{"headers", "client_metadata"} {
				for _, valid := range []bool{true, false, true} {
					create := newResponsesWebSocketCreateRequest(nil)
					values := map[string]string{"X-Initiator": "agent", "X-Interaction-Id": strings.Repeat("i", 1024), "X-Client-Session-Id": strings.Repeat("s", 1024)}
					want := make(http.Header)
					if valid {
						for name, value := range values {
							want.Set(name, value)
						}
					} else {
						values = map[string]string{"X-Initiator": "automatic", "X-Interaction-Id": strings.Repeat("i", 1025), "X-Client-Session-Id": strings.Repeat("s", 1025)}
					}
					if source == "client_metadata" {
						metadata := make(map[string]string)
						for name, value := range values {
							metadata["ws_request_header_"+name] = value
						}
						create[source] = metadata
					} else {
						create[source] = values
					}
					if err := conn.WriteJSON(create); err != nil {
						t.Fatal(err)
					}
					if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
						t.Fatalf("%s valid=%t response = %#v", source, valid, frame)
					}
					assertResponsesAttribution(t, <-captured, want)
				}
			}
			for _, duplicate := range []string{"agent", ""} {
				create := newResponsesWebSocketCreateRequest(nil)
				create["client_metadata"] = map[string]string{
					"ws_request_header_X-Initiator": "user", "ws_request_header_x-initiator": duplicate,
					"ws_request_header_X-Interaction-Id": "one", "ws_request_header_x-interaction-id": duplicate,
					"ws_request_header_X-Client-Session-Id": "one", "ws_request_header_x-client-session-id": duplicate,
				}
				if err := conn.WriteJSON(create); err != nil {
					t.Fatal(err)
				}
				if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
					t.Fatalf("case-aliased metadata response = %#v", frame)
				}
				assertResponsesAttribution(t, <-captured, nil)
			}
			if native {
				if frames.Load() != 8 {
					t.Fatalf("native creates = %d, want 8", frames.Load())
				}
				got := <-handshakes
				assertResponsesAttribution(t, got, http.Header{"X-Initiator": {"agent"}, "X-Interaction-Id": {strings.Repeat("i", 1024)}, "X-Client-Session-Id": {strings.Repeat("s", 1024)}})
				if got.Get("Authorization") != "Bearer test-token" {
					t.Fatal("native handshake lost server authentication")
				}
			}
		})
	}
}

func responsesAttributionTestEndpoints(h *ProxyHandler) []struct {
	path   string
	handle http.HandlerFunc
} {
	return []struct {
		path   string
		handle http.HandlerFunc
	}{
		{"/v1/responses", h.HandleResponses},
		{"/v1/responses/compact", h.HandleCompact},
		{"/v1/memories/trace_summarize", h.HandleMemorySummarize},
	}
}

func assertResponsesAttribution(t *testing.T, got, want http.Header) {
	t.Helper()
	for _, name := range []string{"X-Initiator", "X-Interaction-Id", "X-Client-Session-Id"} {
		if !reflect.DeepEqual(got.Values(name), want.Values(name)) {
			t.Errorf("%s: got %d values (first length %d), want %d values (first length %d)", name, len(got.Values(name)), len(got.Get(name)), len(want.Values(name)), len(want.Get(name)))
		}
	}
}
