package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func nativeReasoningBindingRequest(t *testing.T, model string, streaming, tools bool, history ...[]models.ContentBlock) *http.Request {
	t.Helper()
	maxTokens := 64
	request := models.AnthropicRequest{
		Model: model, MaxTokens: &maxTokens, Stream: streaming,
		Messages: []models.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"lookup"`)}},
	}
	if tools {
		request.Tools = []models.AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	}
	for _, turn := range history {
		if turn == nil {
			continue
		}
		content, err := json.Marshal(turn)
		if err != nil {
			t.Fatal(err)
		}
		followup := json.RawMessage(`"continue"`)
		if tools {
			followup = json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_lookup","content":"found"}]`)
		}
		request.Messages = append(request.Messages,
			models.AnthropicMessage{Role: "assistant", Content: content},
			models.AnthropicMessage{Role: "user", Content: followup},
		)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
}

func nativeReasoningBindingRoutesConfig() ProvidersConfig {
	config := ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{
			{ID: "native", Type: string(providerTypeCopilot), Default: true},
		},
	}
	for _, publicModel := range []string{"public-model", "other-model"} {
		config.ModelRoutes = append(config.ModelRoutes, ModelRouteConfig{
			ID: "route-" + publicModel, PublicID: publicModel,
			Endpoints: []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{
				{ID: "primary-target", Provider: "native", UpstreamModel: "physical-primary"},
				{ID: "secondary-target", Provider: "native", UpstreamModel: "physical-secondary"},
			},
			Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
		})
	}
	return config
}

