package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestExtractResponsesOutputText(t *testing.T) {
	body := []byte(`{
		"output":[
			{"type":"message","content":[
				{"type":"output_text","text":"Alpha "},
				{"type":"text","text":"Beta"},
				{"type":"refusal","text":"ignored"}
			]},
			{"type":"tool_call","content":[{"type":"output_text","text":"ignored"}]},
			{"type":"message","content":[{"type":"output_text","text":" citeturn1view0Gamma"}]},
			{"type":"message","content":"bad-shape"}
		]
	}`)

	text, err := extractResponsesOutputText(body)
	if err != nil {
		t.Fatalf("extractResponsesOutputText: %v", err)
	}
	if text != "Alpha Beta Gamma" {
		t.Fatalf("text = %q, want %q", text, "Alpha Beta Gamma")
	}
}

func TestExtractResponsesOutputText_InvalidJSON(t *testing.T) {
	if _, err := extractResponsesOutputText([]byte(`{`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestExtractResponsesOutputText_RejectsInvalidSummaryResults(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "incomplete", body: `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`},
		{name: "refusal only", body: `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`},
		{name: "tool only", body: `{"status":"completed","output":[{"type":"function_call","name":"summarize","arguments":"{}"}]}`},
		{name: "empty", body: `{"status":"completed","output":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if text, err := extractResponsesOutputText([]byte(tt.body)); err == nil {
				t.Fatalf("expected invalid summary response to fail, got text %q", text)
			}
		})
	}
}

func TestParseResponsesRequestMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
		want responsesRequestMetadata
	}{
		{
			name: "extracts fields after input",
			body: []byte(`{"input":[{"type":"message","content":"history"}],"model":"  gpt-5.4  ","previous_response_id":"  resp-123  ","stream":true}`),
			want: responsesRequestMetadata{Model: "gpt-5.4", PreviousResponseID: "resp-123", Stream: true},
		},
		{
			name: "invalid stream leaves other metadata intact",
			body: []byte(`{"model":"gpt-5.4","stream":"yes","previous_response_id":"resp-123"}`),
			want: responsesRequestMetadata{Model: "gpt-5.4", PreviousResponseID: "resp-123"},
		},
		{
			name: "invalid model leaves stream intact",
			body: []byte(`{"model":42,"stream":true}`),
			want: responsesRequestMetadata{Stream: true},
		},
		{
			name: "stream false",
			body: []byte(`{"model":"gpt-5.4","stream":false}`),
			want: responsesRequestMetadata{Model: "gpt-5.4"},
		},
		{
			name: "stream omitted",
			body: []byte(`{"model":"gpt-5.4"}`),
			want: responsesRequestMetadata{Model: "gpt-5.4"},
		},
		{
			name: "invalid json",
			body: []byte(`{`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseResponsesRequestMetadata(tt.body); got != tt.want {
				t.Fatalf("parseResponsesRequestMetadata() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStripUnsupportedResponsesRequestFieldsHandlesEscapedPolicyValues(t *testing.T) {
	t.Parallel()

	t.Run("sampling field names", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5-codex","input":"hi","top_\u0070":0.9,"temper\u0061ture":0.2}`)
		rewritten, fields := stripUnsupportedResponsesRequestFields(body, &providerRuntime{kind: providerTypeOpenAICodex})
		if len(fields) != 2 {
			t.Fatalf("stripped fields = %v, want top_p and temperature", fields)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(rewritten, &payload); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if _, ok := payload["top_p"]; ok {
			t.Fatalf("rewritten body retained top_p: %s", rewritten)
		}
		if _, ok := payload["temperature"]; ok {
			t.Fatalf("rewritten body retained temperature: %s", rewritten)
		}
	})

	t.Run("tool type", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.4","input":"hi","tools":[{"type":"image_\u0067eneration"}],"tool_choice":"required"}`)
		rewritten, fields := stripUnsupportedResponsesRequestFields(body, &providerRuntime{kind: providerTypeCopilot})
		if len(fields) != 2 {
			t.Fatalf("stripped fields = %v, want tool and tool_choice", fields)
		}
		var payload struct {
			Tools      []json.RawMessage `json:"tools"`
			ToolChoice json.RawMessage   `json:"tool_choice"`
		}
		if err := json.Unmarshal(rewritten, &payload); err != nil {
			t.Fatalf("decode rewritten body: %v", err)
		}
		if len(payload.Tools) != 0 {
			t.Fatalf("rewritten tools = %s, want empty", payload.Tools)
		}
		if len(payload.ToolChoice) != 0 {
			t.Fatalf("rewritten body retained tool_choice: %s", payload.ToolChoice)
		}
	})
}

func TestPrepareResponsesRequestBuildsHeaderAndStreamingPlan(t *testing.T) {
	t.Parallel()

	handler, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelInfo))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	body := []byte(`{"model":"gpt-test","stream":true,"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Add("X-OpenAI-Subagent", "  worker-1  ")
	req.Header.Add("X-OpenAI-Subagent", "   ")
	req.Header.Set("X-Codex-Window-Id", " window-123 ")
	req.Header.Set("X-Ignored-By-Proxy", "ignored")

	prepared := handler.prepareResponsesRequest(req.Context(), req, body)
	if !bytes.Equal(prepared.body, body) {
		t.Fatalf("prepared body = %s, want unchanged %s", prepared.body, body)
	}
	if !prepared.streaming {
		t.Fatal("prepared.streaming = false, want true")
	}
	if got := prepared.model; got != "gpt-test" {
		t.Fatalf("prepared.model = %q, want %q", got, "gpt-test")
	}
	if got := prepared.headerToolScope; got != "codex:|window-123" {
		t.Fatalf("headerToolScope = %q, want codex:|window-123", got)
	}
	if got := prepared.extraHeaders.Values("X-OpenAI-Subagent"); len(got) != 1 || got[0] != "worker-1" {
		t.Fatalf("extra X-OpenAI-Subagent values = %v, want [worker-1]", got)
	}
	if got := prepared.extraHeaders.Get("X-Codex-Window-Id"); got != "window-123" {
		t.Fatalf("extra X-Codex-Window-Id = %q, want window-123", got)
	}
	if got := prepared.extraHeaders.Get("X-Ignored-By-Proxy"); got != "" {
		t.Fatalf("unexpected forwarded ignored header %q", got)
	}
	if got := prepared.upstreamHeaders.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("upstream Accept = %q, want text/event-stream", got)
	}
	if got := prepared.extraHeaders.Get("Accept"); got != "" {
		t.Fatalf("extra Accept mutated to %q, want empty", got)
	}
}

type summaryResponseRoundTripFunc func(*http.Request) (*http.Response, error)

func (f summaryResponseRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type summaryResponseTrackingBody struct {
	reader    *strings.Reader
	bytesRead int
	closed    bool
}

func newSummaryResponseTrackingBody(body string) *summaryResponseTrackingBody {
	return &summaryResponseTrackingBody{reader: strings.NewReader(body)}
}

func (b *summaryResponseTrackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.bytesRead += n
	return n, err
}

func (b *summaryResponseTrackingBody) Close() error {
	b.closed = true
	return nil
}

func newSummaryResponseHandler(t testing.TB, body *summaryResponseTrackingBody) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelInfo))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.copilotURL = "http://upstream.test"
	handler.client = &http.Client{Transport: summaryResponseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        header,
			Body:          body,
			ContentLength: int64(body.reader.Len()),
			Request:       req,
		}, nil
	})}
	return handler
}

func TestHandleCompact_BoundsSuccessfulSummaryResponse(t *testing.T) {
	oversizedText := strings.Repeat("x", proxySummaryResponseBodySize+4096)
	upstreamBody := newSummaryResponseTrackingBody(`{"id":"resp-oversized","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + oversizedText + `"}]}]}`)
	handler := newSummaryResponseHandler(t, upstreamBody)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.4","input":"history"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected oversized successful compaction response to fail with 502, got %d", resp.StatusCode)
	}
	if bytes.Contains(body, []byte(syntheticCompactionPrefix)) {
		t.Fatalf("oversized compaction response must not emit a checkpoint: %s", body)
	}
	if upstreamBody.bytesRead > proxySummaryResponseBodySize+1 {
		t.Fatalf("expected bounded compaction response read, got %d bytes", upstreamBody.bytesRead)
	}
	if !upstreamBody.closed {
		t.Fatal("expected oversized compaction response body to be closed")
	}
}

func TestHandleMemorySummarize_BoundsSuccessfulSummaryResponse(t *testing.T) {
	oversizedText := strings.Repeat("m", proxySummaryResponseBodySize+4096)
	upstreamBody := newSummaryResponseTrackingBody(`{"id":"resp-memory-oversized","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + oversizedText + `"}]}]}`)
	handler := newSummaryResponseHandler(t, upstreamBody)

	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(`{"model":"gpt-5.4","traces":[{"id":"trace-1","items":[]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleMemorySummarize(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected oversized successful memory summary response to fail with 502, got %d", resp.StatusCode)
	}
	if bytes.Contains(body, []byte(`"trace_summary"`)) {
		t.Fatalf("oversized memory response must not emit summary output: %s", body)
	}
	if upstreamBody.bytesRead > proxySummaryResponseBodySize+1 {
		t.Fatalf("expected bounded memory response read, got %d bytes", upstreamBody.bytesRead)
	}
	if !upstreamBody.closed {
		t.Fatal("expected oversized memory response body to be closed")
	}
}
