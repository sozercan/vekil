package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

func TestHandleOpenAIChatCompletionsResponsesBackedNonStreamingText(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != providerEndpointResponses {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Result().Body)
		t.Fatalf("status = %d body=%s", rec.Code, body)
	}
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "gpt-public" || len(response.Choices) != 1 || response.Choices[0].Message.Content != "Synthetic fixture text response." || response.Choices[0].FinishReason != "stop" {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandleOpenAIChatCompletionsResponsesBackedStreamingText(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`))
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) || !strings.Contains(body, `Synthetic fixture `) || !strings.Contains(body, `text response.`) || !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, `"prompt_tokens":11`) || strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("stream body = %s", body)
	}
}

func TestHandleOpenAIChatCompletionsResponsesBackedStreamingToolCallOmitsUnchangedName(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_one_tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"gpt-public",
		"messages":[{"role":"user","content":"use lookup"}],
		"stream":true,
		"tools":[{"type":"function","function":{"name":"lookup_synthetic_widget","parameters":{"type":"object"}}}]
	}`))
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var sawStart, sawArguments, sawToolFinish bool
	doneCount := 0
	for _, event := range parseSSEEvents(rec.Body.String()) {
		if event.Data == "[DONE]" {
			doneCount++
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function map[string]json.RawMessage `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			t.Fatalf("decode stream event: %v\nevent=%s", err, event.Data)
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
				sawToolFinish = true
			}
			for _, call := range choice.Delta.ToolCalls {
				rawName, hasName := call.Function["name"]
				name := ""
				if hasName {
					if err := json.Unmarshal(rawName, &name); err != nil {
						t.Fatalf("decode tool name: %v", err)
					}
					if name == "" {
						t.Fatalf("streamed tool-call delta contains an empty function name: %s", event.Data)
					}
				}
				rawArguments, hasArguments := call.Function["arguments"]
				if !hasArguments {
					if name == "lookup_synthetic_widget" {
						t.Fatalf("initial tool-call delta is missing arguments: %s", event.Data)
					}
					continue
				}
				var arguments string
				if err := json.Unmarshal(rawArguments, &arguments); err != nil {
					t.Fatalf("decode tool arguments: %v", err)
				}
				if name == "lookup_synthetic_widget" {
					if arguments != "" {
						t.Fatalf("initial tool-call delta arguments = %q, want empty", arguments)
					}
					sawStart = true
				}
				if arguments == `{"widget":"alpha-fixture"}` {
					if hasName {
						t.Fatalf("argument-only tool-call delta contains function name: %s", event.Data)
					}
					sawArguments = true
				}
			}
		}
	}
	if !sawStart || !sawArguments || !sawToolFinish || doneCount != 1 {
		t.Fatalf("stream state start=%t arguments=%t tool_finish=%t done=%d\nbody=%s", sawStart, sawArguments, sawToolFinish, doneCount, rec.Body.String())
	}
}

func TestHandleOpenAIChatCompletionsResponsesBackedForcedStreamToolCall(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_one_tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request["stream"] != true {
			t.Errorf("upstream stream = %#v", request["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	requestBody := `{
		"model":"gpt-public",
		"messages":[{"role":"user","content":"use lookup"}],
		"tools":[{"type":"function","function":{"name":"lookup_synthetic_widget","parameters":{"type":"object","properties":{"widget":{"type":"string"}},"required":["widget"],"additionalProperties":false}}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(requestBody))
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response models.OpenAIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) != 1 || response.Choices[0].FinishReason == nil || *response.Choices[0].FinishReason != "tool_calls" || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("response = %#v", response)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if !strings.HasPrefix(call.ID, responsesChatReplayCallIDPrefix) || call.Function.Name != "lookup_synthetic_widget" || call.Function.Arguments != `{"widget":"alpha-fixture"}` {
		t.Fatalf("call = %#v", call)
	}
}

func TestHandleOpenAIChatCompletionsResponsesBackedParallelToolContinuation(t *testing.T) {
	firstFixture, err := os.ReadFile("testdata/chat_over_responses/stream_parallel_interleaved_tool_calls.sse")
	if err != nil {
		t.Fatal(err)
	}
	secondFixture, err := os.ReadFile("testdata/chat_over_responses/stream_reasoning_message_continuation.sse")
	if err != nil {
		t.Fatal(err)
	}
	requestNumber := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		var request struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requestNumber == 1 {
			_, _ = w.Write(firstFixture)
			return
		}
		var callIDs, outputIDs []string
		for _, item := range request.Input {
			switch item["type"] {
			case "function_call":
				callIDs = append(callIDs, item["call_id"].(string))
			case "function_call_output":
				outputIDs = append(outputIDs, item["call_id"].(string))
			}
		}
		if !reflect.DeepEqual(callIDs, []string{"call_synth_parallel_alpha_stream_001", "call_synth_parallel_beta_stream_001"}) || !reflect.DeepEqual(outputIDs, callIDs) {
			t.Fatalf("replayed call/output IDs = %v / %v", callIDs, outputIDs)
		}
		_, _ = w.Write(secondFixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "lookup_synthetic_widget", "parameters": map[string]any{"type": "object"}}},
		{"type": "function", "function": map[string]any{"name": "inspect_synthetic_region", "parameters": map[string]any{"type": "object"}}},
	}
	firstRequest, _ := json.Marshal(map[string]any{
		"model": "gpt-public", "messages": []any{map[string]any{"role": "user", "content": "run both"}}, "tools": tools,
	})
	firstRecorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(firstRecorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(firstRequest)))
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}
	var first models.OpenAIResponse
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	calls := first.Choices[0].Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("first calls = %#v", calls)
	}
	secondRequest, _ := json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{"role": "assistant", "content": first.Choices[0].Message.Content, "tool_calls": calls},
			map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": "alpha result"},
			map[string]any{"role": "tool", "tool_call_id": calls[1].ID, "content": "beta result"},
		},
		"tools": tools,
	})
	secondRecorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(secondRecorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(secondRequest)))
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}
	var second models.OpenAIResponse
	if err := json.Unmarshal(secondRecorder.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if string(second.Choices[0].Message.Content) != `"Synthetic continuation finished."` {
		t.Fatalf("second response = %#v", second)
	}
}

