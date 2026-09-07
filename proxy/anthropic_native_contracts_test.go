package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func TestHandleAnthropicCountTokensUsesDiscoveredNativeEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		catalog  string
		wantPath string
	}{
		{name: "native Messages", catalog: `{"data":[{"id":"count-model","supported_endpoints":["/v1/messages","/chat/completions"]}]}`, wantPath: providerEndpointMessagesCount},
		{name: "Chat only", catalog: `{"data":[{"id":"count-model","supported_endpoints":["/chat/completions"]}]}`, wantPath: providerEndpointChatCompletions},
		{name: "missing endpoints", catalog: `{"data":[{"id":"count-model"}]}`, wantPath: providerEndpointChatCompletions},
		{name: "unknown model", catalog: `{"data":[]}`, wantPath: providerEndpointChatCompletions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discoveryCalls, inferenceCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && r.URL.Path == providerEndpointModels {
					discoveryCalls.Add(1)
					if r.Header.Get("X-Interaction-Id") != "" || r.Header.Get("X-Client-Session-Id") != "" {
						t.Errorf("caller inference metadata reached discovery: %v", r.Header)
					}
					_, _ = io.WriteString(w, tc.catalog)
					return
				}
				inferenceCalls.Add(1)
				if r.URL.Path != tc.wantPath {
					t.Errorf("inference path = %q, want %q", r.URL.Path, tc.wantPath)
				}
				for name, want := range map[string]string{"X-Initiator": "agent", "X-Interaction-Id": "interaction-count", "X-Client-Session-Id": "session-count"} {
					if r.Header.Get(name) != want {
						t.Errorf("upstream %s = %q, want %q", name, r.Header.Get(name), want)
					}
				}
				if r.URL.Path == providerEndpointMessagesCount {
					_, _ = io.WriteString(w, `{"input_tokens":23}`)
				} else {
					_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":23,"completion_tokens":1,"total_tokens":24}}`)
				}
			}))
			defer upstream.Close()
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			req := httptest.NewRequest(http.MethodPost, providerEndpointMessagesCount, strings.NewReader(`{"model":"count-model","messages":[{"role":"user","content":"count this"}]}`))
			req.Header.Set("X-Initiator", "agent")
			req.Header.Set("X-Interaction-Id", "interaction-count")
			req.Header.Set("X-Client-Session-Id", "session-count")
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessagesCountTokens(recorder, req)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"input_tokens":23`) {
				t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
			}
			if discoveryCalls.Load() != 1 || inferenceCalls.Load() != 1 {
				t.Fatalf("discovery/inference calls = %d/%d, want 1/1", discoveryCalls.Load(), inferenceCalls.Load())
			}
			usage := h.stats.taskUsage.snapshot()
			if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || len(usage.ByKind) != 1 || usage.ByKind[0].Kind != "token_count" {
				t.Fatalf("count-token work lost its kind or completion through detached context: %+v", usage)
			}
		})
	}
}

func TestHandleAnthropicNativeCountTokensKeepsAccessChecksAndStripsCarriers(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "denied"}[allowed], func(t *testing.T) {
			var countCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && r.URL.Path == providerEndpointModels {
					_, _ = io.WriteString(w, `{"data":[{"id":"native-count","supported_endpoints":["/v1/messages"]}]}`)
					return
				}
				countCalls.Add(1)
				if r.URL.Path != providerEndpointMessagesCount {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), reasoningCarrierPrefix) || !strings.Contains(string(body), "native-signature") || !strings.Contains(string(body), "kept text") {
					t.Errorf("forwarded transcript = %s", body)
				}
				_, _ = io.WriteString(w, `{"input_tokens":23}`)
			}))
			defer upstream.Close()
			opts := []Option{WithCopilotBaseURL(upstream.URL)}
			if !allowed {
				opts = append(opts, WithAllowedModels("another-model"))
			}
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			body := `{"model":"native-count","messages":[{"role":"user","content":"count"},{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"vekil1.OPAQUE"},{"type":"thinking","thinking":"native thought","signature":"native-signature"},{"type":"text","text":"kept text"}]}]}`
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessagesCountTokens(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessagesCount, strings.NewReader(body)))
			if allowed {
				if recorder.Code != http.StatusOK || countCalls.Load() != 1 {
					t.Fatalf("status/calls = %d/%d: %s", recorder.Code, countCalls.Load(), recorder.Body.String())
				}
			} else if recorder.Code != http.StatusBadRequest || countCalls.Load() != 0 {
				t.Fatalf("denied status/calls = %d/%d: %s", recorder.Code, countCalls.Load(), recorder.Body.String())
			}
		})
	}
}

func TestAnthropicToolResultImagesFailBeforeChatDispatch(t *testing.T) {
	for _, content := range []string{
		`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]`,
		`[{"type":"text","text":"screen capture"},{"type":"image","source":{"type":"url","url":"https://example.test/screen.png"}}]`,
	} {
		for _, count := range []bool{false, true} {
			t.Run(content+map[bool]string{true: " count", false: " messages"}[count], func(t *testing.T) {
				var calls atomic.Int32
				h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) })
				body := `{"model":"chat-model","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_image","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_image","content":` + content + `}]}]}`
				recorder := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body))
				if count {
					h.HandleAnthropicMessagesCountTokens(recorder, req)
				} else {
					h.HandleAnthropicMessages(recorder, req)
				}
				if recorder.Code != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(recorder.Body.String(), "unsupported tool_result content block") {
					t.Fatalf("status/calls = %d/%d: %s", recorder.Code, calls.Load(), recorder.Body.String())
				}
			})
		}
	}
}

func TestAnthropicNativeMessagesPreserveToolResultImagesAndCacheHints(t *testing.T) {
	const body = `{"model":"native-image","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_image","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_image","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}],"cache_control":{"type":"ephemeral"}}]}]}`
	seen := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == providerEndpointModels {
			_, _ = io.WriteString(w, `{"data":[{"id":"native-image","supported_endpoints":["/v1/messages"]}]}`)
			return
		}
		if r.URL.Path != providerEndpointMessages {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		seen <- raw
		_, _ = io.WriteString(w, `{"id":"native-response","type":"message","role":"assistant","model":"native-image","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	select {
	case got := <-seen:
		if string(got) != body {
			t.Fatalf("native transcript changed: %s", got)
		}
	default:
		t.Fatal("native request was not forwarded")
	}
}

func TestAnthropicNativeChatCacheControlPreservesPromptBoundaries(t *testing.T) {
	const body = `{"model":"chat-model","max_tokens":64,"system":[{"type":"text","text":"one","cache_control":{"type":"ephemeral","ttl":"5m"}},{"type":"text","text":"two"},{"type":"text","text":"three","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}},{"type":"text","text":"tail"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`
	for _, count := range []bool{false, true} {
		t.Run(map[bool]string{false: "messages", true: "count"}[count], func(t *testing.T) {
			seen := make(chan models.OpenAIRequest, 1)
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				seen <- request
				if request.Stream != nil && *request.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, nativeChatExtensionStream())
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":23,"completion_tokens":1,"total_tokens":24}}`)
				}
			})
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body))
			if count {
				h.HandleAnthropicMessagesCountTokens(recorder, req)
			} else {
				h.HandleAnthropicMessages(recorder, req)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			var got models.OpenAIRequest
			select {
			case got = <-seen:
			default:
				t.Fatal("request was not forwarded")
			}
			if len(got.Messages) != 4 {
				t.Fatalf("message count = %d: %+v", len(got.Messages), got.Messages)
			}
			for i, want := range []struct {
				role, text, ttl string
				cached          bool
			}{
				{role: "system", text: "one", ttl: "5m", cached: true},
				{role: "system", text: "\ntwo\nthree", ttl: "1h", cached: true},
				{role: "user", text: "cached", cached: true},
				{role: "user", text: "tail"},
			} {
				message := got.Messages[i]
				var text string
				if err := json.Unmarshal(message.Content, &text); err != nil {
					t.Fatal(err)
				}
				if message.Role != want.role || text != want.text || !rawJSONIsNullOrEmpty(message.CopilotCacheControl) != want.cached {
					t.Fatalf("message[%d] = %+v, want %+v", i, message, want)
				}
				if want.cached {
					var cache struct {
						Type string `json:"type"`
						TTL  string `json:"ttl"`
					}
					_ = json.Unmarshal(message.CopilotCacheControl, &cache)
					if cache.Type != "ephemeral" || cache.TTL != want.ttl {
						t.Fatalf("message[%d] cache = %s", i, message.CopilotCacheControl)
					}
				}
			}
			if len(got.Tools) != 1 || got.Tools[0].Function.Name != "lookup" || !strings.Contains(string(got.Tools[0].CopilotCacheControl), `"ttl":"1h"`) {
				t.Fatalf("tools = %+v", got.Tools)
			}
		})
	}
}

