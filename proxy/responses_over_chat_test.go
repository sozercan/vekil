package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const rocCopilotCatalog = `{"object":"list","data":[
	{"id":"gemini-test","name":"Gemini Test","object":"model","owned_by":"copilot","model_picker_enabled":true,
	 "supported_endpoints":["/chat/completions"],
	 "capabilities":{"limits":{"max_context_window_tokens":1048576,"max_prompt_tokens":983040,"max_output_tokens":65536},
	  "supports":{"tool_calls":true,"parallel_tool_calls":true,"streaming":true,"reasoning_effort":["low","medium","high"]}}},
	{"id":"gemini-plain","object":"model","owned_by":"copilot","supported_endpoints":["/chat/completions"],
	 "capabilities":{"supports":{"tool_calls":true}}},
	{"id":"chat-no-tools","object":"model","owned_by":"copilot","supported_endpoints":["/chat/completions"],
	 "capabilities":{"supports":{"tool_calls":false}}},
	{"id":"gpt-native","object":"model","owned_by":"copilot","model_picker_enabled":true,
	 "supported_endpoints":["/chat/completions","/responses"],"capabilities":{"supports":{"tool_calls":true}}}
]}`

type rocUpstream struct {
	t             *testing.T
	server        *httptest.Server
	mu            sync.Mutex
	chatBodies    []map[string]any
	chatHits      atomic.Int32
	responsesHits atomic.Int32
	chat          func(w http.ResponseWriter, r *http.Request)
}

func newROCUpstream(t *testing.T, chat func(w http.ResponseWriter, r *http.Request)) *rocUpstream {
	t.Helper()
	u := &rocUpstream{t: t, chat: chat}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case providerEndpointModels:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, rocCopilotCatalog)
		case providerEndpointChatCompletions:
			u.chatHits.Add(1)
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			u.mu.Lock()
			u.chatBodies = append(u.chatBodies, body)
			u.mu.Unlock()
			u.chat(w, r)
		case providerEndpointResponses:
			u.responsesHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-native","output":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *rocUpstream) lastChatBody() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.chatBodies) == 0 {
		u.t.Fatal("no upstream Chat request")
	}
	return u.chatBodies[len(u.chatBodies)-1]
}

func newROCHandler(t *testing.T, upstreamURL string, opts ...Option) *ProxyHandler {
	t.Helper()
	options := append([]Option{WithCopilotBaseURL(upstreamURL), WithResponsesOverChatModels("gemini-test", "gemini-plain", "chat-no-tools", "gpt-native", "unknown-model")}, opts...)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture"), logger.NewWithWriter(logger.LevelError, io.Discard), options...)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func writeROCChunk(t *testing.T, w http.ResponseWriter, chunk string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func rocSSE(t *testing.T, chunks ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			writeROCChunk(t, w, chunk)
		}
	}
}

type rocEvent struct {
	Type string
	Data map[string]any
}

func parseROCEvents(t *testing.T, body string) []rocEvent {
	t.Helper()
	var events []rocEvent
	if err := consumeResponsesSSEMessages(strings.NewReader(body), func(msg responsesSSEMessage) error {
		var data map[string]any
		if err := json.Unmarshal([]byte(msg.data), &data); err != nil {
			return err
		}
		eventType, _ := data["type"].(string)
		if msg.event != "" && msg.event != eventType {
			return fmt.Errorf("SSE event %q does not match payload type %q", msg.event, eventType)
		}
		events = append(events, rocEvent{Type: eventType, Data: data})
		return nil
	}); err != nil {
		t.Fatalf("parse SSE: %v\n%s", err, body)
	}
	for i, event := range events {
		if seq, _ := event.Data["sequence_number"].(float64); int(seq) != i {
			t.Fatalf("event %d (%s) sequence_number = %v, want %d", i, event.Type, event.Data["sequence_number"], i)
		}
	}
	return events
}

func rocEventTypes(events []rocEvent) []string {
	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.Type
	}
	return types
}

func postROC(t *testing.T, h *ProxyHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	return recorder
}

const rocStop = `{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}]}`

func rocUsage(prompt, completion int) string {
	return fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, prompt, completion, prompt+completion)
}

func TestResponsesOverChatDeliversFirstDeltaBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"content":"lo"}}]}`)
		writeROCChunk(t, w, rocStop)
		writeROCChunk(t, w, rocUsage(11, 2))
		writeROCChunk(t, w, "[DONE]")
	})
	h := newROCHandler(t, upstream.server.URL)
	proxy := httptest.NewServer(http.HandlerFunc(h.HandleResponses))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL, "application/json", strings.NewReader(`{"model":"gemini-test","input":"hi","stream":true,"store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	reader := bufio.NewReader(resp.Body)
	var transcript strings.Builder
	deadline := time.AfterFunc(5*time.Second, func() { _ = resp.Body.Close() })
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before the first delta: %v\n%s", err, transcript.String())
		}
		transcript.WriteString(line)
		if strings.Contains(line, `"type":"response.output_text.delta"`) && strings.Contains(line, `"delta":"Hel"`) {
			break
		}
	}
	deadline.Stop()
	// The upstream is still blocked: the delta was delivered incrementally.
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	transcript.Write(rest)

	events := parseROCEvents(t, transcript.String())
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	if got := rocEventTypes(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v\nwant %v", got, want)
	}
	completed := events[len(events)-1].Data["response"].(map[string]any)
	if completed["status"] != "completed" || completed["model"] != "gemini-test" {
		t.Fatalf("completed response = %v", completed)
	}
	createdID := events[0].Data["response"].(map[string]any)["id"]
	if completed["id"] != createdID {
		t.Fatalf("response id changed: %v -> %v", createdID, completed["id"])
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(2) || usage["total_tokens"] != float64(13) {
		t.Fatalf("usage = %v", usage)
	}
	output := completed["output"].([]any)
	content := output[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("output = %v", output)
	}
	if upstream.responsesHits.Load() != 0 || upstream.chatHits.Load() != 1 {
		t.Fatalf("upstream hits chat=%d responses=%d", upstream.chatHits.Load(), upstream.responsesHits.Load())
	}
	chat := upstream.lastChatBody()
	if chat["stream"] != true || chat["model"] != "gemini-test" {
		t.Fatalf("upstream chat body = %v", chat)
	}
}

