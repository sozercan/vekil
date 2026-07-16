package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func TestChatStreamEventPipeCarriesChunksToTerminalSuccess(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	want := []models.OpenAIStreamChunk{
		{ID: "chatcmpl-1", Object: "chat.completion.chunk", Model: "gpt-test"},
		{ID: "chatcmpl-1", Object: "chat.completion.chunk", Model: "gpt-test", Usage: &models.OpenAIUsage{TotalTokens: 7}},
	}

	producerDone := make(chan error, 1)
	go func() {
		for _, chunk := range want {
			if err := writer.sendChunk(chunk); err != nil {
				producerDone <- err
				return
			}
		}
		producerDone <- writer.succeed()
	}()

	var got []models.OpenAIStreamChunk
	err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("consumeChatStreamEvents() error = %v", err)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks = %#v, want %#v", got, want)
	}
}

func TestChatStreamEventPipePropagatesTypedTerminalError(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	want := &chatStreamError{
		StatusCode: http.StatusServiceUnavailable,
		Type:       "overloaded_error",
		Code:       "model_overloaded",
		Message:    "try again later",
	}

	producerDone := make(chan error, 1)
	go func() {
		producerDone <- writer.fail(want)
	}()

	err := consumeChatStreamEvents(stream, nil)
	if err != want {
		t.Fatalf("consumeChatStreamEvents() error = %#v, want same typed error %#v", err, want)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}
}

