package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/models"
)

// scriptedBackend is a CodeExecutionBackend that returns pre-programmed results
// and records the requests it received, for asserting loop behavior.
type scriptedBackend struct {
	mu       sync.Mutex
	results  []CodeExecResult
	errs     []error
	calls    []CodeExecRequest
	callIdx  int
	name     string
	fallback CodeExecResult
}

func (b *scriptedBackend) Name() string {
	if b.name != "" {
		return b.name
	}
	return "scripted"
}

func (b *scriptedBackend) RunCommand(_ context.Context, req CodeExecRequest) (CodeExecResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, req)
	idx := b.callIdx
	b.callIdx++
	if idx < len(b.errs) && b.errs[idx] != nil {
		return CodeExecResult{}, b.errs[idx]
	}
	if idx < len(b.results) {
		return b.results[idx], nil
	}
	return b.fallback, nil
}

func (b *scriptedBackend) requests() []CodeExecRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CodeExecRequest, len(b.calls))
	copy(out, b.calls)
	return out
}

// sseToolCallResponse builds an SSE chat-completions stream that emits a single
// tool call and finishes with reason "tool_calls".
func sseToolCallResponse(id, name, arguments string) string {
	argsJSON, _ := json.Marshal(arguments)
	// Embed the already-JSON-escaped argument string as the streamed arguments.
	var b strings.Builder
	fmt.Fprintf(&b, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":%q,\"index\":0,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]}}]}\n\n", id, name, string(argsJSON))
	b.WriteString("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"total_tokens\":10}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// sseTwoToolCallResponse emits two tool calls in one response (used for
// owned+unowned mixed-ownership tests).
func sseTwoToolCallResponse(id1, name1, args1, id2, name2, args2 string) string {
	a1, _ := json.Marshal(args1)
	a2, _ := json.Marshal(args2)
	var b strings.Builder
	fmt.Fprintf(&b, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":%q,\"index\":0,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]}}]}\n\n", id1, name1, string(a1))
	fmt.Fprintf(&b, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":%q,\"index\":1,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]}}]}\n\n", id2, name2, string(a2))
	b.WriteString("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"total_tokens\":10}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// sseFinalTextResponse builds an SSE chat-completions stream that emits final
// assistant text and finishes with reason "stop".
func sseFinalTextResponse(text string) string {
	textJSON, _ := json.Marshal(text)
	var b strings.Builder
	fmt.Fprintf(&b, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", string(textJSON))
	b.WriteString("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":5,\"total_tokens\":10}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// scriptedUpstream returns an http.HandlerFunc that serves the given SSE
// response bodies in order, one per POST, and records the request bodies it
// received.
func scriptedUpstream(t *testing.T, bodies ...string) (http.HandlerFunc, *[]string) {
	t.Helper()
	var mu sync.Mutex
	idx := 0
	received := make([]string, 0, len(bodies))
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		buf, _ := io.ReadAll(r.Body)
		received = append(received, string(buf))
		if idx >= len(bodies) {
			t.Errorf("scriptedUpstream received more requests (%d) than scripted (%d)", idx+1, len(bodies))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		body := bodies[idx]
		idx++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
	return handler, &received
}

func newCodeExecLoopHandler(t *testing.T, backend CodeExecutionBackend, upstream http.HandlerFunc) *ProxyHandler {
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

func aggregateInitial(t *testing.T, sse string) *models.OpenAIResponse {
	t.Helper()
	resp, err := aggregateStreamToResponse(newNopReadCloser(sse))
	if err != nil {
		t.Fatalf("aggregate initial: %v", err)
	}
	return resp
}

func newNopReadCloser(s string) *trackingReadCloser {
	return newTrackingReadCloser(s)
}

func requestBodyWithBashResult() []byte {
	return []byte(`{"model":"m","messages":[{"role":"user","content":"run pytest"}],"tools":[{"type":"function","function":{"name":"Bash"}}]}`)
}

func TestCodeExecLoop_SingleToolConsumedThenFinal(t *testing.T) {
	backend := &scriptedBackend{
		results: []CodeExecResult{{Command: "pytest", ExitCode: 0, Stdout: "1 passed"}},
	}
	// After the tool result is fed back, the model returns final text.
	upstream, received := scriptedUpstream(t, sseFinalTextResponse("all tests passed"))
	h := newCodeExecLoopHandler(t, backend, upstream)

	initial := aggregateInitial(t, sseToolCallResponse("toolu_1", "Bash", `{"command":"pytest"}`))

	final, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), initial)
	if err != nil {
		t.Fatalf("runCodeExecLoop error = %v", err)
	}

	// Final response must contain assistant text and NO tool calls.
	if len(final.Choices) == 0 {
		t.Fatal("final response has no choices")
	}
	if len(final.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("final response still has %d tool calls, want 0", len(final.Choices[0].Message.ToolCalls))
	}
	var content string
	_ = json.Unmarshal(final.Choices[0].Message.Content, &content)
	if !strings.Contains(content, "all tests passed") {
		t.Errorf("final content = %q, want final assistant text", content)
	}

	// Backend was invoked once with the extracted command.
	calls := backend.requests()
	if len(calls) != 1 {
		t.Fatalf("backend calls = %d, want 1", len(calls))
	}
	if calls[0].Command != "pytest" {
		t.Errorf("executed command = %q, want pytest", calls[0].Command)
	}
	if calls[0].ToolUseID != "toolu_1" {
		t.Errorf("tool use id = %q, want toolu_1", calls[0].ToolUseID)
	}

	// The follow-up upstream request must carry the tool result message.
	if len(*received) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(*received))
	}
	if !strings.Contains((*received)[0], "\"role\":\"tool\"") {
		t.Errorf("follow-up request missing tool result message: %s", (*received)[0])
	}
	if !strings.Contains((*received)[0], "1 passed") {
		t.Errorf("follow-up request missing execution stdout: %s", (*received)[0])
	}
}

func TestCodeExecLoop_MultiStep(t *testing.T) {
	backend := &scriptedBackend{
		results: []CodeExecResult{
			{Command: "step1", ExitCode: 0, Stdout: "one"},
			{Command: "step2", ExitCode: 0, Stdout: "two"},
		},
	}
	// First follow-up returns a second owned tool call, then a final response.
	upstream, _ := scriptedUpstream(t,
		sseToolCallResponse("toolu_2", "Bash", `{"command":"step2"}`),
		sseFinalTextResponse("done"),
	)
	h := newCodeExecLoopHandler(t, backend, upstream)

	initial := aggregateInitial(t, sseToolCallResponse("toolu_1", "Bash", `{"command":"step1"}`))

	final, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), initial)
	if err != nil {
		t.Fatalf("runCodeExecLoop error = %v", err)
	}
	if len(final.Choices[0].Message.ToolCalls) != 0 {
		t.Fatal("final response still contains tool calls")
	}
	calls := backend.requests()
	if len(calls) != 2 {
		t.Fatalf("backend calls = %d, want 2", len(calls))
	}
	if calls[0].Command != "step1" || calls[1].Command != "step2" {
		t.Errorf("commands = %q,%q want step1,step2", calls[0].Command, calls[1].Command)
	}
}

// TestCodeExecLoop_InternalTurnUsageMetered verifies token usage from internal
// (non-final) loop turns is accumulated as additive internal usage rather than
// dropped. The final turn's own usage is metered separately by the handler via
// observeOpenAIUsage, so only the initial + intermediate turns land here.
func TestCodeExecLoop_InternalTurnUsageMetered(t *testing.T) {
	backend := &scriptedBackend{
		results: []CodeExecResult{
			{Command: "step1", ExitCode: 0, Stdout: "one"},
			{Command: "step2", ExitCode: 0, Stdout: "two"},
		},
	}
	// Two internal turns (initial + one follow-up owned call), then a final turn.
	// Each SSE response reports usage of prompt=5, completion=5.
	upstream, _ := scriptedUpstream(t,
		sseToolCallResponse("toolu_2", "Bash", `{"command":"step2"}`),
		sseFinalTextResponse("done"),
	)
	h := newCodeExecLoopHandler(t, backend, upstream)

	ctx, summary := WithRequestSummary(context.Background())
	initial := aggregateInitial(t, sseToolCallResponse("toolu_1", "Bash", `{"command":"step1"}`))

	if _, err := h.runCodeExecLoop(ctx, requestBodyWithBashResult(), initial); err != nil {
		t.Fatalf("runCodeExecLoop error = %v", err)
	}

	// Two internal turns each contributed prompt=5, completion=5.
	if summary.extraPromptTokens != 10 {
		t.Errorf("internal prompt tokens = %d, want 10 (2 internal turns × 5)", summary.extraPromptTokens)
	}
	if summary.extraCompletionTokens != 10 {
		t.Errorf("internal completion tokens = %d, want 10 (2 internal turns × 5)", summary.extraCompletionTokens)
	}
}

func TestCodeExecLoop_MaxDepthEnforced(t *testing.T) {
	backend := &scriptedBackend{fallback: CodeExecResult{Command: "loop", ExitCode: 0}}
	// Upstream always returns another owned tool call → never terminates.
	bodies := make([]string, 10)
	for i := range bodies {
		bodies[i] = sseToolCallResponse(fmt.Sprintf("toolu_%d", i), "Bash", `{"command":"loop"}`)
	}
	upstream, _ := scriptedUpstream(t, bodies...)
	h := newCodeExecLoopHandler(t, backend, upstream)
	h.codeExecConfig.MaxLoopDepth = 3

	initial := aggregateInitial(t, sseToolCallResponse("toolu_0", "Bash", `{"command":"loop"}`))

	_, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), initial)
	if err == nil {
		t.Fatal("runCodeExecLoop error = nil, want max depth error")
	}
	if !strings.Contains(err.Error(), "max depth") {
		t.Errorf("error = %v, want max depth", err)
	}
}

func TestCodeExecLoop_UnownedToolMidLoopFailsClosed(t *testing.T) {
	backend := &scriptedBackend{results: []CodeExecResult{{Command: "pytest", ExitCode: 0}}}
	// The follow-up mixes an owned Bash call with an unowned WebSearch call.
	upstream, _ := scriptedUpstream(t,
		sseTwoToolCallResponse("toolu_2", "Bash", `{"command":"again"}`, "toolu_3", "WebSearch", `{"query":"x"}`),
	)
	h := newCodeExecLoopHandler(t, backend, upstream)

	initial := aggregateInitial(t, sseToolCallResponse("toolu_1", "Bash", `{"command":"pytest"}`))

	_, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), initial)
	if err == nil {
		t.Fatal("runCodeExecLoop error = nil, want mixed-ownership error")
	}
	if !strings.Contains(err.Error(), "mixes owned and unowned") {
		t.Errorf("error = %v, want mixed-ownership error", err)
	}
}

