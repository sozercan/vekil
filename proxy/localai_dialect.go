package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// providerUpstreamDialect names an upstream implementation whose protocol
// quirks Vekil corrects before any surface or route logic sees a response.
type providerUpstreamDialect string

const (
	providerUpstreamDialectNone providerUpstreamDialect = ""
	// providerUpstreamDialectLocalAI is LocalAI (and AIKit images). It drops
	// non-function Responses tools and unknown input items silently, answers a
	// context overflow with HTTP 500, and streams the overflow text as assistant
	// content before an error event.
	providerUpstreamDialectLocalAI providerUpstreamDialect = "localai"
)

// localAIOverflowPeekTimeout bounds how long a stream is held while Vekil
// checks for LocalAI's streamed overflow error. LocalAI reports an overflow
// only after rendering and tokenizing the prompt, which takes seconds for a
// Codex-sized tool catalog, so the stream is held until its first meaningful
// event. LocalAI sends nothing else during prefill, so a client waits the same
// time either way; the bound matches common client idle timeouts.
var localAIOverflowPeekTimeout = 5 * time.Minute

const (
	// localAIOverflowPeekBytes bounds the held prefix. LocalAI's
	// response.created and response.in_progress events echo the whole request,
	// including every tool schema, so a Codex catalog makes them large.
	localAIOverflowPeekBytes = 16 << 20
	localAIErrorBodyLimit    = 64 << 10
)

func configuredProviderUpstreamDialect(kind providerType, value string) (providerUpstreamDialect, error) {
	switch providerUpstreamDialect(strings.TrimSpace(value)) {
	case providerUpstreamDialectNone:
		return providerUpstreamDialectNone, nil
	case providerUpstreamDialectLocalAI:
		if kind != providerTypeOpenAICompatible {
			return "", fmt.Errorf("upstream_dialect %q is only supported for openai-compatible providers", value)
		}
		return providerUpstreamDialectLocalAI, nil
	default:
		return "", fmt.Errorf("unsupported upstream_dialect %q; use localai", value)
	}
}

// contextOverflow is an upstream refusal of a prompt longer than the model's
// context window.
type contextOverflow struct {
	promptTokens  int64
	contextTokens int64
	detail        string
}

var (
	llamaCPPOverflowPattern = regexp.MustCompile(`request \((\d+) tokens\) exceeds the available context size \((\d+) tokens\)`)
	openAIOverflowPattern   = regexp.MustCompile(`(?i)maximum context length is (\d+) tokens.*?(?:resulted in|requested|has|contains) (\d+)`)
	anthropicOverflow       = regexp.MustCompile(`(?i)prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)`)
)

// parseContextOverflowMessage recognizes context-overflow wording from
// llama.cpp, LocalAI, vLLM, OpenAI, and Anthropic.
func parseContextOverflowMessage(message string) (contextOverflow, bool) {
	if match := llamaCPPOverflowPattern.FindStringSubmatch(message); match != nil {
		return contextOverflow{promptTokens: parseTokenCount(match[1]), contextTokens: parseTokenCount(match[2]), detail: match[0]}, true
	}
	if match := openAIOverflowPattern.FindStringSubmatch(message); match != nil {
		return contextOverflow{promptTokens: parseTokenCount(match[2]), contextTokens: parseTokenCount(match[1]), detail: strings.TrimSpace(message)}, true
	}
	if match := anthropicOverflow.FindStringSubmatch(message); match != nil {
		return contextOverflow{promptTokens: parseTokenCount(match[1]), contextTokens: parseTokenCount(match[2]), detail: strings.TrimSpace(message)}, true
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "exceeds the available context size") {
		return contextOverflow{detail: strings.TrimSpace(message)}, true
	}
	if strings.Contains(lower, "exceed") {
		for _, phrase := range []string{"context length", "context window", "context size", "maximum context"} {
			if strings.Contains(lower, phrase) {
				return contextOverflow{detail: strings.TrimSpace(message)}, true
			}
		}
	}
	return contextOverflow{}, false
}

