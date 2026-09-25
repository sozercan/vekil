package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFlattenLocalAINamespaceTools(t *testing.T) {
	body := `{"model":"m","tools":[{"type":"function","name":"exec_command"},{"type":"namespace","name":"mcp__repl","description":"REPL tools","tools":[{"type":"function","name":"js","description":"Run JS","parameters":{"type":"object"},"defer_loading":true}]}],"input":[{"type":"function_call","namespace":"mcp__repl","name":"js","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`
	out, aliases, err := flattenLocalAINamespaceTools([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Tools []map[string]any `json:"tools"`
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tools) != 2 || payload.Tools[1]["type"] != "function" || payload.Tools[1]["name"] != "mcp__repl__js" ||
		payload.Tools[1]["description"] != "REPL tools\n\nRun JS" || payload.Tools[1]["defer_loading"] != nil {
		t.Fatalf("tools = %v", payload.Tools)
	}
	if payload.Input[0]["name"] != "mcp__repl__js" || payload.Input[0]["namespace"] != nil {
		t.Fatalf("input = %v", payload.Input)
	}
	if aliases["mcp__repl__js"] != (localAIToolAlias{namespace: "mcp__repl", name: "js"}) {
		t.Fatalf("aliases = %v", aliases)
	}

	// History-only namespaces keep their original name in the alias map.
	history := `{"tools":[{"type":"namespace","name":"a","tools":[{"type":"function","name":"f"}]}],"input":[{"type":"function_call","namespace":"old","name":"g","call_id":"c","arguments":"{}"}]}`
	_, historyAliases, err := flattenLocalAINamespaceTools([]byte(history))
	if err != nil || historyAliases["old__g"] != (localAIToolAlias{namespace: "old", name: "g"}) {
		t.Fatalf("history aliases = %v, %v", historyAliases, err)
	}

	choice := `{"tools":[{"type":"namespace","name":"agents","tools":[{"type":"function","name":"spawn"}]}],"tool_choice":{"type":"function","namespace":"agents","name":"spawn"}}`
	out, _, err = flattenLocalAINamespaceTools([]byte(choice))
	if err != nil || !strings.Contains(string(out), `"tool_choice":{"name":"agents__spawn","type":"function"}`) {
		t.Fatalf("tool_choice = %s, %v", out, err)
	}
	allowed := `{"tools":[{"type":"namespace","name":"agents","tools":[{"type":"function","name":"spawn"}]}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","namespace":"agents","name":"spawn"},{"type":"function","name":"plain"}]}}`
	out, _, err = flattenLocalAINamespaceTools([]byte(allowed))
	if err != nil || !strings.Contains(string(out), `{"name":"agents__spawn","type":"function"}`) || !strings.Contains(string(out), `{"name":"plain","type":"function"}`) {
		t.Fatalf("allowed_tools = %s, %v", out, err)
	}

	if out, aliases, err := flattenLocalAINamespaceTools([]byte(`{"tools":[{"type":"function","name":"f"}]}`)); err != nil || aliases != nil || string(out) != `{"tools":[{"type":"function","name":"f"}]}` {
		t.Fatalf("plain request changed: %s %v %v", out, aliases, err)
	}
	historyCollision := `{"tools":[{"type":"function","name":"ns__f"},{"type":"namespace","name":"other","tools":[{"type":"function","name":"g"}]}],"input":[{"type":"function_call","namespace":"ns","name":"f","call_id":"c","arguments":"{}"}]}`
	if _, _, err := flattenLocalAINamespaceTools([]byte(historyCollision)); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("history collision error = %v", err)
	}
	collision := `{"tools":[{"type":"function","name":"ns__f"},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}]}]}`
	if _, _, err := flattenLocalAINamespaceTools([]byte(collision)); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("collision error = %v", err)
	}
	custom := `{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"f"}]}]}`
	if _, _, err := flattenLocalAINamespaceTools([]byte(custom)); err == nil || providerRequestErrorCode(err) != "unsupported_tool_type" {
		t.Fatalf("custom child error = %v", err)
	}
}

func TestRestoreLocalAIToolAliasesPassesLargeBodiesThrough(t *testing.T) {
	previous := localAIAliasBodyLimit
	localAIAliasBodyLimit = 16
	t.Cleanup(func() { localAIAliasBodyLimit = previous })
	body := `{"id":"r","output":[{"type":"function_call","name":"mcp__repl__js"}]}`
	resp := restoreLocalAIToolAliases(upstreamResponse(http.StatusOK, "application/json", strings.NewReader(body)), localAIToolAliases{"mcp__repl__js": {namespace: "mcp__repl", name: "js"}})
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatalf("large body = %q, want it unchanged", got)
	}
}

func TestRestoreLocalAIToolAliases(t *testing.T) {
	aliases := localAIToolAliases{"mcp__repl__js": {namespace: "mcp__repl", name: "js"}}
	jsonResp := upstreamResponse(http.StatusOK, "application/json", strings.NewReader(`{"id":"r","output":[{"type":"function_call","name":"mcp__repl__js","call_id":"c","arguments":"{}"},{"type":"function_call","name":"exec_command","call_id":"d","arguments":"{}"}]}`))
	body, _ := io.ReadAll(restoreLocalAIToolAliases(jsonResp, aliases).Body)
	var payload struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Output[0]["name"] != "js" || payload.Output[0]["namespace"] != "mcp__repl" || payload.Output[1]["name"] != "exec_command" || payload.Output[1]["namespace"] != nil {
		t.Fatalf("output = %v", payload.Output)
	}

	stream := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"mcp__repl__js\",\"call_id\":\"c\"}}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"name\":\"mcp__repl__js\",\"arguments\":\"{}\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":\"mcp__repl__js\"}]}}\n\n" +
		"data: [DONE]\n\n"
	restored, _ := io.ReadAll(restoreLocalAIToolAliases(upstreamResponse(http.StatusOK, "text/event-stream", strings.NewReader(stream)), aliases).Body)
	text := string(restored)
	if strings.Contains(text, "mcp__repl__js") || strings.Count(text, `"namespace":"mcp__repl"`) != 3 ||
		!strings.Contains(text, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n") || !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("stream = %s", text)
	}
}
