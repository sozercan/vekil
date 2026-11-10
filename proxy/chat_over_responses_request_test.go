package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestTranslateChatRequestToResponsesBasicText(t *testing.T) {
	input := []byte(`{
		"model":"gpt-public",
		"messages":[
			{"role":"system","content":"follow instructions"},
			{"role":"user","content":"hello"}
		],
		"max_tokens":128,
		"temperature":0.2,
		"top_p":0.9,
		"stream":false
	}`)

	plan, err := translateChatRequestToResponses(input, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream",
	})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	if plan.Stream {
		t.Fatal("plan.Stream = true, want false")
	}
	if plan.IncludeUsage {
		t.Fatal("plan.IncludeUsage = true, want false")
	}

	var got map[string]any
	if err := json.Unmarshal(plan.Body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	wantJSON := []byte(`{
		"model":"gpt-upstream",
		"input":[
			{"type":"message","role":"system","content":[{"type":"input_text","text":"follow instructions"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		],
		"max_output_tokens":128,
		"temperature":0.2,
		"top_p":0.9,
		"stream":false,
		"store":false,
		"include":["reasoning.encrypted_content"]
	}`)
	var want map[string]any
	if err := json.Unmarshal(wantJSON, &want); err != nil {
		t.Fatal(err)
	}
	if !jsonDeepEqual(got, want) {
		gotBody, _ := json.MarshalIndent(got, "", "  ")
		wantBody, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("translated body mismatch\ngot: %s\nwant: %s", gotBody, wantBody)
	}
}

func jsonDeepEqual(a, b any) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}

func TestTranslateChatRequestToResponsesFunctionTools(t *testing.T) {
	input := []byte(`{
		"model":"gpt-public",
		"messages":[{"role":"user","content":"use the tool"}],
		"stream":true,
		"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup a value","parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false},"strict":true}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"parallel_tool_calls":true
	}`)

	plan, err := translateChatRequestToResponses(input, responsesChatRequestOptions{UpstreamModel: "gpt-upstream"})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	if !plan.Stream || !plan.IncludeUsage {
		t.Fatalf("stream/include_usage = %v/%v, want true/true", plan.Stream, plan.IncludeUsage)
	}
	var got map[string]any
	if err := json.Unmarshal(plan.Body, &got); err != nil {
		t.Fatal(err)
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", got["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "lookup" || tool["description"] != "Lookup a value" || tool["strict"] != true {
		t.Fatalf("flattened tool = %#v", tool)
	}
	if _, ok := tool["function"]; ok {
		t.Fatalf("flattened tool retained nested function: %#v", tool)
	}
	choice := got["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice = %#v", choice)
	}
	if got["parallel_tool_calls"] != true {
		t.Fatalf("parallel_tool_calls = %#v", got["parallel_tool_calls"])
	}
	if _, ok := got["stream_options"]; ok {
		t.Fatalf("stream_options leaked upstream: %#v", got["stream_options"])
	}
}

func TestTranslateChatRequestToResponsesPreservesContentPartOrder(t *testing.T) {
	input := []byte(`{
		"model":"gpt-public",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"before"},
				{"type":"image_url","image_url":{"url":"https://example.test/image.png","detail":"high"}},
				{"type":"text","text":"after"}
			]},
			{"role":"assistant","content":[{"type":"text","text":"prior answer"}]}
		]
	}`)

	plan, err := translateChatRequestToResponses(input, responsesChatRequestOptions{})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(plan.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 2 {
		t.Fatalf("input len = %d", len(body.Input))
	}
	var user struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
			Detail   string `json:"detail"`
		} `json:"content"`
	}
	_ = json.Unmarshal(body.Input[0], &user)
	parts := user.Content
	if len(parts) != 3 || parts[0].Type != "input_text" || parts[0].Text != "before" || parts[1].Type != "input_image" || parts[1].ImageURL != "https://example.test/image.png" || parts[1].Detail != "high" || parts[2].Type != "input_text" || parts[2].Text != "after" {
		t.Fatalf("user parts = %#v", parts)
	}
	var assistant map[string]any
	_ = json.Unmarshal(body.Input[1], &assistant)
	if assistant["role"] != "assistant" || assistant["content"] != "prior answer" || assistant["type"] != nil {
		t.Fatalf("assistant item = %#v", assistant)
	}
}