func TestResponsesOverChatStreamsInterleavedNamespacedParallelToolCalls(t *testing.T) {
	var aliasRead, aliasWrite string
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Names and arguments arrive in separate, interleaved chunks.
		writeROCChunk(t, w, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":%q,"arguments":""}}]}}]}`, aliasRead))
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`)
		writeROCChunk(t, w, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":%q}}]}}]}`, aliasWrite))
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"b\"}"}}]}}]}`)
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`)
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`)
		writeROCChunk(t, w, "[DONE]")
	})
	aliasRead = policyResponsesStableToolAlias(policyResponsesToolDescriptor{Name: "read", Namespace: "fs", Kind: policyResponsesToolKindFunction})
	aliasWrite = policyResponsesStableToolAlias(policyResponsesToolDescriptor{Name: "read", Namespace: "remote", Kind: policyResponsesToolKindFunction})
	h := newROCHandler(t, upstream.server.URL)

	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"store":false,"parallel_tool_calls":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read both"}]}],
		"tools":[
			{"type":"namespace","name":"fs","description":"Local files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},
			{"type":"namespace","name":"remote","tools":[{"type":"function","name":"read","parameters":{"type":"object"},"strict":false}]}
		]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	events := parseROCEvents(t, recorder.Body.String())
	var argumentDeltas []string
	var done []map[string]any
	for _, event := range events {
		switch event.Type {
		case "response.function_call_arguments.delta":
			argumentDeltas = append(argumentDeltas, event.Data["delta"].(string))
		case "response.output_item.done":
			done = append(done, event.Data["item"].(map[string]any))
		}
	}
	if len(argumentDeltas) != 3 {
		t.Fatalf("argument deltas = %v, want incremental fragments", argumentDeltas)
	}
	if len(done) != 2 {
		t.Fatalf("done items = %v", done)
	}
	if done[0]["namespace"] != "fs" || done[0]["name"] != "read" || done[0]["call_id"] != "call_a" || done[0]["arguments"] != `{"path":"a"}` {
		t.Fatalf("first call = %v", done[0])
	}
	if done[1]["namespace"] != "remote" || done[1]["name"] != "read" || done[1]["call_id"] != "call_b" || done[1]["arguments"] != `{"path":"b"}` {
		t.Fatalf("second call = %v", done[1])
	}
	completed := events[len(events)-1]
	if completed.Type != "response.completed" {
		t.Fatalf("terminal event = %s", completed.Type)
	}
	if output := completed.Data["response"].(map[string]any)["output"].([]any); len(output) != 2 {
		t.Fatalf("final output = %v", output)
	}

	chat := upstream.lastChatBody()
	tools := chat["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("upstream tools = %v", tools)
	}
	first := tools[0].(map[string]any)["function"].(map[string]any)
	if first["name"] != aliasRead || first["description"] != "Local files" {
		t.Fatalf("first upstream tool = %v", first)
	}
	if _, hasStrict := first["strict"]; hasStrict {
		t.Fatalf("omitted strict must stay omitted: %v", first)
	}
	if second := tools[1].(map[string]any)["function"].(map[string]any); second["strict"] != false {
		t.Fatalf("explicit strict:false must be preserved: %v", second)
	}
	if chat["parallel_tool_calls"] != true {
		t.Fatalf("parallel_tool_calls = %v", chat["parallel_tool_calls"])
	}
}

func TestResponsesOverChatReplaysToolResultsAndReturnsJSON(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Paris 09:00, Tokyo 16:00."}}]}`,
		rocStop, "[DONE]"))
	h := newROCHandler(t, upstream.server.URL)

	recorder := postROC(t, h, `{"model":"gemini-test","store":false,"instructions":"Be brief.",
		"input":[
			{"type":"message","role":"user","content":"times?"},
			{"type":"function_call","call_id":"call_1","name":"get_time","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{\"city\":\"Tokyo\"}"},
			{"type":"function_call_output","call_id":"call_2","output":"16:00"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"09:00"}]},
			{"type":"message","role":"user","content":"thanks"}
		],
		"tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "completed" || response["object"] != "response" || response["store"] != false {
		t.Fatalf("response = %v", response)
	}
	if response["usage"] != nil {
		t.Fatalf("usage must stay null when upstream reports none: %v", response["usage"])
	}

	messages := upstream.lastChatBody()["messages"].([]any)
	roles := make([]string, len(messages))
	for i, raw := range messages {
		roles[i] = raw.(map[string]any)["role"].(string)
	}
	if got := strings.Join(roles, ","); got != "developer,user,assistant,tool,tool,user" {
		t.Fatalf("roles = %s", got)
	}
	assistant := messages[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 2 || calls[0].(map[string]any)["id"] != "call_1" || calls[1].(map[string]any)["id"] != "call_2" {
		t.Fatalf("assistant tool calls = %v", calls)
	}
	if messages[3].(map[string]any)["tool_call_id"] != "call_2" || messages[4].(map[string]any)["content"] != "09:00" {
		t.Fatalf("tool results = %v %v", messages[3], messages[4])
	}
}

func TestResponsesOverChatNonStreamingToolCallsUseSameValidation(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t,
		// Copilot's Gemini stream shape: complete call in one chunk, an opaque
		// vendor field, per-chunk usage, and finish_reason "stop".
		`{"choices":[{"index":0,"delta":{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\":\"Paris\"}","name":"get_time"},"id":"call_x","index":0,"type":"function"}],"reasoning_opaque":"c3ludGhldGlj"}}],"copilot_usage":{"total_nano_aiu":0},"usage":{"completion_tokens":0,"prompt_tokens":0,"total_tokens":0}}`,
		`{"choices":[{"finish_reason":"stop","index":0,"delta":{"content":null,"role":"assistant"}}],"usage":{"completion_tokens":60,"prompt_tokens":33,"total_tokens":93,"reasoning_tokens":28}}`,
		"[DONE]"))
	h := newROCHandler(t, upstream.server.URL)

	recorder := postROC(t, h, `{"model":"gemini-test","input":"time in Paris?","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
		Usage  map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "completed" || len(response.Output) != 1 || response.Output[0]["type"] != "function_call" || response.Output[0]["call_id"] != "call_x" {
		t.Fatalf("response = %s", recorder.Body.String())
	}
	if response.Usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(28) {
		t.Fatalf("usage = %v", response.Usage)
	}
}