func TestHandleAnthropicMessagesResponsesBackedNonStreamingText(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-public","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "gpt-public" || response.StopReason == nil || *response.StopReason != "end_turn" || len(response.Content) != 1 || response.Content[0].Text == nil || *response.Content[0].Text != "Synthetic fixture text response." {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandleAnthropicMessagesResponsesBackedStreamingText(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-public","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `event: message_start`) || !strings.Contains(body, `Synthetic fixture `) || !strings.Contains(body, `text response.`) || !strings.Contains(body, `event: message_stop`) {
		t.Fatalf("stream = %s", body)
	}
}

func TestHandleAnthropicCountTokensResponsesBacked(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["max_output_tokens"] != float64(responsesChatMinimumOutputTokens) || request["stream"] != false {
			t.Fatalf("probe request = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewBufferString(`{"model":"gpt-public","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls)
	}
	var response models.AnthropicCountTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.InputTokens != 11 {
		t.Fatalf("input_tokens = %d", response.InputTokens)
	}
}

func TestHandleGeminiGenerateContentResponsesBacked(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-public:generateContent", bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	rec := httptest.NewRecorder()
	h.HandleGeminiModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response models.GeminiGenerateContentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Candidates) != 1 || response.Candidates[0].Content == nil || len(response.Candidates[0].Content.Parts) != 1 || response.Candidates[0].Content.Parts[0].Text == nil || *response.Candidates[0].Content.Parts[0].Text != "Synthetic fixture text response." {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandleGeminiStreamGenerateContentResponsesBacked(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-public:streamGenerateContent", bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	rec := httptest.NewRecorder()
	h.HandleGeminiModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `Synthetic fixture `) || !strings.Contains(body, `text response.`) || !strings.Contains(body, `finishReason`) {
		t.Fatalf("stream = %s", body)
	}
}

func TestHandleGeminiCountTokensResponsesBacked(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-public:countTokens", bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	rec := httptest.NewRecorder()
	h.HandleGeminiModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d", upstreamCalls)
	}
	var response models.GeminiCountTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TotalTokens != 11 {
		t.Fatalf("totalTokens = %d response=%#v", response.TotalTokens, response)
	}
}

