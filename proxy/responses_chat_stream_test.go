package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func TestResponsesChatStream_TextFixture(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_text.sse")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
		PublicModel:      "gpt-public",
		PrecommitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("prepareResponsesChatStream() error = %v", err)
	}

	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 5 {
		t.Fatalf("chunk count = %d, want 5: %#v", len(chunks), chunks)
	}
	for i, chunk := range chunks {
		if chunk.ID != "chatcmpl-synth_text_stream_001" || chunk.Object != openAIChatCompletionChunkObject || chunk.Created != 1_700_000_000 || chunk.Model != "gpt-public" {
			t.Fatalf("chunk[%d] envelope = %#v", i, chunk)
		}
	}
	if got := chunks[0].Choices[0].Delta.Role; got != "assistant" {
		t.Fatalf("role = %q, want assistant", got)
	}
	if got := streamChunkText(t, chunks[1]); got != "Synthetic fixture " {
		t.Fatalf("first text = %q", got)
	}
	if got := streamChunkText(t, chunks[2]); got != "text response." {
		t.Fatalf("second text = %q", got)
	}
	if got := chunks[3].Choices[0].FinishReason; got == nil || *got != "stop" {
		t.Fatalf("finish reason = %v, want stop", got)
	}
	wantUsage := &models.OpenAIUsage{
		PromptTokens:        11,
		CompletionTokens:    6,
		TotalTokens:         17,
		PromptTokensDetails: &models.OpenAIPromptTokensDetails{CachedTokens: 3},
	}
	if !reflect.DeepEqual(chunks[4].Usage, wantUsage) || len(chunks[4].Choices) != 0 {
		t.Fatalf("usage chunk = %#v, want %#v", chunks[4], wantUsage)
	}
}

func readResponsesChatStreamFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/chat_over_responses/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func collectResponsesChatStreamChunks(t *testing.T, stream *chatStreamEventStream) []models.OpenAIStreamChunk {
	t.Helper()
	var chunks []models.OpenAIStreamChunk
	if err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	}, nil); err != nil {
		t.Fatalf("consumeChatStreamEvents() error = %v", err)
	}
	return chunks
}

func streamChunkText(t *testing.T, chunk models.OpenAIStreamChunk) string {
	t.Helper()
	if len(chunk.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(chunk.Choices))
	}
	var text string
	if err := json.Unmarshal(chunk.Choices[0].Delta.Content, &text); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	return text
}

func TestResponsesChatStream_OneToolPublishesReplayBeforeProxyID(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_one_tool_call.sse")
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	route := responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}

	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
		PublicModel: "gpt-public",
		ReplayStore: store,
		ReplayRoute: route,
		ReplayToolDefaults: responsesChatReplayToolDefaults{
			"lookup_synthetic_widget": {"mode": json.RawMessage(`"standard"`)},
		},
		PrecommitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("prepareResponsesChatStream() error = %v", err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 5 {
		t.Fatalf("chunk count = %d, want 5: %#v", len(chunks), chunks)
	}
	start := chunks[1].Choices[0].Delta.ToolCalls
	if len(start) != 1 || start[0].Index == nil || *start[0].Index != 0 || start[0].Function.Name != "lookup_synthetic_widget" || !strings.HasPrefix(start[0].ID, responsesChatReplayCallIDPrefix) || start[0].ID == "call_synth_lookup_stream_001" {
		t.Fatalf("tool start = %#v", start)
	}
	args := chunks[2].Choices[0].Delta.ToolCalls
	if len(args) != 1 || args[0].Index == nil || *args[0].Index != 0 || args[0].Function.Arguments != `{"widget":"alpha-fixture"}` {
		t.Fatalf("tool args = %#v", args)
	}
	if got := chunks[3].Choices[0].FinishReason; got == nil || *got != "tool_calls" {
		t.Fatalf("finish reason = %v", got)
	}
	if stats := store.Stats(); stats.Groups != 1 || stats.Calls != 1 {
		t.Fatalf("replay stats = %#v", stats)
	}
	resolved, err := store.Resolve(route, responsesChatReplayAssistantProjection{
		Content: json.RawMessage(`""`),
		Calls: []responsesChatReplayProjectedCall{{
			ID: start[0].ID, Name: start[0].Function.Name, Arguments: `{"widget":"alpha-fixture","mode":"standard"}`,
		}},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(resolved.OutputItems) != 1 || !bytes.Contains(resolved.OutputItems[0], []byte(`"call_id":"call_synth_lookup_stream_001"`)) {
		t.Fatalf("resolved replay = %#v", resolved)
	}
}

func TestResponsesChatStream_TrailingCarrierFollowsPrecommitChunks(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_one_tool_call.sse")
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
		PublicModel:       "gpt-public",
		ReplayStore:       store,
		ReplayRoute:       responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		PrecommitTimeout:  time.Hour,
		PrecommitMaxBytes: len(fixture) + 1,
	})
	if err != nil {
		t.Fatalf("prepareResponsesChatStream() error = %v", err)
	}
	lastChunk, carrier := -1, -1
	for position := 0; ; position++ {
		event, nextErr := stream.next()
		if nextErr != nil {
			t.Fatalf("stream.next() error = %v", nextErr)
		}
		switch event.kind {
		case chatStreamEventChunk:
			lastChunk = position
		case chatStreamEventCarriedReasoning:
			carrier = position
		case chatStreamEventSuccess:
			if lastChunk < 0 || carrier < 0 || carrier <= lastChunk {
				t.Fatalf("event order last chunk=%d carrier=%d, want every precommit chunk before trailing carrier", lastChunk, carrier)
			}
			return
		}
	}
}

