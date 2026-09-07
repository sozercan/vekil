package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/models"
)

func nativeReasoningBoundaryChunks(boundary string) []string {
	return []string{
		`{"choices":[{"index":0,"delta":{"reasoning_text":"inspect","reasoning_opaque":"first-"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_opaque":"signature",` + boundary + `}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_text":"verify","reasoning_opaque":"second-signature"},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`,
	}
}

func TestNativeChatForcedAggregationRejectsIndependentSignatures(t *testing.T) {
	for _, boundary := range []struct{ name, delta string }{
		{"text", `"content":"answer"`},
		{"refusal", `"refusal":"cannot answer"`},
		{"tool", `"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]`},
	} {
		for _, explicit := range []bool{false, true} {
			for _, anthropic := range []bool{false, true} {
				name := boundary.name + map[bool]string{false: "/legacy", true: "/explicit"}[explicit] + map[bool]string{false: "/chat", true: "/messages"}[anthropic]
				t.Run(name, func(t *testing.T) {
					var primaryCalls, secondaryCalls atomic.Int32
					backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						primaryCalls.Add(1)
						var request models.OpenAIRequest
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if request.Stream == nil || !*request.Stream {
							t.Error("upstream request was not force-streamed")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						body := buildSSEStream(append(nativeReasoningBoundaryChunks(boundary.delta), "[DONE]")...)
						defer func() { _ = body.Close() }()
						_, _ = io.Copy(w, body)
					})
					var h *ProxyHandler
					if explicit {
						primary := httptest.NewServer(backend)
						t.Cleanup(primary.Close)
						secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							secondaryCalls.Add(1)
							w.WriteHeader(http.StatusNoContent)
						}))
						t.Cleanup(secondary.Close)
						h = newExplicitRouteSurfaceHandler(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, primary.URL, secondary.URL)
					} else {
						h = newTestProxyHandler(t, backend)
					}
					w := httptest.NewRecorder()
					if anthropic {
						h.HandleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"public-model","max_tokens":64,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)))
					} else {
						h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)))
					}
					if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "multiple native reasoning blocks") {
						t.Fatalf("status/body = %d/%s, want explicit aggregation failure", w.Code, w.Body.String())
					}
					if primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
						t.Fatalf("unsafe retry after reasoning: primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
					}
					for _, invalidOutput := range []string{"first-signature", "second-signature", "call_lookup"} {
						if strings.Contains(w.Body.String(), invalidOutput) {
							t.Fatalf("unsupported response exposed tool history: %s", w.Body.String())
						}
					}
				})
			}
		}
	}
}

func TestNativeReasoningAggregationPreservesRepresentableFragments(t *testing.T) {
	for _, tc := range []struct {
		name, first, between, last, thinking, signature string
	}{
		{"fragments", `"reasoning_text":"inspect ","reasoning_opaque":"first-"`, `"content":"","refusal":null`, `"reasoning_text":"input","reasoning_opaque":"signature","content":"answer"`, "inspect input", "first-signature"},
		{"signature only", `"reasoning_opaque":"first-"`, `"content":null`, `"reasoning_opaque":"signature"`, "", "first-signature"},
		{"unsigned native text", `"reasoning_text":"inspect "`, `"content":"answer"`, `"reasoning_text":"input"`, "inspect input", ""},
		{"other reasoning fields", `"reasoning_content":"inspect"`, `"content":"answer"`, `"reasoning_content":"verify"`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := buildSSEStream(
				`{"choices":[{"index":0,"delta":{`+tc.first+`}}]}`,
				`{"choices":[{"index":0,"delta":{`+tc.between+`}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`,
				`{"choices":[{"index":0,"delta":{`+tc.last+`},"finish_reason":"stop"}]}`,
				"[DONE]",
			)
			response, err := aggregateStreamToResponse(body)
			if err != nil || response == nil || len(response.Choices) != 1 {
				t.Fatalf("response/error = %+v/%v", response, err)
			}
			message := response.Choices[0].Message
			if message.ReasoningText != tc.thinking || message.ReasoningOpaque != tc.signature {
				t.Fatalf("native reasoning changed: %+v", message)
			}
		})
	}
}

func TestCanonicalChatAggregationRejectsIndependentNativeSignatures(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic", true: "policy"}[policy], func(t *testing.T) {
			writer, stream := newChatStreamEventPipe(context.Background())
			for _, raw := range nativeReasoningBoundaryChunks(`"content":"answer"`) {
				var chunk models.OpenAIStreamChunk
				if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
					t.Fatal(err)
				}
				if err := writer.sendChunk(chunk); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.succeed(); err != nil {
				t.Fatal(err)
			}
			aggregate := aggregateChatStreamEvents
			if policy {
				aggregate = aggregatePolicyChatStreamEvents
			}
			response, err := aggregate(stream)
			var executionErr *chatExecutionError
			if response != nil || !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusBadGateway || executionErr.Code != "unsupported_native_reasoning_blocks" {
				t.Fatalf("response/error = %+v/%v, want native reasoning rejection", response, err)
			}
			if executionErr.Usage == nil || executionErr.Usage.TotalTokens != 17 {
				t.Fatalf("aggregation error lost terminal usage: %+v", executionErr)
			}
		})
	}
}