func TestResponsesOverChatSanitizedCopilotGeminiFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/responses_over_chat/copilot_gemini_parallel_tools.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	})
	h := newROCHandler(t, upstream.server.URL)
	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"input":"Call get_time for Paris and Tokyo.","tool_choice":"required",
		"tools":[{"type":"function","name":"get_time","description":"Get local time","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`)
	events := parseROCEvents(t, recorder.Body.String())
	terminal := events[len(events)-1]
	if terminal.Type != "response.completed" {
		t.Fatalf("terminal = %s body=%s", terminal.Type, recorder.Body.String())
	}
	output := terminal.Data["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["arguments"] != `{"city":"Paris"}` || output[1].(map[string]any)["arguments"] != `{"city":"Tokyo"}` {
		t.Fatalf("output = %v", output)
	}
	if usage := terminal.Data["response"].(map[string]any)["usage"].(map[string]any); usage["total_tokens"] != float64(93) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestResponsesOverChatLengthTerminationNeverExposesPartialToolCall(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Working"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p","function":{"name":"get_time","arguments":"{\"ci"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		"[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"input":"go","tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}}]}`)
	events := parseROCEvents(t, recorder.Body.String())
	for _, event := range events {
		if event.Type == "response.output_item.done" && event.Data["item"].(map[string]any)["type"] == "function_call" {
			t.Fatalf("partial function call was exposed as done: %v", event.Data)
		}
	}
	terminal := events[len(events)-1]
	response := terminal.Data["response"].(map[string]any)
	if terminal.Type != "response.incomplete" || response["status"] != "incomplete" {
		t.Fatalf("terminal = %s %v", terminal.Type, response)
	}
	if reason := response["incomplete_details"].(map[string]any)["reason"]; reason != "max_output_tokens" {
		t.Fatalf("incomplete reason = %v", reason)
	}
	for _, item := range response["output"].([]any) {
		if item.(map[string]any)["type"] == "function_call" {
			t.Fatalf("final output contains a partial function call: %v", response["output"])
		}
	}
}

func TestResponsesOverChatPrematureEndFailsHonestly(t *testing.T) {
	chunks := []string{`{"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`}
	upstream := newROCUpstream(t, rocSSE(t, chunks...))
	h := newROCHandler(t, upstream.server.URL)

	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"input":"go"}`)
	events := parseROCEvents(t, recorder.Body.String())
	terminal := events[len(events)-1]
	if terminal.Type != "response.failed" {
		t.Fatalf("terminal = %s", terminal.Type)
	}
	failed := terminal.Data["response"].(map[string]any)
	if failed["status"] != "failed" || failed["error"].(map[string]any)["code"] != "incomplete_upstream_stream" {
		t.Fatalf("failed response = %v", failed)
	}

	recorder = postROC(t, h, `{"model":"gemini-test","input":"go"}`)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "incomplete_upstream_stream") {
		t.Fatalf("non-streaming status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesOverChatUpstreamStreamErrorAndStatus(t *testing.T) {
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"a"}}]}`)
		_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n")
	})
	h := newROCHandler(t, upstream.server.URL)
	events := parseROCEvents(t, postROC(t, h, `{"model":"gemini-test","stream":true,"input":"go"}`).Body.String())
	terminal := events[len(events)-1]
	if terminal.Type != "response.failed" || terminal.Data["response"].(map[string]any)["error"].(map[string]any)["code"] != "rate_limit_exceeded" {
		t.Fatalf("terminal = %v", terminal)
	}

	statusUpstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded","type":"rate_limit_error"}}`)
	})
	h = newROCHandler(t, statusUpstream.server.URL)
	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"input":"go"}`)
	if recorder.Code != http.StatusTooManyRequests || strings.Contains(recorder.Body.String(), "response.created") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesOverChatRejectsUnsupportedRequestsWithoutUpstreamSends(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"store", `{"model":"gemini-test","input":"hi","store":true}`, "store must be false or omitted for Responses-over-Chat models"},
		{"previous response", `{"model":"gemini-test","input":"hi","previous_response_id":"resp_1"}`, "previous_response_id is not supported for Responses-over-Chat models"},
		{"conversation", `{"model":"gemini-test","input":"hi","conversation":"conv_1"}`, "unsupported JSON field"},
		{"image", `{"model":"gemini-test","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:,"}]}]}`, "image content is not supported for Responses-over-Chat models"},
		{"encrypted reasoning", `{"model":"gemini-test","input":[{"type":"reasoning","encrypted_content":"x","summary":[]}]}`, `Responses input item type \"reasoning\" is not supported for Responses-over-Chat models`},
		{"hosted tool", `{"model":"gemini-test","input":"hi","tools":[{"type":"web_search"}]}`, `hosted or custom tool type \"web_search\" is not supported`},
		{"custom tool", `{"model":"gemini-test","input":"hi","tools":[{"type":"custom","name":"apply_patch"}]}`, `hosted or custom tool type \"custom\" is not supported`},
		{"deferred tool", `{"model":"gemini-test","input":"hi","tools":[{"type":"function","name":"f","parameters":{},"defer_loading":true}]}`, "deferred tool loading is not supported"},
		{"undeclared forced tool", `{"model":"gemini-test","input":"hi","tool_choice":{"type":"function","name":"missing"},"tools":[{"type":"function","name":"f","parameters":{}}]}`, "tool_choice must name a declared function tool"},
		{"missing tool result", `{"model":"gemini-test","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"message","role":"user","content":"next"}],"tools":[{"type":"function","name":"f","parameters":{}}]}`, "missing a following function_call_output"},
		{"unknown tool result", `{"model":"gemini-test","input":[{"type":"function_call_output","call_id":"nope","output":"x"}]}`, "unknown prior call"},
		{"duplicate key", `{"model":"gemini-test","input":"a","input":"b"}`, "duplicate JSON field"},
		{"unsupported effort", `{"model":"gemini-test","input":"hi","reasoning":{"effort":"xhigh"}}`, "reasoning.effort must be one of: low, medium, high"},
		{"effort without controls", `{"model":"gemini-plain","input":"hi","reasoning":{"effort":"high"}}`, "reasoning.effort is not supported by this model"},
		{"replay id", `{"model":"gemini-test","input":[{"type":"function_call","call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","output":"x"}],"tools":[{"type":"function","name":"f","parameters":{}}]}`, "cannot be replayed to a Responses-over-Chat model"},
		{"no tool support", `{"model":"chat-no-tools","input":"hi"}`, "does not advertise function-tool support"},
		{"unknown model", `{"model":"unknown-model","input":"hi"}`, "not in the provider model catalog"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := postROC(t, h, tc.body)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s, want 400 containing %q", recorder.Code, recorder.Body.String(), tc.want)
			}
		})
	}
	if upstream.chatHits.Load() != 0 || upstream.responsesHits.Load() != 0 {
		t.Fatalf("rejected requests reached upstream: chat=%d responses=%d", upstream.chatHits.Load(), upstream.responsesHits.Load())
	}
}

