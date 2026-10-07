package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeCopilotResponsesItemIDsRotatingItem(t *testing.T) {
	stream := responsesItemIDTestStream(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"stable-message","type":"message"}}`,
		`{"type":"response.content_part.added","output_index":0,"item_id":"part-id"}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"delta-id","delta":"hello"}`,
		`{"type":"response.output_text.done","output_index":0,"item_id":"text-done-id","text":"hello"}`,
		`{"type":"response.content_part.done","output_index":0,"item_id":"part-done-id"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"done-id","type":"message"}}`,
		`{"type":"response.completed","response":{"id":"resp-1","output":[{"id":"completed-id","type":"message"}]}}`,
	)

	events := normalizedResponsesItemIDTestEvents(t, stream)
	assertResponsesItemIDTestEvent(t, events[0], "stable-message")
	for _, event := range events[1:6] {
		assertResponsesItemIDTestEvent(t, event, "stable-message")
	}
	assertResponsesItemIDTestOutput(t, events[6], []string{"stable-message"})
}

func TestNormalizeCopilotResponsesItemIDsInterleavedItems(t *testing.T) {
	stream := responsesItemIDTestStream(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"stable-zero","type":"message"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"stable-one","type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":1,"item_id":"rotating-one"}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"rotating-zero"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"done-one","type":"message"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"done-zero","type":"message"}}`,
		`{"type":"response.completed","response":{"output":[{"id":"final-zero","type":"message"},{"id":"final-one","type":"message"}]}}`,
	)

	events := normalizedResponsesItemIDTestEvents(t, stream)
	want := []string{"stable-zero", "stable-one", "stable-one", "stable-zero", "stable-one", "stable-zero"}
	for i, stableID := range want {
		assertResponsesItemIDTestEvent(t, events[i], stableID)
	}
	assertResponsesItemIDTestOutput(t, events[6], []string{"stable-zero", "stable-one"})
}

func TestNormalizeCopilotResponsesItemIDsReasoningAndFunctionCall(t *testing.T) {
	stream := responsesItemIDTestStream(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"stable-reasoning","type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"reasoning-part"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"reasoning-delta","delta":"brief"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"item_id":"reasoning-text-done"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"item_id":"reasoning-part-done"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"reasoning-done","type":"reasoning","encrypted_content":"opaque"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"stable-call","type":"function_call","call_id":"call-1"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"call-delta","delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"call-arguments-done","arguments":"{}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"call-done","type":"function_call","call_id":"call-1"}}`,
		`{"type":"response.completed","response":{"output":[{"id":"reasoning-final","type":"reasoning"},{"id":"call-final","type":"function_call","call_id":"call-1"}]}}`,
	)

	events := normalizedResponsesItemIDTestEvents(t, stream)
	for _, event := range events[:6] {
		assertResponsesItemIDTestEvent(t, event, "stable-reasoning")
	}
	for _, event := range events[6:10] {
		assertResponsesItemIDTestEvent(t, event, "stable-call")
	}
	assertResponsesItemIDTestOutput(t, events[10], []string{"stable-reasoning", "stable-call"})
}

func TestNormalizeCopilotResponsesItemIDsPreservesRouteAttemptOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := &routeAttemptTransportOwner{cancel: cancel}
	resp := responsesItemIDTestResponse("")
	resp.Body = &routeAttemptTransportBody{inner: resp.Body, owner: owner}
	normalizeCopilotResponsesItemIDs(resp, providerEndpointResponses)
	defer func() { _ = resp.Body.Close() }()

	if got := routeAttemptTransportOwnership(resp.Body); got != owner {
		t.Fatalf("routeAttemptTransportOwnership() = %p, want %p", got, owner)
	}
	if !cancelRouteAttemptBody(resp.Body) {
		t.Fatal("cancelRouteAttemptBody() = false, want true")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("request context error = %v, want context.Canceled", ctx.Err())
	}
}

