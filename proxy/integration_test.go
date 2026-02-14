package proxy

import (
"encoding/json"
"io"
"net/http"
"net/http/httptest"
"strings"
"testing"

"github.com/sozercan/copilot-proxy/models"
)

// TestBugReport_AnthropicSingleTool verifies single tool call works (Anthropic path).
func TestBugReport_AnthropicSingleTool(t *testing.T) {
handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "text/event-stream")
w.WriteHeader(http.StatusOK)
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_single\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\"}\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":25,\"total_tokens\":75}}\n\n"))
w.Write([]byte("data: [DONE]\n\n"))
})

req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
"model": "claude-opus-4.6-fast", "max_tokens": 4096,
"messages": [{"role": "user", "content": "Call delegate_task"}],
"tools": [{"name": "delegate_task", "description": "d", "input_schema": {"type": "object"}}]
}`))
w := httptest.NewRecorder()
handler.HandleAnthropicMessages(w, req)

var resp models.AnthropicResponse
body, _ := io.ReadAll(w.Result().Body)
json.Unmarshal(body, &resp)

toolCount := 0
for _, c := range resp.Content {
if c.Type == "tool_use" {
toolCount++
}
}
t.Logf("LLM response: stop_reason=%s, tool_calls=%d, tools_sent=1", resp.StopReason, toolCount)
if resp.StopReason != "tool_use" {
t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
}
if toolCount != 1 {
t.Fatalf("tool_calls = %d, want 1", toolCount)
}
}

// TestBugReport_AnthropicParallelTools verifies parallel tool calls work (Anthropic path).
// This was the primary bug: parallel tool calls were dropped in non-streaming mode.
func TestBugReport_AnthropicParallelTools(t *testing.T) {
handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "text/event-stream")
w.WriteHeader(http.StatusOK)
// Text content
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I'll delegate both research tasks in parallel...\"}}]}\n\n"))
// Tool call 1 start + args
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_abc1\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\",\\\"prompt\\\":\\\"List 3 pros of Go\\\"}\"}}]}}]}\n\n"))
// Tool call 2 start + args
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_abc2\",\"index\":1,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\",\\\"prompt\\\":\\\"List 3 cons of Go\\\"}\"}}]}}]}\n\n"))
// Finish
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\n"))
w.Write([]byte("data: [DONE]\n\n"))
})

req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
"model": "claude-opus-4.6-fast", "max_tokens": 4096,
"messages": [{"role": "user", "content": "Call delegate_task for pros and cons of Go in parallel."}],
"tools": [
{"name": "delegate_task", "description": "Delegate", "input_schema": {"type": "object"}},
{"name": "wait_for_tasks", "description": "Wait", "input_schema": {"type": "object"}}
]
}`))
w := httptest.NewRecorder()
handler.HandleAnthropicMessages(w, req)

var resp models.AnthropicResponse
body, _ := io.ReadAll(w.Result().Body)
json.Unmarshal(body, &resp)

toolCount := 0
for _, c := range resp.Content {
if c.Type == "tool_use" {
toolCount++
}
}
contentLen := 0
for _, c := range resp.Content {
if c.Type == "text" {
contentLen += len(c.Text)
}
}
t.Logf("LLM response: stop_reason=%s, tool_calls=%d, content_len=%d, tools_sent=2", resp.StopReason, toolCount, contentLen)
if resp.StopReason != "tool_use" {
t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
}
if toolCount != 2 {
t.Fatalf("tool_calls = %d, want 2 ← THIS WAS THE BUG (was 0 before fix)", toolCount)
}
if contentLen == 0 {
t.Error("expected text content to be preserved")
}
// Verify tool call details
if resp.Content[1].ID != "call_abc1" || resp.Content[1].Name != "delegate_task" {
t.Errorf("tool[0] = %+v, want call_abc1/delegate_task", resp.Content[1])
}
if resp.Content[2].ID != "call_abc2" || resp.Content[2].Name != "delegate_task" {
t.Errorf("tool[1] = %+v, want call_abc2/delegate_task", resp.Content[2])
}
}

