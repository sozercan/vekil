package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordingToolOptimizer struct {
	mu sync.Mutex

	rewriteRequests []ToolCommandRewriteRequest
	reduceRequests  []ToolOutputReduceRequest
}

func (o *recordingToolOptimizer) ID() string {
	return "recording-tool-optimizer"
}

func (o *recordingToolOptimizer) RewriteCommand(_ context.Context, req ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	o.mu.Lock()
	o.rewriteRequests = append(o.rewriteRequests, req)
	o.mu.Unlock()

	return ToolCommandRewriteResult{
		Changed: true,
		Command: "rg foo big.log",
	}, nil
}

func (o *recordingToolOptimizer) ReduceOutput(_ context.Context, req ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	o.mu.Lock()
	o.reduceRequests = append(o.reduceRequests, req)
	o.mu.Unlock()

	return ToolOutputReduceResult{
		Changed: true,
		Output:  "reduced output",
	}, nil
}

func (o *recordingToolOptimizer) snapshotRewriteRequests() []ToolCommandRewriteRequest {
	o.mu.Lock()
	defer o.mu.Unlock()

	out := make([]ToolCommandRewriteRequest, len(o.rewriteRequests))
	copy(out, o.rewriteRequests)
	return out
}

func (o *recordingToolOptimizer) snapshotReduceRequests() []ToolOutputReduceRequest {
	o.mu.Lock()
	defer o.mu.Unlock()

	out := make([]ToolOutputReduceRequest, len(o.reduceRequests))
	copy(out, o.reduceRequests)
	return out
}

func configureRecordingToolOptimizer(handler *ProxyHandler, fake *recordingToolOptimizer) {
	configureRecordingToolOptimizerWithShellFunctionCalls(handler, fake, []string{"shell_command"}, "/command")
}

func configureRecordingToolOptimizerWithShellFunctionCalls(handler *ProxyHandler, fake *recordingToolOptimizer, names []string, commandArgPath string) {
	enabled := true
	cfg := ToolOptimizersConfig{
		Enabled: true,
		Tools: ToolOptimizerToolsConfig{
			ShellFunctionCalls: ToolOptimizerShellFunctionCallsConfig{
				Enabled:        &enabled,
				Names:          names,
				CommandArgPath: commandArgPath,
			},
		},
		CommandRewrite: ToolOptimizerRewriteConfig{
			Enabled: true,
		},
		OutputReduce: ToolOptimizerOutputConfig{
			Enabled:       true,
			MinInputBytes: 1,
		},
	}
	handler.toolOptimizers = NewToolOptimizerManager(cfg, []stagedToolOptimizer{{
		optimizer:      fake,
		commandRewrite: true,
		outputReduce:   true,
	}})
	handler.toolContexts = NewToolExecutionContextStore()
}