func TestCodeExecLoop_UnownedOnlyResponseReturnedToClient(t *testing.T) {
	// A response with only unowned tool calls is a valid final response: the loop
	// returns it unchanged so it can be forwarded to the client.
	backend := &scriptedBackend{}
	h := newCodeExecLoopHandler(t, backend, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for an unowned-only response")
	})

	initial := aggregateInitial(t, sseToolCallResponse("toolu_1", "WebSearch", `{"query":"x"}`))

	final, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), initial)
	if err != nil {
		t.Fatalf("runCodeExecLoop error = %v", err)
	}
	if len(final.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("unowned tool call was not preserved: %d calls", len(final.Choices[0].Message.ToolCalls))
	}
	if len(backend.requests()) != 0 {
		t.Errorf("backend was invoked for an unowned tool call")
	}
}

func TestCodeExecLoop_MissingCommandFailsClosed(t *testing.T) {
	backend := &scriptedBackend{}
	h := newCodeExecLoopHandler(t, backend, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called when command extraction fails")
	})

	initial := aggregateInitial(t, sseToolCallResponse("toolu_1", "Bash", `{"notcommand":"x"}`))

	_, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), initial)
	if err == nil {
		t.Fatal("runCodeExecLoop error = nil, want missing-command error")
	}
	if len(backend.requests()) != 0 {
		t.Error("backend was invoked despite missing command")
	}
}

