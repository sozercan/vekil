package proxy

import (
"encoding/json"
"io"
"net/http"
"net/http/httptest"
"strings"
"testing"

"github.com/sozercan/vekil/models"
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
t.Logf("LLM response: stop_reason=%v, tool_calls=%d, tools_sent=1", resp.StopReason, toolCount)
if resp.StopReason == nil || *resp.StopReason != "tool_use" {
t.Errorf("stop_reason = %v, want tool_use", resp.StopReason)
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
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I'll delegate both research tasks in parallel...\"}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_abc1\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\",\\\"prompt\\\":\\\"List 3 pros of Go\\\"}\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_abc2\",\"index\":1,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\",\\\"prompt\\\":\\\"List 3 cons of Go\\\"}\"}}]}}]}\n\n"))
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
t.Logf("LLM response: stop_reason=%v, tool_calls=%d, content_blocks=%d", resp.StopReason, toolCount, len(resp.Content))
if resp.StopReason == nil || *resp.StopReason != "tool_use" {
t.Errorf("stop_reason = %v, want tool_use", resp.StopReason)
}
if toolCount != 2 {
t.Fatalf("tool_calls = %d, want 2 ← THIS WAS THE BUG (was 0 before fix)", toolCount)
}
if resp.Content[1].ID != "call_abc1" || resp.Content[1].Name != "delegate_task" {
t.Errorf("tool[0] = %+v, want call_abc1/delegate_task", resp.Content[1])
}
if resp.Content[2].ID != "call_abc2" || resp.Content[2].Name != "delegate_task" {
t.Errorf("tool[1] = %+v, want call_abc2/delegate_task", resp.Content[2])
}
}

// TestBugReport_OpenAISingleTool verifies single tool call works (OpenAI path with forced streaming).
func TestBugReport_OpenAISingleTool(t *testing.T) {
handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "text/event-stream")
w.WriteHeader(http.StatusOK)
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_single\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\"}\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":25,\"total_tokens\":75}}\n\n"))
w.Write([]byte("data: [DONE]\n\n"))
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

// TestBugReport_OpenAIParallelTools verifies parallel tool calls via OpenAI path with forced streaming.
func TestBugReport_OpenAIParallelTools(t *testing.T) {
handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
body, _ := io.ReadAll(r.Body)
var oaiReq map[string]json.RawMessage
json.Unmarshal(body, &oaiReq)
if string(oaiReq["parallel_tool_calls"]) != "true" {
t.Error("expected parallel_tool_calls=true injected by proxy")
}
if string(oaiReq["stream"]) != "true" {
t.Error("expected stream=true forced by proxy")
}

w.Header().Set("Content-Type", "text/event-stream")
w.WriteHeader(http.StatusOK)
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I'll delegate both research tasks in parallel...\"}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_abc1\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\",\\\"prompt\\\":\\\"List 3 pros of Go\\\"}\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_abc2\",\"index\":1,\"type\":\"function\",\"function\":{\"name\":\"delegate_task\",\"arguments\":\"\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"agent\\\":\\\"researcher\\\",\\\"prompt\\\":\\\"List 3 cons of Go\\\"}\"}}]}}]}\n\n"))
w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"claude-opus-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\n"))
w.Write([]byte("data: [DONE]\n\n"))
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
t.Logf("LLM response: stop_reason=%s, tool_calls=%d, tools_sent=2", fr, tc)
if fr != "tool_calls" {
t.Errorf("finish_reason = %q, want tool_calls", fr)
}
if tc != 2 {
t.Fatalf("tool_calls = %d, want 2 ← THIS WAS THE BUG (was 0 before fix)", tc)
}
}