func parseTokenCount(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

// parseUpstreamContextOverflow recognizes an error body whose code, type, or
// message reports a context overflow. It accepts numeric codes, which
// llama.cpp and LocalAI send.
func parseUpstreamContextOverflow(statusCode int, body []byte) (contextOverflow, bool) {
	switch statusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusInternalServerError:
	default:
		return contextOverflow{}, false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return parseContextOverflowMessage(string(body))
	}
	fields := envelope
	if rawError, ok := envelope["error"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(rawError, &nested) == nil {
			fields = nested
		} else {
			var message string
			if json.Unmarshal(rawError, &message) == nil {
				return parseContextOverflowMessage(message)
			}
		}
	}
	message := jsonStringField(fields, "message")
	if message == "" {
		message = jsonStringField(fields, "detail")
	}
	overflow, matched := parseContextOverflowMessage(message)
	code := strings.ToLower(jsonStringField(fields, "code"))
	errType := strings.ToLower(jsonStringField(fields, "type"))
	switch {
	case matched:
	case code == "context_length_exceeded" || code == "model_max_prompt_tokens_exceeded" || code == "max_prompt_tokens_exceeded":
		overflow = contextOverflow{detail: message}
	case errType == "exceed_context_size_error":
		overflow = contextOverflow{detail: message}
	default:
		return contextOverflow{}, false
	}
	if overflow.promptTokens == 0 {
		overflow.promptTokens = jsonIntField(fields, "n_prompt_tokens")
	}
	if overflow.contextTokens == 0 {
		overflow.contextTokens = jsonIntField(fields, "n_ctx")
	}
	return overflow, true
}

func jsonStringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

func jsonIntField(fields map[string]json.RawMessage, key string) int64 {
	raw, ok := fields[key]
	if !ok {
		return 0
	}
	var value int64
	if json.Unmarshal(raw, &value) == nil && value > 0 {
		return value
	}
	return 0
}

// openAIMessage renders the overflow in OpenAI's wording, which clients parse.
func (o contextOverflow) openAIMessage() string {
	if o.contextTokens > 0 && o.promptTokens > 0 {
		return fmt.Sprintf("This model's maximum context length is %d tokens. However, your messages resulted in %d tokens. Please reduce the length of the messages.", o.contextTokens, o.promptTokens)
	}
	if o.contextTokens > 0 {
		return fmt.Sprintf("This model's maximum context length is %d tokens. Please reduce the length of the messages.", o.contextTokens)
	}
	if o.detail != "" {
		return "The request exceeds the model's context window: " + o.detail
	}
	return "The request exceeds the model's context window. Please reduce the length of the messages."
}

// anthropicMessage renders the overflow in the wording Claude Code parses to
// trigger compaction: "prompt is too long: N tokens > M maximum".
func (o contextOverflow) anthropicMessage() string {
	if o.contextTokens > 0 && o.promptTokens > 0 {
		return fmt.Sprintf("prompt is too long: %d tokens > %d maximum", o.promptTokens, o.contextTokens)
	}
	return "prompt is too long: " + o.openAIMessage()
}

func (o contextOverflow) openAIErrorBody(param string) []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": o.openAIMessage(),
			"type":    "invalid_request_error",
			"param":   param,
			"code":    "context_length_exceeded",
		},
	})
	return body
}

type upstreamDialectContextKey struct{}

type upstreamDialectRequest struct {
	dialect     providerUpstreamDialect
	endpoint    string
	toolAliases localAIToolAliases
}

// withProviderUpstreamDialect marks an inference request so the send path can
// normalize the provider's response.
func withProviderUpstreamDialect(req *http.Request, provider *providerRuntime, endpoint string, toolAliases localAIToolAliases) *http.Request {
	if req == nil || provider == nil || provider.dialect == providerUpstreamDialectNone {
		return req
	}
	marker := upstreamDialectRequest{dialect: provider.dialect, endpoint: endpoint, toolAliases: toolAliases}
	return req.WithContext(context.WithValue(req.Context(), upstreamDialectContextKey{}, marker))
}