func TestResponsesChatStream_ImmediateFailureBeforeCommit(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_immediate_failure.sse")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
		PublicModel:      "gpt-public",
		PrecommitTimeout: time.Second,
	})
	if stream != nil {
		t.Fatal("stream is non-nil on precommit failure")
	}
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("error = %T %v, want *chatExecutionError", err, err)
	}
	if executionErr.StatusCode != http.StatusServiceUnavailable || executionErr.Code != "model_overloaded" || executionErr.Usage == nil || executionErr.Usage.TotalTokens != 5 {
		t.Fatalf("execution error = %#v", executionErr)
	}
}

func TestResponsesChatStream_ParallelToolsUseDenseFirstSeenIndexes(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_parallel_interleaved_tool_calls.sse")
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
		PublicModel: "gpt-public", ReplayStore: store,
		ReplayRoute:      responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		PrecommitTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 7 {
		t.Fatalf("chunk count = %d, want 7", len(chunks))
	}
	for i, chunkIndex := range []int{1, 3} {
		call := chunks[chunkIndex].Choices[0].Delta.ToolCalls[0]
		if call.Index == nil || *call.Index != i || !strings.HasPrefix(call.ID, responsesChatReplayCallIDPrefix) {
			t.Fatalf("tool start %d = %#v", i, call)
		}
		args := chunks[chunkIndex+1].Choices[0].Delta.ToolCalls[0]
		if args.Index == nil || *args.Index != i || !json.Valid([]byte(args.Function.Arguments)) {
			t.Fatalf("tool args %d = %#v", i, args)
		}
	}
	if stats := store.Stats(); stats.Groups != 1 || stats.Calls != 2 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestResponsesChatStream_ReasoningDoesNotProduceChunks(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_reasoning_message_continuation.sse")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 5 || streamChunkText(t, chunks[1])+streamChunkText(t, chunks[2]) != "Synthetic continuation finished." {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesChatStream_RefusalMapsToRefusalField(t *testing.T) {
	fixture := []byte("event: response.created\n" +
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_refusal","created_at":1700000001,"status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_refusal","status":"in_progress","role":"assistant","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_refusal","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}` + "\n\n" +
		"event: response.refusal.delta\n" +
		`data: {"type":"response.refusal.delta","sequence_number":3,"item_id":"msg_refusal","output_index":0,"content_index":0,"delta":"Synthetic refusal."}` + "\n\n" +
		"event: response.refusal.done\n" +
		`data: {"type":"response.refusal.done","sequence_number":4,"item_id":"msg_refusal","output_index":0,"content_index":0,"refusal":"Synthetic refusal."}` + "\n\n" +
		"event: response.content_part.done\n" +
		`data: {"type":"response.content_part.done","sequence_number":5,"item_id":"msg_refusal","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"Synthetic refusal."}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"type":"message","id":"msg_refusal","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"Synthetic refusal."}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":7,"response":{"id":"resp_refusal","created_at":1700000001,"status":"completed","output":[{"type":"message","id":"msg_refusal","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"Synthetic refusal."}]}]}}` + "\n\n")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 3 || chunks[1].Choices[0].Delta.Content != nil {
		t.Fatalf("chunks = %#v", chunks)
	}
	var refusal string
	if err := json.Unmarshal(chunks[1].Choices[0].Delta.Refusal, &refusal); err != nil || refusal != "Synthetic refusal." {
		t.Fatalf("refusal = %q err=%v chunks=%#v", refusal, err, chunks)
	}
}

func TestResponsesChatStream_IncompleteLength(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_incomplete_length.sse")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 4 || streamChunkText(t, chunks[1]) != "Synthetic partial output" {
		t.Fatalf("chunks = %#v", chunks)
	}
	if got := chunks[2].Choices[0].FinishReason; got == nil || *got != "length" {
		t.Fatalf("finish reason = %v", got)
	}
}

func TestResponsesChatStream_UnknownEventFailsBeforeCommit(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_malformed_unknown_event.sse")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if stream != nil {
		t.Fatal("stream is non-nil")
	}
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_event" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesChatStream_PostcommitFailureCarriesUsageAndTypedError(t *testing.T) {
	fixture := []byte("event: response.created\n" +
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_post_failure","created_at":1700000002,"status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_post_failure","status":"in_progress","role":"assistant","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_post_failure","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_post_failure","output_index":0,"content_index":0,"delta":"partial"}` + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","sequence_number":4,"response":{"id":"resp_post_failure","created_at":1700000002,"status":"failed","error":{"type":"rate_limit_error","code":"too_many_requests","message":"slow down"},"output":[],"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}}` + "\n\n")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []models.OpenAIStreamChunk
	err = consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	}, nil)
	var streamErr *chatStreamError
	if !errors.As(err, &streamErr) || streamErr.StatusCode != http.StatusTooManyRequests || streamErr.Code != "too_many_requests" {
		t.Fatalf("stream error = %#v", err)
	}
	if len(chunks) != 3 || streamChunkText(t, chunks[1]) != "partial" || chunks[2].Usage == nil || chunks[2].Usage.TotalTokens != 11 {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesChatStream_EOFBeforeTerminalIsTruncation(t *testing.T) {
	fixture := []byte("event: response.created\n" +
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_truncated","created_at":1700000003,"status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_truncated","status":"in_progress","role":"assistant","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_truncated","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_truncated","output_index":0,"content_index":0,"delta":"partial"}` + "\n\n")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = consumeChatStreamEvents(stream, nil, nil)
	var streamErr *chatStreamError
	if !errors.As(err, &streamErr) || streamErr.Code != "responses_stream_truncated" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesChatStream_PrecommitTimeoutEmitsRoleAndCloseCleansUp(t *testing.T) {
	prefix := []byte("event: response.created\n" +
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_hidden","created_at":1700000004,"status":"in_progress"}}` + "\n\n" +
		"event: response.in_progress\n" +
		`data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_hidden","created_at":1700000004,"status":"in_progress"}}` + "\n\n")
	body := newResponsesChatBlockingBody(prefix)
	start := time.Now()
	stream, err := prepareResponsesChatStream(context.Background(), body, responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("precommit elapsed = %v", elapsed)
	}
	event, err := stream.next()
	if err != nil || event.kind != chatStreamEventChunk || event.chunk.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first event = %#v, err = %v", event, err)
	}
	stream.stop(context.Canceled)
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream body was not closed")
	}
}