func TestHandleOpenAIChatCompletionsResponsesBackedDropsInjectedUsage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"prompt_tokens"`) {
		t.Fatalf("client received proxy-injected usage chunk: %s", rec.Body.String())
	}
}

func TestHandleOpenAIChatCompletionsResponsesReplayMissingAfterRestart(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	callID := "call_vekil_AAAAAAAAAAAAAAAAAAAAAA"
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{}`}}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result"},
		},
	})
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d", upstreamCalls)
	}
	var response struct {
		Error struct {
			Type, Code, Param, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Type != "invalid_request_error" || response.Error.Code != "responses_replay_state_missing" || response.Error.Param != "messages" || response.Error.Message != "Responses-backed tool state is no longer available; restart the assistant tool-call turn." {
		t.Fatalf("error = %#v", response.Error)
	}
}

func TestHandleOpenAIChatCompletionsResponsesPrecommitFailurePreservesSafeHeaders(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_immediate_failure.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Set-Cookie", "secret=hidden")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}],"stream":true}`)))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "3" || len(rec.Header().Values("Retry-After")) != 1 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("status/headers = %d %#v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestResponsesBackedCountTokensPreservesMissingUsageSemantics(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(fixture, &payload); err != nil {
		t.Fatal(err)
	}
	delete(payload, "usage")
	fixture, _ = json.Marshal(payload)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	t.Run("anthropic errors", func(t *testing.T) {
		h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
		rec := httptest.NewRecorder()
		h.HandleAnthropicMessagesCountTokens(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`)))
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "did not include usage") {
			t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("gemini estimates", func(t *testing.T) {
		h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
		rec := httptest.NewRecorder()
		h.HandleGeminiModels(rec, httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-public:countTokens", bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
		}
		var response models.GeminiCountTokensResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.TotalTokens <= 0 {
			t.Fatalf("response = %#v err=%v", response, err)
		}
	})
}

func TestHandleOpenAIChatCompletionsCanonicalizesNonJSONResponsesError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Set-Cookie", "fixture=discarded")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "plain upstream failure")
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`)))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("Set-Cookie") != "" || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("status/headers/body = %d %#v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	var response struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if response.Error.Type != "server_error" || !strings.Contains(response.Error.Message, "plain upstream failure") {
		t.Fatalf("response = %#v", response)
	}
}

func TestResponsesSemanticFailureRetainsRouteAndRequestMetadata(t *testing.T) {
	body, err := os.ReadFile("testdata/chat_over_responses/nonstream_immediate_failure.json")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-semantic-failure")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`))
	ctx, summary := WithRequestSummary(req.Context())
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, req.WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	fields := map[string]any{}
	for _, field := range summary.LoggerFields() {
		fields[field.Key] = field.Value
	}
	if fields["provider"] != "test-provider" || fields["provider_kind"] != "openai-compatible" || fields["upstream_request_id"] != "req-semantic-failure" {
		t.Fatalf("summary fields = %#v", fields)
	}
}

func TestHandleAnthropicMessagesResponsesHTTPErrorPreservesSafeHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "4")
		w.Header().Set("X-Request-Id", "req-anthropic-error")
		w.Header().Set("Set-Cookie", "fixture=discarded")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"too_many_requests","message":"slow down"}}`)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-public","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "4" || rec.Header().Get("X-Request-Id") != "req-anthropic-error" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("status/headers/body = %d %#v %s", rec.Code, rec.Header(), rec.Body.String())
	}
}