// normalizeUpstreamDialectResponse rewrites dialect-specific responses into the
// OpenAI error contract before route classification or surface translation.
func normalizeUpstreamDialectResponse(req *http.Request, resp *http.Response) *http.Response {
	if req == nil || resp == nil || resp.Body == nil {
		return resp
	}
	marker, ok := req.Context().Value(upstreamDialectContextKey{}).(upstreamDialectRequest)
	if !ok || marker.dialect != providerUpstreamDialectLocalAI {
		return resp
	}
	if marker.endpoint != providerEndpointChatCompletions && marker.endpoint != providerEndpointResponses {
		return resp
	}
	param := "messages"
	if marker.endpoint == providerEndpointResponses {
		param = "input"
	}
	if resp.StatusCode != http.StatusOK {
		return normalizeLocalAIErrorResponse(resp, param)
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp = peekLocalAIStreamOverflow(req.Context(), resp, marker.endpoint, param)
	}
	return restoreLocalAIToolAliases(resp, marker.toolAliases)
}

func normalizeLocalAIErrorResponse(resp *http.Response, param string) *http.Response {
	body, err := io.ReadAll(io.LimitReader(resp.Body, localAIErrorBodyLimit+1))
	if err != nil || len(body) > localAIErrorBodyLimit {
		resp.Body = newLocalAIPrefixedBody(body, resp.Body)
		return resp
	}
	_ = resp.Body.Close()
	overflow, ok := parseUpstreamContextOverflow(resp.StatusCode, body)
	if !ok {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp
	}
	return syntheticOverflowResponse(resp, overflow, param)
}

func syntheticOverflowResponse(resp *http.Response, overflow contextOverflow, param string) *http.Response {
	body := overflow.openAIErrorBody(param)
	header := resp.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Content-Type", "application/json")
	header.Del("Content-Encoding")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", http.StatusBadRequest, http.StatusText(http.StatusBadRequest)),
		StatusCode:    http.StatusBadRequest,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       resp.Request,
		TLS:           resp.TLS,
	}
}

// localAIBodyLifecycle delegates route-attempt lifecycle hooks to the upstream
// body a LocalAI wrapper reads from, as other response-body wrappers do.
type localAIBodyLifecycle struct {
	source io.ReadCloser
}

func (l localAIBodyLifecycle) cancelRouteAttempt() { cancelRouteAttemptBody(l.source) }

func (l localAIBodyLifecycle) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	return routeAttemptTransportOwnership(l.source)
}

func (l localAIBodyLifecycle) canceledAtFailure() bool {
	observed, ok := l.source.(interface{ canceledAtFailure() bool })
	return ok && observed.canceledAtFailure()
}

// localAIPrefixedBody replays bytes already read before the rest of a body.
type localAIPrefixedBody struct {
	localAIBodyLifecycle
	reader io.Reader
}

func newLocalAIPrefixedBody(prefix []byte, source io.ReadCloser) *localAIPrefixedBody {
	return &localAIPrefixedBody{localAIBodyLifecycle: localAIBodyLifecycle{source: source}, reader: io.MultiReader(bytes.NewReader(prefix), source)}
}

func (b *localAIPrefixedBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *localAIPrefixedBody) Close() error { return b.source.Close() }

type streamChunk struct {
	data []byte
	err  error
}

// chunkReadCloser reads a body through a goroutine so a caller can stop
// waiting for it at a deadline without losing data.
type chunkReadCloser struct {
	localAIBodyLifecycle
	prefix  []byte
	chunks  <-chan streamChunk
	pending []byte
	err     error
	stop    chan struct{}
	once    sync.Once
}