func TestResponsesChatStream_PrecommitByteLimitCommitsAtExactBoundary(t *testing.T) {
	const limit = 1024
	body := newResponsesChatBlockingBody(bytes.Repeat([]byte{':'}, limit))
	stream, err := prepareResponsesChatStream(context.Background(), body, responsesChatStreamConfig{
		PublicModel: "gpt-public", PrecommitTimeout: time.Second, PrecommitMaxBytes: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.next()
	if err != nil || event.chunk.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first event = %#v, err = %v", event, err)
	}
	stream.stop(context.Canceled)
}

func TestResponsesChatStream_EventLimitBoundary(t *testing.T) {
	if responsesChatMaxSSEEventBytes != 8*1024*1024 || responsesChatReadChunkSize != 4*1024 || responsesChatPrecommitMaxBytes != 64*1024 || responsesChatPrecommitTimeout != 750*time.Millisecond {
		t.Fatal("frozen stream constants changed")
	}
	const limit = 2048
	base := "event: response.failed\n" +
		`data: {"type":"response.failed","sequence_number":0,"response":{"id":"resp_limit","status":"failed","error":{"type":"server_error","code":"model_overloaded","message":"limit"},"output":[]}}` + "\n\n"
	padding := limit - len(base) - 2
	if padding < 0 {
		t.Fatal("test event base exceeds limit")
	}
	atLimit := []byte(":" + strings.Repeat("x", padding) + "\n" + base)
	if len(atLimit) != limit {
		t.Fatalf("fixture bytes = %d", len(atLimit))
	}
	_, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(atLimit)), responsesChatStreamConfig{
		PublicModel: "gpt-public", PrecommitTimeout: time.Second, PrecommitMaxBytes: limit + 1, MaxEventBytes: limit,
	})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "model_overloaded" {
		t.Fatalf("at-limit error = %#v", err)
	}

	overLimit := append([]byte("x"), atLimit...)
	_, err = prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(overLimit)), responsesChatStreamConfig{
		PublicModel: "gpt-public", PrecommitTimeout: time.Second, PrecommitMaxBytes: limit + 2, MaxEventBytes: limit,
	})
	if !errors.As(err, &executionErr) || executionErr.Code != "responses_sse_event_too_large" {
		t.Fatalf("over-limit error = %#v", err)
	}
}

