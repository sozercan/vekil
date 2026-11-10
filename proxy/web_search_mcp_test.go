package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

// fakeGitHubMCP is a minimal Streamable HTTP MCP server that answers over SSE
// and enforces the session and header contract Vekil relies on.
type fakeGitHubMCP struct {
	t          *testing.T
	tools      []string
	initStatus int
	callResult string
	// callStarted, when set, is closed when tools/call arrives; the call then
	// stalls until its request is canceled.
	callStarted chan struct{}

	mu       sync.Mutex
	methods  []string
	callArgs json.RawMessage
}

func (f *fakeGitHubMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp/" {
		http.NotFound(w, r)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		f.t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("X-MCP-Tools"); got != "web_search" {
		f.t.Errorf("X-MCP-Tools = %q, want web_search", got)
	}
	if got := r.Header.Get("X-MCP-Readonly"); got != "true" {
		f.t.Errorf("X-MCP-Readonly = %q, want true", got)
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
		f.t.Errorf("decode upstream request: %v", err)
	}
	f.mu.Lock()
	f.methods = append(f.methods, message.Method)
	f.mu.Unlock()

	if message.Method != "initialize" {
		if got := r.Header.Get("Mcp-Session-Id"); got != "session-1" {
			f.t.Errorf("%s Mcp-Session-Id = %q, want session-1", message.Method, got)
		}
		if got := r.Header.Get("Mcp-Protocol-Version"); got != "2025-11-25" {
			f.t.Errorf("%s Mcp-Protocol-Version = %q", message.Method, got)
		}
	}

	var result string
	switch message.Method {
	case "initialize":
		if f.initStatus != 0 {
			w.WriteHeader(f.initStatus)
			return
		}
		w.Header().Set("Mcp-Session-Id", "session-1")
		result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"github"}}`
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		tools := make([]string, 0, len(f.tools))
		for _, name := range f.tools {
			tools = append(tools, fmt.Sprintf(`{"name":%q,"inputSchema":{"type":"object"}}`, name))
		}
		result = `{"tools":[` + strings.Join(tools, ",") + `]}`
	case "tools/call":
		f.mu.Lock()
		f.callArgs = message.Params.Arguments
		f.mu.Unlock()
		if message.Params.Name != "web_search" {
			f.t.Errorf("tools/call name = %q", message.Params.Name)
		}
		if f.callStarted != nil {
			close(f.callStarted)
			<-r.Context().Done()
			return
		}
		result = f.callResult
	default:
		f.t.Errorf("unexpected upstream method %q", message.Method)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// A notification ahead of the response must be skipped.
	_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n\n", message.ID, result)
}

func newWebSearchMCPTestHandler(t *testing.T, upstream *fakeGitHubMCP) *ProxyHandler {
	t.Helper()
	upstream.t = t
	return newTestProxyHandler(t, upstream.ServeHTTP)
}

func postWebSearchMCP(t *testing.T, h *ProxyHandler, body string) (*httptest.ResponseRecorder, mcpMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp/web-search", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleWebSearchMCP(rec, req)
	var message mcpMessage
	if rec.Code != http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, message
}

func TestWebSearchMCPInitializeNegotiatesProtocolVersion(t *testing.T) {
	h := newWebSearchMCPTestHandler(t, &fakeGitHubMCP{})
	for requested, want := range map[string]string{
		"2025-06-18": "2025-06-18",
		"2099-01-01": webSearchMCPLatestProtocolVersion,
	} {
		_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+requested+`"}}`)
		var result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			t.Fatalf("decode initialize result: %v", err)
		}
		if result.ProtocolVersion != want || result.Capabilities.Tools == nil {
			t.Fatalf("initialize(%s) = %s, want protocol %s with tools capability", requested, response.Result, want)
		}
	}
}

func TestWebSearchMCPAcceptsNotifications(t *testing.T) {
	upstream := &fakeGitHubMCP{}
	h := newWebSearchMCPTestHandler(t, upstream)
	rec, _ := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(upstream.methods) != 0 {
		t.Fatalf("notification reached upstream: %v", upstream.methods)
	}
}

