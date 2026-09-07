package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func nativeChatExtensionStream() string {
	return "data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[{"index":0,"delta":{"role":"assistant","reasoning_text":"inspect "}}]}` + "\n\n" +
		"data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[{"index":0,"delta":{"reasoning_text":"the input","reasoning_opaque":"opaque-"}}]}` + "\n\n" +
		"data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[{"index":0,"delta":{"reasoning_opaque":"signature","tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"reasoning_tokens":3}}` + "\n\n" +
		"data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[],"copilot_usage":{"total_nano_aiu":27,"compute_units":2,"token_details":[{"model":"physical-terminal","token_type":"input","token_count":12,"batch_size":1000000,"cost_per_batch":30}]}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func TestNativeChatForcedStreamPreservesReasoningAndAccounting(t *testing.T) {
	var requests atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.URL.Path != providerEndpointChatCompletions || request.Stream == nil || !*request.Stream {
			t.Errorf("upstream path/stream = %q/%v", r.URL.Path, request.Stream)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Copilot-Service-Request-Id", "service-attempt")
		w.Header().Set("X-Quota-Snapshot-Chat", "remaining=10")
		_, _ = io.WriteString(w, nativeChatExtensionStream())
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, request)
	if recorder.Code != http.StatusOK || requests.Load() != 1 {
		t.Fatalf("status/requests = %d/%d: %s", recorder.Code, requests.Load(), recorder.Body.String())
	}
	var response models.OpenAIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) != 1 {
		t.Fatalf("choices = %+v", response.Choices)
	}
	message := response.Choices[0].Message
	if message.ReasoningText != "inspect the input" || message.ReasoningOpaque != "opaque-signature" || len(message.ToolCalls) != 1 {
		t.Fatalf("aggregated message = %+v", message)
	}
	if response.Usage == nil || response.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	var usage struct {
		TotalNanoAIU int               `json:"total_nano_aiu"`
		ComputeUnits int               `json:"compute_units"`
		TokenDetails []json.RawMessage `json:"token_details"`
	}
	if err := json.Unmarshal(response.CopilotUsage, &usage); err != nil || usage.TotalNanoAIU != 27 || usage.ComputeUnits != 2 || len(usage.TokenDetails) != 1 {
		t.Fatalf("accounting = %s, error = %v", response.CopilotUsage, err)
	}
	if recorder.Header().Get("X-Copilot-Service-Request-Id") != "service-attempt" || recorder.Header().Get("X-Quota-Snapshot-Chat") != "remaining=10" {
		t.Fatalf("converted headers = %v", recorder.Header())
	}
}

func TestNativeChatAnthropicNonStreamingPreservesReasoning(t *testing.T) {
	for _, signatureOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "thinking and signature", true: "signature only"}[signatureOnly], func(t *testing.T) {
			wantThinking := "inspect the input"
			if signatureOnly {
				wantThinking = ""
			}
			var sends atomic.Int32
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				turn := sends.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if r.URL.Path != providerEndpointChatCompletions || request.Stream == nil || !*request.Stream {
					t.Errorf("upstream path/stream = %q/%v", r.URL.Path, request.Stream)
				}
				if turn == 2 {
					if len(request.Messages) != 3 {
						t.Errorf("replayed messages = %+v", request.Messages)
					} else {
						assistant, result := request.Messages[1], request.Messages[2]
						if assistant.Role != "assistant" || assistant.ReasoningText != wantThinking || assistant.ReasoningOpaque != "opaque-signature" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_lookup" {
							t.Errorf("replayed assistant lost native reasoning or tool call: %+v", assistant)
						}
						if result.Role != "tool" || result.ToolCallID != "call_lookup" || jsonRawString(result.Content) != "found" {
							t.Errorf("replayed tool result changed: %+v", result)
						}
					}
				}
				stream := nativeChatExtensionStream()
				if signatureOnly {
					stream = strings.ReplaceAll(stream, `"reasoning_text":"inspect "`, `"reasoning_text":""`)
					stream = strings.ReplaceAll(stream, `"reasoning_text":"the input"`, `"reasoning_text":""`)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
			})
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"chat-model","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)))
			if recorder.Code != http.StatusOK || sends.Load() != 1 {
				t.Fatalf("status/sends = %d/%d: %s", recorder.Code, sends.Load(), recorder.Body.String())
			}
			var response models.AnthropicResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Content) != 2 {
				t.Fatalf("aggregated content = %s, want thinking followed by tool use", recorder.Body.String())
			}
			thinking, tool := response.Content[0], response.Content[1]
			if thinking.Type != "thinking" || thinking.Thinking == nil || *thinking.Thinking != wantThinking || thinking.Signature != "opaque-signature" {
				t.Fatalf("native reasoning changed: %+v", thinking)
			}
			if tool.Type != "tool_use" || tool.ID != "call_lookup" || tool.Name != "lookup" || string(tool.Input) != "{}" {
				t.Fatalf("tool result changed: %+v", tool)
			}
			if response.Model != "chat-model" || response.StopReason == nil || *response.StopReason != "tool_use" || response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 5 {
				t.Fatalf("aggregated response contract changed: %+v", response)
			}
			history, err := json.Marshal(response.Content)
			if err != nil {
				t.Fatal(err)
			}
			followup := `{"model":"chat-model","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":` + string(history) + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_lookup","content":"found"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
			recorder = httptest.NewRecorder()
			h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(followup)))
			if recorder.Code != http.StatusOK || sends.Load() != 2 {
				t.Fatalf("follow-up status/sends = %d/%d: %s", recorder.Code, sends.Load(), recorder.Body.String())
			}
		})
	}
}

func TestNativeChatAnthropicStreamPreservesReasoningBlocks(t *testing.T) {
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != providerEndpointChatCompletions {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		stream := buildSSEStream(
			`{"choices":[{"index":0,"delta":{"reasoning_text":"inspect "}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_text":"input","reasoning_opaque":"opaque-"}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_opaque":"signature","content":"answer"}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_text":"verify","reasoning_opaque":"second-signature"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_text":"finish","reasoning_opaque":"last-signature"},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		)
		defer func() { _ = stream.Close() }()
		_, _ = io.Copy(w, stream)
	})
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"chat-model","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	type block struct{ kind, text, signature string }
	var blocks []block
	openIndex := -1
	stopped := false
	for _, wire := range parseSSEEvents(recorder.Body.String()) {
		var event models.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(wire.Data), &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "content_block_start":
			if openIndex != -1 || event.Index == nil || *event.Index != len(blocks) || event.ContentBlock == nil {
				t.Fatalf("invalid block ordering: %s", recorder.Body.String())
			}
			openIndex = *event.Index
			blocks = append(blocks, block{kind: event.ContentBlock.Type})
		case "content_block_delta":
			if event.Index == nil || *event.Index != openIndex || openIndex < 0 || event.Delta == nil {
				t.Fatalf("delta outside open block: %s", wire.Data)
			}
			switch event.Delta.Type {
			case "thinking_delta":
				if blocks[openIndex].kind != "thinking" {
					t.Fatalf("thinking delta in %s block", blocks[openIndex].kind)
				}
				blocks[openIndex].text += event.Delta.Thinking
			case "signature_delta":
				if blocks[openIndex].kind != "thinking" {
					t.Fatalf("signature delta in %s block", blocks[openIndex].kind)
				}
				blocks[openIndex].signature += event.Delta.Signature
			case "text_delta":
				blocks[openIndex].text += event.Delta.Text
			case "input_json_delta":
				blocks[openIndex].text += event.Delta.PartialJSON
			}
		case "content_block_stop":
			if event.Index == nil || *event.Index != openIndex || openIndex < 0 {
				t.Fatalf("stop outside open block: %s", wire.Data)
			}
			openIndex = -1
		case "message_stop":
			stopped = true
		case "error":
			t.Fatalf("stream failed: %s", wire.Data)
		}
	}
	want := []block{
		{kind: "thinking", text: "inspect input", signature: "opaque-signature"},
		{kind: "text", text: "answer"},
		{kind: "thinking", text: "verify", signature: "second-signature"},
		{kind: "tool_use", text: "{}"},
		{kind: "thinking", text: "finish", signature: "last-signature"},
	}
	if !reflect.DeepEqual(blocks, want) || openIndex != -1 || !stopped {
		t.Fatalf("reasoning stream = %+v, open=%d stopped=%t; want %+v", blocks, openIndex, stopped, want)
	}
}