func TestChatStreamEventPipeIsBoundedAndCancellationReleasesProducer(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	for i := 0; i < chatStreamEventBufferSize; i++ {
		if err := writer.sendChunk(models.OpenAIStreamChunk{Created: int64(i)}); err != nil {
			t.Fatalf("sendChunk(%d) error = %v", i, err)
		}
	}

	blocked := make(chan error, 1)
	go func() {
		blocked <- writer.sendChunk(models.OpenAIStreamChunk{Created: int64(chatStreamEventBufferSize)})
	}()

	select {
	case err := <-blocked:
		t.Fatalf("send beyond bounded capacity returned early with %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	cause := errors.New("adapter stopped")
	stream.stop(cause)
	select {
	case err := <-blocked:
		if !errors.Is(err, cause) {
			t.Fatalf("blocked send error = %v, want cancellation cause %v", err, cause)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked producer did not return after stream cancellation")
	}
}

func TestStreamChatEventsToOpenAIWritesCanonicalChatSSE(t *testing.T) {
	stop := "stop"
	chunks := []models.OpenAIStreamChunk{
		{
			ID:      "chatcmpl-openai",
			Object:  "chat.completion.chunk",
			Created: 42,
			Model:   "gpt-public",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"hello"`)},
			}},
		},
		{
			ID:      "chatcmpl-openai",
			Object:  "chat.completion.chunk",
			Created: 42,
			Model:   "gpt-public",
			Choices: []models.OpenAIStreamChoice{{Index: 0, FinishReason: &stop}},
			Usage:   &models.OpenAIUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		},
	}
	writer, stream := newChatStreamEventPipe(context.Background())
	producerDone := produceSuccessfulChatStream(writer, chunks)

	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Length", "999")
	if err := streamChatEventsToOpenAI(recorder, stream); err != nil {
		t.Fatalf("streamChatEventsToOpenAI() error = %v", err)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}

	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed", got)
	}

	var want strings.Builder
	for _, chunk := range chunks {
		payload, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("json.Marshal(chunk) error = %v", err)
		}
		fmt.Fprintf(&want, "data: %s\n\n", payload)
	}
	want.WriteString("data: [DONE]\n\n")
	if got := recorder.Body.String(); got != want.String() {
		t.Fatalf("OpenAI SSE = %q, want %q", got, want.String())
	}
}

func produceSuccessfulChatStream(writer *chatStreamEventWriter, chunks []models.OpenAIStreamChunk) <-chan error {
	done := make(chan error, 1)
	go func() {
		for _, chunk := range chunks {
			if err := writer.sendChunk(chunk); err != nil {
				done <- err
				return
			}
		}
		done <- writer.succeed()
	}()
	return done
}

func TestStreamChatEventsToOpenAIWritesTypedErrorWithoutDone(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	streamErr := &chatStreamError{
		StatusCode: http.StatusTooManyRequests,
		Type:       "rate_limit_error",
		Code:       "rate_limit_exceeded",
		Message:    "slow down",
	}
	producerDone := make(chan error, 1)
	go func() { producerDone <- writer.fail(streamErr) }()

	recorder := httptest.NewRecorder()
	err := streamChatEventsToOpenAI(recorder, stream)
	if err != streamErr {
		t.Fatalf("streamChatEventsToOpenAI() error = %#v, want same typed error %#v", err, streamErr)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}
	if got, want := recorder.Body.String(), "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n"; got != want {
		t.Fatalf("OpenAI error SSE = %q, want %q", got, want)
	}
	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("OpenAI error SSE unexpectedly contains [DONE]: %q", recorder.Body.String())
	}
}

func TestAggregateChatStreamEventsBuildsOpenAIResponse(t *testing.T) {
	toolIndex := 1
	finishReason := "tool_calls"
	chunks := []models.OpenAIStreamChunk{
		{
			ID:                "chatcmpl-aggregate",
			Object:            "chat.completion.chunk",
			Created:           123,
			Model:             "gpt-public",
			SystemFingerprint: "fp_test",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hello "`)},
			}},
		},
		{
			ID: "chatcmpl-aggregate",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{
					Content: json.RawMessage(`"world"`),
					ToolCalls: []models.OpenAIToolCall{{
						ID:    "call_weather",
						Type:  "function",
						Index: &toolIndex,
						Function: models.OpenAIFunctionCall{
							Name:      "weather",
							Arguments: `{"city":"`,
						},
					}},
				},
			}},
		},
		{
			ID: "chatcmpl-aggregate",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
					Index:    &toolIndex,
					Function: models.OpenAIFunctionCall{Arguments: `Paris"}`},
				}}},
				FinishReason: &finishReason,
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
		},
	}
	writer, stream := newChatStreamEventPipe(context.Background())
	producerDone := produceSuccessfulChatStream(writer, chunks)

	response, err := aggregateChatStreamEvents(stream)
	if err != nil {
		t.Fatalf("aggregateChatStreamEvents() error = %v", err)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}

	if response.ID != "chatcmpl-aggregate" || response.Object != "chat.completion" || response.Created != 123 || response.Model != "gpt-public" {
		t.Fatalf("response metadata = %#v", response)
	}
	if response.SystemFingerprint != "fp_test" {
		t.Fatalf("SystemFingerprint = %q, want fp_test", response.SystemFingerprint)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 9 {
		t.Fatalf("Usage = %#v, want total_tokens=9", response.Usage)
	}
	if len(response.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(response.Choices))
	}
	choice := response.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %#v, want tool_calls", choice.FinishReason)
	}
	var content string
	if err := json.Unmarshal(choice.Message.Content, &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if content != "Hello world" {
		t.Fatalf("content = %q, want Hello world", content)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %#v, want one", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_weather" || call.Function.Name != "weather" || call.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestStreamChatEventsToAnthropicMatchesNativeTranslation(t *testing.T) {
	toolIndex := 0
	finishReason := "tool_calls"
	chunks := []models.OpenAIStreamChunk{
		{
			ID: "chatcmpl-anthropic",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Before "`)},
			}},
		},
		{
			ID: "chatcmpl-anthropic",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_1",
					Type:  "function",
					Index: &toolIndex,
					Function: models.OpenAIFunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"`,
					},
				}}},
			}},
		},
		{
			ID: "chatcmpl-anthropic",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
					Index:    &toolIndex,
					Function: models.OpenAIFunctionCall{Arguments: `test"}`},
				}}},
			}},
		},
		{
			ID: "chatcmpl-anthropic",
			Choices: []models.OpenAIStreamChoice{{
				Index:        0,
				Delta:        models.OpenAIMessage{Content: json.RawMessage(`"after"`)},
				FinishReason: &finishReason,
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10},
		},
	}

	nativeRecorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(nativeRecorder, buildOpenAIChatSSEBody(t, chunks), "claude-public", "req-typed")

	writer, stream := newChatStreamEventPipe(context.Background())
	producerDone := produceSuccessfulChatStream(writer, chunks)
	typedRecorder := httptest.NewRecorder()
	if err := streamChatEventsToAnthropic(typedRecorder, stream, "claude-public", "req-typed"); err != nil {
		t.Fatalf("streamChatEventsToAnthropic() error = %v", err)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}

	if got, want := typedRecorder.Body.String(), nativeRecorder.Body.String(); got != want {
		t.Fatalf("typed Anthropic SSE differs from native translation\ntyped:\n%s\nnative:\n%s", got, want)
	}
}