func TestResponsesOverChatValidatedReasoningIsForwarded(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`, "[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	if recorder := postROC(t, h, `{"model":"gemini-test","input":"hi","reasoning":{"effort":"high","summary":"auto"}}`); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if effort := upstream.lastChatBody()["reasoning_effort"]; effort != "high" {
		t.Fatalf("reasoning_effort = %v", effort)
	}
	if recorder := postROC(t, h, `{"model":"gemini-plain","input":"hi","reasoning":{"effort":"none"}}`); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, present := upstream.lastChatBody()["reasoning_effort"]; present {
		t.Fatalf("explicit none must be dropped for a model without reasoning controls: %v", upstream.lastChatBody())
	}
}

func TestResponsesOverChatSelectionKeepsNativeAndDisabledModelsUnchanged(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
	// gpt-native is opted in but has native /responses support.
	h := newROCHandler(t, upstream.server.URL)
	if recorder := postROC(t, h, `{"model":"gpt-native","input":"hi"}`); recorder.Code != http.StatusOK {
		t.Fatalf("native status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstream.responsesHits.Load() != 1 || upstream.chatHits.Load() != 0 {
		t.Fatalf("native model hits chat=%d responses=%d", upstream.chatHits.Load(), upstream.responsesHits.Load())
	}

	// Without opt-in, a Chat-only model keeps the existing native passthrough.
	disabled, err := NewProxyHandler(auth.NewTestAuthenticator("fixture"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(disabled.BeginShutdown)
	postROC(t, disabled, `{"model":"gemini-test","input":"hi"}`)
	if upstream.responsesHits.Load() != 2 || upstream.chatHits.Load() != 0 {
		t.Fatalf("disabled model hits chat=%d responses=%d", upstream.chatHits.Load(), upstream.responsesHits.Load())
	}
}

func TestResponsesOverChatRespectsLaunchModelAllowlist(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
	h := newROCHandler(t, upstream.server.URL, WithAllowedModels("gpt-native"))
	recorder := postROC(t, h, `{"model":"gemini-test","input":"hi"}`)
	if recorder.Code != http.StatusBadRequest || upstream.chatHits.Load() != 0 {
		t.Fatalf("status=%d chat hits=%d body=%s", recorder.Code, upstream.chatHits.Load(), recorder.Body.String())
	}
}

func TestResponsesOverChatClientDisconnectCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"first"}}]}`)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(10 * time.Second):
		}
	})
	h := newROCHandler(t, upstream.server.URL)
	proxy := httptest.NewServer(http.HandlerFunc(h.HandleResponses))
	defer proxy.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL, strings.NewReader(`{"model":"gemini-test","input":"hi","stream":true}`))
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v", err)
		}
		if strings.Contains(line, `"delta":"first"`) {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not canceled after the client disconnected")
	}
}

func TestResponsesOverChatRejectsPolicyModelOptIn(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	_, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
		WithResponsesOverChatModels("coding-economy"),
	)
	if err == nil || !strings.Contains(err.Error(), "is a policy model") {
		t.Fatalf("NewProxyHandler() error = %v, want policy contradiction", err)
	}
}

