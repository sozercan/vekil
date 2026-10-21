package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func rewriteResponsesChatStreamSequenceNumbers(t *testing.T, fixture []byte, number func(int) int64) []byte {
	t.Helper()
	index := 0
	// Keep every semantic event and all other fixture bytes unchanged.
	rewritten := regexp.MustCompile(`"sequence_number":-?[0-9]+`).ReplaceAllFunc(fixture, func([]byte) []byte {
		sequence := number(index)
		index++
		return []byte(fmt.Sprintf(`"sequence_number":%d`, sequence))
	})
	if index == 0 {
		t.Fatal("fixture has no numeric sequence metadata")
	}
	return rewritten
}

func responsesChatStreamSequenceFixtures(t *testing.T, fixture []byte) []struct {
	name string
	data []byte
} {
	t.Helper()
	return []struct {
		name string
		data []byte
	}{
		{"contiguous", fixture},
		{"forward gaps", rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 { return int64(index * 13) })},
		{"repeated", rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(int) int64 { return 17 })},
		{"backward", rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 { return int64(100 - index) })},
		{"reset", rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 { return int64(index % 4) })},
	}
}

func responsesChatStreamParallelToolFixtures(t *testing.T) []struct {
	name string
	data []byte
} {
	t.Helper()
	fixture := readResponsesChatStreamFixture(t, "stream_parallel_interleaved_tool_calls.sse")
	variants := responsesChatStreamSequenceFixtures(t, fixture)
	frames := bytes.Split(fixture, []byte("\n\n"))
	if len(frames) < 4 || !bytes.HasPrefix(frames[2], []byte("event: response.output_item.added\n")) || !bytes.HasPrefix(frames[3], []byte("event: response.output_item.added\n")) {
		t.Fatal("parallel fixture does not have the expected tool announcements")
	}
	frames[2], frames[3] = frames[3], frames[2]
	return append(variants, struct {
		name string
		data []byte
	}{"swapped tool announcements", bytes.Join(frames, []byte("\n\n"))}, struct {
		name string
		data []byte
	}{"numeric-only announcement step back", rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 {
		switch index {
		case 2:
			return 3
		case 3:
			return 2
		default:
			return int64(index)
		}
	})})
}

func TestResponsesChatStream_SequenceDiscontinuitiesPreserveText(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_text.sse")
	config := responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second}
	baseline, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), config)
	if err != nil {
		t.Fatal(err)
	}
	want := collectResponsesChatStreamChunks(t, baseline)
	variants := responsesChatStreamSequenceFixtures(t, fixture)
	for _, gap := range []struct {
		name  string
		index int
	}{
		{"gap before first visible delta", 4},
		{"gap after first visible delta", 5},
		{"gap at terminal", 9},
	} {
		variants = append(variants, struct {
			name string
			data []byte
		}{gap.name, rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 {
			if index >= gap.index {
				return int64(index + 20)
			}
			return int64(index)
		})})
	}
	for _, tt := range variants {
		t.Run(tt.name, func(t *testing.T) {
			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), config)
			if err != nil {
				t.Fatalf("prepareResponsesChatStream() error = %v", err)
			}
			if got := collectResponsesChatStreamChunks(t, stream); !reflect.DeepEqual(got, want) {
				t.Fatalf("chunks = %#v, want unchanged text, finish, and usage %#v", got, want)
			}
		})
	}
}

func TestResponsesChatStream_SequenceGapsRetainValidation(t *testing.T) {
	gap := func(index int) int64 { return int64(index * 13) }
	text := strings.Split(strings.TrimSpace(string(rewriteResponsesChatStreamSequenceNumbers(t,
		readResponsesChatStreamFixture(t, "stream_text.sse"), gap))), "\n\n")
	tools := strings.Split(strings.TrimSpace(string(rewriteResponsesChatStreamSequenceNumbers(t,
		readResponsesChatStreamFixture(t, "stream_parallel_interleaved_tool_calls.sse"), gap))), "\n\n")
	join := func(frames []string) string { return strings.Join(frames, "\n\n") + "\n\n" }
	replace := func(frames []string, index int, old, replacement string) string {
		t.Helper()
		if !strings.Contains(frames[index], old) {
			t.Fatalf("frame %d does not contain %q", index, old)
		}
		changed := append([]string(nil), frames...)
		changed[index] = strings.Replace(changed[index], old, replacement, 1)
		return join(changed)
	}
	duplicateItem := append([]string(nil), text[:3]...)
	duplicateItem = append(duplicateItem, text[2:]...)
	duplicateDelta := append([]string(nil), text[:5]...)
	duplicateDelta = append(duplicateDelta, text[4:]...)
	missingCompletion := append([]string(nil), text[:8]...)
	missingCompletion = append(missingCompletion, text[9:]...)
	terminalReordered := append([]string(nil), tools...)
	_, terminalData, ok := strings.Cut(terminalReordered[len(terminalReordered)-1], "\ndata: ")
	if !ok {
		t.Fatal("parallel fixture has no terminal data")
	}
	var terminal map[string]json.RawMessage
	if err := json.Unmarshal([]byte(terminalData), &terminal); err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(terminal["response"], &response); err != nil {
		t.Fatal(err)
	}
	var output []json.RawMessage
	if err := json.Unmarshal(response["output"], &output); err != nil || len(output) != 2 {
		t.Fatalf("parallel terminal output = %q, error = %v", response["output"], err)
	}
	output[0], output[1] = output[1], output[0]
	response["output"], _ = json.Marshal(output)
	terminal["response"], _ = json.Marshal(response)
	reorderedData, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	terminalReordered[len(terminalReordered)-1] = "event: response.completed\ndata: " + string(reorderedData)
	tests := []struct {
		name        string
		fixture     string
		code        string
		message     string
		committed   bool
		usageTokens int
	}{
		{"duplicate semantic item", join(duplicateItem), "invalid_responses_stream", "duplicate Responses output index", false, 0},
		{"uncorrelated output index", replace(text, 4, `"output_index":0`, `"output_index":9`), "invalid_responses_stream", "output text delta correlation is invalid", false, 0},
		{"uncorrelated content index", replace(text, 4, `"content_index":0`, `"content_index":9`), "invalid_responses_stream", "output text delta correlation is invalid", false, 0},
		{"authoritative text differs", replace(text, 6, "Synthetic fixture text response.", "different text"), "invalid_responses_stream", "output text completion does not match streamed deltas", true, 0},
		{"duplicate semantic delta is not deduplicated", join(duplicateDelta), "invalid_responses_stream", "output text completion does not match streamed deltas", true, 0},
		{"authoritative arguments differ", replace(tools, 7, "beta-fixture", "different-region"), "invalid_responses_stream", "completed function arguments diverge from streamed deltas", false, 0},
		{"terminal arguments differ", replace(tools, 12, "alpha-fixture", "different-widget"), "unsupported_responses_output", "terminal function call does not match streamed arguments", false, 42},
		{"terminal call order differs", join(terminalReordered), "unsupported_responses_output", "terminal function call does not match streamed arguments", false, 42},
		{"terminal requires item completion", join(missingCompletion), "invalid_responses_stream", "response.completed arrived before output completion", true, 17},
		{"unknown event", strings.ReplaceAll(join(text[:4]), "response.content_part.added", "response.future_event"), "unsupported_responses_event", `upstream Responses event "response.future_event" is not supported`, false, 0},
		{"SSE name mismatch", replace(text, 3, "event: response.content_part.added", "event: response.output_item.added"), "invalid_responses_stream", "Responses SSE event name does not match its JSON type", false, 0},
		{"string sequence", replace(text, 3, `"sequence_number":39`, `"sequence_number":"39"`), "invalid_responses_stream", "upstream returned malformed Responses stream JSON", false, 0},
		{"fractional sequence", replace(text, 3, `"sequence_number":39`, `"sequence_number":39.5`), "invalid_responses_stream", "upstream returned malformed Responses stream JSON", false, 0},
		{"overflowing sequence", replace(text, 3, `"sequence_number":39`, `"sequence_number":9223372036854775808`), "invalid_responses_stream", "upstream returned malformed Responses stream JSON", false, 0},
		{"EOF before terminal", join(text[:6]), "responses_stream_truncated", "upstream Responses stream ended before a terminal event", true, 0},
		{"truncated SSE frame", join(text[:5]) + "event: response.completed\ndata: {\"type\":\"response.completed\"", "invalid_responses_stream", "upstream returned truncated Responses SSE", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, chunks, err := runFailingResponsesChatStream(t, []byte(tt.fixture), tt.committed)
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusBadGateway || executionErr.Code != tt.code || executionErr.Message != tt.message {
				t.Fatalf("error = %#v, want %s: %s", err, tt.code, tt.message)
			}
			if tt.usageTokens > 0 {
				if executionErr.Usage == nil || executionErr.Usage.TotalTokens != tt.usageTokens {
					t.Fatalf("error usage = %#v, want %d tokens", executionErr.Usage, tt.usageTokens)
				}
				if tt.committed && (len(chunks) == 0 || chunks[len(chunks)-1].Usage == nil || chunks[len(chunks)-1].Usage.TotalTokens != tt.usageTokens) {
					t.Fatalf("terminal validation lost committed usage: %#v", chunks)
				}
			}
			for _, chunk := range chunks {
				for _, choice := range chunk.Choices {
					if choice.FinishReason != nil || len(choice.Delta.ToolCalls) != 0 {
						t.Fatalf("invalid stream exposed success or tool calls: %#v", chunk)
					}
				}
			}
			if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 {
				t.Fatalf("invalid stream published replay: %#v", stats)
			}
		})
	}
}

