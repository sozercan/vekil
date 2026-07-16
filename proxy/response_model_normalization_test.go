package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type responseModelNormalizationWriter struct {
	header           http.Header
	body             bytes.Buffer
	writeHeaderCalls int
	writeCalls       int
	statusCode       int
}

func newResponseModelNormalizationWriter() *responseModelNormalizationWriter {
	return &responseModelNormalizationWriter{header: make(http.Header)}
}

func (w *responseModelNormalizationWriter) Header() http.Header {
	return w.header
}

func (w *responseModelNormalizationWriter) WriteHeader(statusCode int) {
	w.writeHeaderCalls++
	w.statusCode = statusCode
}

func (w *responseModelNormalizationWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	return w.body.Write(p)
}

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
	defer func() { _ = body.Close() }()
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

func TestNormalizeResponsesStreamBodyPassesThroughDoneWithoutBinding(t *testing.T) {
	const stream = "data: [DONE]\n\n"
	body := normalizeResponsesStreamBodyWithBinding(&ProxyHandler{}, io.NopCloser(strings.NewReader(stream)), explicitRouteResponseInfo{
		routeID:  "route-1",
		publicID: "public",
		targetID: "target-1",
	})
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != stream {
		t.Fatalf("stream = %q, want exact passthrough %q", got, stream)
	}
}

func TestWriteExplicitResponsesResponseRejectsMalformedSuccessBeforeCommit(t *testing.T) {
	w := newResponseModelNormalizationWriter()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Length": []string{"12"},
			"Content-Type":   []string{"application/json"},
			"X-Upstream":     []string{"present"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":`)),
	}

	err := writeExplicitResponsesResponse(context.Background(), &ProxyHandler{}, w, resp, explicitRouteResponseInfo{
		routeID:  "route-1",
		publicID: "public",
		targetID: "target-1",
	}, nil, "")
	if err == nil {
		t.Fatal("writeExplicitResponsesResponse() error = nil")
	}
	var bodyErr *responseBodyWriteError
	if !errors.As(err, &bodyErr) {
		t.Fatalf("error type = %T, want *responseBodyWriteError", err)
	}
	if bodyErr.committed {
		t.Fatal("response error was marked committed")
	}
	if !bodyErr.upstream {
		t.Fatal("response parse failure was not marked upstream")
	}
	assertResponseModelNormalizationWriterUncommitted(t, w)
}

func TestWriteExplicitResponsesResponsePreservesHeaderConflictFailure(t *testing.T) {
	w := newResponseModelNormalizationWriter()
	header := make(http.Header)
	header.Add("X-Codex-Turn-State", "state-a")
	header.Add("X-Codex-Turn-State", "state-b")
	header.Set("Content-Type", "application/json")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(`{"model":"physical","output":[]}`)),
	}

	err := writeExplicitResponsesResponse(context.Background(), &ProxyHandler{}, w, resp, explicitRouteResponseInfo{
		routeID:  "route-1",
		publicID: "public",
		targetID: "target-1",
	}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "conflicting X-Codex-Turn-State") {
		t.Fatalf("writeExplicitResponsesResponse() error = %v, want header conflict", err)
	}
	assertResponseModelNormalizationWriterUncommitted(t, w)
}

func TestWriteExplicitResponsesResponsePreservesModelNormalization(t *testing.T) {
	const input = `{"model":"physical","output":[]}`
	w := newResponseModelNormalizationWriter()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Length": []string{strconv.Itoa(len(input))},
			"Content-Type":   []string{"application/json"},
			"X-Upstream":     []string{"present"},
		},
		Body: io.NopCloser(strings.NewReader(input)),
	}

	err := writeExplicitResponsesResponse(context.Background(), &ProxyHandler{}, w, resp, explicitRouteResponseInfo{
		routeID:  "route-1",
		publicID: "public",
		targetID: "target-1",
	}, nil, "")
	if err != nil {
		t.Fatalf("writeExplicitResponsesResponse() error = %v", err)
	}
	if w.writeHeaderCalls != 1 || w.statusCode != http.StatusOK {
		t.Fatalf("WriteHeader calls/status = %d/%d, want 1/%d", w.writeHeaderCalls, w.statusCode, http.StatusOK)
	}
	if w.writeCalls != 1 {
		t.Fatalf("Write calls = %d, want 1", w.writeCalls)
	}
	if got := w.body.String(); strings.Contains(got, `"physical"`) || !strings.Contains(got, `"model":"public"`) {
		t.Fatalf("body = %s", got)
	}
	if got := w.header.Get("X-Upstream"); got != "present" {
		t.Fatalf("X-Upstream = %q, want present", got)
	}
	if got, want := w.header.Get("Content-Length"), strconv.Itoa(w.body.Len()); got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
}

func assertResponseModelNormalizationWriterUncommitted(t *testing.T, w *responseModelNormalizationWriter) {
	t.Helper()
	if w.writeHeaderCalls != 0 {
		t.Fatalf("WriteHeader calls = %d, want 0", w.writeHeaderCalls)
	}
	if w.writeCalls != 0 || w.body.Len() != 0 {
		t.Fatalf("Write calls/body length = %d/%d, want 0/0", w.writeCalls, w.body.Len())
	}
	if len(w.header) != 0 {
		t.Fatalf("downstream headers were mutated before failure: %v", w.header)
	}
}