func buildOpenAIChatSSEBody(t *testing.T, chunks []models.OpenAIStreamChunk) io.ReadCloser {
	t.Helper()
	var body strings.Builder
	for _, chunk := range chunks {
		payload, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("json.Marshal(chunk) error = %v", err)
		}
		fmt.Fprintf(&body, "data: %s\n\n", payload)
	}
	body.WriteString("data: [DONE]\n\n")
	return io.NopCloser(strings.NewReader(body.String()))
}

func TestStreamChatEventsToGeminiMatchesNativeTranslation(t *testing.T) {
	toolIndex := 0
	finishReason := "tool_calls"
	chunks := []models.OpenAIStreamChunk{
		{
			ID: "chatcmpl-gemini",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{Content: json.RawMessage(`"hello"`)},
			}},
		},
		{
			ID: "chatcmpl-gemini",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
					ID:    "call_gemini",
					Type:  "function",
					Index: &toolIndex,
					Function: models.OpenAIFunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"`,
					},
				}}},
			}},
		},
		{
			ID: "chatcmpl-gemini",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
					Index:    &toolIndex,
					Function: models.OpenAIFunctionCall{Arguments: `test"}`},
				}}},
				FinishReason: &finishReason,
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		},
	}

	nativeRecorder := httptest.NewRecorder()
	StreamOpenAIToGemini(nativeRecorder, buildOpenAIChatSSEBody(t, chunks))

	writer, stream := newChatStreamEventPipe(context.Background())
	producerDone := produceSuccessfulChatStream(writer, chunks)
	typedRecorder := httptest.NewRecorder()
	if err := streamChatEventsToGemini(typedRecorder, stream); err != nil {
		t.Fatalf("streamChatEventsToGemini() error = %v", err)
	}
	if err := <-producerDone; err != nil {
		t.Fatalf("producer error = %v", err)
	}

	if got, want := typedRecorder.Body.String(), nativeRecorder.Body.String(); got != want {
		t.Fatalf("typed Gemini SSE differs from native translation\ntyped:\n%s\nnative:\n%s", got, want)
	}
}

