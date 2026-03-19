package proxy

import "testing"

func TestExtractResponsesOutputText(t *testing.T) {
	body := []byte(`{
		"output":[
			{"type":"message","content":[
				{"type":"output_text","text":"Alpha "},
				{"type":"text","text":"Beta"},
				{"type":"refusal","text":"ignored"}
			]},
			{"type":"tool_call","content":[{"type":"output_text","text":"ignored"}]},
			{"type":"message","content":[{"type":"output_text","text":" citeturn1view0Gamma"}]},
			{"type":"message","content":"bad-shape"}
		]
	}`)

	text, err := extractResponsesOutputText(body)
	if err != nil {
		t.Fatalf("extractResponsesOutputText: %v", err)
	}
	if text != "Alpha Beta Gamma" {
		t.Fatalf("text = %q, want %q", text, "Alpha Beta Gamma")
	}
}

func TestExtractResponsesOutputText_InvalidJSON(t *testing.T) {
	if _, err := extractResponsesOutputText([]byte(`{`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
