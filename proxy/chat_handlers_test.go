package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func TestPrepareOpenAIChatCompletionsRequest_ForceStreamWithTools(t *testing.T) {
	input := []byte(`{
		"model":"gpt-4.1",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup_weather","parameters":{"type":"object"}}}]
	}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	if mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = true, want false")
	}
	if !mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = false, want true")
	}

	var req models.OpenAIRequest
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if req.Stream == nil || !*req.Stream {
		t.Fatalf("stream = %v, want true", req.Stream)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %+v, want include_usage=true", req.StreamOptions)
	}
	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v, want true", req.ParallelToolCalls)
	}
}

func TestPrepareOpenAIChatCompletionsRequest_EmptyToolsRemainNonStreaming(t *testing.T) {
	input := []byte(`{
		"model":"gpt-4.1",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[]
	}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	if mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = true, want false")
	}
	if mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = true, want false")
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if _, ok := req["stream"]; ok {
		t.Fatal("stream present, want omitted")
	}
	if _, ok := req["stream_options"]; ok {
		t.Fatal("stream_options present, want omitted")
	}
	if _, ok := req["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls present, want omitted")
	}
}

func TestPrepareOpenAIChatCompletionsRequest_ClientStreamGetsIncludeUsage(t *testing.T) {
	input := []byte(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	if !mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = false, want true")
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so, ok := req["stream_options"]
	if !ok {
		t.Fatal("stream_options not injected for client stream")
	}
	if !strings.Contains(string(so), `"include_usage":true`) {
		t.Fatalf("include_usage not set: %s", so)
	}
	if !mode.injectedClientStreamUsage {
		t.Fatal("injectedClientStreamUsage = false, want true when proxy injects include_usage")
	}
}

func TestPrepareOpenAIChatCompletionsRequest_ClientStreamOptionsPreserved(t *testing.T) {
	// Client already set stream_options — we must not clobber it.
	input := []byte(`{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hi"}]}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(string(req["stream_options"]), `"include_usage":false`) {
		t.Fatalf("client stream_options was overwritten: %s", req["stream_options"])
	}
	if mode.injectedClientStreamUsage {
		t.Fatal("injectedClientStreamUsage = true, want false when client supplied stream_options")
	}
}

func TestPrepareAnthropicChatCompletionsRequest_ForcesStreaming(t *testing.T) {
	req := &models.AnthropicRequest{
		Model: "claude-sonnet-4",
		Messages: []models.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		Stream: false,
	}

	prepared, mode, err := prepareAnthropicChatCompletionsRequest(req)
	if err != nil {
		t.Fatalf("prepareAnthropicChatCompletionsRequest: %v", err)
	}
	if mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = true, want false")
	}
	if !mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = false, want true")
	}

	var oaiReq models.OpenAIRequest
	if err := json.Unmarshal(prepared, &oaiReq); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if oaiReq.Stream == nil || !*oaiReq.Stream {
		t.Fatalf("stream = %v, want true", oaiReq.Stream)
	}
	if oaiReq.StreamOptions == nil || !oaiReq.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %+v, want include_usage=true", oaiReq.StreamOptions)
	}
}

func TestAggregateStreamToResponseWithProgress(t *testing.T) {
	tests := []struct {
		name         string
		stream       string
		wantProgress upstreamSemanticProgress
	}{
		{
			name: "role preamble before rate limit remains replay safe",
			stream: "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressAllowedPreamble,
		},
		{
			name: "text before rate limit is semantic progress",
			stream: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressSemanticOutput,
		},
		{
			name: "tool call before reset is tool progress",
			stream: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressToolActivity,
		},
		{
			name: "malformed event makes progress unknown",
			stream: "data: not-json\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, progress, err := aggregateStreamToResponseWithProgress(io.NopCloser(strings.NewReader(tt.stream)))
			if err == nil {
				t.Fatal("aggregate error = nil")
			}
			if progress != tt.wantProgress {
				t.Fatalf("progress = %q want %q", progress, tt.wantProgress)
			}
		})
	}
}

func TestInspectOpenAIChatStreamEventUnknownTopLevelIsNotReplaySafe(t *testing.T) {
	result := inspectOpenAIChatStreamEvent("", `{"id":"chat","choices":[],"vendor_tool_progress":{"started":true}}`)
	if result.progress != upstreamProgressUnknown {
		t.Fatalf("progress = %q, want unknown", result.progress)
	}
}

func TestConsumeOpenAIStreamChunksWithProgressEventOnlyIsUnknown(t *testing.T) {
	_, progress, err := consumeOpenAIStreamChunksWithProgress(strings.NewReader("event: vendor.tool.started\n\n"), nil)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if progress != upstreamProgressUnknown {
		t.Fatalf("progress = %q, want unknown", progress)
	}
}

func TestExplicitRoutePreparedStreamTimeoutFlushesBufferedPreamble(t *testing.T) {
	body := newBlockingSSEReadCloser("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	prepared := newExplicitRoutePreparedStream(resp, explicitRouteStreamOpenAIChat, responsesPrecommitMaxPeekBytes)
	if _, hasResult, err := prepared.await(context.Background(), context.Background(), 10*time.Millisecond); err != nil || hasResult {
		t.Fatalf("await result has=%v err=%v, want timeout without decision", hasResult, err)
	}
	committed := prepared.commitResponse()
	readDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := committed.Body.Read(buf)
		readDone <- string(buf[:n])
	}()
	select {
	case got := <-readDone:
		if !strings.Contains(got, `"role":"assistant"`) {
			t.Fatalf("flushed prefix = %q", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("committed preamble did not flush after timeout")
	}
	_ = committed.Body.Close()
}

func TestInspectOpenAIChatErrorWithEmbeddedProgressIsNotReplaySafe(t *testing.T) {
	result := inspectOpenAIChatStreamEvent("error", `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"late"},"choices":[{"delta":{"content":"partial"}}]}`)
	if result.failure == nil {
		t.Fatal("failure = nil")
	}
	if upstreamProgressAllowsTargetSwitch(result.progress) {
		t.Fatalf("progress = %q unexpectedly replay safe", result.progress)
	}
}
