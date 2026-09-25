package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

// LocalAI v4.10.0 overflow bodies, captured from a live AIKit container.
const (
	localAIChatOverflowBody      = `{"error":{"code":500,"message":"rpc error: code = Internal desc = request (12012 tokens) exceeds the available context size (8192 tokens), try increasing it","type":""}}`
	localAIResponsesOverflowBody = `{"error":{"message":"model inference failed: rpc error: code = Internal desc = request (12010 tokens) exceeds the available context size (8192 tokens), try increasing it","type":"model_error"}}`
	localAIChatOverflowStream    = "data: {\"object\":\"chat.completion.chunk\",\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":null}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"content\":\"request (12012 tokens) exceeds the available context size (8192 tokens), try increasing it\"}}]}\n\n" +
		"data: {\"error\":{\"code\":\"server_error\",\"message\":\"rpc error: code = Internal desc = request (12012 tokens) exceeds the available context size (8192 tokens), try increasing it\",\"type\":\"server_error\"}}\n\n" +
		"data: [DONE]\n\n"
	localAIResponsesOverflowStream = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":{\"id\":\"r\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"status\":\"in_progress\",\"content\":[]}}\n\n" +
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"request (12010 tokens) exceeds the available context size (8192 tokens), try increasing it\"}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"model_error\",\"message\":\"model inference failed: rpc error: code = Internal desc = request (12010 tokens) exceeds the available context size (8192 tokens), try increasing it\"}}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\"}}\n\n" +
		"data: [DONE]\n\n"
	normalChatStream = "data: {\"object\":\"chat.completion.chunk\",\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":null}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n" +
		"data: [DONE]\n\n"
)

func TestParseUpstreamContextOverflow(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		ok      bool
		prompt  int64
		context int64
	}{
		{name: "localai chat 500 numeric code", status: 500, body: localAIChatOverflowBody, ok: true, prompt: 12012, context: 8192},
		{name: "localai responses 500", status: 500, body: localAIResponsesOverflowBody, ok: true, prompt: 12010, context: 8192},
		{name: "llama-server 400", status: 400, body: `{"error":{"code":400,"message":"request (9 tokens) exceeds the available context size (8 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":9,"n_ctx":8}}`, ok: true, prompt: 9, context: 8},
		{name: "llama-server type only", status: 400, body: `{"error":{"code":400,"message":"too big","type":"exceed_context_size_error","n_prompt_tokens":90,"n_ctx":80}}`, ok: true, prompt: 90, context: 80},
		{name: "openai code", status: 400, body: `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, ok: true, prompt: 130000, context: 128000},
		{name: "vllm top-level", status: 400, body: `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens in the messages.","type":"BadRequestError","code":400}`, ok: true, prompt: 40000, context: 32768},
		{name: "unrelated 400", status: 400, body: `{"error":{"message":"invalid tool schema","type":"invalid_request_error","code":400}}`},
		{name: "unrelated 500", status: 500, body: `{"error":{"message":"backend crashed","type":""}}`},
		{name: "deadline is not overflow", status: 500, body: `{"error":{"message":"context deadline exceeded while reading 4096 size body","type":""}}`},
		{name: "generic window wording", status: 400, body: `{"error":{"message":"Input exceeds the context window of this model"}}`, ok: true},
		{name: "overflow status out of range", status: 503, body: localAIChatOverflowBody},
		{name: "plain text", status: 400, body: `request (5 tokens) exceeds the available context size (4 tokens)`, ok: true, prompt: 5, context: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overflow, ok := parseUpstreamContextOverflow(tt.status, []byte(tt.body))
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && (overflow.promptTokens != tt.prompt || overflow.contextTokens != tt.context) {
				t.Fatalf("overflow = %+v", overflow)
			}
		})
	}
}

