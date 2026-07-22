package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

func TestTranslatePolicyResponsesRequestToChatBasicCodexRequest(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-semantic",
		"instructions":"You are a coding agent.",
		"input":"Fix the failing test.",
		"store":false,
		"stream":true,
		"max_output_tokens":4096,
		"temperature":0.2,
		"top_p":0.9,
		"parallel_tool_calls":true,
		"reasoning":{"effort":"high"},
		"include":["reasoning.encrypted_content"],
		"prompt_cache_key":"session-1",
		"client_metadata":{"originator":"codex_cli_rs"},
		"metadata":{"session":"one"},
		"safety_identifier":"safe-1",
		"user":"user-1",
		"tools":[{
			"type":"function",
			"name":"exec_command",
			"description":"Run a command",
			"strict":false,
			"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}
		}],
		"tool_choice":"auto"
	}`)

	got, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatalf("translatePolicyResponsesRequestToChat() error = %v", err)
	}
	if got.PublicModel != "gpt-5.6-semantic" {
		t.Fatalf("PublicModel = %q", got.PublicModel)
	}
	if !got.Stream {
		t.Fatal("Stream = false, want true")
	}
	if descriptor := got.Tools["exec_command"]; descriptor.Name != "exec_command" || descriptor.Namespace != "" {
		t.Fatalf("Tools[exec_command] = %#v", descriptor)
	}

	var chat map[string]any
	if err := json.Unmarshal(got.Body, &chat); err != nil {
		t.Fatalf("decode canonical Chat body: %v", err)
	}
	if chat["model"] != "gpt-5.6-semantic" || chat["stream"] != true {
		t.Fatalf("canonical model/stream = %#v/%#v", chat["model"], chat["stream"])
	}
	if chat["max_completion_tokens"] != float64(4096) || chat["temperature"] != 0.2 || chat["top_p"] != 0.9 {
		t.Fatalf("canonical sampling/token fields = %#v", chat)
	}
	if chat["parallel_tool_calls"] != true || chat["reasoning_effort"] != "high" || chat["tool_choice"] != "auto" {
		t.Fatalf("canonical controls = %#v", chat)
	}
	messages, ok := chat["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", chat["messages"])
	}
	if messages[0].(map[string]any)["role"] != "developer" || messages[0].(map[string]any)["content"] != "You are a coding agent." {
		t.Fatalf("instruction message = %#v", messages[0])
	}
	if messages[1].(map[string]any)["role"] != "user" || messages[1].(map[string]any)["content"] != "Fix the failing test." {
		t.Fatalf("input message = %#v", messages[1])
	}
	if _, exists := chat["include"]; exists {
		t.Fatalf("benign Responses metadata leaked into Chat body: %#v", chat)
	}
	if string(got.Response.Tools) == "" || string(got.Response.ToolChoice) != `"auto"` || !got.Response.ParallelToolCalls {
		t.Fatalf("response tool metadata = tools:%s choice:%s parallel:%v", got.Response.Tools, got.Response.ToolChoice, got.Response.ParallelToolCalls)
	}
}

func TestTranslatePolicyResponsesRequestToChatStructuredOutput(t *testing.T) {
	body := []byte(`{
		"model":"policy",
		"input":"return JSON",
		"store":false,
		"text":{"format":{"type":"json_schema","name":"result","description":"result shape","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false},"strict":true}}
	}`)
	got, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat models.OpenAIRequest
	if err := json.Unmarshal(got.Body, &chat); err != nil {
		t.Fatal(err)
	}
	var format struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(chat.ResponseFormat, &format); err != nil {
		t.Fatalf("decode response_format: %v; body=%s", err, got.Body)
	}
	if format.Type != "json_schema" || format.JSONSchema.Name != "result" || format.JSONSchema.Description != "result shape" || format.JSONSchema.Strict == nil || !*format.JSONSchema.Strict {
		t.Fatalf("response_format = %+v", format)
	}
	var schema map[string]any
	if err := json.Unmarshal(format.JSONSchema.Schema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("schema = %#v, error = %v", schema, err)
	}
	if !strings.Contains(string(got.Response.Text), `"type":"json_schema"`) || !strings.Contains(string(got.Response.Text), `"name":"result"`) {
		t.Fatalf("response text metadata = %s", got.Response.Text)
	}
}

func TestTranslatePolicyResponsesRequestToChatSimpleTextFormats(t *testing.T) {
	for _, formatType := range []string{"text", "json_object"} {
		t.Run(formatType, func(t *testing.T) {
			body := []byte(`{"model":"policy","input":"hi","text":{"format":{"type":` + string(policyResponsesJSONString(formatType)) + `}}}`)
			got, err := translatePolicyResponsesRequestToChat(body)
			if err != nil {
				t.Fatal(err)
			}
			var chat models.OpenAIRequest
			if err := json.Unmarshal(got.Body, &chat); err != nil {
				t.Fatal(err)
			}
			var format map[string]any
			if err := json.Unmarshal(chat.ResponseFormat, &format); err != nil {
				t.Fatal(err)
			}
			if format["type"] != formatType {
				t.Fatalf("response_format = %#v", format)
			}
		})
	}
}

func TestTranslatePolicyResponsesRequestToChatTracksRequiredToolChoice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   string
		required bool
	}{
		{name: "auto", choice: `"auto"`},
		{name: "required", choice: `"required"`, required: true},
		{name: "forced function", choice: `{"type":"function","name":"lookup"}`, required: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"policy","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":` + tc.choice + `}`)
			got, err := translatePolicyResponsesRequestToChat(body)
			if err != nil {
				t.Fatal(err)
			}
			if got.Response.RequiresToolCall != tc.required {
				t.Fatalf("RequiresToolCall = %v, want %v", got.Response.RequiresToolCall, tc.required)
			}
		})
	}
}