func TestTypedChatStreamErrorsReachAnthropicGeminiAndAggregation(t *testing.T) {
	streamErr := &chatStreamError{
		StatusCode: http.StatusServiceUnavailable,
		Type:       "overloaded_error",
		Code:       "model_overloaded",
		Message:    "provider overloaded",
	}

	t.Run("Anthropic", func(t *testing.T) {
		writer, stream := newChatStreamEventPipe(context.Background())
		producerDone := make(chan error, 1)
		go func() { producerDone <- writer.fail(streamErr) }()

		recorder := httptest.NewRecorder()
		err := streamChatEventsToAnthropic(recorder, stream, "claude-public", "req-error")
		if err != streamErr {
			t.Fatalf("streamChatEventsToAnthropic() error = %#v, want %#v", err, streamErr)
		}
		if err := <-producerDone; err != nil {
			t.Fatalf("producer error = %v", err)
		}
		events := parseSSEEvents(recorder.Body.String())
		if len(events) != 2 || events[0].Event != "message_start" || events[1].Event != "error" {
			t.Fatalf("Anthropic events = %#v, want message_start then error", events)
		}
		if !strings.Contains(events[1].Data, `"message":"provider overloaded"`) {
			t.Fatalf("Anthropic error event = %q", events[1].Data)
		}
		if strings.Contains(recorder.Body.String(), "message_stop") {
			t.Fatalf("Anthropic error stream unexpectedly contains message_stop: %s", recorder.Body.String())
		}
	})

	t.Run("Gemini", func(t *testing.T) {
		writer, stream := newChatStreamEventPipe(context.Background())
		producerDone := make(chan error, 1)
		go func() { producerDone <- writer.fail(streamErr) }()

		recorder := httptest.NewRecorder()
		err := streamChatEventsToGemini(recorder, stream)
		if err != streamErr {
			t.Fatalf("streamChatEventsToGemini() error = %#v, want %#v", err, streamErr)
		}
		if err := <-producerDone; err != nil {
			t.Fatalf("producer error = %v", err)
		}
		frames := parseGeminiSSEFrames(recorder.Body.String())
		if len(frames) != 1 {
			t.Fatalf("Gemini frames = %#v, want one error frame", frames)
		}
		var response models.GeminiErrorResponse
		if err := json.Unmarshal([]byte(frames[0]), &response); err != nil {
			t.Fatalf("unmarshal Gemini error: %v", err)
		}
		if response.Error.Code != http.StatusServiceUnavailable || response.Error.Status != "UNAVAILABLE" || response.Error.Message != "provider overloaded" {
			t.Fatalf("Gemini error = %#v", response.Error)
		}
	})

	t.Run("aggregate", func(t *testing.T) {
		writer, stream := newChatStreamEventPipe(context.Background())
		producerDone := make(chan error, 1)
		go func() { producerDone <- writer.fail(streamErr) }()

		response, err := aggregateChatStreamEvents(stream)
		if response != nil {
			t.Fatalf("aggregateChatStreamEvents() response = %#v, want nil", response)
		}
		if err != streamErr {
			t.Fatalf("aggregateChatStreamEvents() error = %#v, want %#v", err, streamErr)
		}
		if err := <-producerDone; err != nil {
			t.Fatalf("producer error = %v", err)
		}
	})
}

func BenchmarkChatStreamEventTransport(b *testing.B) {
	const chunksPerStream = 10_000
	chunk := models.OpenAIStreamChunk{
		ID:      "chatcmpl-bench",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "gpt-bench",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{Content: json.RawMessage(`"x"`)},
		}},
	}

	b.ReportAllocs()
	b.SetBytes(chunksPerStream)
	for i := 0; i < b.N; i++ {
		writer, stream := newChatStreamEventPipe(context.Background())
		producerDone := make(chan error, 1)
		go func() {
			for j := 0; j < chunksPerStream; j++ {
				if err := writer.sendChunk(chunk); err != nil {
					producerDone <- err
					return
				}
			}
			producerDone <- writer.succeed()
		}()

		count := 0
		if err := consumeChatStreamEvents(stream, func(models.OpenAIStreamChunk) error {
			count++
			return nil
		}); err != nil {
			b.Fatalf("consumeChatStreamEvents() error = %v", err)
		}
		if err := <-producerDone; err != nil {
			b.Fatalf("producer error = %v", err)
		}
		if count != chunksPerStream {
			b.Fatalf("chunk count = %d, want %d", count, chunksPerStream)
		}
	}
}