func TestHandleResponses_ToolOptimizerRewritesCommandAndReducesOutput(t *testing.T) {
	fake := &recordingToolOptimizer{}

	var mu sync.Mutex
	var upstreamBodies []map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBodyForToolOptimizerTest(t, r)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		requestNumber := len(upstreamBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":     "resp-tool-1",
				"object": "response",
				"status": "completed",
				"output": []interface{}{
					shellCommandItemForToolOptimizerTest(t, "call-tool-1", "grep foo big.log"),
				},
			})
		default:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":     "resp-tool-2",
				"object": "response",
				"status": "completed",
				"output": []interface{}{},
			})
		}
	})
	configureRecordingToolOptimizer(handler, fake)

	firstResp := postResponsesForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"input": "run a command",
		"stream": false
	}`)
	defer firstResp.Body.Close()

	if firstResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(firstResp.Body)
		t.Fatalf("expected first response status 200, got %d: %s", firstResp.StatusCode, body)
	}

	firstPayload := decodeJSONBodyForToolOptimizerTest(t, firstResp.Body)
	firstOutput := outputItemsForToolOptimizerTest(t, firstPayload)
	if len(firstOutput) != 1 {
		t.Fatalf("expected 1 first output item, got %d", len(firstOutput))
	}
	gotCommand := shellCommandFromItemForToolOptimizerTest(t, firstOutput[0])
	if gotCommand != "rg foo big.log" {
		t.Fatalf("expected rewritten command, got %q", gotCommand)
	}

	secondResp := postResponsesForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"input": [
			{
				"type": "function_call_output",
				"call_id": "call-tool-1",
				"output": "large output"
			}
		],
		"stream": false
	}`)
	defer secondResp.Body.Close()

	if secondResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(secondResp.Body)
		t.Fatalf("expected second response status 200, got %d: %s", secondResp.StatusCode, body)
	}

	mu.Lock()
	if len(upstreamBodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(upstreamBodies))
	}
	secondUpstreamBody := upstreamBodies[1]
	mu.Unlock()

	items := upstreamInputItems(t, secondUpstreamBody)
	foundReducedOutput := false
	for _, item := range items {
		if item["type"] == "function_call_output" && item["call_id"] == "call-tool-1" {
			foundReducedOutput = true
			if got := item["output"]; got != "reduced output" {
				t.Fatalf("expected reduced tool output upstream, got %v", got)
			}
		}
	}
	if !foundReducedOutput {
		t.Fatalf("expected second upstream request to contain function_call_output")
	}

	rewriteRequests := fake.snapshotRewriteRequests()
	if len(rewriteRequests) != 1 {
		t.Fatalf("expected 1 rewrite request, got %d", len(rewriteRequests))
	}
	if rewriteRequests[0].Command != "grep foo big.log" {
		t.Fatalf("expected rewrite command to see original command, got %q", rewriteRequests[0].Command)
	}

	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("expected 1 reduce request, got %d", len(reduceRequests))
	}
	if reduceRequests[0].Command != "rg foo big.log" {
		t.Fatalf("expected reduce command to use rewritten command, got %q", reduceRequests[0].Command)
	}
	if reduceRequests[0].Output != "large output" {
		t.Fatalf("expected reduce request to see original output, got %q", reduceRequests[0].Output)
	}
}

func TestHandleResponses_ToolOptimizerRewritesExecCommandCmdArgPathAndReducesOutput(t *testing.T) {
	fake := &recordingToolOptimizer{}

	var mu sync.Mutex
	var upstreamBodies []map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBodyForToolOptimizerTest(t, r)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		requestNumber := len(upstreamBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":     "resp-codex-tool-1",
				"object": "response",
				"status": "completed",
				"output": []interface{}{
					execCommandItemForToolOptimizerTest(t, "call-codex-tool-1", "grep foo big.log"),
				},
			})
		default:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":     "resp-codex-tool-2",
				"object": "response",
				"status": "completed",
				"output": []interface{}{},
			})
		}
	})
	configureRecordingToolOptimizerWithShellFunctionCalls(handler, fake, []string{"exec_command"}, "/cmd")

	firstResp := postResponsesForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"input": "run a command",
		"stream": false
	}`)
	defer firstResp.Body.Close()

	if firstResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(firstResp.Body)
		t.Fatalf("expected first response status 200, got %d: %s", firstResp.StatusCode, body)
	}

	firstPayload := decodeJSONBodyForToolOptimizerTest(t, firstResp.Body)
	firstOutput := outputItemsForToolOptimizerTest(t, firstPayload)
	if len(firstOutput) != 1 {
		t.Fatalf("expected 1 first output item, got %d", len(firstOutput))
	}
	if gotName := firstOutput[0]["name"]; gotName != "exec_command" {
		t.Fatalf("expected exec_command tool name, got %v", gotName)
	}
	if gotCommand := commandFromItemAtPathForToolOptimizerTest(t, firstOutput[0], "/cmd"); gotCommand != "rg foo big.log" {
		t.Fatalf("expected rewritten /cmd command, got %q", gotCommand)
	}
	assertCommandArgumentAbsentForToolOptimizerTest(t, firstOutput[0])

	secondResp := postResponsesForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"input": [
			{
				"type": "function_call_output",
				"call_id": "call-codex-tool-1",
				"output": "large output"
			}
		],
		"stream": false
	}`)
	defer secondResp.Body.Close()

	if secondResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(secondResp.Body)
		t.Fatalf("expected second response status 200, got %d: %s", secondResp.StatusCode, body)
	}

	mu.Lock()
	if len(upstreamBodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(upstreamBodies))
	}
	secondUpstreamBody := upstreamBodies[1]
	mu.Unlock()

	items := upstreamInputItems(t, secondUpstreamBody)
	foundReducedOutput := false
	for _, item := range items {
		if item["type"] == "function_call_output" && item["call_id"] == "call-codex-tool-1" {
			foundReducedOutput = true
			if got := item["output"]; got != "reduced output" {
				t.Fatalf("expected reduced tool output upstream, got %v", got)
			}
		}
	}
	if !foundReducedOutput {
		t.Fatalf("expected second upstream request to contain function_call_output")
	}

	rewriteRequests := fake.snapshotRewriteRequests()
	if len(rewriteRequests) != 1 {
		t.Fatalf("expected 1 rewrite request, got %d", len(rewriteRequests))
	}
	if rewriteRequests[0].ToolName != "exec_command" {
		t.Fatalf("expected rewrite tool name exec_command, got %q", rewriteRequests[0].ToolName)
	}
	if rewriteRequests[0].Command != "grep foo big.log" {
		t.Fatalf("expected rewrite command to see original /cmd command, got %q", rewriteRequests[0].Command)
	}

	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("expected 1 reduce request, got %d", len(reduceRequests))
	}
	if reduceRequests[0].ToolName != "exec_command" {
		t.Fatalf("expected reduce tool name exec_command, got %q", reduceRequests[0].ToolName)
	}
	if reduceRequests[0].Command != "rg foo big.log" {
		t.Fatalf("expected reduce command to use rewritten /cmd command, got %q", reduceRequests[0].Command)
	}
	if reduceRequests[0].Output != "large output" {
		t.Fatalf("expected reduce request to see original output, got %q", reduceRequests[0].Output)
	}
}