func TestNormalizeCopilotResponsesItemIDsPreservesDuplicateKeys(t *testing.T) {
	for _, tt := range []struct {
		name  string
		event string
	}{
		{"event", `{"type":"response.output_text.delta","output_index":0,"item_id":"rotating","delta":"first","delta":"second"}`},
		{"item", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rotating","type":"reasoning","encrypted_content":"first","encrypted_content":"second"}}`},
		{"response", `{"type":"response.completed","response":{"id":"first","id":"second","output":[{"id":"rotating","type":"reasoning"}]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stream := responsesItemIDTestStream(
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"stable","type":"reasoning"}}`,
				tt.event,
			)
			resp := responsesItemIDTestResponse(stream)
			normalizeCopilotResponsesItemIDs(resp, providerEndpointResponses)
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if string(got) != stream {
				t.Fatalf("ambiguous stream changed: got %q, want %q", got, stream)
			}
			if _, err := extractDurableResponsesOutputState([]byte(tt.event)); err == nil {
				t.Fatal("durable state validator accepted duplicate keys")
			}
		})
	}
}

func TestNormalizeCopilotResponsesItemIDsClearsContentLength(t *testing.T) {
	resp := responsesItemIDTestResponse("data: [DONE]\n\n")
	resp.Header.Set("Content-Length", "14")
	resp.ContentLength = 14
	normalizeCopilotResponsesItemIDs(resp, providerEndpointResponses)
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length header = %q, want empty", got)
	}
	if resp.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1", resp.ContentLength)
	}
}

func TestNormalizeCopilotResponsesItemIDsIsResponsesOnly(t *testing.T) {
	const stream = "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"first\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"second\"}}\n\n"
	for _, endpoint := range []string{providerEndpointChatCompletions, providerEndpointMessages} {
		resp := responsesItemIDTestResponse(stream)
		normalizeCopilotResponsesItemIDs(resp, endpoint)
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll(%s) error = %v", endpoint, err)
		}
		if string(got) != stream {
			t.Fatalf("endpoint %s changed stream: %q", endpoint, got)
		}
	}
}

func TestFinishCopilotInferenceLeavesNonCopilotResponsesUntouched(t *testing.T) {
	const stream = "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"first\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"second\"}}\n\n"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := responsesItemIDTestResponse(stream)
	resp.Request = req
	(&ProxyHandler{}).finishCopilotInference(req, resp, nil, nil)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != stream {
		t.Fatalf("non-Copilot stream changed: %q", got)
	}
}

func normalizedResponsesItemIDTestEvents(t *testing.T, stream string) []map[string]json.RawMessage {
	t.Helper()
	resp := responsesItemIDTestResponse(stream)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.test/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(context.WithValue(req.Context(), copilotInferenceRequestContextKey{}, copilotInferenceRequest{endpoint: providerEndpointResponses}))
	resp.Request = req
	(&ProxyHandler{}).finishCopilotInference(req, resp, nil, nil)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	var events []map[string]json.RawMessage
	for _, block := range bytes.Split(body, []byte("\n\n")) {
		for _, line := range bytes.Split(block, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			var event map[string]json.RawMessage
			if err := json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &event); err != nil {
				t.Fatalf("decode event %q: %v", line, err)
			}
			events = append(events, event)
		}
	}
	return events
}

func responsesItemIDTestResponse(stream string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
}

func responsesItemIDTestStream(events ...string) string {
	var stream strings.Builder
	for _, event := range events {
		var envelope map[string]json.RawMessage
		_ = json.Unmarshal([]byte(event), &envelope)
		eventType := rawJSONString(envelope["type"])
		stream.WriteString("event: ")
		stream.WriteString(eventType)
		stream.WriteString("\ndata: ")
		stream.WriteString(event)
		stream.WriteString("\n\n")
	}
	return stream.String()
}

func assertResponsesItemIDTestEvent(t *testing.T, event map[string]json.RawMessage, want string) {
	t.Helper()
	if itemID := rawJSONString(event["item_id"]); itemID != "" {
		if itemID != want {
			t.Fatalf("item_id = %q, want %q in %s", itemID, want, event["type"])
		}
		return
	}
	if got := responsesEventItemID(event["item"]); got != want {
		t.Fatalf("item.id = %q, want %q in %s", got, want, event["type"])
	}
}

func assertResponsesItemIDTestOutput(t *testing.T, event map[string]json.RawMessage, want []string) {
	t.Helper()
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(event["response"], &response); err != nil {
		t.Fatalf("decode response output: %v", err)
	}
	if len(response.Output) != len(want) {
		t.Fatalf("output length = %d, want %d", len(response.Output), len(want))
	}
	for i, item := range response.Output {
		if got := responsesEventItemID(item); got != want[i] {
			t.Fatalf("output[%d].id = %q, want %q", i, got, want[i])
		}
	}
}
