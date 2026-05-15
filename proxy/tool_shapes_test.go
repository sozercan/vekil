package proxy

import "testing"

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
		{name: "null", raw: `{"type":"function_call_output","call_id":"call-1","output":null}`, want: `null`},
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

func toolShapesTestManager() *ToolOptimizerManager {
	enabled := true
	return NewToolOptimizerManager(ToolOptimizersConfig{
		Enabled: true,
		Tools: ToolOptimizerToolsConfig{
			ShellFunctionCalls: ToolOptimizerShellFunctionCallsConfig{Enabled: &enabled},
		},
	}, nil)
}