func TestResponsesChatStream_CancellationBeforeCommitClosesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := newResponsesChatBlockingBody(nil)
	result := make(chan error, 1)
	go func() {
		_, err := prepareResponsesChatStream(ctx, body, responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
		result <- err
	}()
	<-body.readStarted
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prepare did not return after cancellation")
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("body was not closed")
	}
}

type responsesChatBlockingBody struct {
	prefix      []byte
	readStarted chan struct{}
	closed      chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

func newResponsesChatBlockingBody(prefix []byte) *responsesChatBlockingBody {
	return &responsesChatBlockingBody{prefix: append([]byte(nil), prefix...), readStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (b *responsesChatBlockingBody) Read(p []byte) (int, error) {
	b.startOnce.Do(func() { close(b.readStarted) })
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *responsesChatBlockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestResponsesChatStream_CorrelatesOpaqueChangingItemIDsByOutputIndex(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_text.sse")
	frames := strings.Split(strings.TrimSpace(string(fixture)), "\n\n")
	var rewritten strings.Builder
	for index, frame := range frames {
		lines := strings.Split(frame, "\n")
		eventName := ""
		data := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "event: ") {
				eventName = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatal(err)
		}
		payload["item_id"] = fmt.Sprintf("opaque-event-%d", index)
		if item, ok := payload["item"].(map[string]any); ok {
			item["id"] = fmt.Sprintf("opaque-item-%d", index)
		}
		if response, ok := payload["response"].(map[string]any); ok {
			if output, ok := response["output"].([]any); ok {
				for outputIndex, raw := range output {
					if item, ok := raw.(map[string]any); ok {
						item["id"] = fmt.Sprintf("opaque-terminal-%d-%d", index, outputIndex)
					}
				}
			}
		}
		encoded, _ := json.Marshal(payload)
		fmt.Fprintf(&rewritten, "event: %s\ndata: %s\n\n", eventName, encoded)
	}
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(strings.NewReader(rewritten.String())), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if got := streamChunkText(t, chunks[1]) + streamChunkText(t, chunks[2]); got != "Synthetic fixture text response." {
		t.Fatalf("text = %q", got)
	}
}

func TestResponsesChatStream_EnforcesCumulativeReplayLimitsBeforeTerminal(t *testing.T) {
	state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
	state.createdSeen = true
	_, err := state.handleOutputItemAdded([]byte(`{"output_index":0,"item":{"type":"function_call","id":"opaque-added","call_id":"upstream-call","name":"tool","arguments":""}}`))
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 1024)
	for i := 0; ; i++ {
		_, err = state.handleFunctionArgumentsDelta([]byte(fmt.Sprintf(`{"item_id":"opaque-%d","output_index":0,"delta":%q}`, i, chunk)))
		if err != nil {
			break
		}
	}
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusBadGateway || executionErr.Code != "responses_replay_state_too_large" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesChatStream_MultipleAssistantContentParts(t *testing.T) {
	messageOutput := map[string]any{
		"type": "message", "id": "terminal-message", "status": "completed", "role": "assistant", "phase": "final_answer",
		"content": []any{
			map[string]any{"type": "output_text", "text": "first", "annotations": []any{}},
			map[string]any{"type": "output_text", "text": "second", "annotations": []any{}},
		},
	}
	events := []map[string]any{
		{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp-multipart", "created_at": 1700000000, "status": "in_progress"}},
		{"type": "response.in_progress", "sequence_number": 1},
		{"type": "response.output_item.added", "sequence_number": 2, "output_index": 0, "item": map[string]any{"type": "message", "id": "added-message", "role": "assistant", "status": "in_progress", "phase": "final_answer", "content": []any{}}},
		{"type": "response.content_part.added", "sequence_number": 3, "item_id": "part-a", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		{"type": "response.output_text.delta", "sequence_number": 4, "item_id": "delta-a", "output_index": 0, "content_index": 0, "delta": "first"},
		{"type": "response.output_text.done", "sequence_number": 5, "item_id": "done-a", "output_index": 0, "content_index": 0, "text": "first"},
		{"type": "response.content_part.done", "sequence_number": 6, "item_id": "part-done-a", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "first", "annotations": []any{}}},
		{"type": "response.content_part.added", "sequence_number": 7, "item_id": "part-b", "output_index": 0, "content_index": 1, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		{"type": "response.output_text.delta", "sequence_number": 8, "item_id": "delta-b", "output_index": 0, "content_index": 1, "delta": "second"},
		{"type": "response.output_text.done", "sequence_number": 9, "item_id": "done-b", "output_index": 0, "content_index": 1, "text": "second"},
		{"type": "response.content_part.done", "sequence_number": 10, "item_id": "part-done-b", "output_index": 0, "content_index": 1, "part": map[string]any{"type": "output_text", "text": "second", "annotations": []any{}}},
		{"type": "response.output_item.done", "sequence_number": 11, "output_index": 0, "item": messageOutput},
		{"type": "response.completed", "sequence_number": 12, "response": map[string]any{"id": "resp-multipart", "created_at": 1700000000, "status": "completed", "output": []any{messageOutput}}},
	}
	var fixture strings.Builder
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		fmt.Fprintf(&fixture, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(strings.NewReader(fixture.String())), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 4 || streamChunkText(t, chunks[1]) != "first" || streamChunkText(t, chunks[2]) != "second" || chunks[3].Choices[0].FinishReason == nil || *chunks[3].Choices[0].FinishReason != "stop" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesChatStream_ValidatesAndIgnoresReasoningProgressEvents(t *testing.T) {
	state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp-reasoning","created_at":1700000000,"status":"in_progress"}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"reasoning-1","status":"in_progress"}}`,
		`{"type":"response.reasoning_summary_part.added","sequence_number":2,"item_id":"reasoning-1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","sequence_number":3,"item_id":"reasoning-delta","output_index":0,"summary_index":0,"delta":"hidden"}`,
		`{"type":"response.reasoning_summary_text.done","sequence_number":4,"item_id":"reasoning-summary-done","output_index":0,"summary_index":0,"text":"hidden"}`,
		`{"type":"response.reasoning_summary_part.done","sequence_number":5,"item_id":"reasoning-part-done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"hidden"}}`,
		`{"type":"response.reasoning_text.delta","sequence_number":6,"item_id":"reasoning-text-delta","output_index":0,"content_index":0,"delta":"hidden"}`,
		`{"type":"response.reasoning_text.done","sequence_number":7,"item_id":"reasoning-text-done","output_index":0,"content_index":0,"text":"hidden"}`,
	}
	for _, raw := range events {
		var header struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(raw), &header)
		transition, err := state.handleMessage(responsesSSEMessage{event: header.Type, data: raw, semantic: true})
		if err != nil {
			t.Fatalf("event %s error = %v", header.Type, err)
		}
		if len(transition.chunks) != 0 || transition.terminal {
			t.Fatalf("reasoning event %s produced Chat output: %#v", header.Type, transition)
		}
	}
}

func TestResponsesChatStream_RejectsMalformedReasoningProgressEvents(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
		event string
	}{
		{name: "before response created", event: `{"type":"response.reasoning_text.delta","sequence_number":0,"item_id":"reasoning-1","output_index":0,"content_index":0,"delta":"hidden"}`},
		{name: "without active item", setup: []string{`{"type":"response.created","sequence_number":0,"response":{"id":"resp","status":"in_progress"}}`}, event: `{"type":"response.reasoning_text.delta","sequence_number":1,"item_id":"reasoning-1","output_index":0,"content_index":0,"delta":"hidden"}`},
		{name: "empty item id", setup: []string{`{"type":"response.created","sequence_number":0,"response":{"id":"resp","status":"in_progress"}}`, `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"reasoning-1"}}`}, event: `{"type":"response.reasoning_text.delta","sequence_number":2,"item_id":"","output_index":0,"content_index":0,"delta":"hidden"}`},
		{name: "missing content index", setup: []string{`{"type":"response.created","sequence_number":0,"response":{"id":"resp","status":"in_progress"}}`, `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"reasoning-1"}}`}, event: `{"type":"response.reasoning_text.delta","sequence_number":2,"item_id":"reasoning-1","output_index":0,"delta":"hidden"}`},
		{name: "malformed summary part", setup: []string{`{"type":"response.created","sequence_number":0,"response":{"id":"resp","status":"in_progress"}}`, `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"reasoning-1"}}`}, event: `{"type":"response.reasoning_summary_part.added","sequence_number":2,"item_id":"reasoning-1","output_index":0,"summary_index":0,"part":{"type":"output_text","text":"hidden"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
			for _, raw := range tt.setup {
				var header struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(raw), &header)
				if _, err := state.handleMessage(responsesSSEMessage{event: header.Type, data: raw, semantic: true}); err != nil {
					t.Fatalf("setup event %s error = %v", header.Type, err)
				}
			}
			var header struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(tt.event), &header)
			_, err := state.handleMessage(responsesSSEMessage{event: header.Type, data: tt.event, semantic: true})
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) || executionErr.Code != "invalid_responses_stream" {
				t.Fatalf("error = %#v, want invalid_responses_stream", err)
			}
		})
	}
}

func TestResponsesChatStream_TopLevelErrorPreservesClassification(t *testing.T) {
	fixture := "event: error\ndata: {\"type\":\"error\",\"sequence_number\":0,\"code\":\"too_many_requests\",\"message\":\"slow down\",\"param\":\"model\"}\n\n"
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(strings.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if stream != nil {
		t.Fatal("stream is non-nil")
	}
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusTooManyRequests || executionErr.Type != "rate_limit_error" || executionErr.Code != "too_many_requests" || executionErr.Param != "model" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesChatStream_DoesNotExposeIncompleteFunctionCall(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_one_tool_call.sse")
	frames := strings.Split(strings.TrimSpace(string(fixture)), "\n\n")
	var rewritten strings.Builder
	for _, frame := range frames {
		lines := strings.Split(frame, "\n")
		eventName, data := "", ""
		for _, line := range lines {
			if strings.HasPrefix(line, "event: ") {
				eventName = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatal(err)
		}
		if item, ok := payload["item"].(map[string]any); ok && item["type"] == "function_call" {
			item["status"] = "incomplete"
		}
		if eventName == "response.completed" {
			eventName = "response.incomplete"
			payload["type"] = eventName
			response := payload["response"].(map[string]any)
			response["status"] = "incomplete"
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			for _, raw := range response["output"].([]any) {
				if item, ok := raw.(map[string]any); ok && item["type"] == "function_call" {
					item["status"] = "incomplete"
				}
			}
		}
		encoded, _ := json.Marshal(payload)
		fmt.Fprintf(&rewritten, "event: %s\ndata: %s\n\n", eventName, encoded)
	}
	store := newResponsesChatReplayStore()
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(strings.NewReader(rewritten.String())), responsesChatStreamConfig{
		PublicModel:      "gpt-public",
		ReplayStore:      store,
		ReplayRoute:      responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		PrecommitTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if len(choice.Delta.ToolCalls) != 0 {
				t.Fatalf("incomplete tool call was exposed: %#v", chunks)
			}
		}
	}
	if len(chunks) < 2 || chunks[len(chunks)-2].Choices[0].FinishReason == nil || *chunks[len(chunks)-2].Choices[0].FinishReason != "length" {
		t.Fatalf("chunks = %#v", chunks)
	}
	if stats := store.Stats(); stats.Groups != 0 {
		t.Fatalf("replay stats = %#v", stats)
	}
}

func TestResponsesChatSSEDecoderScansLargePendingEventIncrementally(t *testing.T) {
	decoder := newResponsesChatSSEDecoder(2 << 20)
	payload := "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"" + strings.Repeat("x", 1<<20) + "\"}"
	messages := 0
	for offset := 0; offset < len(payload); offset += 4096 {
		end := offset + 4096
		if end > len(payload) {
			end = len(payload)
		}
		if err := decoder.push([]byte(payload[offset:end]), func(responsesSSEMessage) error { messages++; return nil }); err != nil {
			t.Fatal(err)
		}
		if decoder.scanOffset < len(decoder.pending)-3 {
			t.Fatalf("scanOffset = %d pending = %d", decoder.scanOffset, len(decoder.pending))
		}
	}
	if messages != 0 {
		t.Fatalf("messages before delimiter = %d", messages)
	}
	if err := decoder.push([]byte("\n\n"), func(responsesSSEMessage) error { messages++; return nil }); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || len(decoder.pending) != 0 || decoder.scanOffset != 0 {
		t.Fatalf("messages/pending/offset = %d/%d/%d", messages, len(decoder.pending), decoder.scanOffset)
	}
}

func TestResponsesChatStream_AcceptsCROnlySSELineEndings(t *testing.T) {
	fixture := strings.ReplaceAll(string(readResponsesChatStreamFixture(t, "stream_text.sse")), "\n", "\r")
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(strings.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if got := streamChunkText(t, chunks[1]) + streamChunkText(t, chunks[2]); got != "Synthetic fixture text response." {
		t.Fatalf("text = %q", got)
	}
}

func TestResponsesChatStream_MultipleAssistantMessages(t *testing.T) {
	message := func(id, text, phase string) map[string]any {
		return map[string]any{"type": "message", "id": id, "status": "completed", "role": "assistant", "phase": phase, "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	}
	first, second := message("terminal-first", "first ", "commentary"), message("terminal-second", "second", "final_answer")
	events := []map[string]any{{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp-messages", "created_at": 1700000000, "status": "in_progress"}}}
	sequence := 1
	appendMessage := func(outputIndex int, addedID string, terminal map[string]any, text string, phase string) {
		events = append(events,
			map[string]any{"type": "response.output_item.added", "sequence_number": sequence, "output_index": outputIndex, "item": map[string]any{"type": "message", "id": addedID, "status": "in_progress", "role": "assistant", "phase": phase, "content": []any{}}},
			map[string]any{"type": "response.content_part.added", "sequence_number": sequence + 1, "item_id": addedID + "-part", "output_index": outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			map[string]any{"type": "response.output_text.delta", "sequence_number": sequence + 2, "item_id": addedID + "-delta", "output_index": outputIndex, "content_index": 0, "delta": text},
			map[string]any{"type": "response.output_text.done", "sequence_number": sequence + 3, "item_id": addedID + "-done", "output_index": outputIndex, "content_index": 0, "text": text},
			map[string]any{"type": "response.content_part.done", "sequence_number": sequence + 4, "item_id": addedID + "-part-done", "output_index": outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			map[string]any{"type": "response.output_item.done", "sequence_number": sequence + 5, "output_index": outputIndex, "item": terminal},
		)
		sequence += 6
	}
	appendMessage(0, "added-first", first, "first ", "commentary")
	appendMessage(1, "added-second", second, "second", "final_answer")
	events = append(events, map[string]any{"type": "response.completed", "sequence_number": sequence, "response": map[string]any{"id": "resp-messages", "created_at": 1700000000, "status": "completed", "output": []any{first, second}}})
	var fixture strings.Builder
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		fmt.Fprintf(&fixture, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(strings.NewReader(fixture.String())), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	if len(chunks) != 4 || streamChunkText(t, chunks[1])+streamChunkText(t, chunks[2]) != "first second" || chunks[3].Choices[0].FinishReason == nil || *chunks[3].Choices[0].FinishReason != "stop" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesChatSSEDecoderFinalizesLastEventAtEOFWithoutBlankLine(t *testing.T) {
	decoder := newResponsesChatSSEDecoder(1 << 20)
	fixture := "event: response.completed\ndata: {\"type\":\"response.completed\"}\n"
	var messages []responsesSSEMessage
	if err := decoder.push([]byte(fixture), func(message responsesSSEMessage) error {
		messages = append(messages, message)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages before EOF = %d", len(messages))
	}
	if err := decoder.finalize(func(message responsesSSEMessage) error {
		messages = append(messages, message)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].event != "response.completed" || messages[0].data != `{"type":"response.completed"}` || decoder.hasPending() {
		t.Fatalf("messages/pending = %#v/%v", messages, decoder.hasPending())
	}
}

func TestResponsesChatSSEDecoderRejectsUnterminatedLastLineAtEOF(t *testing.T) {
	decoder := newResponsesChatSSEDecoder(1 << 20)
	if err := decoder.push([]byte("event: response.completed\ndata: {}"), func(responsesSSEMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err := decoder.finalize(func(responsesSSEMessage) error { return nil })
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "invalid_responses_stream" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesChatSSEDecoderWaitsForSplitCRLFDelimiter(t *testing.T) {
	decoder := newResponsesChatSSEDecoder(1 << 20)
	first := "event: response.in_progress\r\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":0}\r\n\r"
	messages := 0
	if err := decoder.push([]byte(first), func(responsesSSEMessage) error { messages++; return nil }); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("messages before final LF = %d", messages)
	}
	second := "\nevent: response.reasoning_text.done\r\ndata: {\"type\":\"response.reasoning_text.done\",\"sequence_number\":1}\r\n\r\n"
	if err := decoder.push([]byte(second), func(responsesSSEMessage) error { messages++; return nil }); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || len(decoder.pending) != 0 {
		t.Fatalf("messages/pending = %d/%d", messages, len(decoder.pending))
	}
}

func TestResponsesChatStream_ChargesPriorVisibleTextWhenToolAppears(t *testing.T) {
	state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt", Now: time.Now})
	state.createdSeen = true
	if _, err := state.handleOutputItemAdded([]byte(`{"output_index":0,"item":{"type":"message","id":"m","role":"assistant","status":"in_progress"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.handleContentPartAdded([]byte(`{"item_id":"p","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`)); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 1024)
	for i := 0; i < responsesChatReplayMaxGroupBytes/len(chunk)+1; i++ {
		if _, err := state.handleOutputTextDelta([]byte(fmt.Sprintf(`{"item_id":"d%d","output_index":0,"content_index":0,"delta":%q}`, i, chunk))); err != nil {
			t.Fatal(err)
		}
	}
	_, err := state.handleOutputItemAdded([]byte(`{"output_index":1,"item":{"type":"function_call","id":"f","call_id":"call","name":"tool","arguments":""}}`))
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "responses_replay_state_too_large" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResponsesChatStream_EmitsUsageBeforeCommittedReplayFailure(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_one_tool_call.sse")
	store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{MaxGroupBytes: 1})
	state := newResponsesChatStreamState(responsesChatStreamConfig{
		PublicModel: "gpt-public",
		ReplayStore: store,
		ReplayRoute: responsesChatReplayRoute{ProviderID: "p", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		Now:         time.Now,
	})
	parser := responsesSSEParser{allowBOM: true}
	parser.push(append(fixture, '\n'))
	var failureTransition responsesChatStreamTransition
	var failure error
	for {
		message, ok := parser.nextSemantic()
		if !ok {
			break
		}
		transition, err := state.handleMessage(message)
		if err != nil {
			failureTransition, failure = transition, err
			break
		}
	}
	var executionErr *chatExecutionError
	if !errors.As(failure, &executionErr) || executionErr.Usage == nil {
		t.Fatalf("error = %#v", failure)
	}
	if len(failureTransition.chunks) != 1 || failureTransition.chunks[0].Usage == nil {
		t.Fatalf("transition = %#v", failureTransition)
	}
}