func newChunkReader(body io.ReadCloser) (*chunkReadCloser, <-chan streamChunk) {
	chunks := make(chan streamChunk, 8)
	reader := &chunkReadCloser{localAIBodyLifecycle: localAIBodyLifecycle{source: body}, chunks: chunks, stop: make(chan struct{})}
	go func() {
		defer close(chunks)
		buf := make([]byte, 16<<10)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				select {
				case chunks <- streamChunk{data: append([]byte(nil), buf[:n]...)}:
				case <-reader.stop:
					return
				}
			}
			if err != nil {
				select {
				case chunks <- streamChunk{err: err}:
				case <-reader.stop:
				}
				return
			}
		}
	}()
	return reader, chunks
}

func (c *chunkReadCloser) Read(buf []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(buf, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	for len(c.pending) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		chunk, ok := <-c.chunks
		if !ok {
			c.err = io.EOF
			continue
		}
		if chunk.err != nil {
			c.err = chunk.err
			continue
		}
		c.pending = chunk.data
	}
	n := copy(buf, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *chunkReadCloser) Close() error {
	var err error
	c.once.Do(func() {
		close(c.stop)
		err = c.source.Close()
	})
	return err
}

// peekLocalAIStreamOverflow holds a stream until its first meaningful event.
// When that event is LocalAI's streamed overflow, the stream is replaced by a
// 400 context_length_exceeded response; otherwise it is replayed unchanged.
func peekLocalAIStreamOverflow(ctx context.Context, resp *http.Response, endpoint, param string) *http.Response {
	reader, chunks := newChunkReader(resp.Body)
	timer := time.NewTimer(localAIOverflowPeekTimeout)
	defer timer.Stop()
	var peeked []byte
	scanned := 0
	for {
		decision, overflow, consumed := classifyLocalAIStreamPrefix(peeked[scanned:], endpoint)
		scanned += consumed
		switch decision {
		case streamPrefixOverflow:
			_ = reader.Close()
			return syntheticOverflowResponse(resp, overflow, param)
		case streamPrefixNormal:
			reader.prefix = peeked
			resp.Body = reader
			return resp
		}
		if len(peeked) >= localAIOverflowPeekBytes {
			reader.prefix = peeked
			resp.Body = reader
			return resp
		}
		select {
		case chunk, ok := <-chunks:
			if !ok || chunk.err != nil {
				if ok {
					reader.err = chunk.err
				} else {
					reader.err = io.EOF
				}
				reader.prefix = peeked
				resp.Body = reader
				return resp
			}
			peeked = append(peeked, chunk.data...)
		case <-timer.C:
			reader.prefix = peeked
			resp.Body = reader
			return resp
		case <-ctx.Done():
			reader.prefix = peeked
			resp.Body = reader
			return resp
		}
	}
}

type streamPrefixDecision int

const (
	streamPrefixUndecided streamPrefixDecision = iota
	streamPrefixNormal
	streamPrefixOverflow
)

// classifyLocalAIStreamPrefix inspects the complete SSE events at the start of
// unscanned and returns how many bytes of complete, undecided events it
// consumed, so callers never re-parse them.
func classifyLocalAIStreamPrefix(unscanned []byte, endpoint string) (streamPrefixDecision, contextOverflow, int) {
	consumed := 0
	for {
		advance, event, _ := splitSSEEvents(unscanned[consumed:], false)
		if advance == 0 {
			return streamPrefixUndecided, contextOverflow{}, consumed
		}
		consumed += advance
		data := sseEventData(event)
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return streamPrefixNormal, contextOverflow{}, consumed
		}
		var decision streamPrefixDecision
		var overflow contextOverflow
		if endpoint == providerEndpointResponses {
			decision, overflow = classifyLocalAIResponsesEvent(data)
		} else {
			decision, overflow = classifyLocalAIChatEvent(data)
		}
		if decision != streamPrefixUndecided {
			return decision, overflow, consumed
		}
	}
}

// splitSSEEvents returns the first complete event, ended by a blank line.
func splitSSEEvents(data []byte, _ bool) (int, []byte, error) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return 0, nil, nil
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf + 4, data[:crlf], nil
	default:
		return lf + 2, data[:lf], nil
	}
}

