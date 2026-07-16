package proxy

import (
	"encoding/json"
	"testing"
)

func TestToolShapesExtractStringArgumentAtConfiguredJSONPointer(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		path      string
		want      string
		wantFound bool
	}{
		{
			name:      "default command",
			args:      `{"command":"grep foo big.log"}`,
			path:      "/command",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "codex cmd",
			args:      `{"cmd":"grep foo big.log"}`,
			path:      "/cmd",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "nested object",
			args:      `{"shell":{"cmd":"grep foo big.log"}}`,
			path:      "/shell/cmd",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "array index",
			args:      `{"commands":["pwd","grep foo big.log"]}`,
			path:      "/commands/1",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "escaped pointer segment",
			args:      `{"shell/cmd":{"name~value":"grep foo big.log"}}`,
			path:      "/shell~1cmd/name~0value",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "missing path",
			args:      `{"command":"grep foo big.log"}`,
			path:      "/cmd",
			wantFound: false,
		},
		{
			name:      "non-string leaf",
			args:      `{"cmd":["grep foo big.log"]}`,
			path:      "/cmd",
			wantFound: false,
		},
		{
			name:      "invalid escape",
			args:      `{"cmd":"grep foo big.log"}`,
			path:      "/cm~2d",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractStringArgumentAtPath(tt.args, tt.path)
			if ok != tt.wantFound {
				t.Fatalf("found = %v, want %v", ok, tt.wantFound)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolShapesReplaceStringArgumentAtConfiguredJSONPointer(t *testing.T) {
	got, ok := replaceStringArgumentAtPath(`{"shell":{"cmd":"grep foo big.log"}}`, "/shell/cmd", "rg foo big.log")
	if !ok {
		t.Fatalf("expected replacement to succeed")
	}
	command, ok := extractStringArgumentAtPath(got, "/shell/cmd")
	if !ok || command != "rg foo big.log" {
		t.Fatalf("expected rewritten nested command, got %q found=%v in %s", command, ok, got)
	}

	got, ok = replaceStringArgumentAtPath(`{"commands":["pwd","grep foo big.log"]}`, "/commands/1", "rg foo big.log")
	if !ok {
		t.Fatalf("expected array replacement to succeed")
	}
	command, ok = extractStringArgumentAtPath(got, "/commands/1")
	if !ok || command != "rg foo big.log" {
		t.Fatalf("expected rewritten array command, got %q found=%v in %s", command, ok, got)
	}

	if _, ok := replaceStringArgumentAtPath(`{"cmd":["grep foo big.log"]}`, "/cmd", "rg foo big.log"); ok {
		t.Fatalf("expected replacement of non-string leaf to fail")
	}
}

func TestToolShapesExtractLocalShellCallCommand(t *testing.T) {
	manager := toolShapesTestManager()
	tests := []struct {
		name         string
		raw          string
		wantToolName string
		wantCallID   string
		wantCommand  string
	}{
		{
			name:         "top-level command",
			raw:          `{"type":"local_shell_call","call_id":"call-local-1","command":"grep foo big.log"}`,
			wantToolName: "local_shell_call",
			wantCallID:   "call-local-1",
			wantCommand:  "grep foo big.log",
		},
		{
			name:         "id fallback and name",
			raw:          `{"type":"local_shell_call","id":"call-local-2","name":"run_shell","cmd":"grep foo big.log"}`,
			wantToolName: "run_shell",
			wantCallID:   "call-local-2",
			wantCommand:  "grep foo big.log",
		},
		{
			name:         "arguments object",
			raw:          `{"type":"local_shell_call","call_id":"call-local-3","arguments":{"command":"grep foo big.log"}}`,
			wantToolName: "local_shell_call",
			wantCallID:   "call-local-3",
			wantCommand:  "grep foo big.log",
		},
		{
			name:         "arguments JSON string",
			raw:          `{"type":"local_shell_call","call_id":"call-local-4","arguments":"{\"cmd\":\"grep foo big.log\"}"}`,
			wantToolName: "local_shell_call",
			wantCallID:   "call-local-4",
			wantCommand:  "grep foo big.log",
		},
		{
			name:         "action command",
			raw:          `{"type":"local_shell_call","call_id":"call-local-5","action":{"command":"grep foo big.log"}}`,
			wantToolName: "local_shell_call",
			wantCallID:   "call-local-5",
			wantCommand:  "grep foo big.log",
		},
		{
			name:         "action argv command",
			raw:          `{"type":"local_shell_call","call_id":"call-local-6","action":{"command":["grep","foo bar","big.log"]}}`,
			wantToolName: "local_shell_call",
			wantCallID:   "call-local-6",
			wantCommand:  "grep 'foo bar' big.log",
		},
		{
			name:         "arguments nested argv command",
			raw:          `{"type":"local_shell_call","call_id":"call-local-7","arguments":{"action":{"command":["bash","-lc","grep foo big.log"]}}}`,
			wantToolName: "local_shell_call",
			wantCallID:   "call-local-7",
			wantCommand:  "bash -lc 'grep foo big.log'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractShellFunctionCommandItem([]byte(tt.raw), manager)
			if !ok {
				t.Fatalf("expected local shell command to be extracted")
			}
			if got.ToolName != tt.wantToolName {
				t.Fatalf("ToolName = %q, want %q", got.ToolName, tt.wantToolName)
			}
			if got.CallID != tt.wantCallID {
				t.Fatalf("CallID = %q, want %q", got.CallID, tt.wantCallID)
			}
			if got.Command != tt.wantCommand {
				t.Fatalf("Command = %q, want %q", got.Command, tt.wantCommand)
			}
		})
	}
}

func TestToolShapesReplaceLocalShellCallCommandFields(t *testing.T) {
	manager := toolShapesTestManager()
	const replacement = "rg foo big.log"
	tests := []struct {
		name   string
		raw    string
		verify func(*testing.T, map[string]json.RawMessage)
	}{
		{
			name: "top-level command",
			raw:  `{"type":"local_shell_call","call_id":"call-local-1","command":"grep foo big.log"}`,
			verify: func(t *testing.T, item map[string]json.RawMessage) {
				requireRawJSONString(t, item["command"], replacement)
			},
		},
		{
			name: "top-level cmd",
			raw:  `{"type":"local_shell_call","call_id":"call-local-2","cmd":"grep foo big.log"}`,
			verify: func(t *testing.T, item map[string]json.RawMessage) {
				requireRawJSONString(t, item["cmd"], replacement)
			},
		},
		{
			name: "arguments object command",
			raw:  `{"type":"local_shell_call","call_id":"call-local-3","arguments":{"command":"grep foo big.log"}}`,
			verify: func(t *testing.T, item map[string]json.RawMessage) {
				var arguments map[string]json.RawMessage
				if err := json.Unmarshal(item["arguments"], &arguments); err != nil {
					t.Fatalf("decode arguments object: %v", err)
				}
				requireRawJSONString(t, arguments["command"], replacement)
			},
		},
		{
			name: "arguments JSON string cmd",
			raw:  `{"type":"local_shell_call","call_id":"call-local-4","arguments":"{\"cmd\":\"grep foo big.log\"}"}`,
			verify: func(t *testing.T, item map[string]json.RawMessage) {
				var arguments string
				if err := json.Unmarshal(item["arguments"], &arguments); err != nil {
					t.Fatalf("decode arguments string: %v", err)
				}
				got, ok := extractStringArgumentAtPath(arguments, "/cmd")
				if !ok || got != replacement {
					t.Fatalf("arguments cmd = %q found=%v, want %q in %s", got, ok, replacement, arguments)
				}
			},
		},
		{
			name: "plain arguments string",
			raw:  `{"type":"local_shell_call","call_id":"call-local-5","arguments":"grep foo big.log"}`,
			verify: func(t *testing.T, item map[string]json.RawMessage) {
				requireRawJSONString(t, item["arguments"], replacement)
			},
		},
		{
			name: "action command",
			raw:  `{"type":"local_shell_call","call_id":"call-local-6","action":{"command":"grep foo big.log"}}`,
			verify: func(t *testing.T, item map[string]json.RawMessage) {
				var action map[string]json.RawMessage
				if err := json.Unmarshal(item["action"], &action); err != nil {
					t.Fatalf("decode action object: %v", err)
				}
				requireRawJSONString(t, action["command"], replacement)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newRaw, ok := replaceShellFunctionCommand([]byte(tt.raw), replacement, manager)
			if !ok {
				t.Fatalf("expected local shell command replacement to succeed")
			}
			var item map[string]json.RawMessage
			if err := json.Unmarshal(newRaw, &item); err != nil {
				t.Fatalf("decode rewritten item: %v", err)
			}
			tt.verify(t, item)

			commandItem, ok := extractShellFunctionCommandItem(newRaw, manager)
			if !ok {
				t.Fatalf("expected rewritten command to remain extractable")
			}
			if commandItem.Command != replacement {
				t.Fatalf("rewritten command = %q, want %q", commandItem.Command, replacement)
			}
		})
	}
}

func TestToolShapesDoesNotRewriteLocalShellArgvArray(t *testing.T) {
	manager := toolShapesTestManager()
	raw := []byte(`{"type":"local_shell_call","call_id":"call-local-1","action":{"command":["grep","foo bar","big.log"]}}`)
	if _, ok := replaceShellFunctionCommand(raw, "rg foo big.log", manager); ok {
		t.Fatalf("expected argv array command replacement to fail without changing the schema")
	}

	commandItem, ok := extractShellFunctionCommandItem(raw, manager)
	if !ok {
		t.Fatalf("expected argv array command to remain extractable")
	}
	if commandItem.Command != "grep 'foo bar' big.log" {
		t.Fatalf("command = %q, want %q", commandItem.Command, "grep 'foo bar' big.log")
	}
}

func TestToolShapesExtractFunctionCallOutputNonStringValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string", raw: `{"type":"function_call_output","call_id":"call-1","output":"line 1\nline 2"}`, want: "line 1\nline 2"},
		{name: "object", raw: `{"type":"function_call_output","call_id":"call-1","output":{"matches":2}}`, want: `{"matches":2}`},
		{name: "array", raw: `{"type":"function_call_output","call_id":"call-1","output":["a","b"]}`, want: `["a","b"]`},
		{name: "number", raw: `{"type":"function_call_output","call_id":"call-1","output":42}`, want: `42`},
		{name: "bool", raw: `{"type":"function_call_output","call_id":"call-1","output":true}`, want: `true`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractFunctionCallOutputItem([]byte(tt.raw))
			if !ok {
				t.Fatalf("expected output item to be extracted")
			}
			if got.CallID != "call-1" {
				t.Fatalf("CallID = %q, want call-1", got.CallID)
			}
			if got.Output != tt.want {
				t.Fatalf("Output = %q, want %q", got.Output, tt.want)
			}
		})
	}
}

