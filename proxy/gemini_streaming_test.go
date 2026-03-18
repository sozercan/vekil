package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/copilot-proxy/models"
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
	if len(frames) != 1 {
		t.Fatalf("len(frames) = %d, want 1\nraw:\n%s", len(frames), w.Body.String())
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
}