func TestAnthropicNativeChatCacheControlRejectsUnrepresentableBoundaries(t *testing.T) {
	for _, fields := range []string{
		`"system":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"go"}]`,
		`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"lookup","input":{},"cache_control":{"type":"ephemeral"}},{"type":"tool_use","id":"call_b","name":"lookup","input":{}}]}]`,
		`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}},{"type":"text","text":"second"}]}]}]`,
		`"messages":[{"role":"user","content":[{"type":"text","text":"prefix"},{"type":"tool_result","tool_use_id":"call_a","content":"result","cache_control":{"type":"ephemeral"}}]}]`,
		`"messages":[{"role":"user","content":[{"type":"text","text":"prefix","cache_control":{"type":"ephemeral"}},{"type":"tool_result","tool_use_id":"call_a","content":"result"}]}]`,
		`"messages":[{"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"persistent"}}]}]`,
		`"messages":[{"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"ephemeral","ttl":"1d"}}]}]`,
	} {
		t.Run(fields, func(t *testing.T) {
			var calls atomic.Int32
			h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) })
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{"model":"chat-model","max_tokens":64,`+fields+`}`)))
			if recorder.Code != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(recorder.Body.String(), "cache_control") {
				t.Fatalf("status/calls = %d/%d: %s", recorder.Code, calls.Load(), recorder.Body.String())
			}
		})
	}
}

func TestAnthropicNativeChatCacheControlKeepsOptimizedToolOutput(t *testing.T) {
	var request models.AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"chat-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"text","text":"long trace","cache_control":{"type":"ephemeral"}}]},{"type":"text","text":"continue"}]}]}`), &request); err != nil {
		t.Fatal(err)
	}
	canonical, err := TranslateAnthropicToOpenAI(&request)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Messages[0].Content = json.RawMessage(`"compressed trace"`)
	body, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withAnthropicChatExtensions(context.Background(), &request)
	got, err := applyAnthropicChatExtensions(ctx, &providerRuntime{kind: providerTypeCopilot}, providerEndpointChatCompletions, body)
	if err != nil {
		t.Fatal(err)
	}
	var output models.OpenAIRequest
	if err := json.Unmarshal(got, &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Messages) != 2 || jsonRawString(output.Messages[0].Content) != "compressed trace" || rawJSONIsNullOrEmpty(output.Messages[0].CopilotCacheControl) || !rawJSONIsNullOrEmpty(output.Messages[1].CopilotCacheControl) {
		t.Fatalf("cache mapping changed optimized content or boundary: %s", got)
	}
}