func newROCExplicitRouteHandler(t *testing.T, primaryURL, secondaryURL string) *ProxyHandler {
	t.Helper()
	provider := func(id, url string, primary bool) ProviderConfig {
		return ProviderConfig{ID: id, Type: string(providerTypeOpenAICompatible), BaseURL: url, Default: primary, AuthType: "none"}
	}
	h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion2,
			StateBindings: &StateBindingsConfig{Mode: "memory"},
			Providers:     []ProviderConfig{provider("primary", primaryURL, true), provider("secondary", secondaryURL, false)},
			ModelRoutes: []ModelRouteConfig{{
				ID: "route-gemini", PublicID: "routed-gemini", Endpoints: []string{providerEndpointChatCompletions},
				Targets: []ModelRouteTargetConfig{
					{ID: "primary-target", Provider: "primary", UpstreamModel: "physical-primary"},
					{ID: "secondary-target", Provider: "secondary", UpstreamModel: "physical-secondary"},
				},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
			}},
		}),
		WithResponsesOverChatModels("routed-gemini"),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func TestResponsesOverChatExplicitRouteCommitmentRules(t *testing.T) {
	const preamble = "data: {\"id\":\"primary\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	const textChunk = "data: {\"id\":\"primary\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
	const rateLimit = "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n"
	tests := []struct {
		name          string
		primary       string
		wantSecondary int32
		wantTerminal  string
	}{
		{name: "rate limit before semantic output fails over", primary: preamble + rateLimit, wantSecondary: 1, wantTerminal: "response.completed"},
		{name: "rate limit after text never switches", primary: preamble + textChunk + rateLimit, wantSecondary: 0, wantTerminal: "response.failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var primarySends, secondarySends, responsesSends atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != providerEndpointChatCompletions {
					responsesSends.Add(1)
				}
				primarySends.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.primary)
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != providerEndpointChatCompletions {
					responsesSends.Add(1)
				}
				secondarySends.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(raw), `"model":"physical-secondary"`) {
					t.Errorf("secondary body did not use its upstream model: %s", raw)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				writeROCChunk(t, w, `{"model":"physical-secondary","choices":[{"index":0,"delta":{"role":"assistant","content":"from secondary"}}]}`)
				writeROCChunk(t, w, rocStop)
				writeROCChunk(t, w, "[DONE]")
			}))
			defer secondary.Close()
			h := newROCExplicitRouteHandler(t, primary.URL, secondary.URL)

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"routed-gemini","input":"hi","stream":true}`))
			ctx, summary := WithRequestSummary(request.Context())
			h.HandleResponses(recorder, request.WithContext(ctx))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			events := parseROCEvents(t, recorder.Body.String())
			terminal := events[len(events)-1]
			if terminal.Type != tt.wantTerminal {
				t.Fatalf("terminal = %s body=%s", terminal.Type, recorder.Body.String())
			}
			if terminal.Data["response"].(map[string]any)["model"] != "routed-gemini" {
				t.Fatalf("public model leaked: %v", terminal.Data["response"])
			}
			if strings.Contains(recorder.Body.String(), "physical-") {
				t.Fatalf("physical model leaked into client stream: %s", recorder.Body.String())
			}
			if primarySends.Load() != 1 || secondarySends.Load() != tt.wantSecondary || responsesSends.Load() != 0 {
				t.Fatalf("sends primary=%d secondary=%d responses=%d", primarySends.Load(), secondarySends.Load(), responsesSends.Load())
			}
			if id := recorder.Header().Get("X-Vekil-Request-ID"); id == "" || id != summary.OperationID() {
				t.Fatalf("request ID header = %q, summary = %q", id, summary.OperationID())
			}
			h.RecordRequest(summary, recorder.Code, "codex", 0)
			if snapshot := h.stats.snapshot(); snapshot.Totals.Requests != 1 {
				t.Fatalf("logical requests = %d, want 1", snapshot.Totals.Requests)
			}
		})
	}
}

func TestResponsesOverChatPreservesMultibyteTextAcrossDeltas(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"naï"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ve 🙂 "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"日本"},"finish_reason":"stop"}]}`,
		"[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	events := parseROCEvents(t, postROC(t, h, `{"model":"gemini-test","stream":true,"input":"hi"}`).Body.String())
	var deltas []string
	for _, event := range events {
		if event.Type == "response.output_text.delta" {
			deltas = append(deltas, event.Data["delta"].(string))
		}
	}
	if strings.Join(deltas, "|") != "naï|ve 🙂 |日本" {
		t.Fatalf("deltas = %q", deltas)
	}
	output := events[len(events)-1].Data["response"].(map[string]any)["output"].([]any)
	if text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "naïve 🙂 日本" {
		t.Fatalf("text = %q", text)
	}
}

func TestResponsesOverChatWebSocketTurnIsRejectedWithoutUpstreamSends(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	server := startResponsesWebSocketProxyServer(t, h)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest(nil)
	request["model"] = "gemini-test"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	payload := mustReadWebSocketJSON(t, conn)
	encoded, _ := json.Marshal(payload)
	if payload["type"] != "error" || payload["status_code"] != float64(http.StatusBadRequest) || !strings.Contains(string(encoded), "supports HTTP POST /v1/responses only") {
		t.Fatalf("websocket response = %s", encoded)
	}
	if upstream.chatHits.Load() != 0 || upstream.responsesHits.Load() != 0 {
		t.Fatalf("websocket turn reached upstream: chat=%d responses=%d", upstream.chatHits.Load(), upstream.responsesHits.Load())
	}
}

