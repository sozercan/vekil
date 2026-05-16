package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func TestOpenAIChatToolOptimizerStructRequestPrefersLocalContextOverStore(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{log: logger.New(logger.LevelInfo)}
	configureRecordingToolOptimizer(handler, fake)

	const scope = "session:chat-local-context-test"
	const callID = "call-chat-local-context-1"
	handler.toolContexts.Put(scope, ToolExecutionContext{
		CallID:           callID,
		ToolName:         "shell_command",
		OriginalCommand:  "stale command",
		RewrittenCommand: "stale rewritten command",
		CreatedAt:        time.Now(),
	})

	content, err := json.Marshal("large output")
	if err != nil {
		t.Fatalf("marshal tool output: %v", err)
	}
	req := &models.OpenAIRequest{Messages: []models.OpenAIMessage{
		{
			Role: "assistant",
			ToolCalls: []models.OpenAIToolCall{{
				ID:   callID,
				Type: "function",
				Function: models.OpenAIFunctionCall{
					Name:      "shell_command",
					Arguments: `{"command":"fresh command"}`,
				},
			}},
		},
		{Role: "tool", ToolCallID: callID, Content: content},
	}}

	changed := handler.maybeReduceOpenAIChatToolOutputs(context.Background(), req, handler.toolContexts, scope)
	if changed != 1 {
		t.Fatalf("changed count = %d, want 1", changed)
	}

	var gotContent string
	if err := json.Unmarshal(req.Messages[1].Content, &gotContent); err != nil {
		t.Fatalf("decode reduced content: %v", err)
	}
	if gotContent != "reduced output" {
		t.Fatalf("tool output = %q, want reduced output", gotContent)
	}
	assertSingleReduceRequestCommand(t, fake, "fresh command")
}

func TestOpenAIChatToolOptimizerRawRequestPrefersLocalContextOverStore(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{log: logger.New(logger.LevelInfo)}
	configureRecordingToolOptimizer(handler, fake)

	const scope = "session:chat-raw-local-context-test"
	const callID = "call-chat-raw-local-context-1"
	handler.toolContexts.Put(scope, ToolExecutionContext{
		CallID:           callID,
		ToolName:         "shell_command",
		OriginalCommand:  "stale command",
		RewrittenCommand: "stale rewritten command",
		CreatedAt:        time.Now(),
	})

	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call-chat-raw-local-context-1",
						"type": "function",
						"function": {
							"name": "shell_command",
							"arguments": "{\"command\":\"fresh command\"}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call-chat-raw-local-context-1",
				"content": "large output"
			}
		]
	}`)

	rewritten := handler.rewriteOpenAIChatRequestBodyWithToolOptimizers(context.Background(), body, handler.toolContexts, scope)
	if string(rewritten) == string(body) {
		t.Fatalf("expected raw request body to be rewritten")
	}

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if len(payload.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(payload.Messages))
	}
	if payload.Messages[1].Content != "reduced output" {
		t.Fatalf("tool output = %q, want reduced output", payload.Messages[1].Content)
	}
	assertSingleReduceRequestCommand(t, fake, "fresh command")
}

func TestOpenAIChatToolOptimizerStructRequestReducesStructuredContent(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{log: logger.New(logger.LevelInfo)}
	configureRecordingToolOptimizer(handler, fake)

	const scope = "session:chat-structured-content-test"
	const callID = "call-chat-structured-content-1"
	req := &models.OpenAIRequest{Messages: []models.OpenAIMessage{
		{
			Role: "assistant",
			ToolCalls: []models.OpenAIToolCall{{
				ID:   callID,
				Type: "function",
				Function: models.OpenAIFunctionCall{
					Name:      "shell_command",
					Arguments: `{"command":"fresh command"}`,
				},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: callID,
			Content:    json.RawMessage(`[{"type":"text","text":"large output"}]`),
		},
	}}

	changed := handler.maybeReduceOpenAIChatToolOutputs(context.Background(), req, handler.toolContexts, scope)
	if changed != 1 {
		t.Fatalf("changed count = %d, want 1", changed)
	}
	var gotContent string
	if err := json.Unmarshal(req.Messages[1].Content, &gotContent); err != nil {
		t.Fatalf("decode reduced content: %v", err)
	}
	if gotContent != "reduced output" {
		t.Fatalf("tool output = %q, want reduced output", gotContent)
	}
	requests := fake.snapshotReduceRequests()
	if len(requests) != 1 {
		t.Fatalf("reduce request count = %d, want 1", len(requests))
	}
	if requests[0].Output != `[{"type":"text","text":"large output"}]` {
		t.Fatalf("reduce output = %q, want raw structured content", requests[0].Output)
	}
}

func TestOpenAIChatToolOptimizerRawRequestPreservesTopLevelOrder(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{log: logger.New(logger.LevelInfo)}
	configureRecordingToolOptimizer(handler, fake)

	const scope = "session:chat-order-test"
	body := []byte(`{"z":1,"messages":[{"role":"assistant","tool_calls":[{"id":"call-chat-order-1","type":"function","function":{"name":"shell_command","arguments":"{\"command\":\"fresh command\"}"}}]},{"role":"tool","tool_call_id":"call-chat-order-1","content":"large output"}],"a":2}`)

	rewritten := handler.rewriteOpenAIChatRequestBodyWithToolOptimizers(context.Background(), body, handler.toolContexts, scope)
	if string(rewritten) == string(body) {
		t.Fatalf("expected raw request body to be rewritten")
	}
	rewrittenText := string(rewritten)
	zIndex := strings.Index(rewrittenText, `"z"`)
	messagesIndex := strings.Index(rewrittenText, `"messages"`)
	aIndex := strings.LastIndex(rewrittenText, `"a"`)
	if zIndex < 0 || messagesIndex < 0 || aIndex < 0 {
		t.Fatalf("rewritten body missing expected top-level fields: %s", rewrittenText)
	}
	if !(zIndex < messagesIndex && messagesIndex < aIndex) {
		t.Fatalf("top-level order changed in rewritten body: %s", rewrittenText)
	}
}

func assertSingleReduceRequestCommand(t *testing.T, fake *recordingToolOptimizer, want string) {
	t.Helper()
	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("reduce request count = %d, want 1", len(reduceRequests))
	}
	if reduceRequests[0].Command != want {
		t.Fatalf("reduce command = %q, want %q", reduceRequests[0].Command, want)
	}
}
