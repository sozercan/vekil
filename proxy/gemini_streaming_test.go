package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func parseGeminiSSEFrames(body string) []string {
	var frames []string
	var current strings.Builder

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			current.WriteString(strings.TrimPrefix(line, "data: "))
		} else if line == "" && current.Len() > 0 {
			frames = append(frames, current.String())
			current.Reset()
		}
	}

	return frames
}

func TestStreamOpenAIToGeminiText(t *testing.T) {
	stop := "stop"
	chunk1 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{Content: json.RawMessage(`"Hello"`)},
		}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{Content: json.RawMessage(`" world"`)},
		}},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			Delta:        models.OpenAIMessage{},
			FinishReason: &stop,
		}},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gemini-2.5-pro",
		Usage: &models.OpenAIUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToGemini(w, body)

	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 3 {
		t.Fatalf("len(frames) = %d, want 3\nraw:\n%s", len(frames), w.Body.String())
	}

	var first models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &first); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if first.Candidates[0].Content == nil || first.Candidates[0].Content.Parts[0].Text == nil || *first.Candidates[0].Content.Parts[0].Text != "Hello" {
		t.Fatalf("first frame = %#v, want text Hello", first)
	}

	var second models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[1]), &second); err != nil {
		t.Fatalf("unmarshal second frame: %v", err)
	}
	if second.Candidates[0].Content == nil || second.Candidates[0].Content.Parts[0].Text == nil || *second.Candidates[0].Content.Parts[0].Text != " world" {
		t.Fatalf("second frame = %#v, want text ' world'", second)
	}

	var tail models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[2]), &tail); err != nil {
		t.Fatalf("unmarshal tail frame: %v", err)
	}
	if tail.Candidates[0].FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", tail.Candidates[0].FinishReason)
	}
	if tail.UsageMetadata == nil || tail.UsageMetadata.TotalTokenCount != 17 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=17", tail.UsageMetadata)
	}
}

func TestStreamOpenAIToGeminiToolCalls(t *testing.T) {
	toolStop := "tool_calls"
	idx := 0
	chunk1 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_1",
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Name: "lookup_weather",
					},
				}},
			},
		}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Arguments: `{"city":"Pa`,
					},
				}},
			},
		}},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Arguments: `ris"}`,
					},
				}},
			},
		}},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			Delta:        models.OpenAIMessage{},
			FinishReason: &toolStop,
		}},
	}
	chunk5 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gemini-2.5-pro",
		Usage: &models.OpenAIUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		mustMarshal(t, chunk5),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToGemini(w, body)

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2\nraw:\n%s", len(frames), w.Body.String())
	}

	var functionCallFrame models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &functionCallFrame); err != nil {
		t.Fatalf("unmarshal function call frame: %v", err)
	}
	if len(functionCallFrame.Candidates) != 1 || functionCallFrame.Candidates[0].Content == nil {
		t.Fatalf("function call frame = %#v, want one candidate with content", functionCallFrame)
	}
	part := functionCallFrame.Candidates[0].Content.Parts[0]
	if part.FunctionCall == nil {
		t.Fatalf("part = %#v, want functionCall", part)
	}
	if part.FunctionCall.ID != "call_1" {
		t.Errorf("FunctionCall.ID = %q, want call_1", part.FunctionCall.ID)
	}
	if part.FunctionCall.Name != "lookup_weather" {
		t.Errorf("FunctionCall.Name = %q, want lookup_weather", part.FunctionCall.Name)
	}

	var args map[string]string
	if err := json.Unmarshal(part.FunctionCall.Args, &args); err != nil {
		t.Fatalf("unmarshal functionCall args: %v", err)
	}
	if args["city"] != "Paris" {
		t.Errorf("args[city] = %q, want Paris", args["city"])
	}

	var tail models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[1]), &tail); err != nil {
		t.Fatalf("unmarshal tail frame: %v", err)
	}
	if tail.Candidates[0].FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", tail.Candidates[0].FinishReason)
	}
	if tail.UsageMetadata == nil || tail.UsageMetadata.TotalTokenCount != 11 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=11", tail.UsageMetadata)
	}
}