func TestOpenAIChatEventAdapterCancelsBlockedProducerAfterClientWriteFailure(t *testing.T) {
	writeErr := errors.New("client disconnected")
	writer, stream := newChatStreamEventPipe(context.Background())
	producerDone := make(chan error, 1)
	go func() {
		chunk := models.OpenAIStreamChunk{ID: "chatcmpl-cancel"}
		for i := 0; i < chatStreamEventBufferSize*4; i++ {
			if err := writer.sendChunk(chunk); err != nil {
				producerDone <- err
				return
			}
		}
		producerDone <- writer.succeed()
	}()

	responseWriter := &chatStreamFailingResponseWriter{writeErr: writeErr}
	if err := streamChatEventsToOpenAI(responseWriter, stream); !errors.Is(err, writeErr) {
		t.Fatalf("streamChatEventsToOpenAI() error = %v, want %v", err, writeErr)
	}
	select {
	case err := <-producerDone:
		if !errors.Is(err, writeErr) {
			t.Fatalf("producer error = %v, want adapter cancellation cause %v", err, writeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("producer remained blocked after adapter write failure")
	}
}

type chatStreamFailingResponseWriter struct {
	header   http.Header
	writeErr error
}

func (w *chatStreamFailingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *chatStreamFailingResponseWriter) Write([]byte) (int, error) {
	return 0, w.writeErr
}

func (w *chatStreamFailingResponseWriter) WriteHeader(int) {}

func TestChatStreamEventQueuedTerminalWinsConcurrentCancellation(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	want := models.OpenAIStreamChunk{ID: "chatcmpl-done", Choices: []models.OpenAIStreamChoice{{Index: 0}}}
	if err := writer.sendChunk(want); err != nil {
		t.Fatal(err)
	}
	if err := writer.succeed(); err != nil {
		t.Fatal(err)
	}
	stream.stop(errProxyLifecycleShutdown)
	var got []models.OpenAIStreamChunk
	if err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error { got = append(got, chunk); return nil }); err != nil {
		t.Fatalf("consume error = %v", err)
	}
	if !reflect.DeepEqual(got, []models.OpenAIStreamChunk{want}) {
		t.Fatalf("chunks = %#v", got)
	}
}

func TestChatStreamFinalCallbackCollectsToolsWithoutText(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	index := 0
	go func() {
		_ = writer.sendChunk(models.OpenAIStreamChunk{ID: "chatcmpl-tools", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Content: json.RawMessage(`"large visible text"`)}}}})
		_ = writer.sendChunk(models.OpenAIStreamChunk{ID: "chatcmpl-tools", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{ID: "call", Type: "function", Index: &index, Function: models.OpenAIFunctionCall{Name: "tool", Arguments: `{}`}}}}}}})
		_ = writer.succeed()
	}()
	var final *models.OpenAIResponse
	recorder := httptest.NewRecorder()
	if err := streamChatEventsToOpenAI(recorder, stream, chatStreamEventCallbacks{OnFinal: func(response *models.OpenAIResponse) { final = response }}); err != nil {
		t.Fatal(err)
	}
	if final == nil || len(final.Choices) != 1 || len(final.Choices[0].Message.Content) != 0 || len(final.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("final = %#v", final)
	}
}

func TestAnthropicChatEventAdapterPreservesErrorClassification(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	go func() {
		_ = writer.fail(&chatExecutionError{StatusCode: http.StatusTooManyRequests, Type: "rate_limit_error", Code: "too_many_requests", Message: "slow down"})
	}()
	recorder := httptest.NewRecorder()
	err := streamChatEventsToAnthropic(recorder, stream, "gpt", "msg-test")
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("error = %#v", err)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"rate_limit_error"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestGeminiChatEventAdapterEmitsFinishValidationError(t *testing.T) {
	writer, stream := newChatStreamEventPipe(context.Background())
	badIndex := -1
	go func() {
		_ = writer.sendChunk(models.OpenAIStreamChunk{Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{ID: "call", Type: "function", Index: &badIndex, Function: models.OpenAIFunctionCall{Name: "tool", Arguments: `{}`}}}}}}})
		_ = writer.succeed()
	}()
	recorder := httptest.NewRecorder()
	err := streamChatEventsToGemini(recorder, stream)
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || !strings.Contains(recorder.Body.String(), `"status":"UNAVAILABLE"`) {
		t.Fatalf("error/body = %#v %s", err, recorder.Body.String())
	}
}