func TestHandleResponses_ToolOptimizerDefaultOffPassesThrough(t *testing.T) {
	var mu sync.Mutex
	var upstreamBodies []map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBodyForToolOptimizerTest(t, r)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		requestNumber := len(upstreamBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":     "resp-tool-default-1",
				"object": "response",
				"status": "completed",
				"output": []interface{}{
					shellCommandItemForToolOptimizerTest(t, "call-tool-1", "grep foo big.log"),
				},
			})
		default:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":     "resp-tool-default-2",
				"object": "response",
				"status": "completed",
				"output": []interface{}{},
			})
		}
	})

	firstResp := postResponsesForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"input": "run a command",
		"stream": false
	}`)
	defer firstResp.Body.Close()

	if firstResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(firstResp.Body)
		t.Fatalf("expected first response status 200, got %d: %s", firstResp.StatusCode, body)
	}

	firstPayload := decodeJSONBodyForToolOptimizerTest(t, firstResp.Body)
	firstOutput := outputItemsForToolOptimizerTest(t, firstPayload)
	if len(firstOutput) != 1 {
		t.Fatalf("expected 1 first output item, got %d", len(firstOutput))
	}
	gotCommand := shellCommandFromItemForToolOptimizerTest(t, firstOutput[0])
	if gotCommand != "grep foo big.log" {
		t.Fatalf("expected original command when optimizer is off, got %q", gotCommand)
	}

	secondResp := postResponsesForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"input": [
			{
				"type": "function_call_output",
				"call_id": "call-tool-1",
				"output": "large output"
			}
		],
		"stream": false
	}`)
	defer secondResp.Body.Close()

	if secondResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(secondResp.Body)
		t.Fatalf("expected second response status 200, got %d: %s", secondResp.StatusCode, body)
	}

	mu.Lock()
	if len(upstreamBodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(upstreamBodies))
	}
	secondUpstreamBody := upstreamBodies[1]
	mu.Unlock()

	items := upstreamInputItems(t, secondUpstreamBody)
	foundOriginalOutput := false
	for _, item := range items {
		if item["type"] == "function_call_output" && item["call_id"] == "call-tool-1" {
			foundOriginalOutput = true
			if got := item["output"]; got != "large output" {
				t.Fatalf("expected original tool output upstream when optimizer is off, got %v", got)
			}
		}
	}
	if !foundOriginalOutput {
		t.Fatalf("expected second upstream request to contain function_call_output")
	}
}