func TestStreamOpenAIToGeminiWithFinalResponse_CapturesStreamedToolCalls(t *testing.T) {
	toolStop := "tool_calls"
	idx := 0
	chunk1 := models.OpenAIStreamChunk{
		ID:      "chatcmpl-gemini-tool-final",
		Created: 456,
		Model:   "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_weather_1",
					Type:  "function",
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Name:      "lookup_weather",
						Arguments: `{"city":"Pa`,
					},
				}},
			},
		}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:      "chatcmpl-gemini-tool-final",
		Created: 456,
		Model:   "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Arguments: `ris","unit":"c`,
					},
				}},
			},
		}},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID:      "chatcmpl-gemini-tool-final",
		Created: 456,
		Model:   "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Arguments: `elsius"}`,
					},
				}},
			},
		}},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID:      "chatcmpl-gemini-tool-final",
		Created: 456,
		Model:   "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			Delta:        models.OpenAIMessage{},
			FinishReason: &toolStop,
		}},
	}
	chunk5 := models.OpenAIStreamChunk{
		ID:      "chatcmpl-gemini-tool-final",
		Created: 456,
		Model:   "gemini-2.5-pro",
		Usage:   &models.OpenAIUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		mustMarshal(t, chunk5),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	var final *models.OpenAIResponse
	StreamOpenAIToGeminiWithFinalResponse(w, body, nil, func(resp *models.OpenAIResponse) {
		final = resp
	})

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2\nraw:\n%s", len(frames), w.Body.String())
	}

	var functionCallFrame models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &functionCallFrame); err != nil {
		t.Fatalf("unmarshal function call frame: %v", err)
	}
	if functionCallFrame.Candidates[0].Content.Parts[0].FunctionCall == nil {
		t.Fatalf("first frame = %#v, want functionCall", functionCallFrame)
	}

	if final == nil {
		t.Fatal("final response callback was not invoked")
	}
	if final.ID != "chatcmpl-gemini-tool-final" {
		t.Errorf("final.ID = %q, want chatcmpl-gemini-tool-final", final.ID)
	}
	if final.Created != 456 {
		t.Errorf("final.Created = %d, want 456", final.Created)
	}
	if final.Usage == nil || final.Usage.TotalTokens != 11 {
		t.Fatalf("final.Usage = %#v, want total tokens 11", final.Usage)
	}
	if len(final.Choices) != 1 {
		t.Fatalf("len(final.Choices) = %d, want 1", len(final.Choices))
	}
	choice := final.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %v, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("len(tool_calls) = %d, want 1", len(choice.Message.ToolCalls))
	}
	toolCall := choice.Message.ToolCalls[0]
	if toolCall.ID != "call_weather_1" {
		t.Errorf("toolCall.ID = %q, want call_weather_1", toolCall.ID)
	}
	if toolCall.Function.Name != "lookup_weather" {
		t.Errorf("toolCall.Function.Name = %q, want lookup_weather", toolCall.Function.Name)
	}
	if toolCall.Function.Arguments != `{"city":"Paris","unit":"celsius"}` {
		t.Errorf("toolCall.Function.Arguments = %q, want combined JSON arguments", toolCall.Function.Arguments)
	}
}

func TestStreamOpenAIToGeminiToolCallWithoutArgumentsFlushesEmptyObject(t *testing.T) {
	toolStop := "tool_calls"
	idx := 0
	chunk1 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-3",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_empty",
					Index: &idx,
					Function: models.OpenAIFunctionCall{
						Name: "lookup_weather",
					},
				}},
			},
		}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-3",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			Delta:        models.OpenAIMessage{},
			FinishReason: &toolStop,
		}},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-3",
		Model: "gemini-2.5-pro",
		Usage: &models.OpenAIUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToGemini(w, body)

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2\nraw:\n%s", len(frames), w.Body.String())
	}

	var functionCallFrame models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &functionCallFrame); err != nil {
		t.Fatalf("unmarshal function call frame: %v", err)
	}
	part := functionCallFrame.Candidates[0].Content.Parts[0]
	if part.FunctionCall == nil {
		t.Fatalf("part = %#v, want functionCall", part)
	}
	if part.FunctionCall.ID != "call_empty" {
		t.Errorf("FunctionCall.ID = %q, want call_empty", part.FunctionCall.ID)
	}
	if string(part.FunctionCall.Args) != "{}" {
		t.Errorf("FunctionCall.Args = %s, want {}", part.FunctionCall.Args)
	}

	var tail models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[1]), &tail); err != nil {
		t.Fatalf("unmarshal tail frame: %v", err)
	}
	if tail.Candidates[0].FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", tail.Candidates[0].FinishReason)
	}
	if tail.UsageMetadata == nil || tail.UsageMetadata.TotalTokenCount != 7 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=7", tail.UsageMetadata)
	}
}

func TestStreamOpenAIToGeminiSparseToolCallIndices(t *testing.T) {
	toolStop := "tool_calls"
	zeroIndex := 0
	sparseIndex := 1 << 30
	sparseChunk := models.OpenAIStreamChunk{
		ID:    "chatcmpl-sparse",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_sparse",
					Index: &sparseIndex,
					Function: models.OpenAIFunctionCall{
						Name:      "sparse_call",
						Arguments: `{}`,
					},
				}},
			},
		}},
	}
	zeroChunk := models.OpenAIStreamChunk{
		ID:    "chatcmpl-sparse",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_zero",
					Index: &zeroIndex,
					Function: models.OpenAIFunctionCall{
						Name:      "zero_call",
						Arguments: `{}`,
					},
				}},
			},
		}},
	}
	finishChunk := models.OpenAIStreamChunk{
		ID:    "chatcmpl-sparse",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			FinishReason: &toolStop,
		}},
	}

	body := buildSSEStream(mustMarshal(t, sparseChunk), mustMarshal(t, zeroChunk), mustMarshal(t, finishChunk), "[DONE]")
	w := httptest.NewRecorder()
	started := time.Now()
	StreamOpenAIToGemini(w, body)
	elapsed := time.Since(started)
	t.Logf("sparse tool-call translation completed in %s", elapsed)
	if elapsed > time.Second {
		t.Fatalf("sparse tool-call translation took %s, want <= 1s", elapsed)
	}

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2\nraw:\n%s", len(frames), w.Body.String())
	}

	var calls models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &calls); err != nil {
		t.Fatalf("unmarshal function-call frame: %v", err)
	}
	if len(calls.Candidates) != 1 || calls.Candidates[0].Content == nil {
		t.Fatalf("function-call frame = %#v, want one candidate with content", calls)
	}
	parts := calls.Candidates[0].Content.Parts
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	if parts[0].FunctionCall == nil || parts[0].FunctionCall.ID != "call_zero" {
		t.Fatalf("parts[0] = %#v, want call_zero", parts[0])
	}
	if parts[1].FunctionCall == nil || parts[1].FunctionCall.ID != "call_sparse" {
		t.Fatalf("parts[1] = %#v, want call_sparse", parts[1])
	}
}

func TestStreamOpenAIToGeminiRejectsNegativeToolCallIndex(t *testing.T) {
	negativeIndex := -1
	chunk := models.OpenAIStreamChunk{
		ID:    "chatcmpl-negative-index",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_negative",
					Index: &negativeIndex,
					Function: models.OpenAIFunctionCall{
						Name:      "bad_call",
						Arguments: `{}`,
					},
				}},
			},
		}},
	}

	body := buildSSEStream(mustMarshal(t, chunk), "[DONE]")
	w := httptest.NewRecorder()
	gotFailureStatus := 0
	StreamOpenAIToGeminiWithFinalResponse(w, body, func(status int) {
		gotFailureStatus = status
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, want committed 200", w.Code)
	}
	if gotFailureStatus != http.StatusBadGateway {
		t.Fatalf("onError status = %d, want 502", gotFailureStatus)
	}

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 1 {
		t.Fatalf("len(frames) = %d, want 1\nraw:\n%s", len(frames), w.Body.String())
	}
	var errResp models.GeminiErrorResponse
	if err := json.Unmarshal([]byte(frames[0]), &errResp); err != nil {
		t.Fatalf("unmarshal error frame: %v", err)
	}
	if errResp.Error.Code != http.StatusBadGateway || errResp.Error.Status != "UNAVAILABLE" {
		t.Fatalf("error = %#v, want 502 UNAVAILABLE", errResp.Error)
	}
	if !strings.Contains(errResp.Error.Message, "tool call index -1") {
		t.Fatalf("Error.Message = %q, want negative index detail", errResp.Error.Message)
	}
}

func BenchmarkGeminiStreamSparseToolCall(b *testing.B) {
	benchmarks := []struct {
		name  string
		index int
	}{
		{name: "index_0", index: 0},
		{name: "index_1<<30", index: 1 << 30},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				state := newGeminiStreamState(w)
				state.bufferedToolCalls[bm.index] = &geminiStreamingToolCall{
					ID:   "call_sparse",
					Name: "sparse_call",
				}
				if !state.flushToolCalls(true) {
					b.Fatal("flushToolCalls returned false")
				}
			}
		})
	}
}

func TestStreamOpenAIToGemini_NoTailWithoutDone(t *testing.T) {
	stop := "stop"
	chunk1 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-no-done",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{Content: json.RawMessage(`"Hello"`)},
		}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-no-done",
		Model: "gemini-2.5-pro",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			Delta:        models.OpenAIMessage{},
			FinishReason: &stop,
		}},
		Usage: &models.OpenAIUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
	)

	w := httptest.NewRecorder()
	StreamOpenAIToGemini(w, body)

	frames := parseGeminiSSEFrames(w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2\nraw:\n%s", len(frames), w.Body.String())
	}

	var first models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &first); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if first.UsageMetadata != nil {
		t.Fatalf("unexpected usage metadata in non-terminal frame: %#v", first.UsageMetadata)
	}
	if len(first.Candidates) == 0 || first.Candidates[0].FinishReason != "" {
		t.Fatalf("unexpected finish reason in non-terminal frame: %#v", first.Candidates)
	}
	var errFrame models.GeminiErrorResponse
	if err := json.Unmarshal([]byte(frames[1]), &errFrame); err != nil {
		t.Fatalf("unmarshal error frame: %v", err)
	}
	if errFrame.Error.Message == "" {
		t.Fatalf("expected truncation error frame, got %#v", errFrame)
	}
}