func TestAnthropicNativeExtensionsLeaveStrictResponsesContractsUnchanged(t *testing.T) {
	var request models.AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"chat-model","messages":[{"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"inspect","signature":"native-signature"},{"type":"text","text":"answer"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]}`), &request); err != nil {
		t.Fatal(err)
	}
	canonical, err := TranslateAnthropicToOpenAI(&request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "cache_control") || strings.Contains(string(body), "reasoning_") {
		t.Fatalf("native-only fields reached canonical Chat: %s", body)
	}
	if _, err := translateChatRequestToResponses(body, responsesChatRequestOptions{}); err != nil {
		t.Fatalf("canonical Responses translation failed: %v", err)
	}
	ctx := withAnthropicChatExtensions(context.Background(), &request)
	for _, route := range []struct {
		kind providerType
		path string
	}{
		{kind: providerTypeCopilot, path: providerEndpointResponses},
		{kind: providerTypeOpenAICompatible, path: providerEndpointChatCompletions},
	} {
		got, err := applyAnthropicChatExtensions(ctx, &providerRuntime{kind: route.kind}, route.path, body)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("extension mapping changed %s %s: %s, error = %v", route.kind, route.path, got, err)
		}
	}
	for _, direct := range []struct {
		body  string
		field string
	}{
		{`{"model":"chat-model","messages":[{"role":"user","content":"go","copilot_cache_control":{"type":"ephemeral"}}]}`, "copilot_cache_control"},
		{`{"model":"chat-model","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","copilot_cache_control":{"type":"ephemeral"},"function":{"name":"lookup","parameters":{"type":"object"}}}]}`, "copilot_cache_control"},
		{`{"model":"chat-model","messages":[{"role":"assistant","content":"answer","reasoning_text":"inspect"}]}`, "reasoning_text"},
		{`{"model":"chat-model","messages":[{"role":"assistant","content":"answer","reasoning_opaque":"native-signature"}]}`, "reasoning_opaque"},
	} {
		_, err := translateChatRequestToResponses([]byte(direct.body), responsesChatRequestOptions{})
		var executionErr *chatExecutionError
		if !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusBadRequest || !strings.Contains(executionErr.Param, direct.field) {
			t.Fatalf("strict public Chat %s field error = %v", direct.field, err)
		}
	}
}

