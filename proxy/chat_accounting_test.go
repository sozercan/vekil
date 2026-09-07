package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func nativeChatAccountingStream() string {
	return "data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"reasoning_tokens":3}}` + "\n\n" +
		"data: " + `{"id":"chat-extensions","object":"chat.completion.chunk","model":"physical-terminal","choices":[],"copilot_usage":{"total_nano_aiu":27,"compute_units":2,"token_details":[{"model":"physical-terminal","token_type":"input","token_count":12,"batch_size":1000000,"cost_per_batch":30}]}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func TestNativeChatForcedStreamPreservesAccounting(t *testing.T) {
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
		_, _ = io.WriteString(w, nativeChatAccountingStream())
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
	if message.Role != "assistant" || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_lookup" {
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

func TestPolicyChatPreservesAccountingAndPublicIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, nativeChatAccountingStream())
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
			for _, want := range []string{`"copilot_usage"`, `"total_nano_aiu":27`, `"model":"coding-economy"`} {
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

func TestPolicySanitizedChatAcceptsAccounting(t *testing.T) {
	body := newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(nativeChatAccountingStream())))
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
	} {
		if recognizedPolicyOpenAIStreamChunk("", malformed) {
			t.Fatalf("accepted malformed extension chunk: %s", malformed)
		}
	}
}

func TestNativeChatAccountingProgressPreventsFailover(t *testing.T) {
	const frame = `{"choices":[],"copilot_usage":{"total_nano_aiu":27}}`
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
