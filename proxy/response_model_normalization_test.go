package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
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

func TestRewriteResponsesResponseModelJSONNormalizesNonStreamingResponses(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "missing model"},
		{name: "null model", model: `,"model":null`},
		{name: "non-string model", model: `,"model":42`},
		{name: "empty model", model: `,"model":""`},
		{name: "valid model", model: `,"model":"physical"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`{"id":"resp_1","object":"response"` + tc.model + `,"output":[]}`)
			got, changed := rewriteResponsesResponseModelJSON(input, "public")
			if !changed {
				t.Fatal("changed = false")
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("decode rewritten response: %v", err)
			}
			if gotModel := payload["model"]; gotModel != "public" {
				t.Fatalf("model = %#v, want public", gotModel)
			}
		})
	}
}

func TestRewriteResponsesResponseModelJSONLeavesNonResponseEventsUntouched(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "vendor event",
			body: `{"type":"vendor.event","model":"vendor-model","response":{"model":"vendor-response"}}`,
		},
		{
			name: "output item event",
			body: `{"type":"response.output_item.done","item":{"type":"message","model":"item-model"}}`,
		},
		{
			name: "malformed event type",
			body: `{"type":42,"model":"vendor-model"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.body)
			got, changed := rewriteResponsesResponseModelJSON(input, "public")
			if changed {
				t.Fatal("changed = true")
			}
			if !bytes.Equal(got, input) {
				t.Fatalf("rewritten = %s, want exact passthrough %s", got, input)
			}
		})
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

func TestNormalizeResponsesStreamBodyWithBindingFailsOpenForExtractionErrors(t *testing.T) {
	tests := []struct {
		name   string
		stream string
	}{
		{
			name:   "malformed JSON",
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":\n\n",
		},
		{
			name:   "vendor non-JSON data",
			stream: "event: vendor.extension\ndata: vendor-payload=opaque\n\n",
		},
		{
			name:   "null JSON root",
			stream: "event: vendor.extension\ndata: null\n\n",
		},
		{
			name:   "array JSON root",
			stream: "event: vendor.extension\ndata: []\n\n",
		},
		{
			name:   "scalar JSON root",
			stream: "event: vendor.extension\ndata: \"opaque\"\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{}
			body := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(tc.stream)), explicitRouteResponseInfo{
				routeID:  "route-1",
				publicID: "public",
				targetID: "target-1",
			})
			defer func() { _ = body.Close() }()

			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if string(got) != tc.stream {
				t.Fatalf("stream = %q, want exact passthrough %q", got, tc.stream)
			}
			store, err := h.ensureStateBindingStore()
			if err != nil {
				t.Fatalf("ensureStateBindingStore() error = %v", err)
			}
			if stats := store.stats(); stats.entries != 0 || stats.tombstones != 0 {
				t.Fatalf("state bindings after unparseable event = %#v, want empty", stats)
			}
		})
	}
}

func TestNormalizeResponsesStreamBodyWithBindingIgnoresVendorEventRootID(t *testing.T) {
	const eventID = "event-vendor"
	const stream = "event: vendor.event\ndata: {\"type\":\"vendor.event\",\"id\":\"event-vendor\",\"model\":\"vendor-model\"}\n\n"
	h := &ProxyHandler{}
	body := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(stream)), explicitRouteResponseInfo{
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
	store, err := h.ensureStateBindingStore()
	if err != nil {
		t.Fatalf("ensureStateBindingStore() error = %v", err)
	}
	if result := store.lookup(stateBindingTypeResponseID, eventID); result.outcome != stateBindingLookupUnknown {
		t.Fatalf("vendor event root id binding outcome = %s, want unknown", result.outcome)
	}
}

func TestNormalizeResponsesStreamBodyWithBindingLeavesOutputItemEventUntouched(t *testing.T) {
	const stream = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"item-1\",\"type\":\"message\",\"model\":\"item-model\"}}\n\n"
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

func TestNormalizeResponsesStreamBodyWithBindingBindsLifecycleNestedResponseIDOnly(t *testing.T) {
	const eventID = "event-lifecycle"
	const responseID = "resp-lifecycle"
	const stream = "event: response.created\ndata: {\"type\":\"response.created\",\"id\":\"event-lifecycle\",\"response\":{\"id\":\"resp-lifecycle\",\"model\":\"physical\"}}\n\n"
	h := &ProxyHandler{}
	info := explicitRouteResponseInfo{routeID: "route-1", publicID: "public", targetID: "target-1"}
	body := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(stream)), info)
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if strings.Contains(string(got), `"physical"`) || !strings.Contains(string(got), `"model":"public"`) {
		t.Fatalf("stream = %s", got)
	}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		t.Fatalf("ensureStateBindingStore() error = %v", err)
	}
	if result := store.lookup(stateBindingTypeResponseID, eventID); result.outcome != stateBindingLookupUnknown {
		t.Fatalf("lifecycle event root id binding outcome = %s, want unknown", result.outcome)
	}
	wantOwner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID}
	result := store.lookup(stateBindingTypeResponseID, responseID)
	if result.outcome != stateBindingLookupKnown || result.owner != wantOwner {
		t.Fatalf("nested response id binding = %#v, want known owner %#v", result, wantOwner)
	}
}