func newNativeReasoningBindingHandler(t *testing.T, primary, secondary http.HandlerFunc) *ProxyHandler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == providerEndpointModels {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"physical-primary","supported_endpoints":["/chat/completions"]},{"id":"physical-secondary","supported_endpoints":["/chat/completions"]}]}`)
			return
		}
		if r.URL.Path != providerEndpointChatCompletions {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var request models.OpenAIRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch request.Model {
		case "physical-primary":
			primary(w, r)
		case "physical-secondary":
			secondary(w, r)
		default:
			t.Errorf("unexpected upstream model: %s", request.Model)
			http.Error(w, "unexpected model", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL), WithProvidersConfig(nativeReasoningBindingRoutesConfig()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func writeNativeReasoningBindingResponse(t *testing.T, w http.ResponseWriter, request models.OpenAIRequest, signature string) {
	t.Helper()
	message := models.OpenAIMessage{Role: "assistant", ReasoningText: "inspect", ReasoningOpaque: signature}
	finish := "stop"
	if len(request.Tools) == 0 {
		message.Content = json.RawMessage(`"answer"`)
	} else {
		finish = "tool_calls"
		index := 0
		message.ToolCalls = []models.OpenAIToolCall{{ID: "call_lookup", Type: "function", Index: &index, Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: "{}"}}}
	}
	usage := &models.OpenAIUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}
	if request.Stream == nil || !*request.Stream {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID: "native-response", Object: "chat.completion", Model: request.Model,
			Choices: []models.OpenAIChoice{{Message: message, FinishReason: &finish}}, Usage: usage,
		}); err != nil {
			t.Error(err)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, delta := range []models.OpenAIMessage{
		{Role: "assistant", ReasoningText: message.ReasoningText, ReasoningOpaque: signature[:len(signature)/2]},
		{ReasoningOpaque: signature[len(signature)/2:]},
		{Content: message.Content, ToolCalls: message.ToolCalls},
	} {
		chunk, err := json.Marshal(models.OpenAIStreamChunk{
			ID: "native-response", Object: "chat.completion.chunk", Model: request.Model,
			Choices: []models.OpenAIStreamChoice{{Delta: delta}},
		})
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
	chunk, _ := json.Marshal(models.OpenAIStreamChunk{
		ID: "native-response", Object: "chat.completion.chunk", Model: request.Model,
		Choices: []models.OpenAIStreamChoice{{FinishReason: &finish}},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	chunk, _ = json.Marshal(models.OpenAIStreamChunk{Model: request.Model, Choices: []models.OpenAIStreamChoice{}, Usage: usage})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
}

func nativeReasoningBindingContent(t *testing.T, response *httptest.ResponseRecorder) []models.ContentBlock {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		var message models.AnthropicResponse
		if err := json.Unmarshal(response.Body.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		return message.Content
	}
	var content []models.ContentBlock
	arguments := map[int]string{}
	stopped := false
	for _, wire := range parseSSEEvents(response.Body.String()) {
		var event models.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(wire.Data), &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "content_block_start":
			if event.Index == nil || *event.Index != len(content) || event.ContentBlock == nil {
				t.Fatalf("invalid block start: %s", wire.Data)
			}
			content = append(content, *event.ContentBlock)
		case "content_block_delta":
			if event.Index == nil || *event.Index >= len(content) || *event.Index < 0 || event.Delta == nil {
				t.Fatalf("invalid block delta: %s", wire.Data)
			}
			block := &content[*event.Index]
			switch event.Delta.Type {
			case "thinking_delta":
				if block.Thinking == nil {
					block.Thinking = new(string)
				}
				*block.Thinking += event.Delta.Thinking
			case "signature_delta":
				block.Signature += event.Delta.Signature
			case "text_delta":
				if block.Text == nil {
					block.Text = new(string)
				}
				*block.Text += event.Delta.Text
			case "input_json_delta":
				arguments[*event.Index] += event.Delta.PartialJSON
			}
		case "message_stop":
			stopped = true
		case "error":
			t.Fatalf("stream error: %s", wire.Data)
		}
	}
	if !stopped {
		t.Fatalf("stream did not finish: %s", response.Body.String())
	}
	for index, value := range arguments {
		content[index].Input = json.RawMessage(value)
	}
	return content
}

func TestNativeReasoningBindingReplayStaysOnIssuingTarget(t *testing.T) {
	for _, mode := range []struct {
		name             string
		streaming, tools bool
	}{
		{name: "json"},
		{name: "forced aggregation", tools: true},
		{name: "streaming", streaming: true, tools: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			var primaryCalls, secondaryCalls atomic.Int32
			primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if primaryCalls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprint(w, `{"error":{"type":"overloaded_error","code":"model_overloaded","message":"temporarily unavailable"}}`)
					return
				}
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-primary")
			})
			secondary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn := secondaryCalls.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if turn == 2 {
					if len(request.Messages) != 3 || request.Messages[1].ReasoningOpaque != "opaque-secondary" || request.Messages[1].ReasoningText != "inspect" {
						t.Errorf("replayed history changed: %+v", request.Messages)
					}
					if mode.tools && (len(request.Messages) != 3 || len(request.Messages[1].ToolCalls) != 1 || request.Messages[2].ToolCallID != "call_lookup") {
						t.Errorf("replayed tool history changed: %+v", request.Messages)
					}
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
			})
			h := newNativeReasoningBindingHandler(t, primary, secondary)
			first := httptest.NewRecorder()
			h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "public-model", mode.streaming, mode.tools, nil))
			history := nativeReasoningBindingContent(t, first)
			if len(history) != 2 || history[0].Type != "thinking" || history[0].Signature != "opaque-secondary" || primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
				t.Fatalf("initial failover did not expose the secondary signature: calls=%d/%d content=%+v", primaryCalls.Load(), secondaryCalls.Load(), history)
			}
			replay := httptest.NewRecorder()
			h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "public-model", mode.streaming, mode.tools, history))
			if replay.Code != http.StatusOK || primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
				t.Fatalf("native signature migrated on replay: status=%d calls=%d/%d body=%s", replay.Code, primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
			}
		})
	}
}

func TestNativeReasoningBindingOwnerFailureDoesNotMigrate(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			var primaryCalls, secondaryCalls atomic.Int32
			primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if primaryCalls.Add(1) > 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprint(w, `{"error":{"type":"overloaded_error","code":"model_overloaded","message":"temporarily unavailable"}}`)
					return
				}
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-primary")
			})
			secondary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
			})
			h := newNativeReasoningBindingHandler(t, primary, secondary)
			first := httptest.NewRecorder()
			h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "public-model", streaming, true))
			history := nativeReasoningBindingContent(t, first)
			if len(history) != 2 || history[0].Signature != "opaque-primary" || primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
				t.Fatalf("initial response did not come from primary: calls=%d/%d content=%+v", primaryCalls.Load(), secondaryCalls.Load(), history)
			}
			replay := httptest.NewRecorder()
			h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "public-model", streaming, true, history))
			if replay.Code != http.StatusServiceUnavailable || primaryCalls.Load() != 2 || secondaryCalls.Load() != 0 {
				t.Fatalf("unavailable signature owner migrated: status=%d calls=%d/%d body=%s", replay.Code, primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
			}
		})
	}
}

func TestNativeReasoningBindingRejectsUnknownConflictingAndWrongRouteState(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	var failPrimary atomic.Bool
	primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		if failPrimary.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"error":{"type":"overloaded_error","code":"model_overloaded","message":"temporarily unavailable"}}`)
			return
		}
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-primary")
	})
	secondary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
	})
	h := newNativeReasoningBindingHandler(t, primary, secondary)
	first := httptest.NewRecorder()
	h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "public-model", false, false))
	primaryHistory := nativeReasoningBindingContent(t, first)
	failPrimary.Store(true)
	second := httptest.NewRecorder()
	h.HandleAnthropicMessages(second, nativeReasoningBindingRequest(t, "public-model", false, false))
	secondaryHistory := nativeReasoningBindingContent(t, second)
	failPrimary.Store(false)
	if len(primaryHistory) != 2 || primaryHistory[0].Signature != "opaque-primary" || len(secondaryHistory) != 2 || secondaryHistory[0].Signature != "opaque-secondary" || primaryCalls.Load() != 2 || secondaryCalls.Load() != 1 {
		t.Fatalf("did not issue signatures from both route targets: calls=%d/%d primary=%+v secondary=%+v", primaryCalls.Load(), secondaryCalls.Load(), primaryHistory, secondaryHistory)
	}
	unknownHistory := append([]models.ContentBlock(nil), primaryHistory...)
	unknownHistory[0].Signature = "opaque-unknown"
	for _, tc := range []struct {
		name, model string
		history     [][]models.ContentBlock
	}{
		{name: "unknown", model: "public-model", history: [][]models.ContentBlock{unknownHistory}},
		{name: "known then unknown", model: "public-model", history: [][]models.ContentBlock{primaryHistory, unknownHistory}},
		{name: "unknown then known", model: "public-model", history: [][]models.ContentBlock{unknownHistory, primaryHistory}},
		{name: "conflicting targets", model: "public-model", history: [][]models.ContentBlock{primaryHistory, secondaryHistory}},
		{name: "wrong route", model: "other-model", history: [][]models.ContentBlock{primaryHistory}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforePrimary, beforeSecondary := primaryCalls.Load(), secondaryCalls.Load()
			response := httptest.NewRecorder()
			h.HandleAnthropicMessages(response, nativeReasoningBindingRequest(t, tc.model, false, false, tc.history...))
			if response.Code != http.StatusBadRequest || primaryCalls.Load() != beforePrimary || secondaryCalls.Load() != beforeSecondary {
				t.Fatalf("invalid native state reached a target: status=%d additional calls=%d/%d body=%s", response.Code, primaryCalls.Load()-beforePrimary, secondaryCalls.Load()-beforeSecondary, response.Body.String())
			}
			for _, signature := range []string{"opaque-primary", "opaque-secondary", "opaque-unknown"} {
				if strings.Contains(response.Body.String(), signature) {
					t.Fatalf("invalid-state error exposed a signature: %s", response.Body.String())
				}
			}
		})
	}
}