func TestResponsesOverChatAcceptsFragmentedToolNames(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t,
		// The first call's name arrives in two fragments before its arguments.
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"time"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}}]}`,
		// The second call's name is fragmented and it never streams arguments.
		// Another call starting says nothing about its name, so it is exposed
		// only at the successful finish, after the third call.
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":"weather"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]"))
	h := newROCHandler(t, upstream.server.URL)

	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"store":false,"parallel_tool_calls":true,"input":"time and weather?",
		"tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}},{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	events := parseROCEvents(t, recorder.Body.String())
	if terminal := events[len(events)-1]; terminal.Type != "response.completed" {
		t.Fatalf("terminal event = %s: %v", terminal.Type, terminal.Data)
	}
	var added, done []map[string]any
	for _, event := range events {
		switch event.Type {
		case "response.output_item.added":
			added = append(added, event.Data["item"].(map[string]any))
		case "response.output_item.done":
			done = append(done, event.Data["item"].(map[string]any))
		}
	}
	want := []struct{ callID, name, arguments string }{
		{"call_a", "get_time", `{"city":"Paris"}`},
		{"call_c", "get_time", "{}"},
		{"call_b", "get_weather", "{}"},
	}
	if len(added) != len(want) || len(done) != len(want) {
		t.Fatalf("added=%v done=%v", added, done)
	}
	for i, call := range want {
		if added[i]["name"] != call.name || added[i]["call_id"] != call.callID {
			t.Fatalf("added[%d] = %v, want %+v", i, added[i], call)
		}
		if done[i]["name"] != call.name || done[i]["call_id"] != call.callID || done[i]["arguments"] != call.arguments {
			t.Fatalf("done[%d] = %v, want %+v", i, done[i], call)
		}
	}
}

// rocDoneItems returns the items carried by output_item.done events.
func rocDoneItems(t *testing.T, events []rocEvent) []map[string]any {
	t.Helper()
	var done []map[string]any
	for _, event := range events {
		if event.Type == "response.output_item.done" {
			done = append(done, event.Data["item"].(map[string]any))
		}
	}
	return done
}

func TestResponsesOverChatNameCompletesOnlyByOwnArgumentsOrFinish(t *testing.T) {
	tools := `[{"type":"function","name":"get","parameters":{"type":"object"}},
		{"type":"function","name":"get_time","parameters":{"type":"object"}},
		{"type":"function","name":"get_weather","parameters":{"type":"object"}}]`
	tests := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{
			name: "another call index does not complete an earlier name",
			chunks: []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"time","arguments":"{}"}}]}}]}`,
			},
			want: []string{"get_weather", "get_time"},
		},
		{
			name: "a declared prefix is not published before its name completes",
			chunks: []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"_time","arguments":"{}"}}]}}]}`,
			},
			want: []string{"get_weather", "get_time"},
		},
		{
			name: "text does not complete a pending name",
			chunks: []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"checking"}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"time","arguments":"{}"}}]}}]}`,
			},
			want: []string{"message", "get_time"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := append(append([]string{}, tt.chunks...), `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, "[DONE]")
			upstream := newROCUpstream(t, rocSSE(t, chunks...))
			h := newROCHandler(t, upstream.server.URL)
			recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"store":false,"parallel_tool_calls":true,"input":"go","tools":`+tools+`}`)
			events := parseROCEvents(t, recorder.Body.String())
			if terminal := events[len(events)-1]; terminal.Type != "response.completed" {
				t.Fatalf("terminal event = %s: %v", terminal.Type, terminal.Data)
			}
			var got []string
			lastIndex := -1.0
			for _, event := range events {
				if event.Type != "response.output_item.added" {
					continue
				}
				if index := event.Data["output_index"].(float64); index <= lastIndex {
					t.Fatalf("output_index %v is not monotonic after %v", index, lastIndex)
				} else {
					lastIndex = index
				}
				item := event.Data["item"].(map[string]any)
				if item["type"] == "message" {
					got = append(got, "message")
				} else {
					got = append(got, item["name"].(string))
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("added items = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponsesOverChatMalformedChunksFailBeforeExecutableCalls(t *testing.T) {
	tests := map[string]string{
		"undecodable chunk":       `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":`,
		"wrongly typed arguments": `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":123}}]}}]}`,
	}
	for name, malformed := range tests {
		t.Run(name, func(t *testing.T) {
			upstream := newROCUpstream(t, rocSSE(t,
				`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
				malformed,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				"[DONE]"))
			h := newROCHandler(t, upstream.server.URL)
			tools := `[{"type":"function","name":"get_time","parameters":{"type":"object"}}]`

			recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"store":false,"input":"time?","tools":`+tools+`}`)
			events := parseROCEvents(t, recorder.Body.String())
			terminal := events[len(events)-1]
			if terminal.Type != "response.failed" {
				t.Fatalf("terminal event = %s: %s", terminal.Type, recorder.Body.String())
			}
			if code := terminal.Data["response"].(map[string]any)["error"].(map[string]any)["code"]; code != "invalid_upstream_stream" {
				t.Fatalf("error code = %v", code)
			}
			if done := rocDoneItems(t, events); len(done) != 0 {
				t.Fatalf("malformed stream exposed executable items: %v", done)
			}

			recorder = postROC(t, h, `{"model":"gemini-test","store":false,"input":"time?","tools":`+tools+`}`)
			if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "invalid_upstream_stream") {
				t.Fatalf("non-streaming status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestResponsesOverChatStreamedItemsReplayVerbatim(t *testing.T) {
	first := rocSSE(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Looking it up."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]")
	second := rocSSE(t, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"It is noon."}}]}`, rocStop, "[DONE]")
	var turns atomic.Int32
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if turns.Add(1) == 1 {
			first(w, r)
			return
		}
		second(w, r)
	})
	h := newROCHandler(t, upstream.server.URL)
	tools := `[{"type":"function","name":"get_time","parameters":{"type":"object"}}]`
	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"store":false,"input":"time?","tools":`+tools+`}`)
	events := parseROCEvents(t, recorder.Body.String())
	done := rocDoneItems(t, events)
	if len(done) != 2 {
		t.Fatalf("done items = %v", done)
	}
	for _, item := range done {
		for key := range item {
			if strings.ContainsRune(key, 0) {
				t.Fatalf("streamed item carries private key %q: %v", key, item)
			}
		}
	}
	final := events[len(events)-1].Data["response"].(map[string]any)["output"].([]any)
	if len(final) != 2 || final[0].(map[string]any)["type"] != "message" || final[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("final output = %v", final)
	}

	// A full-history client replays the streamed items as input.
	input := []any{map[string]any{"type": "message", "role": "user", "content": "time?"}}
	for _, item := range done {
		input = append(input, item)
	}
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "noon"})
	payload, err := json.Marshal(map[string]any{"model": "gemini-test", "store": false, "input": input, "tools": json.RawMessage(tools)})
	if err != nil {
		t.Fatal(err)
	}
	recorder = postROC(t, h, string(payload))
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesOverChatRejectsInternalRouteOptIn(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	_, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
		WithResponsesOverChatModels("light-route"),
	)
	if err == nil || !strings.Contains(err.Error(), "names an internal route") {
		t.Fatalf("NewProxyHandler() error = %v, want internal route rejection", err)
	}
}

func TestResponsesOverChatRequireNativeChatRefusesResponsesBackendBeforeDispatch(t *testing.T) {
	// Admission saw a native Chat model; by execution the catalog lists it as
	// Responses-only. The guard must refuse without any upstream send.
	var chatSends, responsesSends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case providerEndpointModels:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"flipped","object":"model","owned_by":"copilot","supported_endpoints":["/responses"],"capabilities":{"supports":{"tool_calls":true}}}]}`)
			return
		case providerEndpointChatCompletions:
			chatSends.Add(1)
		case providerEndpointResponses:
			responsesSends.Add(1)
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer server.Close()
	h := newROCHandler(t, server.URL)

	body := []byte(`{"model":"flipped","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	mode := parseOpenAIChatCompletionsMode(body)
	_, err := h.executeRoutedChatCompletions(context.Background(), body, mode, chatExecutionOptions{RequireNativeChat: true}, "flipped")
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "native_chat_backend_unavailable" {
		t.Fatalf("error = %v, want native_chat_backend_unavailable", err)
	}
	if chatSends.Load() != 0 || responsesSends.Load() != 0 {
		t.Fatalf("guard dispatched upstream: chat=%d responses=%d", chatSends.Load(), responsesSends.Load())
	}
}