func sseEventData(event []byte) []byte {
	var data [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	return bytes.Join(data, []byte("\n"))
}

func classifyLocalAIChatEvent(data []byte) (streamPrefixDecision, contextOverflow) {
	var event struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Delta struct {
				Content          *string         `json:"content"`
				ReasoningContent *string         `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &event) != nil {
		return streamPrefixNormal, contextOverflow{}
	}
	if len(event.Error) > 0 {
		if overflow, ok := parseUpstreamContextOverflow(http.StatusInternalServerError, append(append([]byte(`{"error":`), event.Error...), '}')); ok {
			return streamPrefixOverflow, overflow
		}
		return streamPrefixNormal, contextOverflow{}
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.Content != nil && *delta.Content != "" {
			if overflow, ok := localAIStreamedOverflow(*delta.Content); ok {
				return streamPrefixOverflow, overflow
			}
			return streamPrefixNormal, contextOverflow{}
		}
		if (delta.ReasoningContent != nil && *delta.ReasoningContent != "") || len(delta.ToolCalls) > 0 || choice.FinishReason != nil {
			return streamPrefixNormal, contextOverflow{}
		}
	}
	return streamPrefixUndecided, contextOverflow{}
}

func classifyLocalAIResponsesEvent(data []byte) (streamPrefixDecision, contextOverflow) {
	var event struct {
		Type  string          `json:"type"`
		Delta string          `json:"delta"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &event) != nil {
		return streamPrefixNormal, contextOverflow{}
	}
	switch event.Type {
	case "response.created", "response.in_progress", "response.output_item.added", "response.content_part.added":
		return streamPrefixUndecided, contextOverflow{}
	case "response.output_text.delta":
		if overflow, ok := localAIStreamedOverflow(event.Delta); ok {
			return streamPrefixOverflow, overflow
		}
		return streamPrefixNormal, contextOverflow{}
	case "error":
		if overflow, ok := parseUpstreamContextOverflow(http.StatusInternalServerError, append(append([]byte(`{"error":`), event.Error...), '}')); ok {
			return streamPrefixOverflow, overflow
		}
		return streamPrefixNormal, contextOverflow{}
	default:
		return streamPrefixNormal, contextOverflow{}
	}
}

// localAIStreamedOverflow matches the exact llama.cpp overflow text LocalAI
// streams as the first assistant content. Requiring the whole delta to be that
// text keeps a model that merely talks about context sizes from matching.
func localAIStreamedOverflow(content string) (contextOverflow, bool) {
	content = strings.TrimSpace(content)
	match := llamaCPPOverflowPattern.FindStringSubmatchIndex(content)
	if match == nil || match[0] != 0 {
		return contextOverflow{}, false
	}
	rest := strings.TrimSpace(content[match[1]:])
	if rest != "" && rest != ", try increasing it" {
		return contextOverflow{}, false
	}
	return parseContextOverflowMessage(content)
}

// normalizeLocalAIRequest rewrites system and developer messages into the one
// leading system message that local chat templates accept. Qwen-family
// templates, for example, raise "System message must be at the beginning" for
// a second system message or one after the first turn, which Claude Code's
// environment block and Codex's developer messages both produce.
func normalizeLocalAIRequest(body []byte, endpoint string) ([]byte, error) {
	switch endpoint {
	case providerEndpointChatCompletions:
		return normalizeLocalAIChatMessages(body)
	case providerEndpointResponses:
		return normalizeLocalAIResponsesInput(body)
	default:
		return body, nil
	}
}

type localAIMessage struct {
	fields map[string]json.RawMessage
	role   string
}

func decodeLocalAIMessages(raw json.RawMessage) ([]localAIMessage, bool) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	messages := make([]localAIMessage, 0, len(items))
	for _, item := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			return nil, false
		}
		var role string
		_ = json.Unmarshal(fields["role"], &role)
		messages = append(messages, localAIMessage{fields: fields, role: role})
	}
	return messages, true
}

func isSystemRole(role string) bool { return role == "system" || role == "developer" }

