package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeResponsesAdditionalToolsNamespaceDescriptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		want      string
		wantCount int
	}{
		{
			name: "normalizes blank namespace descriptions",
			body: `{
				"model":"gpt-5.6-sol",
				"input":[
					{"type":"additional_tools","role":"developer","tools":[
						{"type":"namespace","name":"functions","description":"","tools":[{"type":"function","name":"exec","description":"Run commands","parameters":{"type":"object"}}]},
						{"type":"namespace","name":"agents","description":" \t\r\n","tools":[]},
						{"type":"namespace","name":"editor","description":"Editing tools","tools":[]},
						{"type":"function","name":"lookup","description":"","parameters":{"type":"object"}}
					]},
					{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
				]
			}`,
			want: `{
				"model":"gpt-5.6-sol",
				"input":[
					{"type":"additional_tools","role":"developer","tools":[
						{"type":"namespace","name":"functions","description":"Tools in the functions namespace.","tools":[{"type":"function","name":"exec","description":"Run commands","parameters":{"type":"object"}}]},
						{"type":"namespace","name":"agents","description":"Tools in the agents namespace.","tools":[]},
						{"type":"namespace","name":"editor","description":"Editing tools","tools":[]},
						{"type":"function","name":"lookup","description":"","parameters":{"type":"object"}}
					]},
					{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
				]
			}`,
			wantCount: 2,
		},
		{
			name:      "handles escaped type values and whitespace",
			body:      `{"input":[{"type":"additional_\u0074ools","tools":[{"type":"name\u0073pace","name":"func\u0074ions","description":" \t "}]}]}`,
			want:      `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","description":"Tools in the functions namespace."}]}]}`,
			wantCount: 1,
		},
		{
			name: "preserves nonempty and missing descriptions byte for byte",
			body: "{ \n\t\"input\": [" +
				`{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","description":"Function tools","tools":[]},{"type":"namespace","name":"editor","tools":[]}]} ` +
				"]}",
			wantCount: 0,
		},
		{
			name:      "ignores namespaces outside additional tools",
			body:      `{"input":[{"type":"message","tools":[{"type":"namespace","name":"functions","description":""}]}],"tools":[{"type":"namespace","name":"functions","description":""}]}`,
			wantCount: 0,
		},
		{
			name:      "ignores non-string descriptions",
			body:      `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","description":null},{"type":"namespace","name":"editor","description":42}]}]}`,
			wantCount: 0,
		},
		{
			name:      "fails open for invalid JSON",
			body:      `{"input":[{"type":"additional_tools"}`,
			wantCount: 0,
		},
		{
			name:      "ignores non-array input",
			body:      `{"input":{"type":"additional_tools","tools":[]}}`,
			wantCount: 0,
		},
		{
			name:      "ignores malformed tools collections",
			body:      `{"input":[{"type":"additional_tools","tools":{"type":"namespace","description":""}},{"type":"additional_tools","tools":[null,42,{"type":"namespace","description":{}}]}]}`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := []byte(tt.body)
			got, gotCount := normalizeResponsesAdditionalToolsNamespaceDescriptions(body)
			if gotCount != tt.wantCount {
				t.Fatalf("normalized count = %d, want %d", gotCount, tt.wantCount)
			}
			if tt.wantCount == 0 {
				if !bytes.Equal(got, body) {
					t.Fatalf("unchanged body was rewritten:\n got: %s\nwant: %s", got, body)
				}
				return
			}
			assertResponsesAdditionalToolsJSONEqual(t, got, []byte(tt.want))
		})
	}
}

func TestResponsesAdditionalToolsNamespaceDescriptionFallback(t *testing.T) {
	t.Parallel()

	longName, err := json.Marshal(string(bytes.Repeat([]byte("x"), responsesNamespaceDescriptionNameLimitBytes+1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, rawName := range []json.RawMessage{nil, json.RawMessage(`"  "`), json.RawMessage(`42`), longName} {
		if got := responsesAdditionalToolsNamespaceDescription(rawName); got != "Tools in this namespace." {
			t.Fatalf("description for name %s = %q, want bounded generic fallback", rawName, got)
		}
	}
}

func assertResponsesAdditionalToolsJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()

	var gotValue interface{}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v\n%s", err, got)
	}
	var wantValue interface{}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("rewritten JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}