func TestTranslatePolicyResponsesRequestToChatAcceptsNullReasoningFromCodex(t *testing.T) {
	got, err := translatePolicyResponsesRequestToChat([]byte(`{"model":"policy","input":"hi","reasoning":null,"store":false,"stream":true}`))
	if err != nil {
		t.Fatalf("translatePolicyResponsesRequestToChat() error = %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(got.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if _, exists := chat["reasoning_effort"]; exists {
		t.Fatalf("null reasoning produced reasoning_effort: %#v", chat)
	}
}

func TestTranslatePolicyResponsesRequestToChatAcceptsCodexReasoningSummary(t *testing.T) {
	for _, summary := range []string{"auto", "concise", "detailed"} {
		t.Run(summary, func(t *testing.T) {
			body := []byte(`{"model":"policy","input":"hi","reasoning":{"effort":"high","summary":` + string(policyResponsesJSONString(summary)) + `},"store":false}`)
			got, err := translatePolicyResponsesRequestToChat(body)
			if err != nil {
				t.Fatalf("translatePolicyResponsesRequestToChat() error = %v", err)
			}
			var chat map[string]any
			if err := json.Unmarshal(got.Body, &chat); err != nil {
				t.Fatal(err)
			}
			if chat["reasoning_effort"] != "high" {
				t.Fatalf("reasoning_effort = %#v", chat["reasoning_effort"])
			}
			if _, exists := chat["reasoning"]; exists {
				t.Fatalf("reasoning summary leaked into Chat: %#v", chat)
			}
			if _, exists := chat["reasoning_summary"]; exists {
				t.Fatalf("reasoning_summary leaked into Chat: %#v", chat)
			}
			if strings.Contains(string(got.Body), summary) {
				t.Fatalf("summary value leaked into Chat body: %s", got.Body)
			}
		})
	}
}

func TestTranslatePolicyResponsesRequestToChatMergesAssistantTextWithFollowingCalls(t *testing.T) {
	body := []byte(`{
		"model":"policy",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"}]},
			{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"I will inspect first."}]},
			{"type":"function_call","call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","status":"completed"},
			{"type":"function_call_output","call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","output":"ok"}
		],
		"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}],
		"store":false
	}`)
	got, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat models.OpenAIRequest
	if err := json.Unmarshal(got.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("messages=%+v", chat.Messages)
	}
	assistant := chat.Messages[1]
	if assistant.Role != "assistant" || string(assistant.Content) != `"I will inspect first."` || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_vekil_AAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("merged assistant=%+v", assistant)
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "call_vekil_AAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("tool message=%+v", chat.Messages[2])
	}
}