func TestNativeReasoningAggregationRejectsPartiallySignedBlocks(t *testing.T) {
	for _, tc := range []struct{ name, first, last string }{
		{"signature only", `"reasoning_opaque":"first-signature"`, `"reasoning_opaque":"second-signature"`},
		{"unsigned first block", `"reasoning_text":"inspect"`, `"reasoning_text":"verify","reasoning_opaque":"second-signature"`},
		{"unsigned last block", `"reasoning_text":"inspect","reasoning_opaque":"first-signature"`, `"reasoning_text":"verify"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := buildSSEStream(
				`{"choices":[{"index":0,"delta":{`+tc.first+`}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
				`{"choices":[{"index":0,"delta":{`+tc.last+`},"finish_reason":"stop"}]}`,
				"[DONE]",
			)
			response, progress, err := aggregatePolicyStreamToResponseWithProgress(body)
			var executionErr *chatExecutionError
			if response != nil || !errors.As(err, &executionErr) || executionErr.Code != "unsupported_native_reasoning_blocks" || upstreamProgressAllowsTargetSwitch(progress) {
				t.Fatalf("response/progress/error = %+v/%s/%v, want non-retryable native reasoning rejection", response, progress, err)
			}
		})
	}
}

func TestNativeReasoningAggregationKeepsChoicesIndependent(t *testing.T) {
	body := buildSSEStream(
		`{"choices":[{"index":0,"delta":{"reasoning_text":"inspect ","reasoning_opaque":"first-"}}]}`,
		`{"choices":[{"index":1,"delta":{"content":"answer"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_text":"input","reasoning_opaque":"signature"},"finish_reason":"stop"},{"index":1,"delta":{"reasoning_text":"verify","reasoning_opaque":"second-signature"},"finish_reason":"stop"}]}`,
		"[DONE]",
	)
	response, err := aggregateStreamToResponse(body)
	if err != nil || response == nil || len(response.Choices) != 2 {
		t.Fatalf("response/error = %+v/%v", response, err)
	}
	first, second := response.Choices[0].Message, response.Choices[1].Message
	if first.ReasoningText != "inspect input" || first.ReasoningOpaque != "first-signature" || second.ReasoningText != "verify" || second.ReasoningOpaque != "second-signature" {
		t.Fatalf("separate choices changed reasoning blocks: %+v", response.Choices)
	}
}

func TestNativeReasoningStreamCaptureDoesNotManufactureSignatures(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "messages"}[anthropic], func(t *testing.T) {
			body := buildSSEStream(append(nativeReasoningBoundaryChunks(`"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]`), "[DONE]")...)
			w := httptest.NewRecorder()
			var captured *models.OpenAIResponse
			capture := func(response *models.OpenAIResponse) { captured = response }
			if anthropic {
				StreamOpenAIToAnthropicWithFinalResponse(w, body, "public-model", "message", nil, capture)
			} else {
				StreamOpenAIPassthroughWithFinalResponse(w, body, capture)
			}
			if captured == nil || len(captured.Choices) != 1 || strings.Contains(w.Body.String(), "event: error") {
				t.Fatalf("stream/capture = %s/%+v", w.Body.String(), captured)
			}
			message := captured.Choices[0].Message
			if message.ReasoningText != "" || message.ReasoningOpaque != "" || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_lookup" {
				t.Fatalf("tool capture fabricated native reasoning or lost its call: %+v", message)
			}
			if captured.Usage == nil || captured.Usage.TotalTokens != 17 {
				t.Fatalf("stream capture lost terminal usage: %+v", captured)
			}
		})
	}
}

func TestNativeReasoningAggregationKeepsGeminiToolOutput(t *testing.T) {
	var sends atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Stream == nil || !*request.Stream {
			t.Error("upstream request was not force-streamed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		body := buildSSEStream(append(nativeReasoningBoundaryChunks(`"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]`), "[DONE]")...)
		defer func() { _ = body.Close() }()
		_, _ = io.Copy(w, body)
	})
	w := httptest.NewRecorder()
	h.HandleGeminiModels(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/public-model:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"lookup"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`)))
	if w.Code != http.StatusOK || sends.Load() != 1 {
		t.Fatalf("Gemini native tool output changed: status/sends/body = %d/%d/%s", w.Code, sends.Load(), w.Body.String())
	}
	var response models.GeminiGenerateContentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Candidates) != 1 || response.Candidates[0].Content == nil || len(response.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("Gemini output = %+v", response)
	}
	part := response.Candidates[0].Content.Parts[0]
	if part.FunctionCall == nil || part.FunctionCall.ID != "call_lookup" || part.FunctionCall.Name != "lookup" || string(part.FunctionCall.Args) != "{}" || part.ThoughtSignature != "" {
		t.Fatalf("Gemini native tool output changed: %+v", part)
	}
}