func TestNativeChatReasoningUsageReachesClientLedger(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			details *models.OpenAICompletionTokensDetails
			want    int
		}{
			{name: "top level", want: 3},
			{name: "nested precedence", details: &models.OpenAICompletionTokensDetails{ReasoningTokens: 2}, want: 2},
			{name: "empty nested fallback", details: &models.OpenAICompletionTokensDetails{}, want: 3},
		} {
			t.Run(tc.name+map[bool]string{false: " JSON", true: " SSE"}[stream], func(t *testing.T) {
				usage := &models.OpenAIUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17, ReasoningTokens: 3, CompletionTokensDetails: tc.details}
				h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+mustMarshal(t, models.OpenAIStreamChunk{Usage: usage})+"\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(models.OpenAIResponse{Usage: usage})
				})
				h.stats = newStatsCollector()
				ctx, summary := WithRequestSummary(context.Background())
				body := mustMarshal(t, map[string]any{"model": "chat-model", "stream": stream, "messages": []map[string]string{{"role": "user", "content": "hello"}}})
				recorder := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx))
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
				}
				if got := readSummaryForStats(summary).reasoning; got != tc.want {
					t.Fatalf("summary reasoning tokens = %d, want %d", got, tc.want)
				}
				h.RecordRequest(summary, recorder.Code, "test", 0)
				if got := h.stats.snapshot().Totals.ReasoningTokens; got != int64(tc.want) {
					t.Fatalf("client ledger reasoning tokens = %d, want %d", got, tc.want)
				}
			})
		}
	}
}

