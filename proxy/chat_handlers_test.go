package proxy

import (
	"encoding/json"
	"strings"
	"testing"

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
}

func TestPrepareOpenAIChatCompletionsRequest_ClientStreamOptionsPreserved(t *testing.T) {
	// Client already set stream_options — we must not clobber it.
	input := []byte(`{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hi"}]}`)

	prepared, _ := prepareOpenAIChatCompletionsRequest(input)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(string(req["stream_options"]), `"include_usage":false`) {
		t.Fatalf("client stream_options was overwritten: %s", req["stream_options"])
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