func TestTranslateChatRequestToResponsesMapsResponsesConfiguration(t *testing.T) {
	input := []byte(`{
		"model":"gpt-public",
		"messages":[{"role":"user","content":"return json"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"result","description":"result shape","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false},"strict":true}},
		"reasoning_effort":"high",
		"verbosity":"low",
		"metadata":{"source":"phase0"},
		"store":true,
		"user":"user-1",
		"prompt_cache_key":"cache-1",
		"safety_identifier":"safety-1"
	}`)

	plan, err := translateChatRequestToResponses(input, responsesChatRequestOptions{})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(plan.Body, &got); err != nil {
		t.Fatal(err)
	}
	text := got["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "result" || format["strict"] != true || text["verbosity"] != "low" {
		t.Fatalf("text configuration = %#v", text)
	}
	reasoning := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if got["store"] != true || got["user"] != "user-1" || got["prompt_cache_key"] != "cache-1" || got["safety_identifier"] != "safety-1" {
		t.Fatalf("mapped direct fields = %#v", got)
	}
	include, ok := got["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", got["include"])
	}
	metadata := got["metadata"].(map[string]any)
	if metadata["source"] != "phase0" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestTranslateChatRequestToResponsesRejectsUnsupportedFields(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		param string
	}{
		{name: "service tier", field: "service_tier", value: `"auto"`, param: "service_tier"},
		{name: "stop string", field: "stop", value: `"END"`, param: "stop"},
		{name: "multiple choices", field: "n", value: `2`, param: "n"},
		{name: "presence penalty", field: "presence_penalty", value: `0.1`, param: "presence_penalty"},
		{name: "frequency penalty", field: "frequency_penalty", value: `0.1`, param: "frequency_penalty"},
		{name: "seed", field: "seed", value: `1`, param: "seed"},
		{name: "logit bias", field: "logit_bias", value: `{}`, param: "logit_bias"},
		{name: "logprobs", field: "logprobs", value: `true`, param: "logprobs"},
		{name: "top logprobs", field: "top_logprobs", value: `1`, param: "top_logprobs"},
		{name: "audio", field: "audio", value: `{}`, param: "audio"},
		{name: "modalities", field: "modalities", value: `["text"]`, param: "modalities"},
		{name: "prediction", field: "prediction", value: `{}`, param: "prediction"},
		{name: "legacy functions", field: "functions", value: `[]`, param: "functions"},
		{name: "unknown", field: "vendor_extension", value: `true`, param: "vendor_extension"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hello"}],"` + tt.field + `":` + tt.value + `}`)
			_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
			var execErr *chatExecutionError
			if !errors.As(err, &execErr) {
				t.Fatalf("error = %v, want chatExecutionError", err)
			}
			if execErr.StatusCode != 400 || execErr.Type != "invalid_request_error" || execErr.Param != tt.param {
				t.Fatalf("error = %+v", execErr)
			}
		})
	}
}

func TestTranslateChatRequestToResponsesAcceptsEmptyStopAndSingleChoice(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt","messages":[{"role":"user","content":"hello"}],"stop":null,"n":1}`,
		`{"model":"gpt","messages":[{"role":"user","content":"hello"}],"stop":""}`,
		`{"model":"gpt","messages":[{"role":"user","content":"hello"}],"stop":[]}`,
	} {
		if _, err := translateChatRequestToResponses([]byte(body), responsesChatRequestOptions{}); err != nil {
			t.Fatalf("translate empty stop body error = %v", err)
		}
	}
}

