package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestToolOptimizerResponsesReplaceTopLevelRawJSONFieldPreservesOrderAndRawValues(t *testing.T) {
	body := []byte(`{
  "model" : "gpt-4.1",
  "metadata" : { "b" : 2, "a" : [1,2] },
  "input" : [ { "old": true } ],
  "parallel_tool_calls" : false
}
`)

	got, ok := replaceTopLevelRawJSONField(body, "input", json.RawMessage(`[{"new":true}]`))
	if !ok {
		t.Fatalf("replaceTopLevelRawJSONField returned ok=false")
	}

	want := `{
  "model" : "gpt-4.1",
  "metadata" : { "b" : 2, "a" : [1,2] },
  "input" : [{"new":true}],
  "parallel_tool_calls" : false
}
`
	if string(got) != want {
		t.Fatalf("rewritten body mismatch\ngot:  %s\nwant: %s", got, want)
	}
	if !json.Valid(got) {
		t.Fatalf("rewritten body is not valid JSON: %s", got)
	}
}

func TestToolOptimizerResponsesReplaceTopLevelRawJSONFieldRejectsMissingOrInvalidInputs(t *testing.T) {
	body := []byte(`{"input":[1]}`)

	got, ok := replaceTopLevelRawJSONField(body, "missing", json.RawMessage(`[2]`))
	if ok {
		t.Fatalf("replaceTopLevelRawJSONField returned ok=true for missing field")
	}
	if string(got) != string(body) {
		t.Fatalf("missing field should return original body, got %s", got)
	}

	got, ok = replaceTopLevelRawJSONField(body, "input", json.RawMessage(`not-json`))
	if ok {
		t.Fatalf("replaceTopLevelRawJSONField returned ok=true for invalid replacement")
	}
	if string(got) != string(body) {
		t.Fatalf("invalid replacement should return original body, got %s", got)
	}
}

func TestToolOptimizerResponsesReducePrefersLocalToolContextOverStore(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{}
	configureRecordingToolOptimizer(handler, fake)

	const scope = "session:local-context-test"
	handler.toolContexts.Put(scope, ToolExecutionContext{
		CallID:           "call-local-context-1",
		ToolName:         "shell_command",
		OriginalCommand:  "stale command",
		RewrittenCommand: "stale rewritten command",
		CreatedAt:        time.Now(),
	})

	body := []byte(`{
		"model": "gpt-4",
		"input": [
			{
				"type": "function_call",
				"name": "shell_command",
				"call_id": "call-local-context-1",
				"arguments": "{\"command\":\"fresh command\"}"
			},
			{
				"type": "function_call_output",
				"call_id": "call-local-context-1",
				"output": "large output"
			}
		]
	}`)

	rewritten, count := handler.maybeReduceResponsesToolOutputsInRequestBody(context.Background(), body, handler.toolContexts, scope)
	if count != 1 {
		t.Fatalf("reduced output count = %d, want 1; body=%s", count, rewritten)
	}

	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("reduce request count = %d, want 1", len(reduceRequests))
	}
	if reduceRequests[0].Command != "fresh command" {
		t.Fatalf("reduce command = %q, want fresh command", reduceRequests[0].Command)
	}

	var payload struct {
		Input []map[string]interface{} `json:"input"`
	}
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("rewritten input count = %d, want 2", len(payload.Input))
	}
	if got := payload.Input[1]["output"]; got != "reduced output" {
		t.Fatalf("rewritten output = %v, want reduced output", got)
	}
}
