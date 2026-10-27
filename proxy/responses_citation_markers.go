package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// GPT models trained on ChatGPT browsing sometimes cite with private-use
// markers such as "\ue200cite\ue202turn3view0\ue201" instead of Markdown.
// ChatGPT renders them; Responses clients such as Codex print them raw.
const (
	citationMarkerStart    = "\ue200"
	citationMarkerSep      = '\ue202'
	citationMarkerEnd      = '\ue201'
	citationMarkerMaxBytes = 1024
)

// citationSourceHeader matches a ChatGPT browsing result header, as returned
// by Codex's codex_apps web_run tool: "Title (URL)" followed by "【turnNviewN】".
var citationSourceHeader = regexp.MustCompile(`(?m)^([^\n]*) \((https?://[^\s]+)\)\n【(turn\d+[a-z_]+\d+)】`)

// citationTitleEscaper keeps titles from closing the link text or splitting a
// Markdown table cell.
var citationTitleEscaper = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "|", `\|`)

type responsesCitationSourcesContextKey struct{}

type citationLink struct {
	title string
	url   string
}

// responsesCitationSources resolves cited ref IDs from browsing results in
// the request input. It parses the body only once a marker appears.
type responsesCitationSources struct {
	once  sync.Once
	body  []byte
	links map[string]citationLink
}

func withResponsesCitationSources(req *http.Request, path string, body []byte) *http.Request {
	if req == nil || path != providerEndpointResponses {
		return req
	}
	sources := &responsesCitationSources{body: body}
	return req.WithContext(context.WithValue(req.Context(), responsesCitationSourcesContextKey{}, sources))
}

// normalizeResponsesCitationMarkers rewrites cite markers in a successful
// Responses passthrough stream to Markdown links. Unresolved markers are
// removed; other private-use markers are left unchanged.
func normalizeResponsesCitationMarkers(req *http.Request, resp *http.Response) {
	if req == nil {
		return
	}
	sources, _ := req.Context().Value(responsesCitationSourcesContextKey{}).(*responsesCitationSources)
	if sources == nil {
		return
	}
	// The typed Chat adapter consumes Responses events directly.
	if responsesChat, _ := req.Context().Value(responsesChatStreamContextKey{}).(bool); responsesChat {
		return
	}
	rewriter := &responsesCitationRewriter{sources: sources, held: make(map[citationTextKey]string)}
	wrapResponsesSSEBody(resp, rewriter.rewriteEvent)
}

func (s *responsesCitationSources) lookup(ref string) (citationLink, bool) {
	s.once.Do(func() {
		s.links = parseResponsesCitationSources(s.body)
		s.body = nil
	})
	link, ok := s.links[ref]
	return link, ok
}

func parseResponsesCitationSources(body []byte) map[string]citationLink {
	if !bytes.Contains(body, []byte("【turn")) && !bytes.Contains(body, []byte(`\u3010turn`)) {
		return nil
	}
	var request struct {
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	var items []struct {
		Type   string          `json:"type"`
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(request.Input, &items) != nil {
		return nil
	}
	links := make(map[string]citationLink)
	for _, item := range items {
		if item.Type != "function_call_output" && item.Type != "custom_tool_call_output" {
			continue
		}
		for _, text := range citationToolOutputTexts(item.Output) {
			for _, match := range citationSourceHeader.FindAllStringSubmatch(text, -1) {
				links[match[3]] = citationLink{title: strings.TrimSpace(match[1]), url: match[2]}
			}
		}
	}
	return links
}

func citationToolOutputTexts(raw json.RawMessage) []string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return appendCitationTexts(nil, text)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var texts []string
	for _, part := range parts {
		texts = appendCitationTexts(texts, part.Text)
	}
	return texts
}

// appendCitationTexts adds text plus any browsing results JSON-encoded in it.
// Codex code mode returns MCP results as serialized CallToolResult JSON.
func appendCitationTexts(texts []string, text string) []string {
	if !strings.Contains(text, "【turn") {
		return texts
	}
	texts = append(texts, text)
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return texts
	}
	var value any
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return texts
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case string:
			if strings.Contains(value, "【turn") {
				texts = append(texts, value)
			}
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(value)
	return texts
}

