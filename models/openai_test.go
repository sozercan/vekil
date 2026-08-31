package models

import (
	"encoding/json"
	"testing"
)

func TestOpenAIStreamChoiceOmitsEmptyDeltaRole(t *testing.T) {
	choice := OpenAIStreamChoice{
		Index: 0,
		Delta: OpenAIMessage{Content: json.RawMessage(`"hello"`)},
	}

	got, err := json.Marshal(choice)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"index":0,"delta":{"content":"hello"}}`; string(got) != want {
		t.Fatalf("json.Marshal() = %s, want %s", got, want)
	}
}