func TestTranslateChatRequestToResponsesMapsSyntheticToolHistory(t *testing.T) {
	input := []byte(`{
		"model":"gpt-public",
		"messages":[
			{"role":"user","content":"look it up"},
			{"role":"assistant","content":"checking","tool_calls":[{"id":"call_external_1","type":"function","function":{"name":"lookup","arguments":"{\"key\":\"alpha\"}"}}]},
			{"role":"tool","tool_call_id":"call_external_1","content":{"value":1}}
		]
	}`)

	plan, err := translateChatRequestToResponses(input, responsesChatRequestOptions{})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(plan.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 4 {
		t.Fatalf("input len = %d, want 4: %#v", len(body.Input), body.Input)
	}
	if body.Input[1]["type"] != nil || body.Input[1]["role"] != "assistant" || body.Input[1]["content"] != "checking" {
		t.Fatalf("assistant item = %#v", body.Input[1])
	}
	call := body.Input[2]
	if call["type"] != "function_call" || call["call_id"] != "call_external_1" || call["name"] != "lookup" || call["arguments"] != `{"key":"alpha"}` {
		t.Fatalf("function call = %#v", call)
	}
	output := body.Input[3]
	if output["type"] != "function_call_output" || output["call_id"] != "call_external_1" || output["output"] != `{"value":1}` {
		t.Fatalf("function output = %#v", output)
	}
}

func TestTranslateChatRequestToResponsesValidatesHistoryAndLimits(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
		code  string
	}{
		{
			name:  "duplicate top-level field",
			body:  `{"model":"a","model":"b","messages":[{"role":"user","content":"hello"}]}`,
			param: "model",
		},
		{
			name:  "conflicting token limits",
			body:  `{"model":"a","messages":[{"role":"user","content":"hello"}],"max_tokens":1,"max_completion_tokens":2}`,
			param: "max_completion_tokens",
		},
		{
			name:  "orphan tool result",
			body:  `{"model":"a","messages":[{"role":"tool","tool_call_id":"call_external","content":"result"}]}`,
			param: "messages[0].tool_call_id",
		},
		{
			name:  "duplicate tool result",
			body:  `{"model":"a","messages":[{"role":"assistant","tool_calls":[{"id":"call_external","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_external","content":"one"},{"role":"tool","tool_call_id":"call_external","content":"two"}]}`,
			param: "messages[2].tool_call_id",
		},
		{
			name:  "tagged replay state missing",
			body:  `{"model":"a","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"result"}]}`,
			param: "messages",
			code:  "responses_replay_state_missing",
		},
		{
			name:  "assistant image rejected",
			body:  `{"model":"a","messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]}`,
			param: "messages[0].content[0]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translateChatRequestToResponses([]byte(tt.body), responsesChatRequestOptions{})
			var execErr *chatExecutionError
			if !errors.As(err, &execErr) {
				t.Fatalf("error = %v, want chatExecutionError", err)
			}
			if execErr.Param != tt.param || execErr.Code != tt.code {
				t.Fatalf("error = %+v, want param=%q code=%q", execErr, tt.param, tt.code)
			}
		})
	}
}