// rewriteCitationMarkers replaces complete cite markers in text. Unless final,
// a trailing partial marker is returned separately so a stream can hold it
// until the next delta; holding never changes the concatenated result.
func rewriteCitationMarkers(text string, final bool, lookup func(string) (citationLink, bool)) (string, string) {
	if !strings.Contains(text, citationMarkerStart) {
		return text, ""
	}
	var out strings.Builder
	for {
		i := strings.Index(text, citationMarkerStart)
		if i < 0 {
			out.WriteString(text)
			return out.String(), ""
		}
		out.WriteString(text[:i])
		text = text[i:]
		refs, n, complete := parseCitationMarker(text)
		if complete {
			out.WriteString(formatCitationLinks(refs, lookup))
			text = text[n:]
			continue
		}
		if n > 0 && !final {
			return out.String(), text
		}
		out.WriteString(citationMarkerStart)
		text = text[len(citationMarkerStart):]
	}
}

// parseCitationMarker reads "\ue200cite(\ue202REF)+\ue201" at the start of
// text. It reports the marker length when complete, len(text) when text is a
// bounded prefix of one, and 0 when text cannot start a cite marker.
func parseCitationMarker(text string) ([]string, int, bool) {
	rest := text[len(citationMarkerStart):]
	if !strings.HasPrefix(rest, "cite") {
		if len(rest) < len("cite") && strings.HasPrefix("cite", rest) {
			return nil, len(text), false
		}
		return nil, 0, false
	}
	var refs []string
	// refStart is the byte offset of the current ref, or -1 before the first
	// separator.
	refStart := -1
	offset := len(citationMarkerStart) + len("cite")
	for i, r := range text[offset:] {
		pos := offset + i
		if pos >= citationMarkerMaxBytes {
			return nil, 0, false
		}
		switch {
		case refStart >= 0 && citationRefRune(r):
		case refStart == pos:
			// A separator or end marker needs a nonempty ref before it.
			return nil, 0, false
		case r == citationMarkerSep:
			if refStart >= 0 {
				refs = append(refs, text[refStart:pos])
			}
			refStart = pos + utf8.RuneLen(r)
		case r == citationMarkerEnd && refStart >= 0:
			return append(refs, text[refStart:pos]), pos + utf8.RuneLen(r), true
		default:
			return nil, 0, false
		}
	}
	return nil, len(text), false
}

func citationRefRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
}

func formatCitationLinks(refs []string, lookup func(string) (citationLink, bool)) string {
	var links []string
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		link, ok := lookup(ref)
		if !ok || seen[link.url] {
			continue
		}
		seen[link.url] = true
		title := link.title
		if title == "" {
			if parsed, err := url.Parse(link.url); err == nil && parsed.Host != "" {
				title = parsed.Host
			} else {
				title = link.url
			}
		}
		title = citationTitleEscaper.Replace(title)
		destination := strings.NewReplacer("(", "%28", ")", "%29").Replace(link.url)
		links = append(links, fmt.Sprintf("[%s](%s)", title, destination))
	}
	return strings.Join(links, ", ")
}

type citationTextKey struct {
	outputIndex  int
	contentIndex int
}

type responsesCitationRewriter struct {
	sources *responsesCitationSources
	held    map[citationTextKey]string
	flush   []byte
}

func (r *responsesCitationRewriter) rewriteEvent(raw []byte) ([]byte, bool) {
	if len(r.held) == 0 && !containsCitationMarkerStart(raw) {
		return raw, false
	}
	rewritten, changed := rewriteResponsesSSEEventData(raw, r.rewrite)
	if r.flush == nil {
		return rewritten, changed
	}
	out := append(r.flush, rewritten...)
	r.flush = nil
	return out, true
}

func containsCitationMarkerStart(raw []byte) bool {
	return bytes.Contains(raw, []byte(citationMarkerStart)) || bytes.Contains(raw, []byte(`\ue200`)) || bytes.Contains(raw, []byte(`\uE200`))
}