func TestHandleResponsesWebSocket_ToolOptimizerReducesOutputWithoutRewritingStreamedCommand(t *testing.T) {
	fake := &recordingToolOptimizer{}

	var mu sync.Mutex
	var upstreamBodies []map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBodyForToolOptimizerTest(t, r)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		requestNumber := len(upstreamBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			writeResponsesSSEForToolOptimizerTest(t, w, "response.created", map[string]interface{}{
				"type":     "response.created",
				"response": map[string]interface{}{"id": "resp-ws-tool-1"},
			})
			writeResponsesSSEForToolOptimizerTest(t, w, "response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done",
				"item": shellCommandItemForToolOptimizerTest(t, "call-tool-1", "grep foo big.log"),
			})
			writeResponsesSSEForToolOptimizerTest(t, w, "response.completed", map[string]interface{}{
				"type": "response.completed",
				"response": map[string]interface{}{
					"id":    "resp-ws-tool-1",
					"usage": zeroResponsesUsage(),
				},
			})
		default:
			writeResponsesSSEForToolOptimizerTest(t, w, "response.created", map[string]interface{}{
				"type":     "response.created",
				"response": map[string]interface{}{"id": "resp-ws-tool-2"},
			})
			writeResponsesSSEForToolOptimizerTest(t, w, "response.completed", map[string]interface{}{
				"type": "response.completed",
				"response": map[string]interface{}{
					"id":    "resp-ws-tool-2",
					"usage": zeroResponsesUsage(),
				},
			})
		}
	})
	configureRecordingToolOptimizer(handler, fake)

	server := startResponsesWebSocketProxyServer(t, handler)
	headers := http.Header{}
	headers.Set("session_id", "sess-tool-ws-1")
	conn := mustDialResponsesWebSocket(t, server, headers)
	defer func() { _ = conn.Close() }()

	firstRequest := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "run command"},
			},
		},
	})
	if err := conn.WriteJSON(firstRequest); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}
	responseID := websocketResponseID(t, created)

	output := mustReadWebSocketJSON(t, conn)
	if output["type"] != "response.output_item.done" {
		t.Fatalf("expected response.output_item.done, got %v", output["type"])
	}
	outputItem, ok := output["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected output item object, got %T", output["item"])
	}
	if got := shellCommandFromItemForToolOptimizerTest(t, outputItem); got != "grep foo big.log" {
		t.Fatalf("expected streamed command to remain original, got %q", got)
	}

	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected response.completed, got %v", completed["type"])
	}

	secondRequest := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call-tool-1",
			"output":  "large output",
		},
	})
	secondRequest["previous_response_id"] = responseID

	if err := conn.WriteJSON(secondRequest); err != nil {
		t.Fatalf("failed to write second websocket request: %v", err)
	}

	secondCreated := mustReadWebSocketJSON(t, conn)
	if secondCreated["type"] != "response.created" {
		t.Fatalf("expected second response.created, got %v", secondCreated["type"])
	}
	secondCompleted := mustReadWebSocketJSON(t, conn)
	if secondCompleted["type"] != "response.completed" {
		t.Fatalf("expected second response.completed, got %v", secondCompleted["type"])
	}

	if rewriteRequests := fake.snapshotRewriteRequests(); len(rewriteRequests) != 0 {
		t.Fatalf("expected no rewrite request for streamed websocket command, got %d", len(rewriteRequests))
	}

	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("expected 1 reduce request, got %d", len(reduceRequests))
	}
	if reduceRequests[0].Command != "grep foo big.log" {
		t.Fatalf("expected reduce request to use original streamed command, got %q", reduceRequests[0].Command)
	}

	mu.Lock()
	if len(upstreamBodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(upstreamBodies))
	}
	secondUpstreamBody := upstreamBodies[1]
	mu.Unlock()

	items := upstreamInputItems(t, secondUpstreamBody)
	var foundOutput bool
	for _, item := range items {
		if item["type"] == "function_call_output" && item["call_id"] == "call-tool-1" {
			foundOutput = true
			if got := item["output"]; got != "reduced output" {
				t.Fatalf("expected reduced websocket tool output upstream, got %v", got)
			}
		}
	}
	if !foundOutput {
		t.Fatalf("expected second upstream websocket request to contain function_call_output")
	}
}