// A tolerated discontinuity is logged once per stream, so live traffic shows how
// often upstream numbering skips without one line per event.
func TestResponsesChatStream_SequenceDiscontinuityLoggedOncePerStream(t *testing.T) {
	const message = "tolerated upstream Responses sequence discontinuity"
	fixture := readResponsesChatStreamFixture(t, "stream_text.sse")
	for _, tt := range responsesChatStreamSequenceFixtures(t, fixture) {
		t.Run(tt.name, func(t *testing.T) {
			logs := &conversationLogBuffer{}
			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{
				PublicModel: "gpt-public", PrecommitTimeout: time.Second,
				Carrier: carrierEmit{Log: logger.NewWithWriter(logger.LevelInfo, logs)},
			})
			if err != nil {
				t.Fatalf("prepareResponsesChatStream() error = %v", err)
			}
			collectResponsesChatStreamChunks(t, stream)
			want := 1
			if tt.name == "contiguous" {
				want = 0
			}
			if got := strings.Count(logs.String(), message); got != want {
				t.Fatalf("discontinuity log entries = %d, want %d: %s", got, want, logs.String())
			}
		})
	}
}

func TestResponsesChatStream_SequenceGapsRejectEventsAfterTerminal(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_text.sse")
	fixture = append(bytes.TrimRight(fixture, "\r\n"), []byte("\n\nevent: keepalive\ndata: {\"type\":\"keepalive\",\"sequence_number\":10}\n\n")...)
	fixture = rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 { return int64(index * 13) })
	state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
	decoder := newResponsesChatSSEDecoder(responsesChatMaxSSEEventBytes)
	err := decoder.push(fixture, func(message responsesSSEMessage) error {
		_, err := state.handleMessage(message)
		return err
	})
	var executionErr *chatExecutionError
	if !state.terminalSeen || !errors.As(err, &executionErr) || executionErr.Code != "invalid_responses_stream" || executionErr.Message != "upstream Responses stream continued after a terminal event" {
		t.Fatalf("terminal state = %v, error = %#v", state.terminalSeen, err)
	}
}

func TestResponsesChatStream_SequenceGapsPreserveUpstreamFailures(t *testing.T) {
	text := strings.Split(strings.TrimSpace(string(readResponsesChatStreamFixture(t, "stream_text.sse"))), "\n\n")
	pendingTool := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"type":"function_call","id":"fc_pending","call_id":"call_pending","name":"lookup_synthetic_widget","arguments":"","status":"in_progress"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","sequence_number":6,"item_id":"fc_pending","output_index":1,"delta":"{\"widget\":"}` + "\n\n"
	for _, failure := range []struct {
		name  string
		event string
		usage *models.OpenAIUsage
	}{
		{"response.failed", "event: response.failed\n" + `data: {"type":"response.failed","sequence_number":7,"response":{"id":"resp_synth_text_stream_001","status":"failed","error":{"type":"rate_limit_error","code":"too_many_requests","message":"slow down","param":"model"},"output":[],"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}}` + "\n\n", &models.OpenAIUsage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11}},
		{"top-level error", "event: error\n" + `data: {"type":"error","sequence_number":7,"code":"too_many_requests","message":"slow down","param":"model"}` + "\n\n", nil},
	} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", failure.name, committed), func(t *testing.T) {
				prefix := 4
				if committed {
					prefix = 5
				}
				fixture := []byte(strings.Join(text[:prefix], "\n\n") + "\n\n" + pendingTool + failure.event)
				fixture = rewriteResponsesChatStreamSequenceNumbers(t, fixture, func(index int) int64 { return int64(index * 13) })
				store, chunks, err := runFailingResponsesChatStream(t, fixture, committed)
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusTooManyRequests || executionErr.Type != "rate_limit_error" || executionErr.Code != "too_many_requests" || executionErr.Param != "model" || executionErr.Message != "slow down" || !executionErr.upstreamAuthored {
					t.Fatalf("upstream classification = %#v", err)
				}
				if !reflect.DeepEqual(executionErr.Usage, failure.usage) {
					t.Fatalf("error usage = %#v, want %#v", executionErr.Usage, failure.usage)
				}
				usageChunks := 0
				for _, chunk := range chunks {
					if chunk.Usage != nil {
						usageChunks++
						if !reflect.DeepEqual(chunk.Usage, failure.usage) {
							t.Fatalf("usage chunk = %#v, want %#v", chunk.Usage, failure.usage)
						}
					}
					for _, choice := range chunk.Choices {
						if choice.FinishReason != nil || len(choice.Delta.ToolCalls) != 0 {
							t.Fatalf("upstream failure exposed success or tool calls: %#v", chunk)
						}
					}
				}
				if committed {
					wantChunks := 2
					if failure.usage != nil {
						wantChunks++
					}
					if len(chunks) != wantChunks || chunks[0].Choices[0].Delta.Role != "assistant" || streamChunkText(t, chunks[1]) != "Synthetic fixture " || (usageChunks == 1) != (failure.usage != nil) {
						t.Fatalf("failure chunks = %#v", chunks)
					}
				}
				if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 {
					t.Fatalf("upstream failure published replay: %#v", stats)
				}
			})
		}
	}
}

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