func TestTranslateChatRequestToResponsesRestoresReplayGroups(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_parallel_tool_calls.json")
	if err != nil {
		t.Fatal(err)
	}
	store := newResponsesChatReplayStore()
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	first, err := translateResponsesJSONToChat(fixture, responsesChatResponseOptions{PublicModel: "gpt-public", ReplayStore: store, ReplayRoute: route})
	if err != nil {
		t.Fatal(err)
	}
	assistant := first.Response.Choices[0].Message
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("tool calls = %#v", assistant.ToolCalls)
	}

	t.Run("full reordered results restore exact group once", func(t *testing.T) {
		request := map[string]any{
			"model": "gpt-public",
			"messages": []any{
				map[string]any{"role": "assistant", "content": json.RawMessage("null"), "tool_calls": assistant.ToolCalls},
				map[string]any{"role": "tool", "tool_call_id": assistant.ToolCalls[1].ID, "content": "result beta"},
				map[string]any{"role": "tool", "tool_call_id": assistant.ToolCalls[0].ID, "content": "result alpha"},
			},
		}
		body, _ := json.Marshal(request)
		plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route})
		if err != nil {
			t.Fatal(err)
		}
		var upstream struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal(plan.Body, &upstream); err != nil {
			t.Fatal(err)
		}
		if len(upstream.Input) != 4 || upstream.Input[0]["type"] != "function_call" || upstream.Input[1]["type"] != "function_call" || upstream.Input[2]["call_id"] != "call_synth_parallel_beta_001" || upstream.Input[3]["call_id"] != "call_synth_parallel_alpha_001" {
			t.Fatalf("upstream input = %#v", upstream.Input)
		}
	})

	t.Run("partial result replays only matching call", func(t *testing.T) {
		request := map[string]any{
			"model": "gpt-public",
			"messages": []any{
				map[string]any{"role": "assistant", "content": json.RawMessage("null"), "tool_calls": assistant.ToolCalls},
				map[string]any{"role": "tool", "tool_call_id": assistant.ToolCalls[1].ID, "content": "result beta"},
			},
		}
		body, _ := json.Marshal(request)
		plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route})
		if err != nil {
			t.Fatal(err)
		}
		var upstream struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal(plan.Body, &upstream); err != nil {
			t.Fatal(err)
		}
		if len(upstream.Input) != 2 || upstream.Input[0]["type"] != "function_call" || upstream.Input[0]["call_id"] != "call_synth_parallel_beta_001" || upstream.Input[1]["type"] != "function_call_output" || upstream.Input[1]["call_id"] != "call_synth_parallel_beta_001" {
			t.Fatalf("partial upstream input = %#v", upstream.Input)
		}
	})
}

func FuzzTranslateChatRequestToResponses(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"model":"gpt","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[]}`),
		[]byte(`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_x","content":"ok"}]}`),
		[]byte(`{`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
		if err == nil && !json.Valid(plan.Body) {
			t.Fatalf("successful translation returned invalid JSON: %q", plan.Body)
		}
	})
}

func TestTranslateChatRequestToResponsesRejectsUnknownNestedFields(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{"message", `{"model":"gpt","messages":[{"role":"user","content":"hello","unexpected":true}]}`, "messages[0].unexpected"},
		{"content part", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"text","text":"hello","unexpected":true}]}]}`, "messages[0].content[0].unexpected"},
		{"tool function", `{"model":"gpt","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"},"unexpected":true}}]}`, "tools[0].function.unexpected"},
		{"assistant tool call", `{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}","unexpected":true}}]},{"role":"tool","tool_call_id":"call_x","content":"ok"}]}`, "messages[0].tool_calls[0].function.unexpected"},
		{"response format", `{"model":"gpt","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_object","unexpected":true}}`, "response_format.unexpected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translateChatRequestToResponses([]byte(tt.body), responsesChatRequestOptions{})
			var execErr *chatExecutionError
			if !errors.As(err, &execErr) || execErr.Param != tt.param {
				t.Fatalf("error = %#v, want param %q", err, tt.param)
			}
		})
	}
}

func TestTranslateChatRequestToResponsesFlattensToolMessageTextParts(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_x","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]}`)
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var upstream struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(plan.Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if got := upstream.Input[len(upstream.Input)-1]["output"]; got != "firstsecond" {
		t.Fatalf("output = %#v", got)
	}
}

func TestTranslateChatRequestToResponsesRejectsCapsBelowResponsesMinimum(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		for _, limit := range []int{0, 1, 15} {
			t.Run(fmt.Sprintf("%s_%d", field, limit), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt","messages":[{"role":"user","content":"hello"}],%q:%d}`, field, limit))
				_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.StatusCode != 400 || executionErr.Param != field {
					t.Fatalf("error = %#v", err)
				}
			})
		}
	}
	plan, err := translateChatRequestToResponses([]byte(`{"model":"gpt","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`), responsesChatRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	_ = json.Unmarshal(plan.Body, &request)
	if request["max_output_tokens"] != float64(16) {
		t.Fatalf("max_output_tokens = %#v", request["max_output_tokens"])
	}
}