func TestHandleResponsesWebSocket_ToolOptimizerCapturesExecCommandCmdArgPathAndReducesOutput(t *testing.T) {
	fake := &recordingToolOptimizer{}

	var mu sync.Mutex
	var upstreamBodies []map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBodyForToolOptimizerTest(t, r)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		requestNumber := len(upstreamBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			writeResponsesSSEForToolOptimizerTest(t, w, "response.created", map[string]interface{}{
				"type":     "response.created",
				"response": map[string]interface{}{"id": "resp-ws-codex-tool-1"},
			})
			writeResponsesSSEForToolOptimizerTest(t, w, "response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done",
				"item": execCommandItemForToolOptimizerTest(t, "call-ws-codex-tool-1", "grep foo big.log"),
			})
			writeResponsesSSEForToolOptimizerTest(t, w, "response.completed", map[string]interface{}{
				"type": "response.completed",
				"response": map[string]interface{}{
					"id":    "resp-ws-codex-tool-1",
					"usage": zeroResponsesUsage(),
				},
			})
		default:
			writeResponsesSSEForToolOptimizerTest(t, w, "response.created", map[string]interface{}{
				"type":     "response.created",
				"response": map[string]interface{}{"id": "resp-ws-codex-tool-2"},
			})
			writeResponsesSSEForToolOptimizerTest(t, w, "response.completed", map[string]interface{}{
				"type": "response.completed",
				"response": map[string]interface{}{
					"id":    "resp-ws-codex-tool-2",
					"usage": zeroResponsesUsage(),
				},
			})
		}
	})
	configureRecordingToolOptimizerWithShellFunctionCalls(handler, fake, []string{"exec_command"}, "/cmd")

	server := startResponsesWebSocketProxyServer(t, handler)
	headers := http.Header{}
	headers.Set("session_id", "sess-tool-ws-codex-1")
	conn := mustDialResponsesWebSocket(t, server, headers)
	defer func() { _ = conn.Close() }()

	firstRequest := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "run command"},
			},
		},
	})
	if err := conn.WriteJSON(firstRequest); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}

	created := mustReadWebSocketJSON(t, conn)
	if created["type"] != "response.created" {
		t.Fatalf("expected response.created, got %v", created["type"])
	}
	responseID := websocketResponseID(t, created)

	output := mustReadWebSocketJSON(t, conn)
	if output["type"] != "response.output_item.done" {
		t.Fatalf("expected response.output_item.done, got %v", output["type"])
	}
	outputItem, ok := output["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected output item object, got %T", output["item"])
	}
	if gotName := outputItem["name"]; gotName != "exec_command" {
		t.Fatalf("expected exec_command tool name, got %v", gotName)
	}
	if got := commandFromItemAtPathForToolOptimizerTest(t, outputItem, "/cmd"); got != "grep foo big.log" {
		t.Fatalf("expected streamed /cmd command to remain original, got %q", got)
	}
	assertCommandArgumentAbsentForToolOptimizerTest(t, outputItem)

	completed := mustReadWebSocketJSON(t, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("expected response.completed, got %v", completed["type"])
	}

	secondRequest := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":    "function_call_output",
			"call_id": "call-ws-codex-tool-1",
			"output":  "large output",
		},
	})
	secondRequest["previous_response_id"] = responseID

	if err := conn.WriteJSON(secondRequest); err != nil {
		t.Fatalf("failed to write second websocket request: %v", err)
	}

	secondCreated := mustReadWebSocketJSON(t, conn)
	if secondCreated["type"] != "response.created" {
		t.Fatalf("expected second response.created, got %v", secondCreated["type"])
	}
	secondCompleted := mustReadWebSocketJSON(t, conn)
	if secondCompleted["type"] != "response.completed" {
		t.Fatalf("expected second response.completed, got %v", secondCompleted["type"])
	}

	if rewriteRequests := fake.snapshotRewriteRequests(); len(rewriteRequests) != 0 {
		t.Fatalf("expected no rewrite request for streamed websocket command, got %d", len(rewriteRequests))
	}

	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("expected 1 reduce request, got %d", len(reduceRequests))
	}
	if reduceRequests[0].ToolName != "exec_command" {
		t.Fatalf("expected reduce tool name exec_command, got %q", reduceRequests[0].ToolName)
	}
	if reduceRequests[0].Command != "grep foo big.log" {
		t.Fatalf("expected reduce request to use original streamed /cmd command, got %q", reduceRequests[0].Command)
	}

	mu.Lock()
	if len(upstreamBodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(upstreamBodies))
	}
	secondUpstreamBody := upstreamBodies[1]
	mu.Unlock()

	items := upstreamInputItems(t, secondUpstreamBody)
	var foundOutput bool
	for _, item := range items {
		if item["type"] == "function_call_output" && item["call_id"] == "call-ws-codex-tool-1" {
			foundOutput = true
			if got := item["output"]; got != "reduced output" {
				t.Fatalf("expected reduced websocket tool output upstream, got %v", got)
			}
		}
	}
	if !foundOutput {
		t.Fatalf("expected second upstream websocket request to contain function_call_output")
	}
}