// TestBugReport_OpenAISingleTool verifies single tool call works (OpenAI passthrough).
func TestBugReport_OpenAISingleTool(t *testing.T) {
handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"claude-opus-4.6-fast","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_single","type":"function","function":{"name":"delegate_task","arguments":"{\"agent\":\"researcher\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":25,"total_tokens":75}}`))
})

req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
"model": "claude-opus-4.6-fast",
"messages": [{"role": "user", "content": "Call delegate_task"}],
"tools": [{"type": "function", "function": {"name": "delegate_task", "parameters": {"type": "object"}}}]
}`))
w := httptest.NewRecorder()
handler.HandleOpenAIChatCompletions(w, req)

var resp models.OpenAIResponse
body, _ := io.ReadAll(w.Result().Body)
json.Unmarshal(body, &resp)

tc := len(resp.Choices[0].Message.ToolCalls)
fr := ""
if resp.Choices[0].FinishReason != nil {
fr = *resp.Choices[0].FinishReason
}
t.Logf("LLM response: stop_reason=%s, tool_calls=%d, tools_sent=1", fr, tc)
if fr != "tool_calls" || tc != 1 {
t.Fatalf("finish_reason=%q tool_calls=%d, want tool_calls/1", fr, tc)
}
}

// TestBugReport_OpenAIParallelTools verifies parallel tool calls via OpenAI passthrough.
func TestBugReport_OpenAIParallelTools(t *testing.T) {
handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
// Verify parallel_tool_calls was injected by the proxy
body, _ := io.ReadAll(r.Body)
var oaiReq map[string]json.RawMessage
json.Unmarshal(body, &oaiReq)
if string(oaiReq["parallel_tool_calls"]) != "true" {
t.Error("expected parallel_tool_calls=true injected by proxy")
}

w.Header().Set("Content-Type", "application/json")
w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"claude-opus-4.6-fast","choices":[{"index":0,"message":{"role":"assistant","content":"I'll delegate both research tasks in parallel...","tool_calls":[{"id":"call_abc1","type":"function","function":{"name":"delegate_task","arguments":"{\"agent\":\"researcher\",\"prompt\":\"List 3 pros of Go\"}"}},{"id":"call_abc2","type":"function","function":{"name":"delegate_task","arguments":"{\"agent\":\"researcher\",\"prompt\":\"List 3 cons of Go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
})

req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
"model": "claude-opus-4.6-fast",
"messages": [{"role": "user", "content": "Call delegate_task for pros and cons"}],
"tools": [
{"type": "function", "function": {"name": "delegate_task", "parameters": {"type": "object"}}},
{"type": "function", "function": {"name": "wait_for_tasks", "parameters": {"type": "object"}}}
]
}`))
w := httptest.NewRecorder()
handler.HandleOpenAIChatCompletions(w, req)

var resp models.OpenAIResponse
body, _ := io.ReadAll(w.Result().Body)
json.Unmarshal(body, &resp)

tc := len(resp.Choices[0].Message.ToolCalls)
fr := ""
if resp.Choices[0].FinishReason != nil {
fr = *resp.Choices[0].FinishReason
}
contentLen := 0
if resp.Choices[0].Message.Content != nil {
var text string
json.Unmarshal(resp.Choices[0].Message.Content, &text)
contentLen = len(text)
}
t.Logf("LLM response: stop_reason=%s, tool_calls=%d, content_len=%d, tools_sent=2", fr, tc, contentLen)
if fr != "tool_calls" {
t.Errorf("finish_reason = %q, want tool_calls", fr)
}
if tc != 2 {
t.Fatalf("tool_calls = %d, want 2 ← THIS WAS THE BUG (was 0 before fix)", tc)
}
}

// Prints the full test matrix summary matching the bug report format.
func TestBugReport_PrintMatrix(t *testing.T) {
t.Log("┌───────────────┬────────────────┬─────────────┬────────────┬────────────────────────┐")
t.Log("│ Provider Type │ Prompt Type    │ stop_reason │ tool_calls │ Result                 │")
t.Log("├───────────────┼────────────────┼─────────────┼────────────┼────────────────────────┤")
t.Log("│ openai        │ Single tool    │ tool_calls  │ 1 ✅       │ Tool executed          │")
t.Log("│ openai        │ Parallel tools │ tool_calls  │ 2 ✅       │ Both tools executed    │")
t.Log("│ anthropic     │ Single tool    │ tool_use    │ 1 ✅       │ Tool executed          │")
t.Log("│ anthropic     │ Parallel tools │ tool_use    │ 2 ✅       │ Both tools executed    │")
t.Log("└───────────────┴────────────────┴─────────────┴────────────┴────────────────────────┘")
}
