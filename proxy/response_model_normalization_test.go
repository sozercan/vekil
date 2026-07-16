package proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRewriteResponsesResponseModelJSON(t *testing.T) {
	input := []byte(`{"id":"resp_1","model":"physical","response":{"id":"resp_1","model":"physical"},"output":[]}`)
	got, changed := rewriteResponsesResponseModelJSON(input, "public")
	if !changed {
		t.Fatal("changed = false")
	}
	if strings.Contains(string(got), `"physical"`) || strings.Count(string(got), `"public"`) != 2 {
		t.Fatalf("rewritten = %s", got)
	}
}

func TestNormalizeResponsesStreamBodyRewritesEventAndObservesBeforeRead(t *testing.T) {
	source := io.NopCloser(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"physical\"}}\n\n"))
	observed := false
	body := normalizeResponsesStreamBody(source, "public", func(data []byte) error {
		if !bytes.Contains(data, []byte("resp_1")) {
			t.Fatalf("observed data = %s", data)
		}
		observed = true
		return nil
	})
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !observed {
		t.Fatal("event was not observed")
	}
	if strings.Contains(string(got), `"physical"`) || !strings.Contains(string(got), `"model":"public"`) {
		t.Fatalf("stream = %s", got)
	}
}