func TestWebSearchMCPToolsListFollowsUpstreamAvailability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream []string
		want     int
	}{
		{name: "offered", upstream: []string{"web_search"}, want: 1},
		{name: "withheld", upstream: nil, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &fakeGitHubMCP{tools: tc.upstream}
			h := newWebSearchMCPTestHandler(t, upstream)
			_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			var result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatalf("decode tools/list result %s: %v", response.Result, err)
			}
			if len(result.Tools) != tc.want || (tc.want == 1 && result.Tools[0].Name != "web_search") {
				t.Fatalf("tools = %+v, want %d web_search tool(s)", result.Tools, tc.want)
			}
			if got := strings.Join(upstream.methods, ","); got != "initialize,notifications/initialized,tools/list" {
				t.Fatalf("upstream methods = %s", got)
			}
		})
	}
}

func TestWebSearchMCPToolsCallForwardsOnlyTheQuery(t *testing.T) {
	upstreamResult := `{"content":[{"type":"text","text":"Go 1.27.2 [go.dev]"}]}`
	upstream := &fakeGitHubMCP{callResult: upstreamResult}
	h := newWebSearchMCPTestHandler(t, upstream)
	_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"web_search","arguments":{"query":"  latest Go release  ","owner":"github","repo":"private"}}}`)
	if response.Error != nil || string(response.ID) != `"call-1"` {
		t.Fatalf("response = %+v", response)
	}
	if string(response.Result) != upstreamResult {
		t.Fatalf("result = %s, want upstream result %s", response.Result, upstreamResult)
	}
	if string(upstream.callArgs) != `{"query":"latest Go release"}` {
		t.Fatalf("upstream arguments = %s, want only the trimmed query", upstream.callArgs)
	}
}

func TestWebSearchMCPToolsCallErrors(t *testing.T) {
	t.Run("unknown tool is a protocol error", func(t *testing.T) {
		upstream := &fakeGitHubMCP{}
		h := newWebSearchMCPTestHandler(t, upstream)
		_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_file_contents","arguments":{}}}`)
		if response.Error == nil || response.Error.Code != jsonRPCInvalidParams {
			t.Fatalf("error = %+v, want invalid params", response.Error)
		}
		if len(upstream.methods) != 0 {
			t.Fatalf("unknown tool reached upstream: %v", upstream.methods)
		}
	})
	t.Run("upstream rejection is a tool error", func(t *testing.T) {
		h := newWebSearchMCPTestHandler(t, &fakeGitHubMCP{initStatus: http.StatusUnauthorized})
		_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
		var result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			t.Fatalf("decode result %s: %v", response.Result, err)
		}
		if !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "HTTP 401") {
			t.Fatalf("result = %s, want isError with the upstream status", response.Result)
		}
		if strings.Contains(string(response.Result), "test-token") {
			t.Fatal("tool error exposed the GitHub credential")
		}
	})
	t.Run("missing query is a tool error", func(t *testing.T) {
		upstream := &fakeGitHubMCP{}
		h := newWebSearchMCPTestHandler(t, upstream)
		_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"web_search","arguments":{"query":" "}}}`)
		if !strings.Contains(string(response.Result), `"isError":true`) || len(upstream.methods) != 0 {
			t.Fatalf("result = %s, upstream methods = %v", response.Result, upstream.methods)
		}
	})
}

func TestWebSearchMCPShutdownCancelsUpstreamSearch(t *testing.T) {
	upstream := &fakeGitHubMCP{callStarted: make(chan struct{})}
	h := newWebSearchMCPTestHandler(t, upstream)
	done := make(chan mcpMessage, 1)
	go func() {
		_, response := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
		done <- response
	}()
	select {
	case <-upstream.callStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream tools/call never started")
	}
	h.BeginShutdown()
	select {
	case response := <-done:
		if !strings.Contains(string(response.Result), `"isError":true`) {
			t.Fatalf("result = %s, want a tool error after shutdown", response.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the in-flight search")
	}
}

func TestMessagesAliasResolvesToCopilotCatalogModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == providerEndpointModels {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-opus-5.5","supported_endpoints":["/v1/messages","/chat/completions"]},{"id":"claude-sonnet-5.5","supported_endpoints":["/v1/messages"]}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(upstream.URL), WithAllowedModels("claude-opus-5.5"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	ctx := context.Background()
	if !h.MessagesAliasResolvesTo(ctx, "claude-opus-5-5", "claude-opus-5.5") {
		t.Fatal("hyphenated alias of a discovered Copilot model was not confirmed")
	}
	if h.MessagesAliasResolvesTo(ctx, "claude-sonnet-5-5", "claude-opus-5.5") {
		t.Fatal("alias of a different model was confirmed")
	}
}

func TestMessagesAliasResolvesToRejectsRawOwnerOfAliasSpelling(t *testing.T) {
	newHandler := func(t *testing.T, scoped bool) *ProxyHandler {
		t.Helper()
		options := []Option{WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID: "normalized", Type: "anthropic-compatible", Default: true, BaseURL: "https://normalized.example.test", AuthType: "none",
				Models: []ProviderModelConfig{
					{PublicID: "claude-sonnet-4.5", Deployment: "normalized-sonnet", Endpoints: []string{"/v1/messages"}},
					{PublicID: "claude-haiku-4.5", Deployment: "normalized-haiku", Endpoints: []string{"/v1/messages"}},
				},
			},
			{
				ID: "raw", Type: "anthropic-compatible", BaseURL: "https://raw.example.test", AuthType: "none",
				Models: []ProviderModelConfig{{PublicID: "claude-sonnet-4-5", Deployment: "raw-sonnet", Endpoints: []string{"/v1/messages"}}},
			},
		}})}
		if scoped {
			options = append(options, WithAllowedModels("claude-sonnet-4.5", "claude-haiku-4.5"))
		}
		h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), options...)
		if err != nil {
			t.Fatalf("NewProxyHandler() error = %v", err)
		}
		t.Cleanup(h.BeginShutdown)
		return h
	}
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped=%v", scoped), func(t *testing.T) {
			h := newHandler(t, scoped)
			ctx := context.Background()
			if h.MessagesAliasResolvesTo(ctx, "claude-sonnet-4-5", "claude-sonnet-4.5") {
				t.Fatal("alias owned by a different raw model was confirmed")
			}
			if !h.MessagesAliasResolvesTo(ctx, "claude-haiku-4-5", "claude-haiku-4.5") {
				t.Fatal("unowned alias of a configured model was not confirmed")
			}
		})
	}
}

func TestWebSearchMCPRejectsBrowserOrigins(t *testing.T) {
	upstream := &fakeGitHubMCP{tools: []string{"web_search"}}
	h := newWebSearchMCPTestHandler(t, upstream)
	req := httptest.NewRequest(http.MethodPost, "/mcp/web-search", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Origin", "http://localhost:1337")
	rec := httptest.NewRecorder()
	h.HandleWebSearchMCP(rec, req)
	if rec.Code != http.StatusForbidden || len(upstream.methods) != 0 {
		t.Fatalf("status = %d, upstream methods = %v; want 403 before any upstream call", rec.Code, upstream.methods)
	}
}

func TestWebSearchMCPRequiresCopilotProvider(t *testing.T) {
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "generic",
			Type:     "openai-compatible",
			Default:  true,
			BaseURL:  "https://provider.example.test/v1",
			AuthType: "bearer",
			APIKey:   "fixture",
			Models:   []ProviderModelConfig{{PublicID: "public-model", Endpoints: []string{providerEndpointChatCompletions}}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	rec, _ := postWebSearchMCP(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 without a Copilot provider", rec.Code)
	}
}