func TestTranslateChatRequestToResponsesPreservesOpaqueFunctionArguments(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{not-json"}}]},{"role":"tool","tool_call_id":"call_x","content":"ok"}]}`)
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(plan.Body, &request)
	if request.Input[0]["arguments"] != "{not-json" {
		t.Fatalf("arguments = %#v", request.Input[0]["arguments"])
	}
}

func TestTranslateChatRequestToResponsesAcceptsAssistantRefusalRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"assistant","content":"","refusal":"Synthetic refusal."},{"role":"user","content":"continue"}]}`)
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(plan.Body, &request)
	if request.Input[0]["role"] != "assistant" || request.Input[0]["content"] != "Synthetic refusal." {
		t.Fatalf("assistant history = %#v", request.Input[0])
	}
}

func TestTranslateChatRequestToResponsesRejectsContentPartDiscriminatorConflicts(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{"text with image", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"text","text":"hello","image_url":{"url":"https://example.test/image.png"}}]}]}`, "messages[0].content[0].image_url"},
		{"image with text", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"image_url","text":"hidden","image_url":{"url":"https://example.test/image.png"}}]}]}`, "messages[0].content[0].text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translateChatRequestToResponses([]byte(tt.body), responsesChatRequestOptions{})
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) || executionErr.Param != tt.param {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestTranslateChatRequestToResponsesProjectsPartialSyntheticHistory(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"b","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_b","content":"result"}]}`)
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(plan.Body, &request)
	if len(request.Input) != 2 || request.Input[0]["type"] != "function_call" || request.Input[0]["call_id"] != "call_b" || request.Input[1]["call_id"] != "call_b" {
		t.Fatalf("input = %#v", request.Input)
	}
}

func TestTranslateChatRequestToResponsesRejectsNullArgumentsAndToolRefusal(t *testing.T) {
	tests := []struct{ body, param string }{
		{`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":null}}]},{"role":"tool","tool_call_id":"call_x","content":"ok"}]}`, "messages[0].tool_calls[0].function.arguments"},
		{`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_x","content":"ok","refusal":"not-valid"}]}`, "messages[1].refusal"},
	}
	for _, tt := range tests {
		_, err := translateChatRequestToResponses([]byte(tt.body), responsesChatRequestOptions{})
		var executionErr *chatExecutionError
		if !errors.As(err, &executionErr) || executionErr.Param != tt.param {
			t.Fatalf("error = %#v, want %q", err, tt.param)
		}
	}
}

func TestTranslateChatRequestToResponsesRejectsRefusalOnReplayToolMessage(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"assistant","content":"","refusal":"tampered","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`)
	_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{ReplayStore: newResponsesChatReplayStore()})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Param != "messages[0].refusal" {
		t.Fatalf("error = %#v", err)
	}
}

func TestTranslateChatRequestToResponsesRejectsNullJSONSchema(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":null}}}`)
	_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Param != "response_format.json_schema.schema" {
		t.Fatalf("error = %#v", err)
	}
}

func TestTranslateChatRequestToResponsesRestoresReplayBackedCustomToolCall(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "policy", UpstreamModel: "deployment-a"}
	patch := "*** Begin Patch\n*** Update File: calc.go\n*** End Patch"
	functionArguments, _ := json.Marshal(map[string]string{"input": patch})
	outputItem, _ := json.Marshal(map[string]any{
		"type": "function_call", "id": "item-apply", "call_id": "upstream-apply", "name": "apply_patch", "arguments": string(functionArguments),
	})
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route: route, AssistantContent: json.RawMessage(`null`),
		OutputItems: []json.RawMessage{outputItem},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-apply", Name: "apply_patch", VisibleArguments: string(functionArguments), OutputItemIndex: 0,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyID := published.Projection.Calls[0].ID
	body, _ := json.Marshal(map[string]any{
		"model": "policy",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": proxyID, "type": "custom", "custom": map[string]any{"name": "apply_patch", "input": patch},
			}}},
			map[string]any{"role": "tool", "tool_call_id": proxyID, "content": "Modified 1 file"},
		},
	})
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{ReplayStore: store, ReplayRoute: route})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(plan.Body, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 2 {
		t.Fatalf("input=%+v", request.Input)
	}
	if request.Input[0]["type"] != "function_call" || request.Input[0]["call_id"] != "upstream-apply" || request.Input[0]["arguments"] != string(functionArguments) {
		t.Fatalf("restored custom call=%+v", request.Input[0])
	}
	if request.Input[1]["type"] != "function_call_output" || request.Input[1]["call_id"] != "upstream-apply" {
		t.Fatalf("custom output=%+v", request.Input[1])
	}
}

