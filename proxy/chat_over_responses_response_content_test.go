package proxy

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestResponsesChatMessageContentRequiresTypedContentFields(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantText    string
		wantRefusal string
		wantErr     bool
	}{
		{
			name:    "output text missing text",
			raw:     `{"role":"assistant","content":[{"type":"output_text"}]}`,
			wantErr: true,
		},
		{
			name:    "output text null text",
			raw:     `{"role":"assistant","content":[{"type":"output_text","text":null}]}`,
			wantErr: true,
		},
		{
			name:    "output text non-string text",
			raw:     `{"role":"assistant","content":[{"type":"output_text","text":42}]}`,
			wantErr: true,
		},
		{
			name:     "output text empty string",
			raw:      `{"role":"assistant","content":[{"type":"output_text","text":""}]}`,
			wantText: "",
		},
		{
			name:     "output text nonempty string",
			raw:      `{"role":"assistant","content":[{"type":"output_text","text":"visible text"}]}`,
			wantText: "visible text",
		},
		{
			name:    "refusal missing refusal",
			raw:     `{"role":"assistant","content":[{"type":"refusal"}]}`,
			wantErr: true,
		},
		{
			name:    "refusal null refusal",
			raw:     `{"role":"assistant","content":[{"type":"refusal","refusal":null}]}`,
			wantErr: true,
		},
		{
			name:    "refusal non-string refusal",
			raw:     `{"role":"assistant","content":[{"type":"refusal","refusal":false}]}`,
			wantErr: true,
		},
		{
			name:        "refusal empty string",
			raw:         `{"role":"assistant","content":[{"type":"refusal","refusal":""}]}`,
			wantRefusal: "",
		},
		{
			name:        "refusal nonempty string",
			raw:         `{"role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]}`,
			wantRefusal: "cannot comply",
		},
		{
			name:    "output text rejects malformed refusal field",
			raw:     `{"role":"assistant","content":[{"type":"output_text","text":"visible text","refusal":{}}]}`,
			wantErr: true,
		},
		{
			name:    "refusal rejects malformed text field",
			raw:     `{"role":"assistant","content":[{"type":"refusal","refusal":"cannot comply","text":[]}]}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, refusal, err := responsesChatMessageContent(json.RawMessage(tt.raw))
			if tt.wantErr {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("responsesChatMessageContent() error = %#v, want unsupported_responses_output", err)
				}
				if text != "" || refusal != "" {
					t.Fatalf("responsesChatMessageContent() exposed text %q and refusal %q on error", text, refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("responsesChatMessageContent() error = %v", err)
			}
			if text != tt.wantText || refusal != tt.wantRefusal {
				t.Fatalf("responsesChatMessageContent() = (%q, %q), want (%q, %q)", text, refusal, tt.wantText, tt.wantRefusal)
			}
		})
	}
}