func TestHandleOpenAIChatCompletions_ToolOptimizerReducesOutputWithGlobalFallback(t *testing.T) {
	runOpenAIChatToolOptimizerReducesOutputTest(t, "")
}

func TestHandleOpenAIChatCompletions_ToolOptimizerReducesOutputWithHeaderScope(t *testing.T) {
	runOpenAIChatToolOptimizerReducesOutputTest(t, "sess-chat-tool-1")
}

func runOpenAIChatToolOptimizerReducesOutputTest(t *testing.T, sessionID string) {
	t.Helper()

	fake := &recordingToolOptimizer{}

	var mu sync.Mutex
	var upstreamBodies []map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBodyForToolOptimizerTest(t, r)

		mu.Lock()
		upstreamBodies = append(upstreamBodies, body)
		requestNumber := len(upstreamBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":      "chatcmpl-chat-tool-1",
				"object":  "chat.completion",
				"created": 1700000000,
				"model":   "gpt-4",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role": "assistant",
							"tool_calls": []interface{}{
								map[string]interface{}{
									"id":   "call-chat-1",
									"type": "function",
									"function": map[string]interface{}{
										"name":      "shell_command",
										"arguments": `{"command":"grep foo big.log"}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
			})
		default:
			writeJSONForToolOptimizerTest(t, w, map[string]interface{}{
				"id":      "chatcmpl-chat-tool-2",
				"object":  "chat.completion",
				"created": 1700000001,
				"model":   "gpt-4",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "done",
						},
						"finish_reason": "stop",
					},
				},
			})
		}
	})
	configureRecordingToolOptimizer(handler, fake)

	firstResp := postOpenAIChatForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "run command"}],
		"stream": false
	}`, sessionID)
	defer firstResp.Body.Close()

	if firstResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(firstResp.Body)
		t.Fatalf("expected first response status 200, got %d: %s", firstResp.StatusCode, body)
	}

	secondResp := postOpenAIChatForToolOptimizerTest(t, handler, `{
		"model": "gpt-4",
		"messages": [
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call-chat-1",
						"type": "function",
						"function": {
							"name": "shell_command",
							"arguments": "{\"command\":\"grep foo big.log\"}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call-chat-1",
				"content": "large output"
			}
		],
		"stream": false
	}`, sessionID)
	defer secondResp.Body.Close()

	if secondResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(secondResp.Body)
		t.Fatalf("expected second response status 200, got %d: %s", secondResp.StatusCode, body)
	}

	mu.Lock()
	if len(upstreamBodies) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(upstreamBodies))
	}
	secondUpstreamBody := upstreamBodies[1]
	mu.Unlock()

	rawMessages, ok := secondUpstreamBody["messages"].([]interface{})
	if !ok {
		t.Fatalf("expected messages array, got %T", secondUpstreamBody["messages"])
	}

	foundToolMessage := false
	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("expected message object, got %T", raw)
		}
		if msg["role"] != "tool" || msg["tool_call_id"] != "call-chat-1" {
			continue
		}
		foundToolMessage = true
		if got := msg["content"]; got != "reduced output" {
			t.Fatalf("expected reduced tool output upstream, got %v", got)
		}
	}
	if !foundToolMessage {
		t.Fatalf("expected second upstream request to contain tool message")
	}

	if sessionID != "" {
		if _, ok := handler.toolContexts.Get("session:"+sessionID, "call-chat-1"); !ok {
			t.Fatalf("expected scoped tool context capture")
		}
	}

	rewriteRequests := fake.snapshotRewriteRequests()
	if len(rewriteRequests) != 1 {
		t.Fatalf("expected 1 rewrite request, got %d", len(rewriteRequests))
	}
	if rewriteRequests[0].Command != "grep foo big.log" {
		t.Fatalf("expected rewrite command to see original command, got %q", rewriteRequests[0].Command)
	}

	reduceRequests := fake.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("expected 1 reduce request, got %d", len(reduceRequests))
	}
	if reduceRequests[0].Command != "rg foo big.log" {
		t.Fatalf("expected reduce command to use rewritten command, got %q", reduceRequests[0].Command)
	}
	if reduceRequests[0].Output != "large output" {
		t.Fatalf("expected reduce request to see original output, got %q", reduceRequests[0].Output)
	}
}