func TestResponsesChatStreamPreservesBillingPublicIdentity(t *testing.T) {
	const billing = `{"total_nano_aiu":27,"compute_units":2,"token_details":[{"model":"physical-terminal"}]}`
	fixture := bytes.Replace(readResponsesChatStreamFixture(t, "stream_text.sse"),
		[]byte(`"type":"response.completed"`), []byte(`"type":"response.completed","copilot_usage":`+billing), 1)
	stream, err := prepareResponsesChatStream(t.Context(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{PublicModel: "gpt-public"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	var observed copilotUsageTotals
	err = streamChatEventsToOpenAI(recorder, stream, chatStreamEventCallbacks{
		DropUsage: true,
		OnCopilotUsage: func(raw json.RawMessage) {
			observed, _ = parseCopilotUsage(raw)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed != (copilotUsageTotals{TotalNanoAIU: 27, ComputeUnits: 2}) {
		t.Fatalf("observed accounting = %+v", observed)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "physical-terminal") || !strings.Contains(body, `"copilot_usage"`) || !strings.Contains(body, `"token_details":[{"model":"gpt-public"}]`) {
		t.Fatalf("canonical stream lost accounting or leaked model identity: %s", body)
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

func streamChunkVisibleValue(t *testing.T, chunk models.OpenAIStreamChunk) (string, string) {
	t.Helper()
	if len(chunk.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(chunk.Choices))
	}
	delta := chunk.Choices[0].Delta
	kind, raw := "output_text", delta.Content
	if len(delta.Refusal) > 0 {
		if len(raw) > 0 {
			t.Fatalf("chunk contains both text and refusal: %#v", chunk)
		}
		kind, raw = "refusal", delta.Refusal
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode visible %s: %v", kind, err)
	}
	return kind, value
}

func responsesChatStreamVisiblePart(kind, value string) map[string]any {
	if kind == "refusal" {
		return map[string]any{"type": kind, "refusal": value}
	}
	return map[string]any{"type": kind, "text": value, "annotations": []any{}}
}

func responsesChatStreamVisibleKindPairs() [][2]string {
	return [][2]string{
		{"output_text", "output_text"},
		{"output_text", "refusal"},
		{"refusal", "output_text"},
		{"refusal", "refusal"},
	}
}

// runFailingResponsesChatStream runs a stream that must fail. A committed stream is consumed to
// its error; an uncommitted one must not be exposed at all.
func runFailingResponsesChatStream(t *testing.T, fixture []byte, committed bool) (*responsesChatReplayStore, []models.OpenAIStreamChunk, error) {
	t.Helper()
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
		PublicModel: "gpt-public", ReplayStore: store,
		ReplayRoute:      responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		PrecommitTimeout: time.Second,
	})
	if !committed {
		if stream != nil {
			stream.stop(context.Canceled)
			t.Fatalf("failing precommit stream was exposed (error %v)", err)
		}
		return store, nil, err
	}
	if err != nil || stream == nil {
		t.Fatalf("prepareResponsesChatStream() = %v, %v, want committed stream", stream, err)
	}
	var chunks []models.OpenAIStreamChunk
	err = consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	}, nil)
	return store, chunks, err
}

func assertResponsesChatStreamOrderingFailure(t *testing.T, fixture []byte, wantVisible []string, wantMessage string) {
	t.Helper()
	store, chunks, err := runFailingResponsesChatStream(t, fixture, len(wantVisible) > 0)
	var visible []string
	var finish string
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if len(choice.Delta.Content) > 0 || len(choice.Delta.Refusal) > 0 {
				kind, value := streamChunkVisibleValue(t, chunk)
				visible = append(visible, kind+":"+value)
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
			if len(choice.Delta.ToolCalls) > 0 {
				t.Fatalf("out-of-order stream exposed tool calls: %#v", chunk)
			}
		}
	}
	if !reflect.DeepEqual(visible, wantVisible) || finish != "" {
		t.Fatalf("out-of-order stream exposed %q, finish %q, error %v; want %q without success", visible, finish, err, wantVisible)
	}
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.StatusCode != http.StatusBadGateway || executionErr.Code != "invalid_responses_stream" || executionErr.Message != wantMessage {
		t.Fatalf("error = %#v, want semantic ordering failure: %s", err, wantMessage)
	}
	if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 {
		t.Fatalf("out-of-order stream published replay: %#v", stats)
	}
}

func TestResponsesChatStream_OneToolPublishesReplayBeforeProxyID(t *testing.T) {
	fixture := readResponsesChatStreamFixture(t, "stream_one_tool_call.sse")
	for _, tt := range responsesChatStreamSequenceFixtures(t, fixture) {
		t.Run(tt.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			route := responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}

			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{
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
			if len(start) != 1 || start[0].Index == nil || *start[0].Index != 0 || start[0].Function.Name != "lookup_synthetic_widget" {
				t.Fatalf("tool start = %#v", start)
			}
			if upstreamID, ok := responsesChatReplayUpstreamCallID(start[0].ID); !ok || upstreamID != "call_synth_lookup_stream_001" {
				t.Fatalf("self-describing proxy ID = %q, decoded %q, %v", start[0].ID, upstreamID, ok)
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
		})
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

func TestResponsesChatStream_ParallelToolsUseDenseOutputIndexes(t *testing.T) {
	for _, tt := range responsesChatStreamParallelToolFixtures(t) {
		t.Run(tt.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			route := responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{
				PublicModel: "gpt-public", ReplayStore: store, ReplayRoute: route,
				PrecommitTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			var chunks []models.OpenAIStreamChunk
			err = consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
				for _, choice := range chunk.Choices {
					for _, call := range choice.Delta.ToolCalls {
						if call.ID != "" {
							if stats := store.Stats(); stats.Groups != 1 || stats.Calls != 2 {
								t.Fatalf("tool ID exposed before parallel replay publication: %#v", stats)
							}
						}
					}
				}
				chunks = append(chunks, chunk)
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) != 7 {
				t.Fatalf("chunk count = %d, want 7", len(chunks))
			}
			wantNames := []string{"lookup_synthetic_widget", "inspect_synthetic_region"}
			wantArguments := []string{`{"widget":"alpha-fixture"}`, `{"region":"beta-fixture"}`}
			wantUpstreamIDs := []string{"call_synth_parallel_alpha_stream_001", "call_synth_parallel_beta_stream_001"}
			projection := responsesChatReplayAssistantProjection{Content: json.RawMessage(`""`)}
			opaqueID := regexp.MustCompile(`^call_vekil_[A-Za-z0-9_-]{22}$`)
			for i, chunkIndex := range []int{1, 3} {
				call := chunks[chunkIndex].Choices[0].Delta.ToolCalls[0]
				if call.Index == nil || *call.Index != i || call.Type != "function" || call.Function.Name != wantNames[i] || !opaqueID.MatchString(call.ID) {
					t.Fatalf("tool start %d = %#v", i, call)
				}
				args := chunks[chunkIndex+1].Choices[0].Delta.ToolCalls[0]
				if args.Index == nil || *args.Index != i || args.Function.Arguments != wantArguments[i] {
					t.Fatalf("tool args %d = %#v", i, args)
				}
				projection.Calls = append(projection.Calls, responsesChatReplayProjectedCall{
					ID: call.ID, Name: call.Function.Name, Arguments: args.Function.Arguments,
				})
			}
			if projection.Calls[0].ID == projection.Calls[1].ID {
				t.Fatal("parallel calls share a proxy ID")
			}
			if got := chunks[5].Choices[0].FinishReason; got == nil || *got != "tool_calls" {
				t.Fatalf("finish reason = %v, want tool_calls", got)
			}
			if usage := chunks[6].Usage; usage == nil || usage.PromptTokens != 24 || usage.CompletionTokens != 18 || usage.TotalTokens != 42 {
				t.Fatalf("terminal usage = %#v", usage)
			}
			resolved, err := store.Resolve(route, projection)
			if err != nil || len(resolved.OutputItems) != 2 {
				t.Fatalf("Resolve() = %#v, %v", resolved, err)
			}
			for i, raw := range resolved.OutputItems {
				var call struct {
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}
				if err := json.Unmarshal(raw, &call); err != nil {
					t.Fatal(err)
				}
				if call.CallID != wantUpstreamIDs[i] || call.Name != wantNames[i] || call.Arguments != wantArguments[i] {
					t.Fatalf("replayed call %d = %#v", i, call)
				}
			}
		})
	}
}

func TestResponsesChatStream_ParallelToolsReplayActualAggregatedAssistant(t *testing.T) {
	wantNames := []string{"lookup_synthetic_widget", "inspect_synthetic_region"}
	wantArguments := []string{`{"widget":"alpha-fixture"}`, `{"region":"beta-fixture"}`}
	wantUpstreamIDs := []string{"call_synth_parallel_alpha_stream_001", "call_synth_parallel_beta_stream_001"}
	wantCalls := []map[string]any{
		{"type": "function_call", "id": "fc_synth_parallel_alpha_stream_001", "call_id": wantUpstreamIDs[0], "name": wantNames[0], "arguments": wantArguments[0], "status": "completed"},
		{"type": "function_call", "id": "fc_synth_parallel_beta_stream_001", "call_id": wantUpstreamIDs[1], "name": wantNames[1], "arguments": wantArguments[1], "status": "completed"},
	}
	for _, tt := range responsesChatStreamParallelToolFixtures(t) {
		t.Run(tt.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			route := responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{
				PublicModel: "gpt-public", ReplayStore: store, ReplayRoute: route, PrecommitTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err := aggregateChatStreamEvents(stream)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 2 {
				t.Fatalf("aggregated response = %#v", response)
			}
			assistant := response.Choices[0].Message
			content, err := translateChatMessageContent(assistant.Content, assistant.Role, 0)
			if err != nil {
				t.Fatal(err)
			}
			projectionContent, err := json.Marshal(assistantHistoryText(content))
			if err != nil {
				t.Fatal(err)
			}
			proxyIDs := make(map[string]string, len(assistant.ToolCalls))
			projection := responsesChatReplayAssistantProjection{Content: projectionContent}
			for _, call := range assistant.ToolCalls {
				proxyIDs[call.Function.Name] = call.ID
				projection.Calls = append(projection.Calls, responsesChatReplayProjectedCall{
					ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
				})
			}
			if proxyIDs[wantNames[0]] == "" || proxyIDs[wantNames[1]] == "" {
				t.Fatalf("aggregated tools = %#v", assistant.ToolCalls)
			}
			t.Run("canonical assistant order", func(t *testing.T) {
				for index, call := range assistant.ToolCalls {
					if call.Function.Name != wantNames[index] || call.Function.Arguments != wantArguments[index] {
						t.Errorf("aggregated tool %d = %#v", index, call)
					}
				}
			})
			t.Run("actual replay projection", func(t *testing.T) {
				resolved, err := store.Resolve(route, projection)
				if err != nil {
					t.Fatalf("Resolve(actual aggregated assistant) = %T: %v", err, err)
				}
				var calls []map[string]any
				for _, raw := range resolved.OutputItems {
					var call map[string]any
					if err := json.Unmarshal(raw, &call); err != nil {
						t.Fatal(err)
					}
					calls = append(calls, call)
				}
				if !reflect.DeepEqual(calls, wantCalls) {
					t.Fatalf("replayed calls = %#v, want %#v", calls, wantCalls)
				}
			})
			for _, continuation := range []struct {
				name    string
				results []map[string]any
				want    []map[string]any
			}{
				{
					name: "full reversed results",
					results: []map[string]any{
						{"role": "tool", "tool_call_id": proxyIDs[wantNames[1]], "content": "result beta"},
						{"role": "tool", "tool_call_id": proxyIDs[wantNames[0]], "content": "result alpha"},
					},
					want: append(append([]map[string]any(nil), wantCalls...),
						map[string]any{"type": "function_call_output", "call_id": wantUpstreamIDs[1], "output": "result beta"},
						map[string]any{"type": "function_call_output", "call_id": wantUpstreamIDs[0], "output": "result alpha"}),
				},
				{
					name: "partial beta result",
					results: []map[string]any{
						{"role": "tool", "tool_call_id": proxyIDs[wantNames[1]], "content": "result beta"},
					},
					want: []map[string]any{
						wantCalls[1],
						{"type": "function_call_output", "call_id": wantUpstreamIDs[1], "output": "result beta"},
					},
				},
			} {
				t.Run(continuation.name, func(t *testing.T) {
					messages := []any{assistant}
					for _, result := range continuation.results {
						messages = append(messages, result)
					}
					request, err := json.Marshal(map[string]any{"model": "gpt-public", "messages": messages})
					if err != nil {
						t.Fatal(err)
					}
					plan, err := translateChatRequestToResponses(request, responsesChatRequestOptions{
						UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route,
					})
					if err != nil {
						var executionErr *chatExecutionError
						if errors.As(err, &executionErr) {
							t.Fatalf("unchanged assistant continuation: status=%d code=%s: %v", executionErr.StatusCode, executionErr.Code, err)
						}
						t.Fatal(err)
					}
					var upstream struct {
						Input []map[string]any `json:"input"`
					}
					if err := json.Unmarshal(plan.Body, &upstream); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(upstream.Input, continuation.want) {
						t.Fatalf("continued input = %#v, want %#v", upstream.Input, continuation.want)
					}
				})
			}
		})
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

func TestResponsesChatStream_UnknownEventLogsOnlyBoundedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name           string
		eventType      string
		wantLoggedType string
	}{
		{"private payload", "response.future_event", "response.future_event"},
		{"at limit", strings.Repeat("x", 128), strings.Repeat("x", 128)},
		{"over limit", strings.Repeat("x", 129), strings.Repeat("x", 128)},
		{"large event", strings.Repeat("x", 2<<20), strings.Repeat("x", 128)},
		{"Unicode at limit", strings.Repeat("x", 125) + "☃", strings.Repeat("x", 125) + "☃"},
		{"Unicode across limit", strings.Repeat("x", 127) + "☃", strings.Repeat("x", 127)},
		{"JSON escaping", strings.Repeat("<", 256), strings.Repeat("<", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const privateText = "private-response-text"
			const privateArgument = "private-tool-argument"
			payload, err := json.Marshal(map[string]any{
				"type":            tc.eventType,
				"sequence_number": 1,
				"delta":           privateText,
				"arguments":       `{"value":"` + privateArgument + `"}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture := "event: response.created\n" +
				`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_unknown","created_at":1700000003,"status":"in_progress"}}` + "\n\n" +
				"data: " + string(payload) + "\n\n"
			var logs bytes.Buffer
			stream, err := prepareResponsesChatStream(t.Context(), io.NopCloser(strings.NewReader(fixture)), responsesChatStreamConfig{
				PublicModel:      "gpt-public",
				PrecommitTimeout: time.Second,
				Carrier:          carrierEmit{Log: logger.NewWithWriter(logger.LevelInfo, &logs)},
			})
			if err == nil {
				// Large events cross the precommit byte limit before their type is known.
				err = consumeChatStreamEvents(stream, nil, nil)
			}
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) {
				t.Fatalf("error type = %T, want *chatExecutionError", err)
			}
			if executionErr.Code != "unsupported_responses_event" || executionErr.StatusCode != http.StatusBadGateway {
				t.Errorf("error code/status = %s/%d, want unsupported_responses_event/502", executionErr.Code, executionErr.StatusCode)
			}
			if executionErr.Message != fmt.Sprintf("upstream Responses event %q is not supported", tc.eventType) {
				t.Error("error message did not preserve the original event type")
			}

			if logs.Len() > 1024 {
				t.Errorf("warning size = %d bytes, want at most 1024", logs.Len())
			}
			for _, privateValue := range []string{privateText, privateArgument} {
				if strings.Contains(logs.String(), privateValue) {
					t.Errorf("warning contains private payload value %q", privateValue)
				}
			}
			var entry struct {
				Level       string          `json:"level"`
				Message     string          `json:"msg"`
				EventType   string          `json:"event_type"`
				DataBytes   int             `json:"data_bytes"`
				DataPreview json.RawMessage `json:"data_preview"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("decode warning: %v", err)
			}
			if entry.Level != "warn" || entry.Message != "unhandled upstream Responses event" {
				t.Error("unknown event warning is missing")
			}
			if entry.EventType != tc.wantLoggedType {
				t.Errorf("logged event type differs from expected prefix: got %d bytes, want %d", len(entry.EventType), len(tc.wantLoggedType))
			}
			if entry.DataBytes != len(payload) {
				t.Errorf("data_bytes = %d, want %d", entry.DataBytes, len(payload))
			}
			if entry.DataPreview != nil {
				t.Error("warning contains data_preview")
			}
		})
	}
}

// TestResponsesChatStream_KeepaliveIsDroppedBetweenRealEvents: Copilot
// emits `event: keepalive` during long generations; the case arm drops
// it and lets the surrounding text still assemble.
func TestResponsesChatStream_KeepaliveIsDroppedBetweenRealEvents(t *testing.T) {
	fixture := []byte("event: response.created\n" +
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_ka","created_at":1700000003,"status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_ka","status":"in_progress","role":"assistant","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_ka","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
		"event: keepalive\n" +
		`data: {"type":"keepalive","sequence_number":3}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_ka","output_index":0,"content_index":0,"delta":"hello"}` + "\n\n" +
		"event: keepalive\n" +
		`data: {"type":"keepalive","sequence_number":5}` + "\n\n" +
		"event: response.output_text.done\n" +
		`data: {"type":"response.output_text.done","sequence_number":6,"item_id":"msg_ka","output_index":0,"content_index":0,"text":"hello"}` + "\n\n" +
		"event: response.content_part.done\n" +
		`data: {"type":"response.content_part.done","sequence_number":7,"item_id":"msg_ka","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"type":"message","id":"msg_ka","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_ka","created_at":1700000003,"status":"completed","output":[{"type":"message","id":"msg_ka","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}` + "\n\n")
	for _, tt := range responsesChatStreamSequenceFixtures(t, fixture) {
		t.Run(tt.name, func(t *testing.T) {
			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
			if err != nil {
				t.Fatalf("prepareResponsesChatStream returned error: %v", err)
			}
			chunks := collectResponsesChatStreamChunks(t, stream)
			if len(chunks) != 3 || chunks[0].Choices[0].Delta.Role != "assistant" || streamChunkText(t, chunks[1]) != "hello" {
				t.Fatalf("keepalive changed visible output: %#v", chunks)
			}
			if finish := chunks[2].Choices[0].FinishReason; finish == nil || *finish != "stop" {
				t.Fatalf("finish reason = %v, want stop", finish)
			}
		})
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

func responsesChatStreamMultipleAssistantContentPartsFixture(t *testing.T, firstKind, secondKind string) []byte {
	t.Helper()
	firstValueKey, secondValueKey := "text", "text"
	if firstKind == "refusal" {
		firstValueKey = "refusal"
	}
	if secondKind == "refusal" {
		secondValueKey = "refusal"
	}
	messageOutput := map[string]any{
		"type": "message", "id": "terminal-message", "status": "completed", "role": "assistant", "phase": "final_answer",
		"content": []any{responsesChatStreamVisiblePart(firstKind, "first"), responsesChatStreamVisiblePart(secondKind, "second")},
	}
	events := []map[string]any{
		{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp-multipart", "created_at": 1700000000, "status": "in_progress"}},
		{"type": "response.in_progress", "sequence_number": 1},
		{"type": "response.output_item.added", "sequence_number": 2, "output_index": 0, "item": map[string]any{"type": "message", "id": "added-message", "role": "assistant", "status": "in_progress", "phase": "final_answer", "content": []any{}}},
		{"type": "response.content_part.added", "sequence_number": 3, "item_id": "part-a", "output_index": 0, "content_index": 0, "part": responsesChatStreamVisiblePart(firstKind, "")},
		{"type": "response." + firstKind + ".delta", "sequence_number": 4, "item_id": "delta-a", "output_index": 0, "content_index": 0, "delta": "first"},
		{"type": "response." + firstKind + ".done", "sequence_number": 5, "item_id": "done-a", "output_index": 0, "content_index": 0, firstValueKey: "first"},
		{"type": "response.content_part.done", "sequence_number": 6, "item_id": "part-done-a", "output_index": 0, "content_index": 0, "part": responsesChatStreamVisiblePart(firstKind, "first")},
		{"type": "response.content_part.added", "sequence_number": 7, "item_id": "part-b", "output_index": 0, "content_index": 1, "part": responsesChatStreamVisiblePart(secondKind, "")},
		{"type": "response." + secondKind + ".delta", "sequence_number": 8, "item_id": "delta-b", "output_index": 0, "content_index": 1, "delta": "second"},
		{"type": "response." + secondKind + ".done", "sequence_number": 9, "item_id": "done-b", "output_index": 0, "content_index": 1, secondValueKey: "second"},
		{"type": "response.content_part.done", "sequence_number": 10, "item_id": "part-done-b", "output_index": 0, "content_index": 1, "part": responsesChatStreamVisiblePart(secondKind, "second")},
		{"type": "response.output_item.done", "sequence_number": 11, "output_index": 0, "item": messageOutput},
		{"type": "response.completed", "sequence_number": 12, "response": map[string]any{"id": "resp-multipart", "created_at": 1700000000, "status": "completed", "output": []any{messageOutput}}},
	}
	var fixture strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&fixture, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
	return []byte(fixture.String())
}

func TestResponsesChatStream_MultipleAssistantContentParts(t *testing.T) {
	for _, kinds := range responsesChatStreamVisibleKindPairs() {
		t.Run(strings.Join(kinds[:], "/"), func(t *testing.T) {
			fixture := responsesChatStreamMultipleAssistantContentPartsFixture(t, kinds[0], kinds[1])
			frames := strings.Split(strings.TrimSpace(string(fixture)), "\n\n")
			reorder := func(order ...int) []byte {
				var rewritten strings.Builder
				for _, index := range order {
					fmt.Fprintf(&rewritten, "%s\n\n", frames[index])
				}
				return []byte(rewritten.String())
			}
			emptyProgress := bytes.Replace(reorder(0, 1, 2, 3, 7, 8, 4, 5, 6, 8, 9, 10, 11, 12), []byte(`"delta":"second"`), []byte(`"delta":""`), 1)
			for _, input := range []struct {
				name string
				data []byte
			}{
				{"serial parts", fixture},
				{"parts announced before text", reorder(0, 1, 2, 3, 7, 4, 5, 6, 8, 9, 10, 11, 12)},
				{"part done after later text", reorder(0, 1, 2, 3, 4, 5, 7, 8, 9, 6, 10, 11, 12)},
				{"empty later delta before earlier text", emptyProgress},
			} {
				t.Run(input.name, func(t *testing.T) {
					normalized := rewriteResponsesChatStreamSequenceNumbers(t, input.data, func(index int) int64 { return int64(index) })
					for _, tt := range responsesChatStreamSequenceFixtures(t, normalized) {
						t.Run(tt.name, func(t *testing.T) {
							stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
							if err != nil {
								t.Fatal(err)
							}
							chunks := collectResponsesChatStreamChunks(t, stream)
							if len(chunks) != 4 || chunks[3].Choices[0].FinishReason == nil || *chunks[3].Choices[0].FinishReason != "stop" {
								t.Fatalf("chunks = %#v", chunks)
							}
							for index, want := range []string{"first", "second"} {
								kind, value := streamChunkVisibleValue(t, chunks[index+1])
								if kind != kinds[index] || value != want {
									t.Fatalf("visible part %d = %s %q, want %s %q", index, kind, value, kinds[index], want)
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestResponsesChatStream_RejectsOutOfOrderAssistantContentParts(t *testing.T) {
	for _, kinds := range responsesChatStreamVisibleKindPairs() {
		t.Run(strings.Join(kinds[:], "/"), func(t *testing.T) {
			frames := strings.Split(strings.TrimSpace(string(responsesChatStreamMultipleAssistantContentPartsFixture(t, kinds[0], kinds[1]))), "\n\n")
			for _, tt := range []struct {
				name      string
				order     []int
				committed bool
			}{
				{name: "whole parts reversed", order: []int{0, 1, 2, 7, 8, 9, 10, 3, 4, 5, 6, 11, 12}},
				{name: "parts announced before reversed text", order: []int{0, 1, 2, 3, 7, 8, 9, 10, 4, 5, 6, 11, 12}},
				{name: "interleaved text after commit", order: []int{0, 1, 2, 3, 4, 7, 8, 9, 10, 4, 5, 6, 11, 12}, committed: true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					var fixture strings.Builder
					firstFragment := true
					for _, index := range tt.order {
						frame := frames[index]
						if tt.committed && index == 4 {
							fragment := "fi"
							if !firstFragment {
								fragment = "rst"
							}
							frame = strings.Replace(frame, `"delta":"first"`, fmt.Sprintf(`"delta":%q`, fragment), 1)
							firstFragment = false
						}
						fmt.Fprintf(&fixture, "%s\n\n", frame)
					}
					var wantVisible []string
					if tt.committed {
						wantVisible = []string{kinds[0] + ":fi"}
					}
					assertResponsesChatStreamOrderingFailure(t, []byte(fixture.String()), wantVisible, "assistant content parts are out of order")
				})
			}
		})
	}
}

func TestResponsesChatStream_RejectsSparseVisibleIndexes(t *testing.T) {
	for _, kind := range []string{"output_text", "refusal"} {
		t.Run(kind, func(t *testing.T) {
			fixture := responsesChatStreamMultipleAssistantContentPartsFixture(t, kind, kind)
			for _, field := range []string{"content_index", "output_index"} {
				for _, index := range []int{1, int(^uint(0) >> 1)} {
					t.Run(fmt.Sprintf("%s=%d", field, index), func(t *testing.T) {
						original := []byte(fmt.Sprintf(`"%s":0`, field))
						if !bytes.Contains(fixture, original) {
							t.Fatalf("fixture has no %s", original)
						}
						rewritten := bytes.ReplaceAll(fixture, original, []byte(fmt.Sprintf(`"%s":%d`, field, index)))
						message := "assistant content parts are out of order"
						if field == "output_index" {
							message = "assistant output messages are out of order"
						}
						assertResponsesChatStreamOrderingFailure(t, rewritten, nil, message)
					})
				}
			}
		})
	}
}

func TestResponsesChatStream_ValidatesAndIgnoresReasoningProgressEvents(t *testing.T) {
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
	var fixture strings.Builder
	for _, raw := range events {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(raw), &header); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&fixture, "event: %s\ndata: %s\n\n", header.Type, raw)
	}
	for _, tt := range responsesChatStreamSequenceFixtures(t, []byte(fixture.String())) {
		t.Run(tt.name, func(t *testing.T) {
			state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
			decoder := newResponsesChatSSEDecoder(responsesChatMaxSSEEventBytes)
			processed := 0
			err := decoder.push(tt.data, func(message responsesSSEMessage) error {
				transition, err := state.handleMessage(message)
				if err != nil {
					return err
				}
				if len(transition.chunks) != 0 || transition.terminal {
					t.Fatalf("reasoning event %s produced Chat output: %#v", message.event, transition)
				}
				processed++
				return nil
			})
			if err != nil || processed != len(events) || decoder.hasPending() {
				t.Fatalf("reasoning progress decoded %d events, pending = %v, error = %v", processed, decoder.hasPending(), err)
			}
		})
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

func responsesChatStreamMultipleAssistantMessagesFixture(t *testing.T, firstKind, secondKind string) []byte {
	t.Helper()
	message := func(id, text, phase, kind string) map[string]any {
		return map[string]any{"type": "message", "id": id, "status": "completed", "role": "assistant", "phase": phase, "content": []any{responsesChatStreamVisiblePart(kind, text)}}
	}
	first, second := message("terminal-first", "first ", "commentary", firstKind), message("terminal-second", "second", "final_answer", secondKind)
	events := []map[string]any{{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp-messages", "created_at": 1700000000, "status": "in_progress"}}}
	sequence := 1
	appendMessage := func(outputIndex int, addedID string, terminal map[string]any, text, phase, kind string) {
		valueKey := "text"
		if kind == "refusal" {
			valueKey = "refusal"
		}
		events = append(events,
			map[string]any{"type": "response.output_item.added", "sequence_number": sequence, "output_index": outputIndex, "item": map[string]any{"type": "message", "id": addedID, "status": "in_progress", "role": "assistant", "phase": phase, "content": []any{}}},
			map[string]any{"type": "response.content_part.added", "sequence_number": sequence + 1, "item_id": addedID + "-part", "output_index": outputIndex, "content_index": 0, "part": responsesChatStreamVisiblePart(kind, "")},
			map[string]any{"type": "response." + kind + ".delta", "sequence_number": sequence + 2, "item_id": addedID + "-delta", "output_index": outputIndex, "content_index": 0, "delta": text},
			map[string]any{"type": "response." + kind + ".done", "sequence_number": sequence + 3, "item_id": addedID + "-done", "output_index": outputIndex, "content_index": 0, valueKey: text},
			map[string]any{"type": "response.content_part.done", "sequence_number": sequence + 4, "item_id": addedID + "-part-done", "output_index": outputIndex, "content_index": 0, "part": responsesChatStreamVisiblePart(kind, text)},
			map[string]any{"type": "response.output_item.done", "sequence_number": sequence + 5, "output_index": outputIndex, "item": terminal},
		)
		sequence += 6
	}
	appendMessage(0, "added-first", first, "first ", "commentary", firstKind)
	appendMessage(1, "added-second", second, "second", "final_answer", secondKind)
	events = append(events, map[string]any{"type": "response.completed", "sequence_number": sequence, "response": map[string]any{"id": "resp-messages", "created_at": 1700000000, "status": "completed", "output": []any{first, second}}})
	var fixture strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&fixture, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
	return []byte(fixture.String())
}

func TestResponsesChatStream_MultipleAssistantMessages(t *testing.T) {
	for _, kinds := range responsesChatStreamVisibleKindPairs() {
		t.Run(strings.Join(kinds[:], "/"), func(t *testing.T) {
			fixture := responsesChatStreamMultipleAssistantMessagesFixture(t, kinds[0], kinds[1])
			frames := strings.Split(strings.TrimSpace(string(fixture)), "\n\n")
			var preannounced strings.Builder
			for _, index := range []int{0, 1, 2, 7, 8, 3, 4, 5, 6, 9, 10, 11, 12, 13} {
				fmt.Fprintf(&preannounced, "%s\n\n", frames[index])
			}
			for _, input := range []struct {
				name string
				data []byte
			}{
				{"serial messages", fixture},
				{"messages announced before text", []byte(preannounced.String())},
			} {
				t.Run(input.name, func(t *testing.T) {
					normalized := rewriteResponsesChatStreamSequenceNumbers(t, input.data, func(index int) int64 { return int64(index) })
					for _, tt := range responsesChatStreamSequenceFixtures(t, normalized) {
						t.Run(tt.name, func(t *testing.T) {
							stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Second})
							if err != nil {
								t.Fatal(err)
							}
							chunks := collectResponsesChatStreamChunks(t, stream)
							if len(chunks) != 4 || chunks[3].Choices[0].FinishReason == nil || *chunks[3].Choices[0].FinishReason != "stop" {
								t.Fatalf("chunks = %#v", chunks)
							}
							for index, want := range []string{"first ", "second"} {
								kind, value := streamChunkVisibleValue(t, chunks[index+1])
								if kind != kinds[index] || value != want {
									t.Fatalf("visible message %d = %s %q, want %s %q", index, kind, value, kinds[index], want)
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestResponsesChatStream_RejectsOutOfOrderAssistantMessages(t *testing.T) {
	for _, kinds := range responsesChatStreamVisibleKindPairs() {
		t.Run(strings.Join(kinds[:], "/"), func(t *testing.T) {
			frames := strings.Split(strings.TrimSpace(string(responsesChatStreamMultipleAssistantMessagesFixture(t, kinds[0], kinds[1]))), "\n\n")
			for _, tt := range []struct {
				name      string
				order     []int
				committed bool
			}{
				{name: "completed message groups reversed", order: []int{0, 7, 8, 9, 10, 11, 12, 1, 2, 3, 4, 5, 6, 13}},
				{name: "messages announced before reversed text", order: []int{0, 1, 2, 7, 8, 9, 10, 11, 12, 3, 4, 5, 6, 13}},
				{name: "interleaved message text after commit", order: []int{0, 1, 2, 3, 7, 8, 9, 10, 11, 12, 3, 4, 5, 6, 13}, committed: true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					var fixture strings.Builder
					firstFragment := true
					for _, index := range tt.order {
						frame := frames[index]
						if tt.committed && index == 3 {
							fragment := "fi"
							if !firstFragment {
								fragment = "rst "
							}
							frame = strings.Replace(frame, `"delta":"first "`, fmt.Sprintf(`"delta":%q`, fragment), 1)
							firstFragment = false
						}
						fmt.Fprintf(&fixture, "%s\n\n", frame)
					}
					var wantVisible []string
					if tt.committed {
						wantVisible = []string{kinds[0] + ":fi"}
					}
					assertResponsesChatStreamOrderingFailure(t, []byte(fixture.String()), wantVisible, "assistant output messages are out of order")
				})
			}
		})
	}
}

func TestResponsesChatStream_VisibleOrderAllowsUnfinishedNonvisibleItems(t *testing.T) {
	frames := strings.Split(strings.TrimSpace(string(responsesChatStreamMultipleAssistantMessagesFixture(t, "output_text", "output_text"))), "\n\n")
	frameEvent := func(index int) map[string]any {
		t.Helper()
		_, data, ok := strings.Cut(frames[index], "\ndata: ")
		if !ok {
			t.Fatalf("frame %d has no data", index)
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if outputIndex, ok := event["output_index"].(float64); ok {
			event["output_index"] = int(outputIndex) + 4
		}
		return event
	}
	toolNames := []string{"lookup_synthetic_widget", "inspect_synthetic_region"}
	upstreamIDs := []string{"call_synth_parallel_alpha_stream_001", "call_synth_parallel_beta_stream_001"}
	arguments := []string{`{"widget":"alpha-fixture"}`, `{"region":"beta-fixture"}`}
	toolItem := func(index int, args, status string) map[string]any {
		return map[string]any{"type": "function_call", "id": fmt.Sprintf("fc-%d", index), "call_id": upstreamIDs[index], "name": toolNames[index], "arguments": args, "status": status}
	}
	reasoningItem := func(id, status string) map[string]any {
		return map[string]any{"type": "reasoning", "id": id, "status": status}
	}
	events := []map[string]any{
		frameEvent(0),
		{"type": "response.output_item.added", "output_index": 2, "item": reasoningItem("reasoning-b", "in_progress")},
		{"type": "response.output_item.added", "output_index": 1, "item": toolItem(0, "", "in_progress")},
		{"type": "response.output_item.added", "output_index": 0, "item": reasoningItem("reasoning-a", "in_progress")},
		{"type": "response.output_item.added", "output_index": 3, "item": toolItem(1, "", "in_progress")},
		frameEvent(1), frameEvent(2), frameEvent(3),
	}
	prefixCount := len(events)
	events = append(events,
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc-0", "output_index": 1, "delta": `{"widget":"`},
		map[string]any{"type": "response.reasoning_text.delta", "item_id": "reasoning-a", "output_index": 0, "content_index": 0, "delta": "hidden"},
		frameEvent(4), frameEvent(5), frameEvent(6),
		frameEvent(7), frameEvent(8), frameEvent(9),
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc-1", "output_index": 3, "delta": `{"region":"`},
		map[string]any{"type": "response.reasoning_text.delta", "item_id": "reasoning-b", "output_index": 2, "content_index": 0, "delta": "hidden"},
		frameEvent(10), frameEvent(11), frameEvent(12),
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc-1", "output_index": 3, "delta": `beta-fixture"}`},
		map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc-1", "output_index": 3, "arguments": arguments[1]},
		map[string]any{"type": "response.output_item.done", "output_index": 3, "item": toolItem(1, arguments[1], "completed")},
		map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc-0", "output_index": 1, "delta": `alpha-fixture"}`},
		map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc-0", "output_index": 1, "arguments": arguments[0]},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": toolItem(0, arguments[0], "completed")},
		map[string]any{"type": "response.output_item.done", "output_index": 2, "item": reasoningItem("reasoning-b", "completed")},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoningItem("reasoning-a", "completed")},
	)
	terminal := frameEvent(13)
	response := terminal["response"].(map[string]any)
	response["output"] = append([]any{reasoningItem("reasoning-a", "completed"), toolItem(0, arguments[0], "completed"), reasoningItem("reasoning-b", "completed"), toolItem(1, arguments[1], "completed")}, response["output"].([]any)...)
	response["usage"] = map[string]any{"input_tokens": 24, "output_tokens": 18, "total_tokens": 42}
	events = append(events, terminal)
	render := func(count int) []byte {
		t.Helper()
		var fixture strings.Builder
		for index, event := range events[:count] {
			event["sequence_number"] = index
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&fixture, "event: %s\ndata: %s\n\n", event["type"], encoded)
		}
		return []byte(fixture.String())
	}
	prefixes := responsesChatStreamSequenceFixtures(t, render(prefixCount))
	for variant, tt := range responsesChatStreamSequenceFixtures(t, render(len(events))) {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			body := newResponsesChatBlockingBody(prefixes[variant].data)
			prefix, err := prepareResponsesChatStream(ctx, body, responsesChatStreamConfig{PublicModel: "gpt-public", PrecommitTimeout: time.Hour})
			if err != nil {
				t.Fatalf("unfinished nonvisible items blocked visible output: %v", err)
			}
			defer prefix.stop(context.Canceled)
			role, err := prefix.next()
			if err != nil || role.kind != chatStreamEventChunk || role.chunk.Choices[0].Delta.Role != "assistant" {
				t.Fatalf("role event = %#v, error = %v", role, err)
			}
			text, err := prefix.next()
			if err != nil || text.kind != chatStreamEventChunk || streamChunkText(t, text.chunk) != "first " {
				t.Fatalf("visible event = %#v, error = %v", text, err)
			}
			prefix.stop(context.Canceled)
			cancel()

			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			route := responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
			stream, err := prepareResponsesChatStream(context.Background(), io.NopCloser(bytes.NewReader(tt.data)), responsesChatStreamConfig{
				PublicModel: "gpt-public", ReplayStore: store, ReplayRoute: route, PrecommitTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			chunks := collectResponsesChatStreamChunks(t, stream)
			if len(chunks) != 9 || streamChunkText(t, chunks[1]) != "first " || streamChunkText(t, chunks[2]) != "second" || chunks[7].Choices[0].FinishReason == nil || *chunks[7].Choices[0].FinishReason != "tool_calls" || chunks[8].Usage == nil || chunks[8].Usage.TotalTokens != 42 {
				t.Fatalf("chunks = %#v", chunks)
			}
			projection := responsesChatReplayAssistantProjection{Content: json.RawMessage(`"first second"`)}
			for index, chunkIndex := range []int{3, 5} {
				call := chunks[chunkIndex].Choices[0].Delta.ToolCalls[0]
				args := chunks[chunkIndex+1].Choices[0].Delta.ToolCalls[0]
				if call.Index == nil || *call.Index != index || args.Index == nil || *args.Index != index || call.Function.Name != toolNames[index] || args.Function.Arguments != arguments[index] {
					t.Fatalf("dense tool %d = %#v, %#v", index, call, args)
				}
				projection.Calls = append(projection.Calls, responsesChatReplayProjectedCall{ID: call.ID, Name: call.Function.Name, Arguments: args.Function.Arguments})
			}
			resolved, err := store.Resolve(route, projection)
			if err != nil || len(resolved.OutputItems) != 6 || len(resolved.Calls) != 2 {
				t.Fatalf("Resolve() = %#v, %v", resolved, err)
			}
			for index, call := range resolved.Calls {
				if call.UpstreamCallID != upstreamIDs[index] || call.OutputItemIndex != 2*index+1 {
					t.Fatalf("resolved call %d = %#v", index, call)
				}
			}
		})
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
