package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestResponsesRequestStreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{name: "stream true", body: []byte(`{"stream":true}`), want: true},
		{name: "stream false", body: []byte(`{"stream":false}`), want: false},
		{name: "stream omitted", body: []byte(`{"input":"hi"}`), want: false},
		{name: "invalid json", body: []byte(`{`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := responsesRequestStreams(tt.body); got != tt.want {
				t.Fatalf("responsesRequestStreams() = %v, want %v", got, tt.want)
			}
		})
	}
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
