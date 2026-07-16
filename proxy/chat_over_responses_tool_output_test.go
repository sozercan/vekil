package proxy

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCompactChatToolOutput(t *testing.T) {
	const messageIndex = 2

	tests := []struct {
		name      string
		raw       string
		want      string
		wantParam string
	}{
		{
			name: "ordinary typed object array remains JSON",
			raw:  `[{"type":"record","value":1}]`,
			want: `[{"type":"record","value":1}]`,
		},
		{
			name: "ordinary typed object array is compacted",
			raw:  `[ { "type": "record", "value": 1 }, { "type": "metric", "value": 2 } ]`,
			want: `[{"type":"record","value":1},{"type":"metric","value":2}]`,
		},
		{
			name: "recognized text parts are flattened",
			raw:  `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`,
			want: "firstsecond",
		},
		{
			name:      "recognized text part requires text",
			raw:       `[{"type":"text"}]`,
			wantParam: "messages[2].content[0]",
		},
		{
			name:      "recognized text part rejects unknown fields",
			raw:       `[{"type":"text","text":"value","unexpected":true}]`,
			wantParam: "messages[2].content[0].unexpected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compactChatToolOutput(json.RawMessage(tt.raw), messageIndex)
			if tt.wantParam == "" {
				if err != nil {
					t.Fatalf("compactChatToolOutput() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("compactChatToolOutput() = %q, want %q", got, tt.want)
				}
				return
			}

			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) {
				t.Fatalf("compactChatToolOutput() error = %#v, want *chatExecutionError", err)
			}
			if executionErr.Param != tt.wantParam {
				t.Fatalf("compactChatToolOutput() param = %q, want %q", executionErr.Param, tt.wantParam)
			}
		})
	}
}
