package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
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

// citationTitleEscaper and citationURLEscaper keep links intact and from
// splitting a Markdown table cell.
var (
	citationTitleEscaper = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "|", `\|`)
	citationURLEscaper   = strings.NewReplacer("(", "%28", ")", "%29", "|", "%7C")
)

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
	if req == nil || path != providerEndpointResponses || responsesOutputTextMustStayExact(body) {
		return req
	}
	sources := &responsesCitationSources{body: body}
	return req.WithContext(context.WithValue(req.Context(), responsesCitationSourcesContextKey{}, sources))
}

// responsesOutputTextMustStayExact reports structured output, which a Markdown
// link could make invalid JSON, and output logprobs, which describe the
// original tokens. The byte check keeps large replays from being decoded.
func responsesOutputTextMustStayExact(body []byte) bool {
	if !bytes.Contains(body, []byte("json_schema")) && !bytes.Contains(body, []byte("json_object")) &&
		!bytes.Contains(body, []byte("message.output_text.logprobs")) {
		return false
	}
	var request struct {
		Text    json.RawMessage `json:"text"`
		Include json.RawMessage `json:"include"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	var text struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	}
	if json.Unmarshal(request.Text, &text) == nil && (text.Format.Type == "json_schema" || text.Format.Type == "json_object") {
		return true
	}
	var include []string
	return json.Unmarshal(request.Include, &include) == nil && slices.Contains(include, "message.output_text.logprobs")
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
	rewriter := &responsesCitationRewriter{sources: sources, texts: make(map[citationTextKey]*citationTextState)}
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
// Codex code mode returns MCP results as serialized CallToolResult JSON, and
// other serializers may escape the brackets as \u3010.
func appendCitationTexts(texts []string, text string) []string {
	if !strings.Contains(text, "【turn") && !strings.Contains(text, `\u3010turn`) {
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

// citationEdit records that runes [start, end) of the original text became
// length runes of rewritten text.
type citationEdit struct {
	start, end, length int
}

// rewriteCitationMarkers replaces complete cite markers in text and reports
// each replacement. Unless final, a trailing partial marker is returned
// separately so a stream can hold it until the next delta. A final text drops
// it like an unresolved marker, so holding never changes the result.
func rewriteCitationMarkers(text string, final bool, lookup func(string) (citationLink, bool)) (string, string, []citationEdit) {
	if !strings.Contains(text, citationMarkerStart) {
		return text, "", nil
	}
	var out strings.Builder
	var edits []citationEdit
	offset := 0
	for {
		i := strings.Index(text, citationMarkerStart)
		if i < 0 {
			out.WriteString(text)
			return out.String(), "", edits
		}
		out.WriteString(text[:i])
		offset += utf8.RuneCountInString(text[:i])
		text = text[i:]
		refs, n, complete := parseCitationMarker(text)
		switch {
		case complete:
			links := formatCitationLinks(refs, lookup)
			out.WriteString(links)
			length := utf8.RuneCountInString(text[:n])
			edits = append(edits, citationEdit{start: offset, end: offset + length, length: utf8.RuneCountInString(links)})
			offset += length
			text = text[n:]
		case n > 0 && final:
			edits = append(edits, citationEdit{start: offset, end: offset + utf8.RuneCountInString(text)})
			return out.String(), "", edits
		case n > 0:
			return out.String(), text, edits
		default:
			out.WriteString(citationMarkerStart)
			offset++
			text = text[len(citationMarkerStart):]
		}
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
		links = append(links, fmt.Sprintf("[%s](%s)", title, citationURLEscaper.Replace(link.url)))
	}
	return strings.Join(links, ", ")
}

// mapCitationOffset moves a rune offset in the original text to the rewritten
// text. An offset inside a replaced marker moves to the replacement's end if
// it ends a range, else to its start.
func mapCitationOffset(offset int, edits []citationEdit, rangeEnd bool) int {
	shift := 0
	for _, edit := range edits {
		if offset <= edit.start {
			break
		}
		if offset < edit.end {
			if rangeEnd {
				return edit.start + shift + edit.length
			}
			return edit.start + shift
		}
		shift += edit.length - (edit.end - edit.start)
	}
	return offset + shift
}

// remapCitationAnnotation keeps an annotation's range, such as a url_citation's
// start_index and end_index, on the same text after markers are rewritten.
// Offsets are counted in code points.
func remapCitationAnnotation(raw json.RawMessage, edits []citationEdit) (json.RawMessage, bool) {
	var annotation map[string]json.RawMessage
	if len(edits) == 0 || json.Unmarshal(raw, &annotation) != nil || annotation == nil {
		return raw, false
	}
	changed := false
	for _, field := range []struct {
		name     string
		rangeEnd bool
	}{{"start_index", false}, {"end_index", true}} {
		var offset int
		if json.Unmarshal(annotation[field.name], &offset) != nil {
			continue
		}
		if mapped := mapCitationOffset(offset, edits, field.rangeEnd); mapped != offset {
			annotation[field.name], _ = json.Marshal(mapped)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	rewritten, err := json.Marshal(annotation)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

func remapCitationAnnotations(raw json.RawMessage, edits []citationEdit) (json.RawMessage, bool) {
	var annotations []json.RawMessage
	if len(edits) == 0 || json.Unmarshal(raw, &annotations) != nil {
		return raw, false
	}
	changed := false
	for i, annotation := range annotations {
		if rewritten, annotationChanged := remapCitationAnnotation(annotation, edits); annotationChanged {
			annotations[i] = rewritten
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	rewritten, err := json.Marshal(annotations)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

type citationTextKey struct {
	outputIndex  int
	contentIndex int
}

// citationTextState tracks one streamed output text. consumed counts the
// original runes before held, and edits use original-text offsets.
type citationTextState struct {
	held     string
	consumed int
	edits    []citationEdit
}

type responsesCitationRewriter struct {
	sources *responsesCitationSources
	texts   map[citationTextKey]*citationTextState
}

func (r *responsesCitationRewriter) rewriteEvent(raw []byte) ([]byte, bool) {
	// Text events always pass so streamed annotation offsets can be remapped.
	if !containsCitationMarkerStart(raw) && !bytes.Contains(raw, []byte("response.output_text.")) {
		return raw, false
	}
	return rewriteResponsesSSEEventData(raw, r.rewrite)
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
		if state := r.textState(event); state != nil {
			// The final text drops a held partial marker too.
			state.held = ""
		}
		_, changed = rewriteCitationJSONString(event, "text", r.sources.lookup)
	case "response.output_text.annotation.added":
		changed = r.remapStreamedAnnotation(event)
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

func (r *responsesCitationRewriter) textState(event map[string]json.RawMessage) *citationTextState {
	key, ok := citationEventTextKey(event)
	if !ok {
		return nil
	}
	state := r.texts[key]
	if state == nil {
		state = &citationTextState{}
		r.texts[key] = state
	}
	return state
}

func (r *responsesCitationRewriter) rewriteDelta(event map[string]json.RawMessage) bool {
	state := r.textState(event)
	var delta string
	if state == nil || json.Unmarshal(event["delta"], &delta) != nil {
		return false
	}
	held := state.held
	text := held + delta
	out, nextHeld, edits := rewriteCitationMarkers(text, false, r.sources.lookup)
	for _, edit := range edits {
		edit.start += state.consumed
		edit.end += state.consumed
		state.edits = append(state.edits, edit)
	}
	state.consumed += utf8.RuneCountInString(text) - utf8.RuneCountInString(nextHeld)
	state.held = nextHeld
	if held == "" && out == delta {
		return false
	}
	event["delta"], _ = json.Marshal(out)
	return true
}

// remapStreamedAnnotation remaps an annotation with the edits made so far to
// its output text.
func (r *responsesCitationRewriter) remapStreamedAnnotation(event map[string]json.RawMessage) bool {
	state := r.textState(event)
	if state == nil {
		return false
	}
	annotation, changed := remapCitationAnnotation(event["annotation"], state.edits)
	if changed {
		event["annotation"] = annotation
	}
	return changed
}

func rewriteCitationJSONString(object map[string]json.RawMessage, field string, lookup func(string) (citationLink, bool)) ([]citationEdit, bool) {
	var text string
	if json.Unmarshal(object[field], &text) != nil {
		return nil, false
	}
	rewritten, _, edits := rewriteCitationMarkers(text, true, lookup)
	if rewritten == text {
		return nil, false
	}
	object[field], _ = json.Marshal(rewritten)
	return edits, true
}

func rewriteCitationOutputTextPart(raw json.RawMessage, lookup func(string) (citationLink, bool)) (json.RawMessage, bool) {
	var part map[string]json.RawMessage
	if json.Unmarshal(raw, &part) != nil || rawJSONString(part["type"]) != "output_text" {
		return raw, false
	}
	edits, changed := rewriteCitationJSONString(part, "text", lookup)
	if !changed {
		return raw, false
	}
	if annotations, annotationsChanged := remapCitationAnnotations(part["annotations"], edits); annotationsChanged {
		part["annotations"] = annotations
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