func TestAnthropicNativeChatReasoningHistory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   string
		thinking  string
		signature string
		text      string
		cached    bool
	}{
		{
			name:      "signature-only message",
			content:   `[{"type":"thinking","thinking":"","signature":"native-signature"}]`,
			signature: "native-signature",
		},
		{
			name:     "thinking-only message",
			content:  `[{"type":"thinking","thinking":"inspect"}]`,
			thinking: "inspect",
		},
		{
			name:      "thinking with cache boundary",
			content:   `[{"type":"thinking","thinking":"inspect","signature":"native-signature"},{"type":"text","text":"answer","cache_control":{"type":"ephemeral"}}]`,
			thinking:  "inspect",
			signature: "native-signature",
			text:      "answer",
			cached:    true,
		},
		{
			name:      "carriers stay separate",
			content:   `[{"type":"thinking","thinking":"carrier text","signature":"vekil1.OPAQUE"},{"type":"thinking","thinking":"inspect","signature":"native-signature"},{"type":"redacted_thinking","signature":"vekil1.OTHER"},{"type":"text","text":"answer"}]`,
			thinking:  "inspect",
			signature: "native-signature",
			text:      "answer",
		},
		{
			name:    "carrier alone is not native reasoning",
			content: `[{"type":"thinking","thinking":"carrier text","signature":"vekil1.OPAQUE"},{"type":"text","text":"answer"}]`,
			text:    "answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if r.URL.Path != providerEndpointChatCompletions || len(request.Messages) != 3 {
					t.Errorf("path/messages = %s/%+v", r.URL.Path, request.Messages)
				} else {
					assistant := request.Messages[1]
					if assistant.Role != "assistant" || assistant.ReasoningText != tc.thinking || assistant.ReasoningOpaque != tc.signature || jsonRawString(assistant.Content) != tc.text || !rawJSONIsNullOrEmpty(assistant.CopilotCacheControl) != tc.cached {
						t.Errorf("native history changed: %+v", assistant)
					}
					if jsonRawString(request.Messages[0].Content) != "go" || jsonRawString(request.Messages[2].Content) != "continue" {
						t.Errorf("message ordering changed: %+v", request.Messages)
					}
				}
				if request.Stream != nil && *request.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13}}`+"\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13}}`)
				}
			})
			body := `{"model":"chat-model","max_tokens":64,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":` + tc.content + `},{"role":"user","content":"continue"}]}`
			for _, countTokens := range []bool{false, true} {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				if countTokens {
					request.URL.Path += "/count_tokens"
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
				}
			}
			if sends.Load() != 2 {
				t.Fatalf("sends = %d, want one per endpoint", sends.Load())
			}
		})
	}
}

func TestAnthropicNativeChatRejectsUnrepresentableThinking(t *testing.T) {
	for _, content := range []string{
		`[{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"answer"}]`,
		`[{"type":"thinking","thinking":"first","signature":"first-signature"},{"type":"thinking","thinking":"second","signature":"second-signature"},{"type":"text","text":"answer"}]`,
	} {
		t.Run(content, func(t *testing.T) {
			var sends atomic.Int32
			h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); w.WriteHeader(http.StatusNoContent) })
			body := `{"model":"chat-model","max_tokens":64,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":` + content + `},{"role":"user","content":"continue"}]}`
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest || sends.Load() != 0 || !strings.Contains(recorder.Body.String(), "thinking") {
				t.Fatalf("status/sends = %d/%d: %s", recorder.Code, sends.Load(), recorder.Body.String())
			}
		})
	}
}