func TestTranslatePolicyResponsesRequestToChatGroupsParallelFunctionCalls(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-semantic",
		"instructions":"Work carefully.",
		"input":[
			{"type":"message","id":"msg_user","role":"user","status":"completed","content":[{"type":"input_text","text":"Inspect both targets."}]},
			{"type":"function_call","id":"fc_a","call_id":"call_a","name":"lookup","arguments":"{\"key\":\"a\"}","status":"completed"},
			{"type":"function_call","id":"fc_b","call_id":"call_b","namespace":"agents","name":"spawn","arguments":"{\"task\":\"b\"}","status":"completed"},
			{"type":"function_call_output","call_id":"call_b","output":[{"type":"input_text","text":"spawned"}]},
			{"type":"function_call_output","call_id":"call_a","output":"found"},
			{"type":"message","role":"user","content":"Continue."}
		],
		"tools":[
			{"type":"function","name":"lookup","description":"Look up","parameters":{"type":"object"},"strict":false},
			{"type":"namespace","name":"agents","description":"Agent tools","tools":[
				{"type":"function","name":"spawn","description":"Spawn","parameters":{"type":"object"},"strict":false,"defer_loading":false}
			]}
		],
		"tool_choice":{"type":"function","namespace":"agents","name":"spawn"},
		"parallel_tool_calls":true,
		"store":false
	}`)

	got, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatalf("translatePolicyResponsesRequestToChat() error = %v", err)
	}
	var chat struct {
		Messages   []map[string]json.RawMessage `json:"messages"`
		Tools      []map[string]json.RawMessage `json:"tools"`
		ToolChoice map[string]json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(got.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 6 {
		t.Fatalf("messages len = %d, want 6: %s", len(chat.Messages), got.Body)
	}
	var grouped struct {
		Role      string `json:"role"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	groupedBody, _ := json.Marshal(chat.Messages[2])
	if err := json.Unmarshal(groupedBody, &grouped); err != nil {
		t.Fatal(err)
	}
	if grouped.Role != "assistant" || len(grouped.ToolCalls) != 2 {
		t.Fatalf("grouped assistant message = %#v", grouped)
	}
	if grouped.ToolCalls[0].ID != "call_a" || grouped.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("first grouped call = %#v", grouped.ToolCalls[0])
	}
	spawnAlias := grouped.ToolCalls[1].Function.Name
	if descriptor := got.Tools[spawnAlias]; descriptor != (policyResponsesToolDescriptor{Name: "spawn", Namespace: "agents", Kind: "function"}) {
		t.Fatalf("spawn descriptor = %#v for alias %q", descriptor, spawnAlias)
	}
	if len(got.CallableTools) != 1 || got.CallableTools[spawnAlias] != (policyResponsesToolDescriptor{Name: "spawn", Namespace: "agents", Kind: "function"}) {
		t.Fatalf("callable tools = %#v, want only forced spawn", got.CallableTools)
	}
	var spawnDescription string
	for _, rawTool := range chat.Tools {
		var tool struct {
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"function"`
		}
		encoded, _ := json.Marshal(rawTool)
		_ = json.Unmarshal(encoded, &tool)
		if tool.Function.Name == spawnAlias {
			spawnDescription = tool.Function.Description
		}
	}
	if spawnDescription != "Agent tools\n\nSpawn" {
		t.Fatalf("spawn description = %q", spawnDescription)
	}
	for messageIndex, callID := range map[int]string{3: "call_b", 4: "call_a"} {
		var toolMessage struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		}
		encoded, _ := json.Marshal(chat.Messages[messageIndex])
		_ = json.Unmarshal(encoded, &toolMessage)
		if toolMessage.Role != "tool" || toolMessage.ToolCallID != callID {
			t.Fatalf("tool message %d = %#v", messageIndex, toolMessage)
		}
	}
	var choice struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	choiceBody, _ := json.Marshal(chat.ToolChoice)
	_ = json.Unmarshal(choiceBody, &choice)
	if choice.Type != "function" || choice.Function.Name != spawnAlias {
		t.Fatalf("tool_choice = %#v, spawn alias = %q", choice, spawnAlias)
	}
}

func TestTranslatePolicyResponsesRequestToChatAliasesAreDeterministicAndBounded(t *testing.T) {
	longName := "this.tool-name-is-deliberately-long-to-force-a-bounded-chat-compatible-function-alias-with-a-hash"
	toolsA := `[
		{"type":"function","name":"ns__run","parameters":{"type":"object"}},
		{"type":"namespace","name":"ns","description":"N","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]},
		{"type":"function","name":"alpha.beta","parameters":{"type":"object"}},
		{"type":"function","name":"alpha_beta","parameters":{"type":"object"}},
		{"type":"function","name":"` + longName + `","parameters":{"type":"object"}}
	]`
	toolsB := `[
		{"type":"function","name":"` + longName + `","parameters":{"type":"object"}},
		{"type":"function","name":"alpha_beta","parameters":{"type":"object"}},
		{"type":"function","name":"alpha.beta","parameters":{"type":"object"}},
		{"type":"namespace","name":"ns","description":"N","tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]},
		{"type":"function","name":"ns__run","parameters":{"type":"object"}}
	]`
	translate := func(t *testing.T, tools string) policyResponsesChatRequest {
		t.Helper()
		body := []byte(`{"model":"policy","input":"hi","store":false,"tools":` + tools + `}`)
		got, err := translatePolicyResponsesRequestToChat(body)
		if err != nil {
			t.Fatalf("translate error = %v", err)
		}
		return got
	}
	first := translate(t, toolsA)
	second := translate(t, toolsB)
	firstByDescriptor := aliasesByPolicyDescriptor(first.Tools)
	secondByDescriptor := aliasesByPolicyDescriptor(second.Tools)
	if !equalStringMaps(firstByDescriptor, secondByDescriptor) {
		t.Fatalf("aliases changed with declaration order\nfirst: %#v\nsecond:%#v", firstByDescriptor, secondByDescriptor)
	}
	seen := make(map[string]struct{})
	for alias, descriptor := range first.Tools {
		if len(alias) == 0 || len(alias) > 64 {
			t.Fatalf("alias length = %d for %q", len(alias), alias)
		}
		for _, r := range alias {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
				t.Fatalf("alias %q contains non-Chat-safe rune %q", alias, r)
			}
		}
		if _, duplicate := seen[alias]; duplicate {
			t.Fatalf("duplicate alias %q", alias)
		}
		seen[alias] = struct{}{}
		if descriptor.Kind != "function" {
			t.Fatalf("descriptor kind = %q", descriptor.Kind)
		}
	}
	if got := firstByDescriptor["function\x00\x00ns__run"]; got != "ns__run" {
		t.Fatalf("safe top-level descriptor alias = %q, want ns__run", got)
	}
	if got := firstByDescriptor["function\x00ns\x00run"]; got == "ns__run" || !strings.HasPrefix(got, policyResponsesAliasPrefix) {
		t.Fatalf("namespace descriptor alias = %q, want stable generated alias", got)
	}
	if got := firstByDescriptor["function\x00\x00"+longName]; len(got) > 64 || got == longName {
		t.Fatalf("long alias = %q", got)
	}
}

func TestPolicyResponsesToolAliasesAreStableAcrossCatalogChanges(t *testing.T) {
	namespaced := policyResponsesToolDescriptor{Name: "run", Namespace: "ns", Kind: policyResponsesToolKindFunction}
	topLevelCollision := policyResponsesToolDescriptor{Name: "ns__run", Kind: policyResponsesToolKindFunction}
	unrelated := policyResponsesToolDescriptor{Name: "exec_command", Kind: policyResponsesToolKindFunction}
	reserved := policyResponsesToolDescriptor{Name: policyResponsesAliasPrefix + "user", Kind: policyResponsesToolKindFunction}

	aliasesAlone, reverseAlone, err := buildPolicyResponsesToolAliases(map[policyResponsesToolDescriptor]struct{}{namespaced: {}})
	if err != nil {
		t.Fatal(err)
	}
	aliasesExpanded, reverseExpanded, err := buildPolicyResponsesToolAliases(map[policyResponsesToolDescriptor]struct{}{
		namespaced: {}, topLevelCollision: {}, unrelated: {}, reserved: {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aliasesAlone[namespaced] != aliasesExpanded[namespaced] {
		t.Fatalf("namespaced alias changed with catalog: alone=%q expanded=%q", aliasesAlone[namespaced], aliasesExpanded[namespaced])
	}
	if aliasesExpanded[topLevelCollision] != topLevelCollision.Name || aliasesExpanded[unrelated] != unrelated.Name {
		t.Fatalf("safe top-level aliases changed: %#v", aliasesExpanded)
	}
	for descriptor, alias := range aliasesExpanded {
		if len(alias) == 0 || len(alias) > policyResponsesMaxChatToolNameLen {
			t.Fatalf("alias length=%d descriptor=%+v alias=%q", len(alias), descriptor, alias)
		}
		if reverseExpanded[alias] != descriptor {
			t.Fatalf("reverse alias mismatch for %q: got=%+v want=%+v", alias, reverseExpanded[alias], descriptor)
		}
	}
	if reverseAlone[aliasesAlone[namespaced]] != namespaced {
		t.Fatalf("standalone reverse alias mismatch: %+v", reverseAlone)
	}
	if alias := aliasesExpanded[reserved]; alias == reserved.Name || !strings.HasPrefix(alias, policyResponsesAliasPrefix) {
		t.Fatalf("reserved-prefix alias=%q", alias)
	}
}

func aliasesByPolicyDescriptor(tools policyResponsesToolMap) map[string]string {
	result := make(map[string]string, len(tools))
	for alias, descriptor := range tools {
		result[descriptor.Kind+"\x00"+descriptor.Namespace+"\x00"+descriptor.Name] = alias
	}
	return result
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func TestTranslatePolicyResponsesRequestToChatRejectsUnsupportedOrAmbiguousRequests(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantParam string
		wantText  string
	}{
		{name: "duplicate top-level", body: `{"model":"policy","model":"other","input":"hi"}`, wantParam: "model", wantText: "duplicate"},
		{name: "duplicate nested schema", body: `{"model":"policy","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object","type":"array"}}]}`, wantParam: "tools[0].parameters.type", wantText: "duplicate"},
		{name: "unknown top-level", body: `{"model":"policy","input":"hi","service_tier":"flex"}`, wantParam: "service_tier", wantText: "unsupported"},
		{name: "missing model", body: `{"input":"hi"}`, wantParam: "model", wantText: "required"},
		{name: "null input", body: `{"model":"policy","input":null}`, wantParam: "input", wantText: "required"},
		{name: "null stream", body: `{"model":"policy","input":"hi","stream":null}`, wantParam: "stream", wantText: "boolean"},
		{name: "null include", body: `{"model":"policy","input":"hi","include":null}`, wantParam: "include", wantText: "array"},
		{name: "store true", body: `{"model":"policy","input":"hi","store":true}`, wantParam: "store", wantText: "false or omitted"},
		{name: "store null", body: `{"model":"policy","input":"hi","store":null}`, wantParam: "store", wantText: "false or omitted"},
		{name: "previous response id", body: `{"model":"policy","input":"hi","previous_response_id":null}`, wantParam: "previous_response_id", wantText: "not supported"},
		{name: "hosted web search", body: `{"model":"policy","input":"hi","tools":[{"type":"web_search"}]}`, wantParam: "tools[0].type", wantText: "hosted or custom"},
		{name: "custom tool", body: `{"model":"policy","input":"hi","tools":[{"type":"custom","name":"shell"}]}`, wantParam: "tools[0].type", wantText: "hosted or custom"},
		{name: "tool search", body: `{"model":"policy","input":"hi","tools":[{"type":"tool_search","execution":"client","description":"Search tools","parameters":{"type":"object"}}]}`, wantParam: "tools[0].type", wantText: "hosted or custom"},
		{name: "deferred function", body: `{"model":"policy","input":"hi","tools":[{"type":"function","name":"later","parameters":{"type":"object"},"defer_loading":true}]}`, wantParam: "tools[0].defer_loading", wantText: "not supported"},
		{name: "image input item", body: `{"model":"policy","input":[{"type":"input_image","image_url":"https://example.test/x.png"}]}`, wantParam: "input[0].type", wantText: "image"},
		{name: "image content part", body: `{"model":"policy","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.test/x.png"}]}]}`, wantParam: "input[0].content[0].type", wantText: "image"},
		{name: "unknown item", body: `{"model":"policy","input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, wantParam: "input[0].type", wantText: "not supported"},
		{name: "unknown function field", body: `{"model":"policy","input":"hi","tools":[{"type":"function","name":"f","parameters":{},"extra":true}]}`, wantParam: "tools[0].extra", wantText: "unsupported"},
		{name: "unknown reasoning field", body: `{"model":"policy","input":"hi","reasoning":{"effort":"high","context":"large"}}`, wantParam: "reasoning.context", wantText: "unsupported"},
		{name: "invalid reasoning summary type", body: `{"model":"policy","input":"hi","reasoning":{"summary":true}}`, wantParam: "reasoning.summary", wantText: "auto, concise, or detailed"},
		{name: "invalid reasoning summary value", body: `{"model":"policy","input":"hi","reasoning":{"summary":"verbose"}}`, wantParam: "reasoning.summary", wantText: "auto, concise, or detailed"},
		{name: "unknown text field", body: `{"model":"policy","input":"hi","text":{"verbosity":"high"}}`, wantParam: "text.verbosity", wantText: "unsupported"},
		{name: "unknown text format field", body: `{"model":"policy","input":"hi","text":{"format":{"type":"json_object","extra":true}}}`, wantParam: "text.format.extra", wantText: "unsupported"},
		{name: "unsupported text format", body: `{"model":"policy","input":"hi","text":{"format":{"type":"regex"}}}`, wantParam: "text.format.type", wantText: "unsupported"},
		{name: "missing schema name", body: `{"model":"policy","input":"hi","text":{"format":{"type":"json_schema","schema":{}}}}`, wantParam: "text.format.name", wantText: "required"},
		{name: "null schema", body: `{"model":"policy","input":"hi","text":{"format":{"type":"json_schema","name":"result","schema":null}}}`, wantParam: "text.format.schema", wantText: "object"},
		{name: "invalid include", body: `{"model":"policy","input":"hi","include":["response.output_text.logprobs"]}`, wantParam: "include[0]", wantText: "reasoning.encrypted_content"},
		{name: "invalid metadata value", body: `{"model":"policy","input":"hi","client_metadata":{"attempt":1}}`, wantParam: "client_metadata.attempt", wantText: "bounded strings"},
		{name: "zero max output", body: `{"model":"policy","input":"hi","max_output_tokens":0}`, wantParam: "max_output_tokens", wantText: "positive integer"},
		{name: "temperature out of range", body: `{"model":"policy","input":"hi","temperature":2.1}`, wantParam: "temperature", wantText: "[0, 2]"},
		{name: "top p out of range", body: `{"model":"policy","input":"hi","top_p":1.1}`, wantParam: "top_p", wantText: "[0, 1]"},
		{name: "unknown call output", body: `{"model":"policy","input":[{"type":"function_call_output","call_id":"missing","output":"x"}]}`, wantParam: "input[0].call_id", wantText: "unknown prior call"},
		{name: "missing call output", body: `{"model":"policy","input":[{"type":"function_call","call_id":"call","name":"f","arguments":"{}"}]}`, wantParam: "input[0].call_id", wantText: "missing"},
		{name: "null call arguments", body: `{"model":"policy","input":[{"type":"function_call","call_id":"call","name":"f","arguments":null},{"type":"function_call_output","call_id":"call","output":"x"}]}`, wantParam: "input[0].arguments", wantText: "string"},
		{name: "empty call namespace", body: `{"model":"policy","input":[{"type":"function_call","call_id":"call","namespace":"","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call","output":"x"}]}`, wantParam: "input[0].namespace", wantText: "non-empty"},
		{name: "interleaved call output", body: `{"model":"policy","input":[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"type":"message","role":"user","content":"too early"},{"type":"function_call_output","call_id":"a","output":"x"}]}`, wantParam: "input[0].call_id", wantText: "missing"},
		{name: "forced undeclared tool", body: `{"model":"policy","input":"hi","tool_choice":{"type":"function","name":"missing"}}`, wantParam: "tool_choice", wantText: "declared"},
		{name: "required without tools", body: `{"model":"policy","input":"hi","tool_choice":"required"}`, wantParam: "tool_choice", wantText: "non-empty tools"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translatePolicyResponsesRequestToChat([]byte(tt.body))
			if err == nil {
				t.Fatal("expected error")
			}
			executionErr, ok := err.(*chatExecutionError)
			if !ok {
				t.Fatalf("error type = %T, want *chatExecutionError: %v", err, err)
			}
			if executionErr.Param != tt.wantParam {
				t.Fatalf("Param = %q, want %q (error: %v)", executionErr.Param, tt.wantParam, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantText)
			}
		})
	}
}