func TestExtractCommandArgument(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
		ok   bool
	}{
		{"command field", `{"command":"pytest"}`, "pytest", true},
		{"cmd fallback", `{"cmd":"ls -la"}`, "ls -la", true},
		{"script fallback", `{"script":"echo hi"}`, "echo hi", true},
		{"empty command", `{"command":"  "}`, "", false},
		{"no command key", `{"foo":"bar"}`, "", false},
		{"invalid json", `not json`, "", false},
		{"empty args", ``, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractCommandArgument(tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("command = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyOwnedToolCalls(t *testing.T) {
	cfg := CodeExecConfig{Enabled: true, OwnedTools: []string{"Bash"}}.withDefaults()
	resp := &models.OpenAIResponse{
		Choices: []models.OpenAIChoice{{
			Message: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{
					{ID: "1", Function: models.OpenAIFunctionCall{Name: "Bash"}},
					{ID: "2", Function: models.OpenAIFunctionCall{Name: "WebSearch"}},
					{ID: "3", Function: models.OpenAIFunctionCall{Name: "bash"}}, // case-insensitive
				},
			},
		}},
	}
	owned, unowned := classifyOwnedToolCalls(resp, cfg)
	if len(owned) != 2 {
		t.Errorf("owned = %d, want 2 (case-insensitive)", len(owned))
	}
	if len(unowned) != 1 {
		t.Errorf("unowned = %d, want 1", len(unowned))
	}
}

// multiChoiceResponse builds an n>1 response where the owned tool call sits in a
// choice other than index 0. Choices[0] carries only an unowned call.
func multiChoiceResponse(ownedName string) *models.OpenAIResponse {
	return &models.OpenAIResponse{
		Choices: []models.OpenAIChoice{
			{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:      "assistant",
					ToolCalls: []models.OpenAIToolCall{{ID: "u1", Function: models.OpenAIFunctionCall{Name: "WebSearch", Arguments: `{"query":"x"}`}}},
				},
			},
			{
				Index: 1,
				Message: models.OpenAIMessage{
					Role:      "assistant",
					ToolCalls: []models.OpenAIToolCall{{ID: "toolu_owned", Function: models.OpenAIFunctionCall{Name: ownedName, Arguments: `{"command":"pytest"}`}}},
				},
			},
		},
	}
}

// TestClassifyOwnedToolCalls_ScansAllChoices guards the core invariant: an owned
// tool call in a choice other than index 0 must still be detected. Missing it
// would let responseHasOwnedToolCall report "no owned call" and forward the owned
// call to the client.
func TestClassifyOwnedToolCalls_ScansAllChoices(t *testing.T) {
	cfg := CodeExecConfig{Enabled: true, OwnedTools: []string{"Bash"}}.withDefaults()
	owned, unowned := classifyOwnedToolCalls(multiChoiceResponse("Bash"), cfg)
	if len(owned) != 1 {
		t.Fatalf("owned = %d, want 1 (owned call in Choices[1] must be detected)", len(owned))
	}
	if owned[0].ID != "toolu_owned" {
		t.Errorf("owned call id = %q, want toolu_owned", owned[0].ID)
	}
	if len(unowned) != 1 {
		t.Errorf("unowned = %d, want 1", len(unowned))
	}
}

// TestResponseHasOwnedToolCall_MultiChoice verifies the interception gate sees an
// owned call sitting outside Choices[0] in a multi-choice response.
func TestResponseHasOwnedToolCall_MultiChoice(t *testing.T) {
	h := newCodeExecLoopHandler(t, &scriptedBackend{}, func(http.ResponseWriter, *http.Request) {})
	if !h.responseHasOwnedToolCall(multiChoiceResponse("Bash")) {
		t.Fatal("responseHasOwnedToolCall = false, want true for owned call in Choices[1]")
	}
}

// TestCodeExecLoop_MultiChoiceOwnedCallFailsClosed verifies the loop fails closed
// (rather than executing or forwarding) when an owned call appears in a
// multi-choice (n>1) response — the owned call must never leak to the client.
func TestCodeExecLoop_MultiChoiceOwnedCallFailsClosed(t *testing.T) {
	backend := &scriptedBackend{}
	h := newCodeExecLoopHandler(t, backend, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for a multi-choice owned response")
	})

	_, err := h.runCodeExecLoop(context.Background(), requestBodyWithBashResult(), multiChoiceResponse("Bash"))
	if err == nil {
		t.Fatal("runCodeExecLoop error = nil, want multi-choice error")
	}
	if !errors.Is(err, errCodeExecMultiChoice) {
		t.Errorf("error = %v, want errCodeExecMultiChoice", err)
	}
	if len(backend.requests()) != 0 {
		t.Errorf("backend was invoked for a multi-choice owned response (%d calls); owned call must not execute", len(backend.requests()))
	}
}