func TestPolicyChatExtensionsPreserveAccountingAndPublicIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, nativeChatExtensionStream())
			}))
			defer upstream.Close()
			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
				WithPolicyRoutingMode(PolicyRoutingModeOff))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			body, _ := json.Marshal(map[string]any{
				"model": "coding-economy", "stream": stream,
				"messages": []any{map[string]any{"role": "user", "content": "lookup"}},
				"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
			})
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
			if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "event: error") {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			for _, want := range []string{`"reasoning_text"`, `"reasoning_opaque"`, `"copilot_usage"`, `"total_nano_aiu":27`, `"model":"coding-economy"`} {
				if !strings.Contains(recorder.Body.String(), want) {
					t.Fatalf("response lost %s: %s", want, recorder.Body.String())
				}
			}
			if strings.Contains(recorder.Body.String(), "physical-terminal") {
				t.Fatalf("response leaked terminal identity: %s", recorder.Body.String())
			}
		})
	}
}

func TestPolicySanitizedChatAcceptsReasoningAndAccounting(t *testing.T) {
	body := newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(nativeChatExtensionStream())))
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil || strings.Contains(string(got), "event: error") || !strings.Contains(string(got), `"copilot_usage"`) {
		t.Fatalf("sanitized stream = %s, error = %v", got, err)
	}
	for _, malformed := range []string{
		`{"choices":[],"copilot_usage":"accounting"}`,
		`{"choices":[],"copilot_usage":{},"Copilot_Usage":{}}`,
		`{"choices":[],"copilot_usage":{"token_details":[{"Model":"physical-terminal"}]}}`,
		`{"choices":[],"copilot_usage":{"Token_Details":[{"model":"physical-terminal"}]}}`,
		`{"choices":[],"copilot_usage":{"token_details":[{"model":{"id":"physical-terminal"}}]}}`,
		`{"choices":[],"copilot_usage":{"token_details":[{"model":"a","model":"b"}]}}`,
		`{"choices":[],"copilot_usage":{"total_nano_aiu":"physical-terminal"}}`,
		`{"choices":[],"copilot_usage":{"compute_units":-1}}`,
		`{"choices":[{"index":0,"delta":{"reasoning_opaque":42}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_opaque":"sig","Reasoning_Opaque":null}}]}`,
	} {
		if recognizedPolicyOpenAIStreamChunk("", malformed) {
			t.Fatalf("accepted malformed extension chunk: %s", malformed)
		}
	}
}

func TestNativeChatExtensionProgressPreventsFailover(t *testing.T) {
	for _, frame := range []string{
		`{"choices":[{"index":0,"delta":{"reasoning_opaque":"signature"}}]}`,
		`{"choices":[],"copilot_usage":{"total_nano_aiu":27}}`,
	} {
		t.Run(frame, func(t *testing.T) {
			var secondaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+frame+"\n\nevent: error\ndata: "+`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`+"\n\n")
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondaryCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer secondary.Close()
			h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)))
			if recorder.Code != http.StatusTooManyRequests || secondaryCalls.Load() != 0 {
				t.Fatalf("status/secondary = %d/%d: %s", recorder.Code, secondaryCalls.Load(), recorder.Body.String())
			}
		})
	}
}