func TestNativeReasoningBindingOwnerCooldownDoesNotMigrate(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == providerEndpointModels {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"physical-primary","supported_endpoints":["/chat/completions"]}]}`)
			return
		}
		if r.URL.Path != providerEndpointChatCompletions {
			t.Errorf("unexpected primary path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if primaryCalls.Add(1) > 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":{"code":"user_model_rate_limited","message":"model reset"}}`)
			return
		}
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-primary")
	})
	secondary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
	})
	h := newNativeReasoningBindingHandler(t, primary, secondary)
	now := time.Now()
	h.copilotTraffic.now = func() time.Time { return now }
	first := httptest.NewRecorder()
	h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "public-model", false, true))
	history := nativeReasoningBindingContent(t, first)
	if len(history) != 2 || history[0].Signature != "opaque-primary" || primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
		t.Fatalf("initial response did not come from primary: calls=%d/%d content=%+v", primaryCalls.Load(), secondaryCalls.Load(), history)
	}
	// A separate stateless turn observes the primary's cooldown and can fail over.
	stateless := httptest.NewRecorder()
	h.HandleAnthropicMessages(stateless, nativeReasoningBindingRequest(t, "public-model", false, true))
	if stateless.Code != http.StatusOK || primaryCalls.Load() != 2 || secondaryCalls.Load() != 1 {
		t.Fatalf("stateless turn did not establish cooldown and fail over: status=%d calls=%d/%d body=%s", stateless.Code, primaryCalls.Load(), secondaryCalls.Load(), stateless.Body.String())
	}
	replay := httptest.NewRecorder()
	h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "public-model", false, true, history))
	if replay.Code != http.StatusTooManyRequests || replay.Header().Get("Retry-After") != "86400" || primaryCalls.Load() != 2 || secondaryCalls.Load() != 1 {
		t.Fatalf("cooldown migrated signature state or sent upstream: status=%d retry=%q calls=%d/%d body=%s", replay.Code, replay.Header().Get("Retry-After"), primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
	}
}

