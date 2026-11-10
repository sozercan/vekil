package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	citationTestSpec = "citeturn853486search0"
	citationTestPR   = "citeturn295586view1"
)

var citationTestLinks = map[string]citationLink{
	"turn853486search0": {title: "Authorization API 1.0", url: "https://openid.net/specs/authorization-api-1_0.html"},
	"turn853486search2": {title: "Authorization API 1.0", url: "https://openid.net/specs/authorization-api-1_0.html"},
	"turn295586view1":   {title: "PR #831 | orka", url: "https://github.com/orka-agents/orka/pull/831"},
	"turn1view0":        {title: "Wait (command)", url: "https://en.wikipedia.org/wiki/Wait_(command)"},
	"turn2view0":        {url: "https://raw.githubusercontent.com/org/repo/main/README.md"},
	"turn670467view1":   {title: "Code mode result", url: "https://example.com/code-mode"},
	"turn3view0":        {title: "Escaped result", url: "https://example.com/escaped"},
	"turn4view0":        {title: "Search", url: "https://example.com/search?filter=a|b"},
}

func citationTestLookup(ref string) (citationLink, bool) {
	link, ok := citationTestLinks[ref]
	return link, ok
}

// citationTestRequestBody holds web_run results in the shape Codex replays.
func citationTestRequestBody(t *testing.T) []byte {
	t.Helper()
	results := "Authorization API 1.0 (https://openid.net/specs/authorization-api-1_0.html)\n" +
		"【turn853486search0】 [wordlim: 200] Crawled: today; Final specification.\n\n" +
		"--------------------------------------------------------------------------------\n" +
		"PR #831 | orka (https://github.com/orka-agents/orka/pull/831)\n" +
		"【turn295586view1】 [wordlim: 200] Crawled: today; Content type: text/html; Total lines: 4\n" +
		"L0: Draft RFC\n"
	// Codex code mode returns the MCP result as serialized CallToolResult JSON.
	codeModeResult, err := json.Marshal(map[string]any{"content": []any{map[string]any{
		"type": "text",
		"text": "Code mode result (https://example.com/code-mode)\n【turn670467view1】 [wordlim: 200] Total lines: 1\nL0: hi\n",
	}}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	// Serializers such as Python's json.dumps escape the brackets as \u3010.
	escapedResult := `{"content":[{"type":"text","text":` +
		strconv.QuoteToASCII("Escaped result (https://example.com/escaped)\n【turn3view0】 Total lines: 1\n") + `}]}`
	if strings.ContainsRune(escapedResult, '【') {
		t.Fatalf("escaped result has a literal bracket: %s", escapedResult)
	}
	body, err := json.Marshal(map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "_web_run", "namespace": "mcp__codex_apps__search_service", "arguments": `{"open":[]}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": []any{
				map[string]any{"type": "input_text", "text": "Wall time: 1.0 seconds\nOutput:"},
				map[string]any{"type": "input_text", "text": results},
			}},
			map[string]any{"type": "function_call_output", "call_id": "call-2", "output": " (https://raw.githubusercontent.com/org/repo/main/README.md)\n【turn2view0】 [wordlim: 200] Total lines: 1\nL0: hi\n"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call-3", "output": []any{
				map[string]any{"type": "input_text", "text": "Script completed\nWall time 1.3 seconds\nOutput:\n"},
				map[string]any{"type": "input_text", "text": string(codeModeResult)},
			}},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call-4", "output": escapedResult},
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return body
}

func TestRewriteCitationMarkers(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"single", "Final in 2026. " + citationTestSpec + "\n", "Final in 2026. [Authorization API 1.0](https://openid.net/specs/authorization-api-1_0.html)\n"},
		{"table cell", "| Draft. " + citationTestPR + " |", "| Draft. [PR #831 \\| orka](https://github.com/orka-agents/orka/pull/831) |"},
		{"multiple deduplicated", "x citeturn853486search0turn853486search2turn295586view1", "x [Authorization API 1.0](https://openid.net/specs/authorization-api-1_0.html), [PR #831 \\| orka](https://github.com/orka-agents/orka/pull/831)"},
		{"parentheses in URL", "citeturn1view0", "[Wait (command)](https://en.wikipedia.org/wiki/Wait_%28command%29)"},
		{"untitled uses host", "citeturn2view0", "[raw.githubusercontent.com](https://raw.githubusercontent.com/org/repo/main/README.md)"},
		{"unresolved removed", "a citeturn0search9b", "a b"},
		{"partly resolved", "citeturn0search9turn295586view1", "[PR #831 \\| orka](https://github.com/orka-agents/orka/pull/831)"},
		{"other marker kept", "visualize{\"path\":\"/tmp/chart.html\"}", "visualize{\"path\":\"/tmp/chart.html\"}"},
		{"empty ref kept", "cite", "cite"},
		{"no ref kept", "cite", "cite"},
		{"unterminated removed", "end citeturn295586view1", "end "},
		{"invalid ref kept", "citeturn 1", "citeturn 1"},
		{"start before marker", "citeturn2view0", "[raw.githubusercontent.com](https://raw.githubusercontent.com/org/repo/main/README.md)"},
		{"oversized kept", "cite" + strings.Repeat("a", citationMarkerMaxBytes) + "", "cite" + strings.Repeat("a", citationMarkerMaxBytes) + ""},
		{"plain", "no markers", "no markers"},
		{"pipe in URL", "| citeturn4view0 |", "| [Search](https://example.com/search?filter=a%7Cb) |"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, held, _ := rewriteCitationMarkers(tt.text, true, citationTestLookup)
			if got != tt.want || held != "" {
				t.Fatalf("rewriteCitationMarkers() = %q, held %q; want %q", got, held, tt.want)
			}
		})
	}
}

func TestRewriteCitationMarkersHoldsPartialMarker(t *testing.T) {
	for _, partial := range []string{"", "ci", "cite", "citeturn29", "citeturn295586view1"} {
		got, held, _ := rewriteCitationMarkers("text "+partial, false, citationTestLookup)
		if got != "text " || held != partial {
			t.Fatalf("rewriteCitationMarkers(%q) = %q, held %q; want %q, held %q", partial, got, held, "text ", partial)
		}
	}
	got, held, _ := rewriteCitationMarkers("visualize", false, citationTestLookup)
	if got != "visualize" || held != "" {
		t.Fatalf("non-cite prefix = %q, held %q; want it emitted", got, held)
	}
}

func TestRewriteCitationMarkersStreamingMatchesFinal(t *testing.T) {
	text := "Spec. " + citationTestSpec + " Draft " + citationTestPR + "\n" +
		"visualize{} lost citeturn0search9 citebad ref tail citeturn2view0"
	want, _, _ := rewriteCitationMarkers(text, true, citationTestLookup)
	var boundaries []int
	for i := range text {
		boundaries = append(boundaries, i)
	}
	boundaries = append(boundaries, len(text))
	stream := func(chunks []string) string {
		var out, held string
		for _, chunk := range chunks {
			var emitted string
			emitted, held, _ = rewriteCitationMarkers(held+chunk, false, citationTestLookup)
			out += emitted
		}
		// The text ends inside a marker, which the final text drops.
		return out
	}
	for _, split := range boundaries {
		if got := stream([]string{text[:split], text[split:]}); got != want {
			t.Fatalf("split at %d: got %q, want %q", split, got, want)
		}
	}
	var runes []string
	for _, r := range text {
		runes = append(runes, string(r))
	}
	if got := stream(runes); got != want {
		t.Fatalf("rune deltas: got %q, want %q", got, want)
	}
}

func TestParseResponsesCitationSources(t *testing.T) {
	links := parseResponsesCitationSources(citationTestRequestBody(t))
	want := map[string]citationLink{
		"turn853486search0": citationTestLinks["turn853486search0"],
		"turn295586view1":   citationTestLinks["turn295586view1"],
		"turn2view0":        citationTestLinks["turn2view0"],
		"turn670467view1":   citationTestLinks["turn670467view1"],
		"turn3view0":        citationTestLinks["turn3view0"],
	}
	if len(links) != len(want) {
		t.Fatalf("parseResponsesCitationSources() = %#v, want %#v", links, want)
	}
	for ref, link := range want {
		if links[ref] != link {
			t.Fatalf("links[%q] = %#v, want %#v", ref, links[ref], link)
		}
	}
	if links := parseResponsesCitationSources([]byte(`{"input":"no browsing results"}`)); links != nil {
		t.Fatalf("parseResponsesCitationSources() without results = %#v, want nil", links)
	}
}

func citationTestResponse(t *testing.T, stream string) (*http.Request, *http.Response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/responses", nil)
	req = withResponsesCitationSources(req, providerEndpointResponses, citationTestRequestBody(t))
	resp := responsesItemIDTestResponse(stream)
	return req, resp
}

func citationTestEvents(t *testing.T, req *http.Request, resp *http.Response) []sseEvent {
	t.Helper()
	normalizeResponsesCitationMarkers(req, resp)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return parseSSEEvents(string(body))
}

func citationTestEventField(t *testing.T, event sseEvent, path ...string) string {
	t.Helper()
	value := citationTestEventValue(t, event, path...)
	text, ok := value.(string)
	if !ok {
		t.Fatalf("event %q path %v = %#v, want string", event.Event, path, value)
	}
	return text
}

func citationTestEventValue(t *testing.T, event sseEvent, path ...string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(event.Data), &value); err != nil {
		t.Fatalf("event %q data is invalid JSON: %v", event.Event, err)
	}
	for _, key := range path {
		switch current := value.(type) {
		case map[string]any:
			value = current[key]
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(current) {
				t.Fatalf("event %q path %v: bad array index %q", event.Event, path, key)
			}
			value = current[index]
		default:
			t.Fatalf("event %q path %v: missing %q", event.Event, path, key)
		}
	}
	return value
}

func citationTestDelta(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"type":"response.output_text.delta","item_id":"msg-1","output_index":1,"content_index":0,"delta":` + string(encoded) + `}`
}

func TestNormalizeResponsesCitationMarkersStream(t *testing.T) {
	full := "AuthZEN is final. " + citationTestSpec + "\n| #831 | Draft. " + citationTestPR + " |"
	splitSpec := strings.Index(full, "turn853486")
	splitPR := strings.Index(full, "turn295586")
	encodedFull, _ := json.Marshal(full)
	part := `{"type":"output_text","annotations":[],"text":` + string(encodedFull) + `}`
	item := `{"id":"msg-1","type":"message","role":"assistant","content":[` + part + `]}`
	added := `{"type":"response.output_item.added","output_index":1,"item":{"id":"msg-1","type":"message","content":[]}}`
	stream := responsesItemIDTestStream(
		added,
		citationTestDelta(full[:splitSpec]),
		citationTestDelta(full[splitSpec:splitPR]),
		citationTestDelta(full[splitPR:]),
		`{"type":"response.output_text.done","item_id":"msg-1","output_index":1,"content_index":0,"text":`+string(encodedFull)+`}`,
		`{"type":"response.content_part.done","item_id":"msg-1","output_index":1,"content_index":0,"part":`+part+`}`,
		`{"type":"response.output_item.done","output_index":1,"item":`+item+`}`,
		`{"type":"response.completed","response":{"id":"resp-1","output":[{"id":"rs-1","type":"reasoning","encrypted_content":"opaque"},`+item+`]}}`,
	)

	req, resp := citationTestResponse(t, stream)
	events := citationTestEvents(t, req, resp)
	if len(events) != 8 {
		t.Fatalf("events = %d, want 8", len(events))
	}
	want := "AuthZEN is final. [Authorization API 1.0](https://openid.net/specs/authorization-api-1_0.html)\n" +
		"| #831 | Draft. [PR #831 \\| orka](https://github.com/orka-agents/orka/pull/831) |"
	var streamed string
	for _, event := range events[1:4] {
		streamed += citationTestEventField(t, event, "delta")
	}
	for _, got := range []struct {
		name string
		text string
	}{
		{"deltas", streamed},
		{"output_text.done", citationTestEventField(t, events[4], "text")},
		{"content_part.done", citationTestEventField(t, events[5], "part", "text")},
		{"output_item.done", citationTestEventField(t, events[6], "item", "content", "0", "text")},
		{"response.completed", citationTestEventField(t, events[7], "response", "output", "1", "content", "0", "text")},
	} {
		if got.text != want {
			t.Fatalf("%s text = %q, want %q", got.name, got.text, want)
		}
	}
	if events[0].Data != added {
		t.Fatalf("event without markers changed: %q", events[0].Data)
	}
	if got := citationTestEventField(t, events[7], "response", "output", "0", "encrypted_content"); got != "opaque" {
		t.Fatalf("reasoning item changed: %q", got)
	}
}

func TestNormalizeResponsesCitationMarkersDropsTrailingPartialMarker(t *testing.T) {
	full := "Cut off \ue200cite\ue202turn2"
	encodedFull, _ := json.Marshal(full)
	stream := responsesItemIDTestStream(
		citationTestDelta("Cut off \ue200ci"),
		citationTestDelta("te\ue202turn2"),
		`{"type":"response.output_text.done","item_id":"msg-1","output_index":1,"content_index":0,"text":`+string(encodedFull)+`}`,
	)

	req, resp := citationTestResponse(t, stream)
	events := citationTestEvents(t, req, resp)
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (no synthesized event): %#v", len(events), events)
	}
	streamed := citationTestEventField(t, events[0], "delta") + citationTestEventField(t, events[1], "delta")
	if done := citationTestEventField(t, events[2], "text"); streamed != "Cut off " || done != "Cut off " {
		t.Fatalf("streamed %q, done %q; want both %q", streamed, done, "Cut off ")
	}
}

func TestMapCitationOffset(t *testing.T) {
	// Runes 2-4 became one rune and rune 8 became three.
	edits := []citationEdit{{start: 2, end: 5, length: 1}, {start: 8, end: 9, length: 3}}
	for _, tt := range []struct {
		offset, wantStart, wantEnd int
	}{
		{0, 0, 0},
		{2, 2, 2},
		{3, 2, 3},
		{5, 3, 3},
		{6, 4, 4},
		{8, 6, 6},
		{9, 9, 9},
		{12, 12, 12},
	} {
		if got := mapCitationOffset(tt.offset, edits, false); got != tt.wantStart {
			t.Fatalf("mapCitationOffset(%d, start) = %d, want %d", tt.offset, got, tt.wantStart)
		}
		if got := mapCitationOffset(tt.offset, edits, true); got != tt.wantEnd {
			t.Fatalf("mapCitationOffset(%d, end) = %d, want %d", tt.offset, got, tt.wantEnd)
		}
	}
}

func citationTestRange(t *testing.T, event sseEvent, path ...string) [2]int {
	t.Helper()
	annotation, ok := citationTestEventValue(t, event, path...).(map[string]any)
	if !ok {
		t.Fatalf("event %q path %v is not an annotation", event.Event, path)
	}
	start, _ := annotation["start_index"].(float64)
	end, _ := annotation["end_index"].(float64)
	return [2]int{int(start), int(end)}
}

func TestNormalizeResponsesCitationMarkersRemapsAnnotations(t *testing.T) {
	marker := "citeturn0search9" // unresolved, so removed
	link := "[docs](https://docs.example)"
	full := "Intró " + marker + " see " + link + " end"
	want := "Intró  see " + link + " end"
	encodedFull, _ := json.Marshal(full)
	annotation := func(start, end int) string {
		return fmt.Sprintf(`{"type":"url_citation","url":"https://docs.example","title":"docs","start_index":%d,"end_index":%d}`, start, end)
	}
	linkStart := utf8.RuneCountInString("Intró " + marker + " see ")
	intro := annotation(0, utf8.RuneCountInString("Intró"))
	docs := annotation(linkStart, linkStart+utf8.RuneCountInString(link))
	part := `{"type":"output_text","annotations":[` + intro + `,` + docs + `],"text":` + string(encodedFull) + `}`
	item := `{"id":"msg-1","type":"message","role":"assistant","content":[` + part + `]}`
	annotationAdded := func(index int, annotation string) string {
		return fmt.Sprintf(`{"type":"response.output_text.annotation.added","item_id":"msg-1","output_index":1,"content_index":0,"annotation_index":%d,"annotation":%s}`, index, annotation)
	}
	splitMarker := strings.Index(full, "cite")
	splitLink := strings.Index(full, link)
	stream := responsesItemIDTestStream(
		citationTestDelta(full[:splitMarker]),
		annotationAdded(0, intro),
		citationTestDelta(full[splitMarker:splitLink]),
		citationTestDelta(full[splitLink:]),
		annotationAdded(1, docs),
		`{"type":"response.output_text.done","item_id":"msg-1","output_index":1,"content_index":0,"text":`+string(encodedFull)+`}`,
		`{"type":"response.content_part.done","item_id":"msg-1","output_index":1,"content_index":0,"part":`+part+`}`,
		`{"type":"response.completed","response":{"id":"resp-1","output":[`+item+`]}}`,
	)

	req, resp := citationTestResponse(t, stream)
	events := citationTestEvents(t, req, resp)
	if len(events) != 8 {
		t.Fatalf("events = %d, want 8", len(events))
	}
	streamed := citationTestEventField(t, events[0], "delta") + citationTestEventField(t, events[2], "delta") + citationTestEventField(t, events[3], "delta")
	if streamed != want {
		t.Fatalf("streamed %q, want %q", streamed, want)
	}
	if events[1].Data != annotationAdded(0, intro) {
		t.Fatalf("annotation before the marker changed: %s", events[1].Data)
	}
	wantIntro := [2]int{0, utf8.RuneCountInString("Intró")}
	wantDocs := [2]int{utf8.RuneCountInString("Intró  see "), utf8.RuneCountInString("Intró  see " + link)}
	if got := string([]rune(want)[wantDocs[0]:wantDocs[1]]); got != link {
		t.Fatalf("remapped range covers %q, want %q", got, link)
	}
	for _, tt := range []struct {
		name  string
		event sseEvent
		path  []string
		want  [2]int
	}{
		{"annotation.added", events[4], []string{"annotation"}, wantDocs},
		{"content_part.done before marker", events[6], []string{"part", "annotations", "0"}, wantIntro},
		{"content_part.done after marker", events[6], []string{"part", "annotations", "1"}, wantDocs},
		{"response.completed after marker", events[7], []string{"response", "output", "0", "content", "0", "annotations", "1"}, wantDocs},
	} {
		if got := citationTestRange(t, tt.event, tt.path...); got != tt.want {
			t.Fatalf("%s range = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestResponsesOutputTextMustStayExact(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{`{"text":{"format":{"type":"json_schema","name":"answer","schema":{}}}}`, true},
		{`{"text":{"format":{"type":"json_object"}}}`, true},
		{`{"include":["reasoning.encrypted_content","message.output_text.logprobs"]}`, true},
		{`{"text":"bad","include":["message.output_text.logprobs"]}`, true},
		{`{"text":{"format":{"type":"text"}},"instructions":"Never use json_schema or message.output_text.logprobs."}`, false},
		{`{"text":"json_object"}`, false},
		{`{"include":["reasoning.encrypted_content"]}`, false},
		{`{"input":"plain"}`, false},
	} {
		if got := responsesOutputTextMustStayExact([]byte(tt.body)); got != tt.want {
			t.Fatalf("responsesOutputTextMustStayExact(%s) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

func TestNormalizeResponsesCitationMarkersLeavesOtherStreamsUntouched(t *testing.T) {
	stream := responsesItemIDTestStream(citationTestDelta("x " + citationTestPR))
	for _, tt := range []struct {
		name string
		req  func(*http.Request) *http.Request
	}{
		{"no sources", func(req *http.Request) *http.Request {
			return httptest.NewRequest(http.MethodPost, "https://upstream.example/responses", nil)
		}},
		{"other endpoint", func(*http.Request) *http.Request {
			req := httptest.NewRequest(http.MethodPost, "https://upstream.example/chat/completions", nil)
			return withResponsesCitationSources(req, providerEndpointChatCompletions, citationTestRequestBody(t))
		}},
		{"typed chat stream", func(req *http.Request) *http.Request {
			return req.WithContext(context.WithValue(req.Context(), responsesChatStreamContextKey{}, true))
		}},
		{"structured output", func(*http.Request) *http.Request {
			req := httptest.NewRequest(http.MethodPost, "https://upstream.example/responses", nil)
			return withResponsesCitationSources(req, providerEndpointResponses, []byte(`{"text":{"format":{"type":"json_schema","name":"answer","schema":{}}}}`))
		}},
		{"output logprobs", func(*http.Request) *http.Request {
			req := httptest.NewRequest(http.MethodPost, "https://upstream.example/responses", nil)
			return withResponsesCitationSources(req, providerEndpointResponses, []byte(`{"include":["message.output_text.logprobs"]}`))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, resp := citationTestResponse(t, stream)
			req = tt.req(req)
			normalizeResponsesCitationMarkers(req, resp)
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if string(got) != stream {
				t.Fatalf("stream changed: got %q, want %q", got, stream)
			}
		})
	}
}

func TestHandleResponsesRewritesCitationMarkers(t *testing.T) {
	encodedPR, _ := json.Marshal("Draft. " + citationTestPR)
	sseBody := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":"Draft. cite"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":"turn295586view1"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","annotations":[],"text":` + string(encodedPR) + `}]}]}}` + "\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(citationTestRequestBody(t))))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	events := parseSSEEvents(w.Body.String())
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %q", len(events), w.Body.String())
	}
	want := "Draft. [PR #831 \\| orka](https://github.com/orka-agents/orka/pull/831)"
	streamed := citationTestEventField(t, events[0], "delta") + citationTestEventField(t, events[1], "delta")
	completed := citationTestEventField(t, events[2], "response", "output", "0", "content", "0", "text")
	if streamed != want || completed != want {
		t.Fatalf("streamed %q, completed %q; want both %q", streamed, completed, want)
	}
	if !utf8.ValidString(w.Body.String()) {
		t.Fatal("rewritten stream is not valid UTF-8")
	}
}