func TestTranslateChatRequestToResponsesRejectsNullReplayCustomInput(t *testing.T) {
	body := []byte(`{
		"model":"policy",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"custom","custom":{"name":"apply_patch","input":null}}]},
			{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"done"}
		]
	}`)
	_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{ReplayStore: newResponsesChatReplayStore()})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Param != "messages[0].tool_calls[0].custom.input" {
		t.Fatalf("error = %#v", err)
	}
}

func TestTranslateChatRequestToResponsesCapturesOriginalOptionalToolDefaults(t *testing.T) {
	body := []byte(`{
		"model":"gpt",
		"messages":[{"role":"user","content":"edit"}],
		"tools":[{"type":"function","function":{
			"name":"Edit",
			"parameters":{"type":"object","properties":{
				"file_path":{"type":"string","default":"ignored-required-default"},
				"replace_all":{"type":"boolean","default":false}
			},"required":["file_path"]}
		}}]
	}`)
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defaults := plan.ReplayToolDefaults["Edit"]
	if string(defaults["replace_all"]) != "false" {
		t.Fatalf("replace_all default = %s", defaults["replace_all"])
	}
	if _, exists := defaults["file_path"]; exists {
		t.Fatalf("required property default was captured: %+v", defaults)
	}
}

func TestTranslateChatRequestToResponsesIgnoresContinuationOnlyDefaultsForReplay(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "policy", UpstreamModel: "deployment-a"}
	outputItem := json.RawMessage(`{"type":"function_call","call_id":"upstream-edit","name":"Edit","arguments":"{}"}`)
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route: route, AssistantContent: json.RawMessage(`null`), OutputItems: []json.RawMessage{outputItem},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-edit", Name: "Edit", VisibleArguments: `{}`, OutputItemIndex: 0,
			OptionalDefaults: responsesChatReplayOptionalDefaults{"replace_all": json.RawMessage("false")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	callID := published.Projection.Calls[0].ID
	body, _ := json.Marshal(map[string]any{
		"model": "policy",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": "Edit", "arguments": `{"admin":true}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "done"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "Edit", "parameters": map[string]any{"type": "object", "properties": map[string]any{
				"admin": map[string]any{"type": "boolean", "default": true},
			}},
		}}},
	})
	// Native Chat stays loud: it owns its history and can repair it, so a drifted
	// projection is an error here rather than a silent rebuild. This is upstream's
	// contract and the option's documented one; only surfaces that opt in degrade.
	_, err = translateChatRequestToResponses(body, responsesChatRequestOptions{ReplayStore: store, ReplayRoute: route})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != responsesChatReplayProjectionCode {
		t.Fatalf("error = %#v, want replay projection mismatch", err)
	}

	// When a surface DOES opt in, the guard that matters is what upstream is told: the
	// stored call must not be reused to launder arguments the store never saw.
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		ReplayStore: store, ReplayRoute: route, DegradeUnrestorableReplay: true,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	if strings.Contains(input, "upstream-edit") {
		t.Fatalf("rewritten arguments reused the stored call: %s", input)
	}
	if !strings.Contains(input, callID) {
		t.Fatalf("degraded turn dropped the visible call: %s", input)
	}
}