func TestNativeReasoningBindingStreamingBlockIsReplayableBeforeEOF(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if primaryCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"error":{"type":"overloaded_error","code":"model_overloaded","message":"temporarily unavailable"}}`)
			return
		}
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-primary")
	})
	secondary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secondaryCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w,
				"data: "+`{"choices":[{"index":0,"delta":{"reasoning_text":"inspect","reasoning_opaque":"opaque-"}}]}`+"\n\n"+
					"data: "+`{"choices":[{"index":0,"delta":{"reasoning_opaque":"secondary"}}]}`+"\n\n"+
					"data: "+`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
				_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
			case <-r.Context().Done():
			}
			return
		}
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
	})
	h := newNativeReasoningBindingHandler(t, primary, secondary)
	front := httptest.NewServer(http.HandlerFunc(h.HandleAnthropicMessages))
	t.Cleanup(front.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inbound := nativeReasoningBindingRequest(t, "public-model", true, false)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/messages", inbound.Body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initial streaming status = %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	thinking := models.ContentBlock{Type: "thinking", Thinking: new(string)}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("thinking block was not exposed before EOF: %v", err)
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event models.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatal(err)
		}
		if event.Index == nil || *event.Index != 0 {
			continue
		}
		if event.Type == "content_block_stop" {
			break
		}
		if event.Type == "content_block_delta" && event.Delta != nil {
			*thinking.Thinking += event.Delta.Thinking
			thinking.Signature += event.Delta.Signature
		}
	}
	if thinking.Signature != "opaque-secondary" || *thinking.Thinking != "inspect" {
		t.Fatalf("stream did not preserve the complete signature block: %+v", thinking)
	}
	replay := httptest.NewRecorder()
	h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "public-model", false, false, []models.ContentBlock{thinking}).WithContext(ctx))
	if replay.Code != http.StatusOK || primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
		t.Fatalf("exposed signature was not bound before EOF: status=%d calls=%d/%d body=%s", replay.Code, primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
	}
}