func TestNormalizeResponsesStreamBodyWithBindingNormalizesLifecycleResponseModels(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		model     string
	}{
		{name: "missing model", eventType: "response.created"},
		{name: "null model", eventType: "response.created", model: `,"model":null`},
		{name: "non-string model", eventType: "response.created", model: `,"model":42`},
		{name: "empty model", eventType: "response.created", model: `,"model":""`},
		{name: "valid model", eventType: "response.created", model: `,"model":"physical"`},
		{name: "queued lifecycle model", eventType: "response.queued"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := "event: " + tc.eventType + "\ndata: {\"type\":\"" + tc.eventType + "\",\"response\":{\"id\":\"resp-model\"" + tc.model + "}}\n\n"
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
			if !strings.Contains(string(got), `"model":"public"`) {
				t.Fatalf("stream = %s", got)
			}
			if strings.Contains(string(got), `"model":"physical"`) || strings.Contains(string(got), `"model":42`) || strings.Contains(string(got), `"model":null`) {
				t.Fatalf("stream retained upstream model = %s", got)
			}
		})
	}
}

func TestNormalizeResponsesStreamBodyWithBindingBindsValidState(t *testing.T) {
	const token = "provider-state-1"
	const stream = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"provider-state-1\"}}\n\n"
	h := &ProxyHandler{}
	info := explicitRouteResponseInfo{routeID: "route-1", publicID: "public", targetID: "target-1"}
	body := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(stream)), info)
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != stream {
		t.Fatalf("stream = %q, want exact passthrough %q", got, stream)
	}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		t.Fatalf("ensureStateBindingStore() error = %v", err)
	}
	wantOwner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID}
	result := store.lookup(stateBindingTypeEncryptedContent, token)
	if result.outcome != stateBindingLookupKnown || result.owner != wantOwner {
		t.Fatalf("state binding = %#v, want known owner %#v", result, wantOwner)
	}
}

func TestNormalizeResponsesStreamBodyWithBindingPropagatesConflictAfterEarlierEvents(t *testing.T) {
	const conflictToken = "resp-conflict"
	h := &ProxyHandler{}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		t.Fatalf("ensureStateBindingStore() error = %v", err)
	}
	otherOwner := stateBindingOwner{routeID: "route-1", targetID: "target-other"}
	if result := store.bind(stateBindingTypeResponseID, conflictToken, otherOwner); result.outcome != stateBindingLookupKnown {
		t.Fatalf("seed bind outcome = %s, want known", result.outcome)
	}

	const firstEvent = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-first\",\"model\":\"physical\"}}\n\n"
	const conflictingEvent = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-conflict\",\"model\":\"physical\"}}\n\n"
	info := explicitRouteResponseInfo{routeID: "route-1", publicID: "public", targetID: "target-1"}
	body := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(strings.NewReader(firstEvent+conflictingEvent)), info)
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err == nil || !strings.Contains(err.Error(), "provider state token collided with another route target") {
		t.Fatalf("ReadAll() error = %v, want binding conflict", err)
	}
	if !strings.Contains(string(got), `"id":"resp-first"`) || !strings.Contains(string(got), `"model":"public"`) {
		t.Fatalf("stream before conflict = %q, want rewritten first event", got)
	}
	if strings.Contains(string(got), conflictToken) {
		t.Fatalf("stream before conflict = %q, conflicting event was forwarded", got)
	}
	wantOwner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID}
	firstResult := store.lookup(stateBindingTypeResponseID, "resp-first")
	if firstResult.outcome != stateBindingLookupKnown || firstResult.owner != wantOwner {
		t.Fatalf("first event binding = %#v, want known owner %#v", firstResult, wantOwner)
	}
	if conflictResult := store.lookup(stateBindingTypeResponseID, conflictToken); conflictResult.outcome != stateBindingLookupConflict {
		t.Fatalf("conflicting binding outcome = %s, want conflict", conflictResult.outcome)
	}
}

func TestNormalizeResponsesStreamBodyWithBindingPropagatesTransportErrorAfterEarlierEvents(t *testing.T) {
	transportErr := errors.New("upstream stream failed")
	const firstEvent = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-first\",\"model\":\"physical\"}}\n\n"
	source := io.NopCloser(io.MultiReader(strings.NewReader(firstEvent), iotest.ErrReader(transportErr)))
	body := normalizeResponsesStreamBodyWithBinding(&ProxyHandler{}, source, explicitRouteResponseInfo{
		routeID:  "route-1",
		publicID: "public",
		targetID: "target-1",
	})
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if !errors.Is(err, transportErr) {
		t.Fatalf("ReadAll() error = %v, want %v", err, transportErr)
	}
	if !strings.Contains(string(got), `"id":"resp-first"`) || !strings.Contains(string(got), `"model":"public"`) {
		t.Fatalf("stream before transport error = %q, want rewritten first event", got)
	}
}

func TestWriteExplicitResponsesResponseRejectsMalformedSuccessBeforeCommit(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"id":`},
		{name: "null root", body: `null`},
		{name: "array root", body: `[]`},
		{name: "string root", body: `"response"`},
		{name: "number root", body: `42`},
		{name: "boolean root", body: `true`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newResponseModelNormalizationWriter()
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Length": []string{strconv.Itoa(len(tc.body))},
					"Content-Type":   []string{"application/json"},
					"X-Upstream":     []string{"present"},
				},
				Body: io.NopCloser(strings.NewReader(tc.body)),
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
		})
	}
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

func TestWriteExplicitResponsesResponseNormalizesModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "missing model"},
		{name: "null model", model: `,"model":null`},
		{name: "non-string model", model: `,"model":42`},
		{name: "empty model", model: `,"model":""`},
		{name: "valid model", model: `,"model":"physical"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"id":"resp-model","object":"response"` + tc.model + `,"output":[]}`
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
		})
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
