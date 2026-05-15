package proxy

import (
	"encoding/json"
	"testing"
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