func (r *responsesCitationRewriter) rewrite(data []byte, eventName string) ([]byte, bool) {
	// Preserve ambiguous bytes for the downstream durable state validator.
	if validateUnambiguousResponsesJSON(data) != nil {
		return data, false
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil || event == nil {
		return data, false
	}
	eventType := rawJSONString(event["type"])
	if eventType == "" {
		eventType = eventName
	}
	changed := false
	switch eventType {
	case "response.output_text.delta":
		changed = r.rewriteDelta(event)
	case "response.output_text.done":
		r.flushHeld(event)
		changed = rewriteCitationJSONString(event, "text", r.sources.lookup)
	}
	for _, field := range []struct {
		name    string
		rewrite func(json.RawMessage, func(string) (citationLink, bool)) (json.RawMessage, bool)
	}{
		{"part", rewriteCitationOutputTextPart},
		{"item", rewriteCitationMessageItem},
		{"response", rewriteCitationResponseOutput},
	} {
		if raw, ok := event[field.name]; ok {
			if rewritten, fieldChanged := field.rewrite(raw, r.sources.lookup); fieldChanged {
				event[field.name] = rewritten
				changed = true
			}
		}
	}
	if !changed {
		return data, false
	}
	rewritten, err := json.Marshal(event)
	if err != nil {
		return data, false
	}
	return rewritten, true
}

func citationEventTextKey(event map[string]json.RawMessage) (citationTextKey, bool) {
	outputIndex, ok := responsesEventOutputIndex(event)
	if !ok {
		return citationTextKey{}, false
	}
	var contentIndex int
	if raw, present := event["content_index"]; present && json.Unmarshal(raw, &contentIndex) != nil {
		return citationTextKey{}, false
	}
	return citationTextKey{outputIndex: outputIndex, contentIndex: contentIndex}, true
}

func (r *responsesCitationRewriter) rewriteDelta(event map[string]json.RawMessage) bool {
	key, ok := citationEventTextKey(event)
	if !ok {
		return false
	}
	var delta string
	if json.Unmarshal(event["delta"], &delta) != nil {
		return false
	}
	held := r.held[key]
	out, nextHeld := rewriteCitationMarkers(held+delta, false, r.sources.lookup)
	if nextHeld == "" {
		delete(r.held, key)
	} else {
		r.held[key] = nextHeld
	}
	if held == "" && out == delta {
		return false
	}
	event["delta"], _ = json.Marshal(out)
	return true
}

// flushHeld emits a partial marker that the text ended with as one more delta
// before the done event, so deltas still add up to the final text.
func (r *responsesCitationRewriter) flushHeld(event map[string]json.RawMessage) {
	key, ok := citationEventTextKey(event)
	if !ok || r.held[key] == "" {
		return
	}
	out, _ := rewriteCitationMarkers(r.held[key], true, r.sources.lookup)
	delete(r.held, key)
	delta := make(map[string]json.RawMessage, len(event))
	for _, name := range []string{"item_id", "output_index", "content_index"} {
		if raw, present := event[name]; present {
			delta[name] = raw
		}
	}
	delta["type"], _ = json.Marshal("response.output_text.delta")
	delta["delta"], _ = json.Marshal(out)
	encoded, err := json.Marshal(delta)
	if err != nil {
		return
	}
	r.flush = append(r.flush, "event: response.output_text.delta\ndata: "...)
	r.flush = append(r.flush, encoded...)
	r.flush = append(r.flush, "\n\n"...)
}

func rewriteCitationJSONString(object map[string]json.RawMessage, field string, lookup func(string) (citationLink, bool)) bool {
	var text string
	if json.Unmarshal(object[field], &text) != nil {
		return false
	}
	rewritten, _ := rewriteCitationMarkers(text, true, lookup)
	if rewritten == text {
		return false
	}
	object[field], _ = json.Marshal(rewritten)
	return true
}

func rewriteCitationOutputTextPart(raw json.RawMessage, lookup func(string) (citationLink, bool)) (json.RawMessage, bool) {
	var part map[string]json.RawMessage
	if json.Unmarshal(raw, &part) != nil || rawJSONString(part["type"]) != "output_text" {
		return raw, false
	}
	if !rewriteCitationJSONString(part, "text", lookup) {
		return raw, false
	}
	rewritten, err := json.Marshal(part)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

func rewriteCitationMessageItem(raw json.RawMessage, lookup func(string) (citationLink, bool)) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || rawJSONString(item["type"]) != "message" {
		return raw, false
	}
	var content []json.RawMessage
	if json.Unmarshal(item["content"], &content) != nil {
		return raw, false
	}
	changed := false
	for i, part := range content {
		if rewritten, partChanged := rewriteCitationOutputTextPart(part, lookup); partChanged {
			content[i] = rewritten
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	item["content"], _ = json.Marshal(content)
	rewritten, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

func rewriteCitationResponseOutput(raw json.RawMessage, lookup func(string) (citationLink, bool)) (json.RawMessage, bool) {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil || response == nil {
		return raw, false
	}
	var output []json.RawMessage
	if json.Unmarshal(response["output"], &output) != nil {
		return raw, false
	}
	changed := false
	for i, item := range output {
		if rewritten, itemChanged := rewriteCitationMessageItem(item, lookup); itemChanged {
			output[i] = rewritten
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	response["output"], _ = json.Marshal(output)
	rewritten, err := json.Marshal(response)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}