func TestToolShapesExtractFunctionCallOutputSkipsNullOutput(t *testing.T) {
	if got, ok := extractFunctionCallOutputItem([]byte(`{"type":"function_call_output","call_id":"call-1","output":null}`)); ok {
		t.Fatalf("expected null output to be skipped, got %+v", got)
	}
}

func requireRawJSONString(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode string field: %v", err)
	}
	if got != want {
		t.Fatalf("string field = %q, want %q", got, want)
	}
}

func toolShapesTestManager() *ToolOptimizerManager {
	enabled := true
	return NewToolOptimizerManager(ToolOptimizersConfig{
		Enabled: true,
		Tools: ToolOptimizerToolsConfig{
			ShellFunctionCalls: ToolOptimizerShellFunctionCallsConfig{Enabled: &enabled},
		},
	}, nil)
}

func TestToolShapesLocalShellRawGroupingCommandsRemainExtractableAndReplaceable(t *testing.T) {
	manager := toolShapesTestManager()
	tests := []struct {
		name    string
		command string
	}{
		{name: "test expression", command: `[ -f go.mod ] && go test ./...`},
		{name: "brace group", command: `{ echo hi; }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawArguments, err := json.Marshal(tt.command)
			if err != nil {
				t.Fatalf("marshal arguments: %v", err)
			}
			raw := json.RawMessage(`{"type":"local_shell_call","call_id":"call-grouping","arguments":` + string(rawArguments) + `}`)

			item, ok := extractShellFunctionCommandItem(raw, manager)
			if !ok {
				t.Fatalf("raw grouping command was not extracted: %s", raw)
			}
			if item.Command != tt.command {
				t.Fatalf("command = %q, want %q", item.Command, tt.command)
			}

			const replacement = "go test ./proxy/..."
			rewritten, ok := replaceShellFunctionCommand(raw, replacement, manager)
			if !ok {
				t.Fatalf("raw grouping command was not replaceable: %s", raw)
			}
			rewrittenItem, ok := extractShellFunctionCommandItem(rewritten, manager)
			if !ok {
				t.Fatalf("rewritten grouping command was not extractable: %s", rewritten)
			}
			if rewrittenItem.Command != replacement {
				t.Fatalf("rewritten command = %q, want %q", rewrittenItem.Command, replacement)
			}
		})
	}
}

func TestToolShapesRejectsTrailingToolArgumentJSON(t *testing.T) {
	manager := toolShapesTestManager()
	arguments := `{"command":"go test ./..."} trailing garbage`
	rawArguments, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	raw := json.RawMessage(`{"type":"function_call","name":"shell_command","call_id":"call-trailing","arguments":` + string(rawArguments) + `}`)

	if item, ok := extractShellFunctionCommandItem(raw, manager); ok {
		t.Fatalf("trailing argument JSON was extracted: %+v", item)
	}
	if got, ok := extractStringArgumentAtPath(arguments, "/command"); ok {
		t.Fatalf("trailing argument JSON returned command %q", got)
	}
	if got, ok := replaceStringArgumentAtPath(arguments, "/command", "go test ./proxy"); ok || got != arguments {
		t.Fatalf("trailing argument JSON rewrite = %q, ok=%v; want unchanged false", got, ok)
	}
	if got, ok := replaceShellFunctionCommand(raw, "go test ./proxy", manager); ok || string(got) != string(raw) {
		t.Fatalf("trailing argument item rewrite = %s, ok=%v; want unchanged false", got, ok)
	}

	localTrailingArguments := []struct {
		name      string
		arguments string
	}{
		{name: "object", arguments: arguments},
		{name: "array", arguments: `["bash","-lc","go test ./..."] trailing garbage`},
	}
	for _, tt := range localTrailingArguments {
		t.Run("local "+tt.name, func(t *testing.T) {
			localArguments, err := json.Marshal(tt.arguments)
			if err != nil {
				t.Fatalf("marshal local arguments: %v", err)
			}
			localRaw := json.RawMessage(`{"type":"local_shell_call","call_id":"call-local-trailing","arguments":` + string(localArguments) + `}`)
			if item, ok := extractShellFunctionCommandItem(localRaw, manager); ok {
				t.Fatalf("trailing local-shell argument JSON was extracted: %+v", item)
			}
			if got, ok := replaceShellFunctionCommand(localRaw, "go test ./proxy", manager); ok || string(got) != string(localRaw) {
				t.Fatalf("trailing local-shell argument rewrite = %s, ok=%v; want unchanged false", got, ok)
			}
		})
	}
}