func TestNativeReasoningBindingStreamingSignatureAfterFinishReason(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"error":{"code":"model_overloaded","message":"temporarily unavailable"}}`)
	})
	secondary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := secondaryCalls.Add(1)
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if turn == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w,
				"data: "+`{"choices":[{"index":0,"delta":{"reasoning_text":"inspect","reasoning_opaque":"opaque-"}}]}`+"\n\n"+
					"data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
					"data: "+`{"choices":[{"index":0,"delta":{"reasoning_opaque":"secondary"}}]}`+"\n\n"+
					"data: [DONE]\n\n")
			return
		}
		if len(request.Messages) != 3 || request.Messages[1].ReasoningOpaque != "opaque-secondary" {
			t.Errorf("replayed signature changed: %+v", request.Messages)
		}
		writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
	})
	h := newNativeReasoningBindingHandler(t, primary, secondary)
	first := httptest.NewRecorder()
	h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "public-model", true, false))
	history := nativeReasoningBindingContent(t, first)
	if len(history) != 1 || history[0].Type != "thinking" || history[0].Signature != "opaque-secondary" {
		t.Fatalf("stream did not preserve the complete thinking signature: %+v", history)
	}
	replay := httptest.NewRecorder()
	h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "public-model", false, false, history))
	if replay.Code != http.StatusOK || primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
		t.Fatalf("exposed signature was not bound after finish reason: status=%d calls=%d/%d body=%s", replay.Code, primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
	}
}

func nativeReasoningBindingRawRequest(t *testing.T, streaming, tools bool, assistant *models.OpenAIMessage, model ...string) *http.Request {
	t.Helper()
	request := models.OpenAIRequest{
		Model: "public-model", Stream: &streaming,
		Messages: []models.OpenAIMessage{{Role: "user", Content: json.RawMessage(`"lookup"`)}},
	}
	if len(model) > 0 {
		request.Model = model[0]
	}
	if tools {
		request.Tools = []models.OpenAITool{{Type: "function", Function: models.OpenAIFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	}
	if assistant != nil {
		followup := models.OpenAIMessage{Role: "user", Content: json.RawMessage(`"continue"`)}
		if tools {
			followup = models.OpenAIMessage{Role: "tool", ToolCallID: "call_lookup", Content: json.RawMessage(`"found"`)}
		}
		request.Messages = append(request.Messages, *assistant, followup)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
}

func nativeReasoningBindingRawMessage(t *testing.T, response *httptest.ResponseRecorder) models.OpenAIMessage {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		var completion models.OpenAIResponse
		if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil {
			t.Fatal(err)
		}
		if len(completion.Choices) != 1 {
			t.Fatalf("unexpected choices: %+v", completion.Choices)
		}
		return completion.Choices[0].Message
	}
	var message models.OpenAIMessage
	var text strings.Builder
	done := false
	for _, wire := range parseSSEEvents(response.Body.String()) {
		if wire.Data == "[DONE]" {
			done = true
			continue
		}
		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(wire.Data), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta.Role != "" {
				message.Role = delta.Role
			}
			message.ReasoningText += delta.ReasoningText
			message.ReasoningOpaque += delta.ReasoningOpaque
			message.ToolCalls = append(message.ToolCalls, delta.ToolCalls...)
			if len(delta.Content) != 0 && string(delta.Content) != "null" {
				var value string
				if err := json.Unmarshal(delta.Content, &value); err != nil {
					t.Fatal(err)
				}
				text.WriteString(value)
			}
		}
	}
	if !done {
		t.Fatalf("Chat stream did not finish: %s", response.Body.String())
	}
	if text.Len() != 0 {
		message.Content, _ = json.Marshal(text.String())
	}
	for index := range message.ToolCalls {
		message.ToolCalls[index].Index = nil
	}
	return message
}

func TestNativeReasoningBindingRawChatReplayStaysOnIssuingTarget(t *testing.T) {
	for _, mode := range []struct {
		name             string
		streaming, tools bool
	}{
		{name: "json"},
		{name: "forced aggregation", tools: true},
		{name: "streaming", streaming: true, tools: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			var primaryCalls, secondaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if primaryCalls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprint(w, `{"error":{"type":"overloaded_error","code":"model_overloaded","message":"temporarily unavailable"}}`)
					return
				}
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-primary")
			}))
			t.Cleanup(primary.Close)
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn := secondaryCalls.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if turn == 2 && (len(request.Messages) != 3 || request.Messages[1].ReasoningOpaque != "opaque-secondary" || request.Messages[1].ReasoningText != "inspect") {
					t.Errorf("raw Chat replay changed native reasoning: %+v", request.Messages)
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-secondary")
			}))
			t.Cleanup(secondary.Close)
			h := newExplicitRouteSurfaceHandler(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, primary.URL, secondary.URL)
			first := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(first, nativeReasoningBindingRawRequest(t, mode.streaming, mode.tools, nil))
			message := nativeReasoningBindingRawMessage(t, first)
			if message.ReasoningOpaque != "opaque-secondary" || message.ReasoningText != "inspect" || primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
				t.Fatalf("initial failover did not expose the secondary signature: calls=%d/%d message=%+v", primaryCalls.Load(), secondaryCalls.Load(), message)
			}
			replay := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(replay, nativeReasoningBindingRawRequest(t, mode.streaming, mode.tools, &message))
			if replay.Code != http.StatusOK || primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
				t.Fatalf("raw native signature migrated on replay: status=%d calls=%d/%d body=%s", replay.Code, primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
			}
			message.ReasoningOpaque = "opaque-unknown"
			unknown := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(unknown, nativeReasoningBindingRawRequest(t, mode.streaming, mode.tools, &message))
			if unknown.Code != http.StatusBadRequest || primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
				t.Fatalf("unknown raw signature reached a target: status=%d calls=%d/%d body=%s", unknown.Code, primaryCalls.Load(), secondaryCalls.Load(), unknown.Body.String())
			}
		})
	}
}

func TestNativeReasoningBindingCredentialRotationRejectsReplayBeforeSend(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		signature := "opaque-primary"
		switch r.Header.Get("Authorization") {
		case "Bearer test-token":
		case "Bearer rotated-token":
			signature = "opaque-rotated"
		default:
			t.Error("upstream received an unexpected credential")
		}
		writeNativeReasoningBindingResponse(t, w, request, signature)
	})
	secondary := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		http.Error(w, "unexpected fallback", http.StatusInternalServerError)
	})
	h := newNativeReasoningBindingHandler(t, primary, secondary)
	first := httptest.NewRecorder()
	h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "public-model", false, true))
	history := nativeReasoningBindingContent(t, first)
	if len(history) != 2 || history[0].Signature != "opaque-primary" || primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
		t.Fatalf("initial response did not issue the original signature: calls=%d/%d content=%+v", primaryCalls.Load(), secondaryCalls.Load(), history)
	}
	h.auth = auth.NewTestAuthenticator("rotated-token")
	replay := httptest.NewRecorder()
	h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "public-model", false, true, history))
	if replay.Code != http.StatusBadRequest || primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
		t.Fatalf("credential rotation replayed old state: status=%d calls=%d/%d body=%s", replay.Code, primaryCalls.Load(), secondaryCalls.Load(), replay.Body.String())
	}
	for _, secret := range []string{"test-token", "rotated-token", "opaque-primary"} {
		if strings.Contains(replay.Body.String(), secret) {
			t.Fatalf("credential rejection exposed state: %s", replay.Body.String())
		}
	}
	stateless := httptest.NewRecorder()
	h.HandleAnthropicMessages(stateless, nativeReasoningBindingRequest(t, "public-model", false, true))
	freshHistory := nativeReasoningBindingContent(t, stateless)
	if len(freshHistory) != 2 || freshHistory[0].Signature != "opaque-rotated" || primaryCalls.Load() != 2 || secondaryCalls.Load() != 0 {
		t.Fatalf("stateless request did not use the new credential: calls=%d/%d content=%+v", primaryCalls.Load(), secondaryCalls.Load(), freshHistory)
	}
}
