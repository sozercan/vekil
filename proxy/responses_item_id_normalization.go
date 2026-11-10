package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

type responsesItemIDNormalizer struct {
	byOutputIndex map[int]string
}

func normalizeCopilotResponsesItemIDs(resp *http.Response, endpoint string) {
	if endpoint != providerEndpointResponses {
		return
	}
	normalizer := responsesItemIDNormalizer{byOutputIndex: make(map[int]string)}
	wrapResponsesSSEBody(resp, func(raw []byte) ([]byte, bool) {
		return rewriteResponsesSSEEventData(raw, normalizer.rewrite)
	})
}

// wrapResponsesSSEBody rewrites each successful Responses SSE event in place.
// Oversized events and lines pass through unchanged.
func wrapResponsesSSEBody(resp *http.Response, rewriteEvent func([]byte) ([]byte, bool)) {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return
	}
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream") {
		return
	}
	resp.Body = &responsesSSERewriteBody{
		source:       resp.Body,
		reader:       bufio.NewReaderSize(resp.Body, openAIStreamScannerInitialBuffer),
		rewriteEvent: rewriteEvent,
	}
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
}

type responsesSSERewriteBody struct {
	source       io.ReadCloser
	reader       *bufio.Reader
	rewriteEvent func([]byte) ([]byte, bool)
	pending      []byte
	pendingErr   error
	rawEvent     bool
	passthrough  bool
	closeOnce    sync.Once
	closeErr     error
}

func (b *responsesSSERewriteBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.pendingErr != nil {
			err := b.pendingErr
			b.pendingErr = nil
			return 0, err
		}
		if b.passthrough {
			return b.reader.Read(p)
		}
		next, err := b.readNext()
		b.pending = next
		b.pendingErr = err
		if len(next) == 0 && err == nil {
			continue
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *responsesSSERewriteBody) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.source.Close()
	})
	return b.closeErr
}

func (b *responsesSSERewriteBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	return routeAttemptTransportOwnership(b.source)
}

func (b *responsesSSERewriteBody) cancelRouteAttempt() {
	cancelRouteAttemptBody(b.source)
}

func (b *responsesSSERewriteBody) canceledAtFailure() bool {
	if observed, ok := b.source.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}

func (b *responsesSSERewriteBody) readNext() ([]byte, error) {
	if b.rawEvent {
		line, err := readOpenAISSELine(b.reader)
		if strings.TrimRight(line, "\r\n") == "" {
			b.rawEvent = false
		}
		if errors.Is(err, errOpenAISSELineTooLong) {
			b.passthrough = true
			err = nil
		}
		return []byte(line), err
	}

	var event bytes.Buffer
	for {
		line, err := readOpenAISSELine(b.reader)
		if len(line) > 0 {
			_, _ = event.WriteString(line)
			boundary := strings.TrimRight(line, "\r\n") == ""
			if event.Len() > openAIStreamScannerMaxBuffer {
				b.rawEvent = !boundary
				if errors.Is(err, errOpenAISSELineTooLong) {
					b.passthrough = true
					err = nil
				}
				return event.Bytes(), err
			}
			if boundary {
				rewritten, changed := b.rewriteEvent(event.Bytes())
				if changed {
					return rewritten, err
				}
				return event.Bytes(), err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if event.Len() == 0 {
					return nil, io.EOF
				}
				rewritten, changed := b.rewriteEvent(event.Bytes())
				if changed {
					return rewritten, io.EOF
				}
				return event.Bytes(), io.EOF
			}
			if errors.Is(err, errOpenAISSELineTooLong) {
				b.passthrough = true
				return event.Bytes(), nil
			}
			return event.Bytes(), err
		}
	}
}

func rewriteResponsesSSEEventData(raw []byte, rewrite func(data []byte, eventName string) ([]byte, bool)) ([]byte, bool) {
	lines := splitSSEEventLines(raw)
	if len(lines) == 0 {
		return raw, false
	}
	dataParts := make([]string, 0, 1)
	firstData := -1
	eventName := ""
	for i, line := range lines {
		content, _ := splitSSELineEnding(line)
		if strings.HasPrefix(content, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(content, "event:"))
		}
		if data, ok := parseSSELine(content); ok {
			if firstData < 0 {
				firstData = i
			}
			dataParts = append(dataParts, data)
		}
	}
	if firstData < 0 || len(dataParts) == 0 {
		return raw, false
	}
	data := strings.Join(dataParts, "\n")
	if strings.TrimSpace(data) == "[DONE]" {
		return raw, false
	}
	rewritten, changed := rewrite([]byte(data), eventName)
	if !changed {
		return raw, false
	}

	var out strings.Builder
	inserted := false
	for _, line := range lines {
		content, ending := splitSSELineEnding(line)
		if _, ok := parseSSELine(content); ok {
			if inserted {
				continue
			}
			if ending == "" {
				ending = "\n"
			}
			for _, part := range strings.Split(string(rewritten), "\n") {
				out.WriteString("data: ")
				out.WriteString(part)
				out.WriteString(ending)
			}
			inserted = true
			continue
		}
		out.WriteString(line)
	}
	return []byte(out.String()), true
}

func (n *responsesItemIDNormalizer) rewrite(data []byte, eventName string) ([]byte, bool) {
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
	outputIndex, hasOutputIndex := responsesEventOutputIndex(event)
	if eventType == "response.output_item.added" && hasOutputIndex {
		if id := responsesEventItemID(event["item"]); id != "" {
			n.byOutputIndex[outputIndex] = id
		}
	}

	changed := false
	if stableID := n.byOutputIndex[outputIndex]; hasOutputIndex && stableID != "" {
		if _, present := event["item_id"]; present {
			encodedID, _ := json.Marshal(stableID)
			if !bytes.Equal(event["item_id"], encodedID) {
				event["item_id"] = encodedID
				changed = true
			}
		}
		if item, itemChanged := rewriteResponsesItemID(event["item"], stableID); itemChanged {
			event["item"] = item
			changed = true
		}
	}
	if response, responseChanged := n.rewriteResponseOutput(event["response"]); responseChanged {
		event["response"] = response
		changed = true
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

func responsesEventOutputIndex(event map[string]json.RawMessage) (int, bool) {
	raw, ok := event["output_index"]
	if !ok {
		return 0, false
	}
	var outputIndex int
	if json.Unmarshal(raw, &outputIndex) != nil || outputIndex < 0 {
		return 0, false
	}
	return outputIndex, true
}

func responsesEventItemID(raw json.RawMessage) string {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	return rawJSONString(item["id"])
}

func rewriteResponsesItemID(raw json.RawMessage, stableID string) (json.RawMessage, bool) {
	if len(raw) == 0 || stableID == "" {
		return raw, false
	}
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || item == nil {
		return raw, false
	}
	encodedID, _ := json.Marshal(stableID)
	if bytes.Equal(item["id"], encodedID) {
		return raw, false
	}
	item["id"] = encodedID
	rewritten, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

func (n *responsesItemIDNormalizer) rewriteResponseOutput(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil || response == nil {
		return raw, false
	}
	var output []json.RawMessage
	if json.Unmarshal(response["output"], &output) != nil {
		return raw, false
	}
	changed := false
	for outputIndex, item := range output {
		stableID := n.byOutputIndex[outputIndex]
		if stableID == "" {
			continue
		}
		if rewritten, itemChanged := rewriteResponsesItemID(item, stableID); itemChanged {
			output[outputIndex] = rewritten
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return raw, false
	}
	response["output"] = encodedOutput
	rewritten, err := json.Marshal(response)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}