func TestContextOverflowMessages(t *testing.T) {
	overflow := contextOverflow{promptTokens: 12012, contextTokens: 8192}
	if got := overflow.anthropicMessage(); got != "prompt is too long: 12012 tokens > 8192 maximum" {
		t.Fatalf("anthropic message = %q", got)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(overflow.openAIErrorBody("messages"), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "context_length_exceeded" || body.Error.Type != "invalid_request_error" || body.Error.Param != "messages" ||
		!strings.Contains(body.Error.Message, "maximum context length is 8192 tokens") || !strings.Contains(body.Error.Message, "resulted in 12012 tokens") {
		t.Fatalf("openai body = %+v", body.Error)
	}
	// The synthesized OpenAI body must parse back as the same overflow.
	again, ok := parseUpstreamContextOverflow(http.StatusBadRequest, overflow.openAIErrorBody("input"))
	if !ok || again.promptTokens != 12012 || again.contextTokens != 8192 {
		t.Fatalf("round trip = %+v, %v", again, ok)
	}
}

func dialectRequest(endpoint string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://localai"+endpoint, nil)
	return withProviderUpstreamDialect(req, &providerRuntime{dialect: providerUpstreamDialectLocalAI}, endpoint, nil)
}

func upstreamResponse(status int, contentType string, body io.Reader) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", contentType)
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(body)}
}

func assertSyntheticOverflow(t *testing.T, resp *http.Response, param string) {
	t.Helper()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	overflow, ok := parseUpstreamContextOverflow(resp.StatusCode, body)
	if !ok || overflow.contextTokens != 8192 || !strings.Contains(string(body), `"param":"`+param+`"`) || !strings.Contains(string(body), "context_length_exceeded") {
		t.Fatalf("body = %s", body)
	}
}

func TestNormalizeLocalAIErrorResponses(t *testing.T) {
	resp := normalizeUpstreamDialectResponse(dialectRequest(providerEndpointChatCompletions), upstreamResponse(500, "application/json", strings.NewReader(localAIChatOverflowBody)))
	assertSyntheticOverflow(t, resp, "messages")

	resp = normalizeUpstreamDialectResponse(dialectRequest(providerEndpointResponses), upstreamResponse(500, "application/json", strings.NewReader(localAIResponsesOverflowBody)))
	assertSyntheticOverflow(t, resp, "input")

	other := `{"error":{"message":"backend crashed"}}`
	resp = normalizeUpstreamDialectResponse(dialectRequest(providerEndpointChatCompletions), upstreamResponse(500, "application/json", strings.NewReader(other)))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || string(body) != other {
		t.Fatalf("unrelated error changed: %d %s", resp.StatusCode, body)
	}
}

func TestNormalizeLocalAIStreamedOverflowAfterSlowTokenization(t *testing.T) {
	events := strings.SplitAfterN(localAIResponsesOverflowStream, "\n\n", 3)
	reader := &slowReader{first: []byte(events[0] + events[1]), rest: []byte(events[2]), delay: 3 * time.Second}
	resp := normalizeUpstreamDialectResponse(dialectRequest(providerEndpointResponses), upstreamResponse(200, "text/event-stream", reader))
	assertSyntheticOverflow(t, resp, "input")
}

func TestNormalizeLocalAIStreamedOverflowAfterLargeEchoEvents(t *testing.T) {
	// LocalAI echoes every tool schema in response.created and in_progress.
	echo := `{"type":"function","name":"tool","description":"` + strings.Repeat("x", 200) + `","parameters":{"type":"object"}}`
	tools := "[" + strings.TrimSuffix(strings.Repeat(echo+",", 2000), ",") + "]"
	stream := strings.Replace(localAIResponsesOverflowStream, `"output":[]}`, `"output":[],"tools":`+tools+`}`, 1)
	stream = strings.Replace(stream, `"status":"in_progress"}}`, `"status":"in_progress","tools":`+tools+`}}`, 1)
	if len(stream) < 800<<10 {
		t.Fatalf("fixture is only %d bytes", len(stream))
	}
	resp := normalizeUpstreamDialectResponse(dialectRequest(providerEndpointResponses), upstreamResponse(200, "text/event-stream", strings.NewReader(stream)))
	assertSyntheticOverflow(t, resp, "input")
}

func TestNormalizeLocalAIStreamedOverflow(t *testing.T) {
	resp := normalizeUpstreamDialectResponse(dialectRequest(providerEndpointChatCompletions), upstreamResponse(200, "text/event-stream", strings.NewReader(localAIChatOverflowStream)))
	assertSyntheticOverflow(t, resp, "messages")

	resp = normalizeUpstreamDialectResponse(dialectRequest(providerEndpointResponses), upstreamResponse(200, "text/event-stream", strings.NewReader(localAIResponsesOverflowStream)))
	assertSyntheticOverflow(t, resp, "input")
}