func TestTranslatePolicyResponsesRequestToChatAcceptsCodexScaleNamespaceCatalog(t *testing.T) {
	const (
		topLevelFunctions = 9
		namespaceCount    = 12
		totalFunctions    = 292
	)
	tools := make([]any, 0, topLevelFunctions+namespaceCount)
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "string"},
		},
	}
	longDescription := strings.Repeat("Codex tool description for policy ingress. ", 28)
	for index := 0; index < topLevelFunctions; index++ {
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        fmt.Sprintf("top_level_%02d", index),
			"description": longDescription,
			"strict":      false,
			"parameters":  parameters,
		})
	}
	remaining := totalFunctions - topLevelFunctions
	for namespaceIndex := 0; namespaceIndex < namespaceCount; namespaceIndex++ {
		childCount := remaining / (namespaceCount - namespaceIndex)
		remaining -= childCount
		children := make([]any, 0, childCount)
		for childIndex := 0; childIndex < childCount; childIndex++ {
			children = append(children, map[string]any{
				"type":          "function",
				"name":          fmt.Sprintf("tool_%03d", childIndex),
				"description":   longDescription,
				"strict":        false,
				"defer_loading": false,
				"parameters":    parameters,
			})
		}
		tools = append(tools, map[string]any{
			"type":        "namespace",
			"name":        fmt.Sprintf("mcp__codex_apps__namespace_%02d", namespaceIndex),
			"description": "Codex application namespace",
			"tools":       children,
		})
	}
	requestBody, err := json.Marshal(map[string]any{
		"model":               "gpt-5.6-semantic",
		"instructions":        "Act as Codex.",
		"input":               []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Inspect the repository."}}}},
		"tools":               tools,
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": "high"},
		"store":               false,
		"stream":              true,
		"include":             []any{},
		"prompt_cache_key":    "codex-session",
		"client_metadata":     map[string]any{"originator": "codex_cli_rs", "version": "0.144.6"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requestBody) <= 300<<10 || len(requestBody) >= policyResponsesMaxRequestBytes {
		t.Fatalf("generated request size = %d, want a substantial request below %d", len(requestBody), policyResponsesMaxRequestBytes)
	}

	translated, err := translatePolicyResponsesRequestToChat(requestBody)
	if err != nil {
		t.Fatalf("translate Codex-scale request: %v", err)
	}
	if len(translated.Tools) != totalFunctions {
		t.Fatalf("reverse tool map len = %d, want %d", len(translated.Tools), totalFunctions)
	}
	var chat struct {
		Tools []models.OpenAITool `json:"tools"`
	}
	if err := json.Unmarshal(translated.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != totalFunctions {
		t.Fatalf("canonical Chat tools len = %d, want %d", len(chat.Tools), totalFunctions)
	}
	facts, err := buildPolicyClassifierFacts(translated.Body, policyFactOptions{})
	if err != nil {
		t.Fatalf("build classifier facts: %v", err)
	}
	if facts.Counts.FunctionTools != totalFunctions || facts.Counts.IncludedFunctionTools != policyFactMaxTools || !facts.Truncation.FunctionTools {
		t.Fatalf("classifier tool bounding = counts %#v, truncation %#v", facts.Counts, facts.Truncation)
	}
}

func TestTranslatePolicyResponsesRequestToChatEnforcesIngressBounds(t *testing.T) {
	t.Run("request bytes", func(t *testing.T) {
		body := []byte(`{"model":"policy","input":"` + strings.Repeat("x", policyResponsesMaxRequestBytes) + `"}`)
		_, err := translatePolicyResponsesRequestToChat(body)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v, want request-size rejection", err)
		}
	})

	t.Run("flattened tools", func(t *testing.T) {
		children := make([]any, 0, policyResponsesMaxFunctionTools+1)
		for index := 0; index <= policyResponsesMaxFunctionTools; index++ {
			children = append(children, map[string]any{
				"type":       "function",
				"name":       fmt.Sprintf("f_%04d", index),
				"parameters": map[string]any{"type": "object"},
			})
		}
		body, err := json.Marshal(map[string]any{
			"model": "policy",
			"input": "hi",
			"tools": []any{map[string]any{"type": "namespace", "name": "bulk", "tools": children}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) >= policyResponsesMaxRequestBytes {
			t.Fatalf("tool-bound request unexpectedly exceeded byte bound: %d", len(body))
		}
		_, err = translatePolicyResponsesRequestToChat(body)
		if err == nil || !strings.Contains(err.Error(), "at most 1024") {
			t.Fatalf("error = %v, want flattened-tool rejection", err)
		}
	})
}

func TestTranslatePolicyResponsesRequestToChatCarriesBoundedReasoningEffortForContractValidation(t *testing.T) {
	translated, err := translatePolicyResponsesRequestToChat([]byte(`{"model":"policy","input":"hi","reasoning":{"effort":"max"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(translated.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %#v", chat["reasoning_effort"])
	}
}