func postOpenAIChatForToolOptimizerTest(t *testing.T, handler *ProxyHandler, body string, sessionID string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("session_id", sessionID)
	}

	w := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(w, req)
	return w.Result()
}

func postResponsesForToolOptimizerTest(t *testing.T, handler *ProxyHandler, body string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "sess-tool-1")

	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)
	return w.Result()
}

func decodeRequestBodyForToolOptimizerTest(t *testing.T, r *http.Request) map[string]interface{} {
	t.Helper()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("failed to read upstream request body: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("failed to decode upstream request body %s: %v", string(bodyBytes), err)
	}
	return body
}

func decodeJSONBodyForToolOptimizerTest(t *testing.T, r io.Reader) map[string]interface{} {
	t.Helper()

	bodyBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("failed to decode response body %s: %v", string(bodyBytes), err)
	}
	return body
}

func writeJSONForToolOptimizerTest(t *testing.T, w http.ResponseWriter, value interface{}) {
	t.Helper()

	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("failed to encode upstream response: %v", err)
	}
}

func writeResponsesSSEForToolOptimizerTest(t *testing.T, w io.Writer, event string, value interface{}) {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("failed to encode SSE event: %v", err)
	}
	_, _ = io.WriteString(w, "event: "+event+"\n")
	_, _ = io.WriteString(w, "data: "+string(data)+"\n\n")
}

func shellCommandItemForToolOptimizerTest(t *testing.T, callID, command string) map[string]interface{} {
	t.Helper()
	return commandItemForToolOptimizerTest(t, "shell_command", callID, map[string]interface{}{"command": command})
}

func execCommandItemForToolOptimizerTest(t *testing.T, callID, command string) map[string]interface{} {
	t.Helper()
	return commandItemForToolOptimizerTest(t, "exec_command", callID, map[string]interface{}{"cmd": command})
}

func commandItemForToolOptimizerTest(t *testing.T, toolName, callID string, arguments map[string]interface{}) map[string]interface{} {
	t.Helper()

	args, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("failed to encode shell arguments: %v", err)
	}

	return map[string]interface{}{
		"type":      "function_call",
		"name":      toolName,
		"call_id":   callID,
		"arguments": string(args),
	}
}

func outputItemsForToolOptimizerTest(t *testing.T, payload map[string]interface{}) []map[string]interface{} {
	t.Helper()

	rawItems, ok := payload["output"].([]interface{})
	if !ok {
		t.Fatalf("expected output array, got %T", payload["output"])
	}

	items := make([]map[string]interface{}, len(rawItems))
	for i, raw := range rawItems {
		item, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("expected output item object, got %T", raw)
		}
		items[i] = item
	}
	return items
}

func shellCommandFromItemForToolOptimizerTest(t *testing.T, item map[string]interface{}) string {
	t.Helper()
	return commandFromItemAtPathForToolOptimizerTest(t, item, "/command")
}

func commandFromItemAtPathForToolOptimizerTest(t *testing.T, item map[string]interface{}, path string) string {
	t.Helper()

	rawArgs, ok := item["arguments"].(string)
	if !ok {
		t.Fatalf("expected string arguments, got %T", item["arguments"])
	}

	command, ok := extractStringArgumentAtPath(rawArgs, path)
	if !ok {
		t.Fatalf("expected command string at %s in arguments %q", path, rawArgs)
	}
	return command
}

func assertCommandArgumentAbsentForToolOptimizerTest(t *testing.T, item map[string]interface{}) {
	t.Helper()

	rawArgs, ok := item["arguments"].(string)
	if !ok {
		t.Fatalf("expected string arguments, got %T", item["arguments"])
	}

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		t.Fatalf("failed to decode shell arguments %q: %v", rawArgs, err)
	}
	if _, ok := args["command"]; ok {
		t.Fatalf("expected Codex-style arguments to avoid legacy command key, got %q", rawArgs)
	}
}