func TestNormalizeLocalAIStreamPassesNormalStreamsUnchanged(t *testing.T) {
	resp := normalizeUpstreamDialectResponse(dialectRequest(providerEndpointChatCompletions), upstreamResponse(200, "text/event-stream", strings.NewReader(normalChatStream)))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != normalChatStream {
		t.Fatalf("normal stream changed: %d %q", resp.StatusCode, body)
	}

	// A model that talks about context sizes is not an overflow.
	chatty := strings.Replace(normalChatStream, `"Hello"`, `"The request (12 tokens) exceeds the available context size (8 tokens), try increasing it. That error means..."`, 1)
	resp = normalizeUpstreamDialectResponse(dialectRequest(providerEndpointChatCompletions), upstreamResponse(200, "text/event-stream", strings.NewReader(chatty)))
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != chatty {
		t.Fatalf("chatty stream changed: %d", resp.StatusCode)
	}
}

type slowReader struct {
	first []byte
	rest  []byte
	delay time.Duration
	step  int
}

func (s *slowReader) Read(p []byte) (int, error) {
	switch s.step {
	case 0:
		s.step++
		return copy(p, s.first), nil
	case 1:
		s.step++
		time.Sleep(s.delay)
		return copy(p, s.rest), nil
	default:
		return 0, io.EOF
	}
}

func TestNormalizeLocalAIStreamStopsPeekingAtTimeout(t *testing.T) {
	previous := localAIOverflowPeekTimeout
	localAIOverflowPeekTimeout = 300 * time.Millisecond
	t.Cleanup(func() { localAIOverflowPeekTimeout = previous })
	role := normalChatStream[:strings.Index(normalChatStream, "\n\n")+2]
	rest := normalChatStream[len(role):]
	started := time.Now()
	resp := normalizeUpstreamDialectResponse(dialectRequest(providerEndpointChatCompletions), upstreamResponse(200, "text/event-stream", &slowReader{first: []byte(role), rest: []byte(rest), delay: localAIOverflowPeekTimeout + 500*time.Millisecond}))
	if elapsed := time.Since(started); elapsed > localAIOverflowPeekTimeout+400*time.Millisecond {
		t.Fatalf("peek held the stream for %s", elapsed)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != normalChatStream {
		t.Fatalf("stream after timeout = %q", body)
	}
}

func TestNormalizeUpstreamDialectIgnoresOtherProviders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://upstream/chat/completions", nil)
	resp := normalizeUpstreamDialectResponse(req, upstreamResponse(500, "application/json", strings.NewReader(localAIChatOverflowBody)))
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestConfiguredProviderUpstreamDialect(t *testing.T) {
	if dialect, err := configuredProviderUpstreamDialect(providerTypeOpenAICompatible, "localai"); err != nil || dialect != providerUpstreamDialectLocalAI {
		t.Fatalf("localai = %q, %v", dialect, err)
	}
	if _, err := configuredProviderUpstreamDialect(providerTypeAzureOpenAI, "localai"); err == nil {
		t.Fatal("azure accepted the localai dialect")
	}
	if _, err := configuredProviderUpstreamDialect(providerTypeOpenAICompatible, "ollama"); err == nil {
		t.Fatal("unknown dialect accepted")
	}
}

func TestValidateFunctionToolsOnlyResponsesRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "function tools and known items", body: `{"input":[{"role":"user","content":"hi"},{"type":"message","role":"assistant","content":[]},{"type":"reasoning","summary":[]},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}],"tools":[{"type":"function","name":"f"}],"tool_choice":"auto"}`},
		{name: "string input", body: `{"input":"hello"}`},
		{name: "custom tool", body: `{"tools":[{"type":"function","name":"f"},{"type":"custom","name":"apply_patch"}]}`, code: "unsupported_tool_type"},
		{name: "web search", body: `{"tools":[{"type":"web_search"}]}`, code: "unsupported_tool_type"},
		{name: "namespace of functions", body: `{"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"x"}]}]}`},
		{name: "hosted tool choice", body: `{"tool_choice":{"type":"web_search"}}`, code: "unsupported_tool_type"},
		{name: "function tool choice", body: `{"tool_choice":{"type":"function","name":"f"}}`},
		{name: "allowed function tools", body: `{"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}}`},
		{name: "allowed hosted tool", body: `{"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}}`, code: "unsupported_tool_type"},
		{name: "custom tool call item", body: `{"input":[{"type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"x"}]}`, code: "unsupported_input_item"},
		{name: "web search call item", body: `{"input":[{"type":"web_search_call","id":"w"}]}`, code: "unsupported_input_item"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFunctionToolsOnlyResponsesRequest([]byte(tt.body), "local")
			if tt.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || providerRequestErrorCode(err) != tt.code {
				t.Fatalf("error = %v (code %q), want code %q", err, providerRequestErrorCode(err), tt.code)
			}
		})
	}
}