func TestResponsesOverChatConcatenatesRepeatedNameFragments(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"get"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		// A repeat of the complete name after announcement is tolerated.
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"getget"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	tools := `[{"type":"function","name":"get","parameters":{"type":"object"}},{"type":"function","name":"getget","parameters":{"type":"object"}}]`
	recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"store":false,"input":"go","tools":`+tools+`}`)
	events := parseROCEvents(t, recorder.Body.String())
	if terminal := events[len(events)-1]; terminal.Type != "response.completed" {
		t.Fatalf("terminal event = %s: %v", terminal.Type, terminal.Data)
	}
	done := rocDoneItems(t, events)
	if len(done) != 1 || done[0]["name"] != "getget" {
		t.Fatalf("done items = %v, want one getget call", done)
	}
}

func TestResponsesOverChatFinalOutputWithTextAfterCallReplays(t *testing.T) {
	first := rocSSE(t,
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Checking the clock."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]")
	second := rocSSE(t, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"It is noon."}}]}`, rocStop, "[DONE]")
	var turns atomic.Int32
	upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if turns.Add(1) == 1 {
			first(w, r)
			return
		}
		second(w, r)
	})
	h := newROCHandler(t, upstream.server.URL)
	tools := `[{"type":"function","name":"get_time","parameters":{"type":"object"}}]`

	recorder := postROC(t, h, `{"model":"gemini-test","store":false,"input":"time?","tools":`+tools+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 2 || !strings.Contains(string(response.Output[0]), `"function_call"`) || !strings.Contains(string(response.Output[1]), `"message"`) {
		t.Fatalf("output = %s, want [function_call, message]", recorder.Body.String())
	}

	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"time?"}`)}
	input = append(input, response.Output...)
	input = append(input, json.RawMessage(`{"type":"function_call_output","call_id":"call_a","output":"noon"}`))
	payload, err := json.Marshal(map[string]any{"model": "gemini-test", "store": false, "input": input, "tools": json.RawMessage(tools)})
	if err != nil {
		t.Fatal(err)
	}
	recorder = postROC(t, h, string(payload))
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	messages := upstream.lastChatBody()["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("replayed Chat messages = %v", messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "Checking the clock." || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant turn = %v, want text plus one tool call", assistant)
	}
	if tool := messages[2].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "call_a" {
		t.Fatalf("tool message = %v", tool)
	}
}

func TestPolicyResponsesContractKeepsRejectingInterleavedAssistantText(t *testing.T) {
	body := []byte(`{"model":"m","store":false,"tools":[{"type":"function","name":"get_time","parameters":{"type":"object"}}],"input":[
		{"type":"message","role":"user","content":"time?"},
		{"type":"function_call","call_id":"call_a","name":"get_time","arguments":"{}"},
		{"type":"message","role":"assistant","content":"Checking."},
		{"type":"function_call_output","call_id":"call_a","output":"noon"}]}`)
	if _, err := translatePolicyResponsesRequestToChat(body); err == nil || !strings.Contains(err.Error(), "missing a following function_call_output") {
		t.Fatalf("policy translation error = %v, want unchanged call-sequence rejection", err)
	}
	if _, err := translateResponsesRequestToChat(body, responsesOverChatTranslationContract); err != nil {
		t.Fatalf("Responses-over-Chat translation error = %v", err)
	}
}

func TestResponsesOverChatClientCancelRecords499(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"first"}}]}`)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		})
		h := newROCHandler(t, upstream.server.URL)
		summaries := make(chan *RequestSummary, 1)
		returned := make(chan struct{})
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(returned)
			ctx, summary := WithRequestSummary(r.Context())
			summaries <- summary
			h.HandleResponses(w, r.WithContext(ctx))
		}))
		defer proxy.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL, strings.NewReader(`{"model":"gemini-test","input":"hi","stream":true}`))
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended early: %v", err)
			}
			if strings.Contains(line, `"delta":"first"`) {
				break
			}
		}
		cancel()
		_ = resp.Body.Close()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not return after the client canceled")
		}
		if got := (<-summaries).FailureStatus(); got != 499 {
			t.Fatalf("summary failure status = %d, want 499", got)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		started := make(chan struct{})
		upstream := newROCUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeROCChunk(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"first"}}]}`)
			close(started)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		})
		h := newROCHandler(t, upstream.server.URL)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-test","input":"hi"}`)).WithContext(ctx)
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.HandleResponses(recorder, request)
		}()
		<-started
		// Let the adapter reach the stream read; an earlier cancel takes the
		// pre-response path, which also answers 499.
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not return after the client canceled")
		}
		if recorder.Code != 499 || recorder.Body.Len() != 0 {
			t.Fatalf("status = %d body=%q, want bare 499", recorder.Code, recorder.Body.String())
		}
	})
}

