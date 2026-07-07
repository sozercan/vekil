package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/models"
)

// newCodeExecIntegrationHandler wires a test ProxyHandler with proxy-mediated
// code execution enabled against a scripted backend and the given upstream.
func newCodeExecIntegrationHandler(t *testing.T, backend CodeExecutionBackend, upstream http.HandlerFunc) *ProxyHandler {
	t.Helper()
	h := newTestProxyHandler(t, upstream)
	h.codeExecBackend = backend
	h.codeExecConfig = CodeExecConfig{
		Enabled:      true,
		OwnedTools:   []string{"Bash"},
		Backend:      "scripted",
		MaxLoopDepth: 8,
		Policy: CodeExecPolicyConfig{
			TimeoutMS:      5000,
			MaxOutputBytes: 4096,
			WorkingDir:     t.TempDir(),
		},
	}.withDefaults()
	return h
}

// TestCodeExecIntegration_OpenAIBashToolIntercepted verifies the full handler
// path: an OpenAI chat request whose upstream emits an owned Bash tool call is
// intercepted, executed through the backend, looped back, and the client
// receives only the final assistant text — with no tool call in the response.
func TestCodeExecIntegration_OpenAIBashToolIntercepted(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	upstream := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if callCount == 1 {
			_, _ = w.Write([]byte(sseToolCallResponse("toolu_1", "Bash", `{"command":"pytest"}`)))
			return
		}
		_, _ = w.Write([]byte(sseFinalTextResponse("all tests passed")))
	}

	backend := &scriptedBackend{results: []CodeExecResult{{Command: "pytest", ExitCode: 0, Stdout: "1 passed"}}}
	h := newCodeExecIntegrationHandler(t, backend, upstream)

	reqBody := `{"model":"gpt-4.1","messages":[{"role":"user","content":"run pytest"}],"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp models.OpenAIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, w.Body.String())
	}

	// The client must NOT receive the owned tool call.
	if len(resp.Choices) == 0 {
		t.Fatal("response has no choices")
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("client received %d tool calls, want 0 (owned call must be suppressed)", len(resp.Choices[0].Message.ToolCalls))
	}

	// The response body as a whole must not mention the intercepted tool.
	if strings.Contains(w.Body.String(), "toolu_1") || strings.Contains(w.Body.String(), "\"Bash\"") {
		t.Fatalf("response leaked the owned tool call: %s", w.Body.String())
	}

	var content string
	_ = json.Unmarshal(resp.Choices[0].Message.Content, &content)
	if !strings.Contains(content, "all tests passed") {
		t.Errorf("final content = %q, want final assistant text", content)
	}

	// The backend must have executed exactly the owned command once.
	calls := backend.requests()
	if len(calls) != 1 {
		t.Fatalf("backend executions = %d, want 1", len(calls))
	}
	if calls[0].Command != "pytest" {
		t.Errorf("executed command = %q, want pytest", calls[0].Command)
	}
}

// TestCodeExecIntegration_ConsumedCallNotForwarded is the core invariant test:
// the same tool call cannot be both executed by Vekil AND forwarded to the
// client. It asserts the backend saw the command exactly once and that the
// command string never appears in the client-visible response body.
func TestCodeExecIntegration_ConsumedCallNotForwarded(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	upstream := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if callCount == 1 {
			_, _ = w.Write([]byte(sseToolCallResponse("toolu_secret", "Bash", `{"command":"rm -rf /tmp/secret"}`)))
			return
		}
		_, _ = w.Write([]byte(sseFinalTextResponse("cleanup complete")))
	}

	backend := &scriptedBackend{results: []CodeExecResult{{Command: "rm -rf /tmp/secret", ExitCode: 0}}}
	h := newCodeExecIntegrationHandler(t, backend, upstream)

	reqBody := `{"model":"gpt-4.1","messages":[{"role":"user","content":"clean up"}],"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, req)

	// Executed exactly once.
	if got := len(backend.requests()); got != 1 {
		t.Fatalf("backend executions = %d, want exactly 1", got)
	}

	// The command must never reach the client: if it did, the client harness
	// could re-execute it. This is the invariant the acceptance criteria require.
	body := w.Body.String()
	if strings.Contains(body, "rm -rf /tmp/secret") {
		t.Fatalf("client response contains the executed command (double-execution risk): %s", body)
	}
	if strings.Contains(body, "tool_calls") {
		t.Fatalf("client response still contains tool_calls: %s", body)
	}
}

// TestCodeExecIntegration_AnthropicBashToolIntercepted verifies the Anthropic
// Messages path also intercepts owned tool calls and returns a final assistant
// message with no tool_use blocks.
func TestCodeExecIntegration_AnthropicBashToolIntercepted(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	upstream := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if callCount == 1 {
			_, _ = w.Write([]byte(sseToolCallResponse("toolu_1", "Bash", `{"command":"pytest"}`)))
			return
		}
		_, _ = w.Write([]byte(sseFinalTextResponse("all green")))
	}

	backend := &scriptedBackend{results: []CodeExecResult{{Command: "pytest", ExitCode: 0, Stdout: "ok"}}}
	h := newCodeExecIntegrationHandler(t, backend, upstream)

	reqBody := `{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":"run pytest"}],"tools":[{"name":"Bash","description":"run shell","input_schema":{"type":"object"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	h.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp models.AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, w.Body.String())
	}

	toolUses := 0
	var text strings.Builder
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			toolUses++
		}
		if block.Type == "text" && block.Text != nil {
			text.WriteString(*block.Text)
		}
	}
	if toolUses != 0 {
		t.Fatalf("client received %d tool_use blocks, want 0 (owned call must be suppressed)", toolUses)
	}
	if !strings.Contains(text.String(), "all green") {
		t.Errorf("final text = %q, want final assistant text", text.String())
	}
	if len(backend.requests()) != 1 {
		t.Fatalf("backend executions = %d, want 1", len(backend.requests()))
	}
}

// TestCodeExecIntegration_DisabledPreservesPassthrough verifies that when the
// feature is disabled, an owned-looking tool call is forwarded to the client
// unchanged and the backend is never invoked.
func TestCodeExecIntegration_DisabledPreservesPassthrough(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseToolCallResponse("toolu_1", "Bash", `{"command":"pytest"}`)))
	}
	backend := &scriptedBackend{}
	h := newTestProxyHandler(t, upstream)
	// Feature left disabled (zero-value config); backend present but must be unused.
	h.codeExecBackend = backend

	reqBody := `{"model":"gpt-4.1","messages":[{"role":"user","content":"run pytest"}],"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, req)

	var resp models.OpenAIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, w.Body.String())
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("disabled feature must forward the tool call unchanged; got %s", w.Body.String())
	}
	if len(backend.requests()) != 0 {
		t.Errorf("backend was invoked while feature disabled")
	}
}