// fakeLocalAI emulates LocalAI v4.10.0 responses for the handler tests.
func fakeLocalAI(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if seen != nil {
			*seen = append(*seen, r.URL.Path)
		}
		overflow := bytes.Contains(body, []byte("OVERFLOW"))
		stream := bytes.Contains(body, []byte(`"stream":true`))
		switch r.URL.Path {
		case "/v1/chat/completions":
			switch {
			case overflow && stream:
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, localAIChatOverflowStream)
			case overflow:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, localAIChatOverflowBody)
			case stream:
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, normalChatStream)
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
			}
		case "/v1/responses":
			if overflow {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, localAIResponsesOverflowBody)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"r","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newLocalAIDialectHandler(t *testing.T, baseURL string) *ProxyHandler {
	t.Helper()
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:              "aikit",
				Type:            "openai-compatible",
				Default:         true,
				BaseURL:         baseURL + "/v1",
				AuthType:        "none",
				UpstreamDialect: "localai",
				Models: []ProviderModelConfig{{
					PublicID:  "qwen-local",
					Endpoints: AIKitModelEndpoints(),
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}
	return h
}

func TestLocalAIDialectAnthropicOverflowBecomesPromptTooLong(t *testing.T) {
	upstream := fakeLocalAI(t, nil)
	h := newLocalAIDialectHandler(t, upstream.URL)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{
			"model":      "qwen-local",
			"max_tokens": 16,
			"stream":     stream,
			"messages":   []map[string]any{{"role": "user", "content": "OVERFLOW " + strings.Repeat("word ", 10)}},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleAnthropicMessages(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("stream=%v status = %d body = %s", stream, w.Code, w.Body.String())
		}
		var payload struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v: %s", err, w.Body.String())
		}
		if payload.Error.Type != "invalid_request_error" || payload.Error.Message != "prompt is too long: 12012 tokens > 8192 maximum" {
			t.Fatalf("stream=%v error = %+v", stream, payload.Error)
		}
	}
}