func postROCWithSummary(t *testing.T, h *ProxyHandler, w http.ResponseWriter, body string) *RequestSummary {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	ctx, summary := WithRequestSummary(request.Context())
	h.HandleResponses(w, request.WithContext(ctx))
	return summary
}

func TestResponsesOverChatRecordsIncompleteTurnsAsFailures(t *testing.T) {
	for _, tc := range []struct {
		name, finish string
		want         int
	}{
		{"completed", "stop", 0},
		{"length", "length", http.StatusConflict},
		{"content filter", "content_filter", http.StatusConflict},
	} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s stream=%v", tc.name, stream), func(t *testing.T) {
				upstream := newROCUpstream(t, rocSSE(t,
					`{"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`,
					fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, tc.finish),
					"[DONE]"))
				h := newROCHandler(t, upstream.server.URL)
				recorder := httptest.NewRecorder()
				summary := postROCWithSummary(t, h, recorder, fmt.Sprintf(`{"model":"gemini-test","stream":%v,"input":"go"}`, stream))
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
				}
				if got := summary.FailureStatus(); got != tc.want {
					t.Fatalf("summary failure status = %d, want %d", got, tc.want)
				}
			})
		}
	}
}

type rocFailingWriter struct {
	header http.Header
}

func (w *rocFailingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *rocFailingWriter) WriteHeader(int)           {}
func (w *rocFailingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (w *rocFailingWriter) Flush()                    {}

func TestResponsesOverChatClientWriteFailureRecords499(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			upstream := newROCUpstream(t, rocSSE(t,
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
				rocStop,
				"[DONE]"))
			h := newROCHandler(t, upstream.server.URL)
			summary := postROCWithSummary(t, h, &rocFailingWriter{}, fmt.Sprintf(`{"model":"gemini-test","stream":%v,"input":"go"}`, stream))
			if got := summary.FailureStatus(); got != 499 {
				t.Fatalf("summary failure status = %d, want 499", got)
			}
		})
	}
}

func TestResponsesOverChatOmitsToolChoiceWithoutTools(t *testing.T) {
	// Codex's local compaction turn sends tool_choice with no tools; Copilot
	// Chat rejects any tool_choice that has no tools beside it.
	for _, tc := range []struct{ name, fields string }{
		{"auto with empty tools", `"tools":[],"tool_choice":"auto"`},
		{"none with empty tools", `"tools":[],"tool_choice":"none"`},
		{"auto without tools", `"tool_choice":"auto"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newROCUpstream(t, rocSSE(t, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"summary"}}]}`, rocStop, "[DONE]"))
			h := newROCHandler(t, upstream.server.URL)
			recorder := postROC(t, h, `{"model":"gemini-test","stream":true,"input":"summarize",`+tc.fields+`}`)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			events := parseROCEvents(t, recorder.Body.String())
			if terminal := events[len(events)-1]; terminal.Type != "response.completed" {
				t.Fatalf("terminal = %s", terminal.Type)
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if len(upstream.chatBodies) != 1 {
				t.Fatalf("chat sends = %d", len(upstream.chatBodies))
			}
			for _, field := range []string{"tool_choice", "tools"} {
				if value, ok := upstream.chatBodies[0][field]; ok {
					t.Fatalf("upstream Chat body carries %s=%v without tools", field, value)
				}
			}
		})
	}

	t.Run("required without tools still fails locally", func(t *testing.T) {
		upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
		h := newROCHandler(t, upstream.server.URL)
		recorder := postROC(t, h, `{"model":"gemini-test","input":"go","tools":[],"tool_choice":"required"}`)
		if recorder.Code != http.StatusBadRequest || upstream.chatHits.Load() != 0 {
			t.Fatalf("status = %d chat sends = %d body=%s", recorder.Code, upstream.chatHits.Load(), recorder.Body.String())
		}
	})
}

func TestPolicyResponsesContractKeepsToolChoiceWithoutTools(t *testing.T) {
	body := []byte(`{"model":"m","store":false,"input":"summarize","tools":[],"tool_choice":"auto"}`)
	policy, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(policy.Body), `"tool_choice":"auto"`) {
		t.Fatalf("policy Chat body changed: %s", policy.Body)
	}
	adapter, err := translateResponsesRequestToChat(body, responsesOverChatTranslationContract)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(adapter.Body), "tool_choice") {
		t.Fatalf("Responses-over-Chat Chat body kept tool_choice: %s", adapter.Body)
	}
}