// localAIMessageText joins a message's text content, whether a string or an
// array of text parts. ok is false when the content holds non-text parts.
func localAIMessageText(content json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return "", false
	}
	var builder strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			if builder.Len() > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString(part.Text)
		default:
			return "", false
		}
	}
	return builder.String(), true
}

func systemReminder(text string) string {
	return "<system-reminder>\n" + text + "\n</system-reminder>"
}

func normalizeLocalAIChatMessages(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, nil
	}
	messages, ok := decodeLocalAIMessages(payload["messages"])
	if !ok || !localAIMessagesNeedRewrite(messages, false) {
		return body, nil
	}
	var leading []string
	index := 0
	for ; index < len(messages) && isSystemRole(messages[index].role); index++ {
		text, textOK := localAIMessageText(messages[index].fields["content"])
		if !textOK {
			return body, nil
		}
		if text != "" {
			leading = append(leading, text)
		}
	}
	out := make([]map[string]json.RawMessage, 0, len(messages))
	if len(leading) > 0 {
		out = append(out, map[string]json.RawMessage{
			"role":    json.RawMessage(`"system"`),
			"content": mustMarshalJSON(strings.Join(leading, "\n\n")),
		})
	}
	for _, message := range messages[index:] {
		if isSystemRole(message.role) {
			text, textOK := localAIMessageText(message.fields["content"])
			if !textOK {
				return body, nil
			}
			out = append(out, map[string]json.RawMessage{
				"role":    json.RawMessage(`"user"`),
				"content": mustMarshalJSON(systemReminder(text)),
			})
			continue
		}
		out = append(out, message.fields)
	}
	payload["messages"] = mustMarshalJSON(out)
	return json.Marshal(payload)
}

// localAIMessagesNeedRewrite reports whether messages hold more than one
// system message or one after the first non-system message. With
// hasInstructions, any leading system message follows the instructions.
func localAIMessagesNeedRewrite(messages []localAIMessage, hasInstructions bool) bool {
	seenOther := false
	systemCount := 0
	if hasInstructions {
		systemCount = 1
	}
	for _, message := range messages {
		if isSystemRole(message.role) {
			systemCount++
			// Templates that know only the system role may reject developer.
			if seenOther || systemCount > 1 || message.role == "developer" {
				return true
			}
			continue
		}
		seenOther = true
	}
	return false
}

func normalizeLocalAIResponsesInput(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, nil
	}
	messages, ok := decodeLocalAIMessages(payload["input"])
	if !ok {
		return body, nil
	}
	var instructions string
	if raw, present := payload["instructions"]; present {
		if json.Unmarshal(raw, &instructions) != nil {
			return body, nil
		}
	}
	if !localAIMessagesNeedRewrite(messages, instructions != "") {
		return body, nil
	}
	leading := []string{}
	if instructions != "" {
		leading = append(leading, instructions)
	}
	index := 0
	for ; index < len(messages) && isSystemRole(messages[index].role); index++ {
		text, textOK := localAIMessageText(messages[index].fields["content"])
		if !textOK {
			return body, nil
		}
		if text != "" {
			leading = append(leading, text)
		}
	}
	out := make([]map[string]json.RawMessage, 0, len(messages))
	for _, message := range messages[index:] {
		if isSystemRole(message.role) {
			text, textOK := localAIMessageText(message.fields["content"])
			if !textOK {
				return body, nil
			}
			out = append(out, map[string]json.RawMessage{
				"type":    json.RawMessage(`"message"`),
				"role":    json.RawMessage(`"user"`),
				"content": mustMarshalJSON([]map[string]string{{"type": "input_text", "text": systemReminder(text)}}),
			})
			continue
		}
		out = append(out, message.fields)
	}
	if len(leading) > 0 {
		payload["instructions"] = mustMarshalJSON(strings.Join(leading, "\n\n"))
	}
	payload["input"] = mustMarshalJSON(out)
	return json.Marshal(payload)
}

func mustMarshalJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}