func TestLocalAIDialectChatOverflowIsContextLengthExceeded(t *testing.T) {
	upstream := fakeLocalAI(t, nil)
	h := newLocalAIDialectHandler(t, upstream.URL)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{
			"model":    "qwen-local",
			"stream":   stream,
			"messages": []map[string]any{{"role": "user", "content": "OVERFLOW"}},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		w := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"context_length_exceeded"`) {
			t.Fatalf("stream=%v: %d %s", stream, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "data:") {
			t.Fatalf("stream=%v leaked SSE to the client: %s", stream, w.Body.String())
		}
	}
	// A normal streamed reply is untouched.
	body := `{"model":"qwen-local","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Hello") {
		t.Fatalf("normal stream: %d %s", w.Code, w.Body.String())
	}
}

func TestLocalAIDialectResponsesRejectsDroppedToolsAndMapsOverflow(t *testing.T) {
	var seen []string
	upstream := fakeLocalAI(t, &seen)
	h := newLocalAIDialectHandler(t, upstream.URL)

	custom := `{"model":"qwen-local","store":false,"input":"hi","tools":[{"type":"custom","name":"apply_patch"}]}`
	w := httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(custom)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "only function tools") {
		t.Fatalf("custom tool: %d %s", w.Code, w.Body.String())
	}
	if len(seen) != 0 {
		t.Fatalf("rejected request reached LocalAI: %v", seen)
	}

	overflow := `{"model":"qwen-local","store":false,"input":"OVERFLOW"}`
	w = httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(overflow)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"context_length_exceeded"`) || !strings.Contains(w.Body.String(), `"param":"input"`) {
		t.Fatalf("overflow: %d %s", w.Code, w.Body.String())
	}

	ok := `{"model":"qwen-local","store":false,"input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	w = httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(ok)))
	if w.Code != http.StatusOK {
		t.Fatalf("function tool request: %d %s", w.Code, w.Body.String())
	}
}

func TestExplicitRouteFailsOverOnContextOverflow(t *testing.T) {
	local := fakeLocalAI(t, nil)
	var cloudHits int
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudHits++
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, strings.Replace(normalChatStream, `"Hello"`, `"from cloud"`, 1))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"2","object":"chat.completion","model":"big","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"from cloud"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(cloud.Close)

	build := func(failover bool) *ProxyHandler {
		h, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelError),
			WithProvidersConfig(ProvidersConfig{
				SchemaVersion: ProvidersConfigSchemaVersion2,
				StateBindings: &StateBindingsConfig{Mode: "memory"},
				Providers: []ProviderConfig{
					{ID: "local", Type: "openai-compatible", Default: true, BaseURL: local.URL + "/v1", AuthType: "none", UpstreamDialect: "localai"},
					{ID: "cloud", Type: "openai-compatible", BaseURL: cloud.URL, AuthType: "none"},
				},
				ModelRoutes: []ModelRouteConfig{{
					ID:        "coder",
					PublicID:  "coder",
					Endpoints: []string{"/chat/completions"},
					Targets: []ModelRouteTargetConfig{
						{ID: "local", Provider: "local", UpstreamModel: "qwen"},
						{ID: "cloud", Provider: "cloud", UpstreamModel: "big"},
					},
					Routing: ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 2, MaxUpstreamSends: 3, FailoverOnContextOverflow: failover},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler: %v", err)
		}
		return h
	}

	body := `{"model":"coder","messages":[{"role":"user","content":"OVERFLOW"}]}`
	w := httptest.NewRecorder()
	build(true).HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from cloud") || cloudHits != 1 {
		t.Fatalf("failover: %d %s (cloud hits %d)", w.Code, w.Body.String(), cloudHits)
	}

	cloudHits = 0
	w = httptest.NewRecorder()
	build(false).HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || cloudHits != 0 || !strings.Contains(w.Body.String(), "context_length_exceeded") {
		t.Fatalf("without failover: %d %s (cloud hits %d)", w.Code, w.Body.String(), cloudHits)
	}

	// Claude Code's streamed Messages request fails over before any bytes reach it.
	cloudHits = 0
	anthropic := `{"model":"coder","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"OVERFLOW"}]}`
	w = httptest.NewRecorder()
	build(true).HandleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropic)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from cloud") || strings.Contains(w.Body.String(), "exceeds the available context") || cloudHits != 1 {
		t.Fatalf("anthropic stream failover: %d %s (cloud hits %d)", w.Code, w.Body.String(), cloudHits)
	}

	// Unrelated 400s never switch targets.
	cloudHits = 0
	w = httptest.NewRecorder()
	build(true).HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if w.Code != http.StatusOK || cloudHits != 0 {
		t.Fatalf("normal request: %d (cloud hits %d)", w.Code, cloudHits)
	}
}