func TestNativeChatAccountingObservedBeforeStreamFailure(t *testing.T) {
	var accounting json.RawMessage
	body := io.NopCloser(strings.NewReader("data: " + `{"choices":[],"copilot_usage":{"total_nano_aiu":27}}` + "\n\nevent: error\ndata: " + `{"error":{"type":"rate_limit_error","message":"slow down"}}` + "\n\n"))
	recorder := httptest.NewRecorder()
	streamOpenAIChatPassthroughWithLifecycle(recorder, body, "public-model", false, nil, nil,
		streamLifecycleHooks{onCopilotUsage: func(raw json.RawMessage) { accounting = append(json.RawMessage(nil), raw...) }})
	if !strings.Contains(string(accounting), `"total_nano_aiu":27`) {
		t.Fatalf("accounting callback = %s", accounting)
	}
}

func TestNativeChatCombinedAccountingSurvivesInjectedUsageFiltering(t *testing.T) {
	for _, dropUsage := range []bool{false, true} {
		t.Run(map[bool]string{false: "requested usage", true: "injected usage"}[dropUsage], func(t *testing.T) {
			stream := "data: " + `{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n" +
				"data: " + `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"copilot_usage":{"total_nano_aiu":27}}` + "\n\ndata: [DONE]\n\n"
			recorder := httptest.NewRecorder()
			streamOpenAIChatPassthroughWithLifecycle(recorder, io.NopCloser(strings.NewReader(stream)), "public-model", dropUsage, nil, nil, streamLifecycleHooks{})
			got := recorder.Body.String()
			if !strings.Contains(got, `"copilot_usage":{"total_nano_aiu":27}`) || !strings.Contains(got, "data: [DONE]") {
				t.Fatalf("stream lost accounting or completion: %s", got)
			}
			if strings.Contains(got, `"usage":`) == dropUsage {
				t.Fatalf("drop usage = %v, stream = %s", dropUsage, got)
			}
		})
	}
}

func TestCanonicalChatCombinedAccountingSurvivesUsageFiltering(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	chunk := models.OpenAIStreamChunk{
		Choices:      []models.OpenAIStreamChoice{},
		Usage:        &models.OpenAIUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		CopilotUsage: json.RawMessage(`{"total_nano_aiu":27}`),
	}
	if err := writer.sendChunk(chunk); err != nil {
		t.Fatal(err)
	}
	if err := writer.succeed(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	var observed json.RawMessage
	if err := streamChatEventsToOpenAI(recorder, stream, chatStreamEventCallbacks{
		DropUsage:      true,
		OnCopilotUsage: func(raw json.RawMessage) { observed = append(json.RawMessage(nil), raw...) },
	}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"copilot_usage":{"total_nano_aiu":27}`) || strings.Contains(got, `"usage":`) || string(observed) != string(chunk.CopilotUsage) {
		t.Fatalf("filtered canonical stream = %s, observed = %s", got, observed)
	}
}

func TestNativeChatOptimizedResponseObservesAccounting(t *testing.T) {
	for _, choices := range []string{
		`[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]`,
		`"invalid-choice-shape"`,
	} {
		t.Run(choices, func(t *testing.T) {
			h := &ProxyHandler{log: logger.NewWithWriter(logger.LevelError, io.Discard)}
			configureRecordingToolOptimizer(h, &recordingToolOptimizer{})
			ctx, summary := WithRequestSummary(context.Background())
			body := `{"id":"chat-usage","object":"chat.completion","created":1,"model":"chat-model","choices":` + choices + `,"copilot_usage":{"total_nano_aiu":27,"compute_units":2}}`
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
			recorder := httptest.NewRecorder()
			if err := h.maybeWriteOptimizedOpenAIChatPassthrough(ctx, recorder, resp, "chat-model", h.toolContexts, "session:accounting"); err != nil {
				t.Fatal(err)
			}
			summary.mu.Lock()
			usage := summary.copilotUsage
			summary.mu.Unlock()
			if usage.TotalNanoAIU != 27 || usage.ComputeUnits != 2 || !strings.Contains(recorder.Body.String(), `"copilot_usage"`) {
				t.Fatalf("accounting = %+v, response = %s", usage, recorder.Body.String())
			}
		})
	}
}