func TestNormalizeLocalAIChatMessages(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[{"role":"system","content":"base"},{"role":"developer","content":[{"type":"text","text":"dev"}]},{"role":"user","content":"hi"},{"role":"system","content":"# Environment"}]}`
	out, err := normalizeLocalAIRequest([]byte(body), providerEndpointChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if !payload.Stream || len(payload.Messages) != 3 {
		t.Fatalf("payload = %s", out)
	}
	want := []struct{ role, content string }{
		{"system", "base\n\ndev"},
		{"user", "hi"},
		{"user", "<system-reminder>\n# Environment\n</system-reminder>"},
	}
	for i, message := range payload.Messages {
		if message.Role != want[i].role || message.Content != want[i].content {
			t.Fatalf("message %d = %+v, want %+v", i, message, want[i])
		}
	}

	for _, unchanged := range []string{
		`{"messages":[{"role":"system","content":"a"},{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"x"}}]},{"role":"system","content":"b"}]}`,
	} {
		out, err := normalizeLocalAIRequest([]byte(unchanged), providerEndpointChatCompletions)
		if err != nil || string(out) != unchanged {
			t.Fatalf("changed %s to %s (%v)", unchanged, out, err)
		}
	}
}

func TestNormalizeLocalAIResponsesInput(t *testing.T) {
	body := `{"model":"m","store":false,"instructions":"You are Codex","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"permissions"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"role":"developer","content":"later"}]}`
	out, err := normalizeLocalAIRequest([]byte(body), providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Instructions != "You are Codex\n\npermissions" || len(payload.Input) != 3 {
		t.Fatalf("payload = %s", out)
	}
	var reminder struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload.Input[2], &reminder); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload.Input[1]), `"function_call"`) || reminder.Role != "user" || len(reminder.Content) != 1 || reminder.Content[0].Text != "<system-reminder>\nlater\n</system-reminder>" {
		t.Fatalf("input = %s", out)
	}
	simple := `{"instructions":"x","input":[{"role":"user","content":"hi"}]}`
	if out, _ := normalizeLocalAIRequest([]byte(simple), providerEndpointResponses); string(out) != simple {
		t.Fatalf("simple request changed: %s", out)
	}
	stringInput := `{"instructions":"x","input":"hi"}`
	if out, _ := normalizeLocalAIRequest([]byte(stringInput), providerEndpointResponses); string(out) != stringInput {
		t.Fatalf("string input changed: %s", out)
	}
}

func TestNormalizeLocalAIRequestRewritesLoneDeveloperMessage(t *testing.T) {
	out, err := normalizeLocalAIRequest([]byte(`{"messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`), providerEndpointChatCompletions)
	if err != nil || !strings.Contains(string(out), `{"content":"rules","role":"system"}`) {
		t.Fatalf("chat = %s, %v", out, err)
	}
	out, err = normalizeLocalAIRequest([]byte(`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"role":"user","content":"hi"}]}`), providerEndpointResponses)
	if err != nil || !strings.Contains(string(out), `"instructions":"rules"`) || strings.Contains(string(out), "developer") {
		t.Fatalf("responses = %s, %v", out, err)
	}
}

func TestLocalAIBodyWrappersKeepRouteLifecycle(t *testing.T) {
	owner := &routeAttemptTransportOwner{}
	source := &routeAttemptTransportBody{inner: io.NopCloser(strings.NewReader(normalChatStream)), owner: owner}
	peeked := peekLocalAIStreamOverflow(context.Background(), &http.Response{StatusCode: 200, Header: http.Header{}, Body: source}, providerEndpointChatCompletions, "messages")
	if routeAttemptTransportOwnership(peeked.Body) != owner {
		t.Fatal("stream peek dropped route-attempt ownership")
	}
	aliased := restoreLocalAIToolAliases(&http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: peeked.Body}, localAIToolAliases{"a__b": {namespace: "a", name: "b"}})
	if routeAttemptTransportOwnership(aliased.Body) != owner {
		t.Fatal("alias stream dropped route-attempt ownership")
	}
	if routeAttemptTransportOwnership(newLocalAIPrefixedBody(nil, source)) != owner {
		t.Fatal("prefixed body dropped route-attempt ownership")
	}
}

func TestValidateFunctionToolsOnlyRejectsCatalogsBeyondPeekBudget(t *testing.T) {
	huge := `{"instructions":"` + strings.Repeat("x", localAIOverflowPeekBytes/2+1) + `","tools":[{"type":"function","name":"f"}]}`
	err := validateFunctionToolsOnlyResponsesRequest([]byte(huge), "local")
	if providerRequestErrorCode(err) != "context_length_exceeded" {
		t.Fatalf("error = %v (code %q)", err, providerRequestErrorCode(err))
	}
}
