package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type explicitRouteResponseInfo struct {
	routeID    string
	publicID   string
	targetID   string
	providerID string
}

type explicitRouteResponseContextKey struct{}

func withExplicitRouteResponseInfo(ctx context.Context, info explicitRouteResponseInfo) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, explicitRouteResponseContextKey{}, info)
}

func explicitRouteResponseInfoFromResponse(resp *http.Response) (explicitRouteResponseInfo, bool) {
	if resp == nil || resp.Request == nil {
		return explicitRouteResponseInfo{}, false
	}
	info, ok := resp.Request.Context().Value(explicitRouteResponseContextKey{}).(explicitRouteResponseInfo)
	return info, ok && info.publicID != "" && info.targetID != ""
}

func rewriteResponsesResponseModelJSON(body []byte, publicModel string) ([]byte, bool) {
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return body, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	changed := rewriteResponsesModelObject(payload, publicModel)
	if nested, ok := payload["response"]; ok {
		var response map[string]json.RawMessage
		if json.Unmarshal(nested, &response) == nil && rewriteResponsesModelObject(response, publicModel) {
			if raw, err := json.Marshal(response); err == nil {
				payload["response"] = raw
				changed = true
			}
		}
	}
	if !changed {
		return body, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func rewriteResponsesModelObject(payload map[string]json.RawMessage, publicModel string) bool {
	raw, ok := payload["model"]
	if !ok || rawJSONString(raw) == "" {
		return false
	}
	encoded, err := json.Marshal(publicModel)
	if err != nil {
		return false
	}
	payload["model"] = encoded
	return true
}

func writeExplicitResponsesResponse(ctx context.Context, h *ProxyHandler, w http.ResponseWriter, resp *http.Response, info explicitRouteResponseInfo, store *ToolExecutionContextStore, scope string) error {
	if resp == nil || resp.Body == nil {
		return writeUpstreamResponse(w, resp)
	}
	body := newLifecycleAwareReadCloser(resp.Body, responseRequestContext(resp))
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(io.LimitReader(body, maxLargeRequestBodySize+1))
	if body.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, true)
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, false, true, false)
	}
	if len(data) > maxLargeRequestBodySize {
		return newResponseBodyWriteError(resp, errors.New("explicit route response exceeds normalization limit"), false, true, false)
	}

	success := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if success {
		observeResponsesUsage(ctx, sniffResponsesUsageBody(data))
		bodyTokens, tokenErr := extractExplicitResponsesOutputState(data)
		headerTokens, headerErr := explicitResponseHeaderStateTokens(resp.Header)
		if headerErr != nil {
			return newResponseBodyWriteError(resp, headerErr, false, true, false)
		}
		if tokenErr != nil {
			return newResponseBodyWriteError(resp, fmt.Errorf("malformed explicit route responses response: %w", tokenErr), false, true, false)
		}
		allTokens := append(headerTokens, bodyTokens...)
		if bindErr := h.bindExplicitStateTokens(info, allTokens); bindErr != nil {
			return newResponseBodyWriteError(resp, bindErr, false, true, false)
		}
	}
	out := data
	changed := false
	if success && h != nil && h.toolOptimizers != nil && h.toolOptimizers.ShouldInspectNonStreamingResponses() {
		if optimized, optimizedChanged := h.maybeRewriteResponsesResponseBody(ctx, out, store, scope); optimizedChanged {
			out = optimized
			changed = true
		}
	}
	if normalized, normalizedChanged := rewriteResponsesResponseModelJSON(out, info.publicID); normalizedChanged {
		out = normalized
		changed = true
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	if changed || len(out) != len(data) {
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	} else if w.Header().Get("Content-Length") != "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(out); err != nil {
		return newResponseBodyWriteError(resp, err, true, false, false)
	}
	return nil
}

type normalizedResponsesStreamBody struct {
	reader *io.PipeReader
	source io.ReadCloser
	once   sync.Once
}

func (b *normalizedResponsesStreamBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *normalizedResponsesStreamBody) Close() error {
	var err error
	b.once.Do(func() {
		_ = b.reader.CloseWithError(context.Canceled)
		err = b.source.Close()
	})
	return err
}

func normalizeResponsesStreamBody(source io.ReadCloser, publicModel string, onEvent func([]byte) error) io.ReadCloser {
	if source == nil || strings.TrimSpace(publicModel) == "" {
		return source
	}
	pr, pw := io.Pipe()
	wrapped := &normalizedResponsesStreamBody{reader: pr, source: source}
	go func() {
		err := copyNormalizedResponsesSSE(pw, source, publicModel, onEvent)
		_ = source.Close()
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return wrapped
}

func copyNormalizedResponsesSSE(dst io.Writer, src io.Reader, publicModel string, onEvent func([]byte) error) error {
	reader := bufio.NewReaderSize(src, openAIStreamScannerInitialBuffer)
	var event bytes.Buffer
	flush := func() error {
		if event.Len() == 0 {
			return nil
		}
		raw := append([]byte(nil), event.Bytes()...)
		event.Reset()
		rewritten, changed, eventErr := rewriteResponsesSSEEventModel(raw, publicModel, onEvent)
		if eventErr != nil {
			return eventErr
		}
		if !changed {
			rewritten = raw
		}
		_, err := dst.Write(rewritten)
		return err
	}

	for {
		line, err := readOpenAISSELine(reader)
		if len(line) > 0 {
			boundary := strings.TrimRight(line, "\r\n") == ""
			if event.Len()+len(line) > openAIStreamScannerMaxBuffer {
				return fmt.Errorf("explicit route responses SSE event exceeds inspection limit")
			} else {
				_, _ = event.WriteString(line)
				if boundary {
					if writeErr := flush(); writeErr != nil {
						return writeErr
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flush()
			}
			if errors.Is(err, errOpenAISSELineTooLong) {
				return fmt.Errorf("explicit route responses SSE line exceeds inspection limit")
			}
			return err
		}
	}
}

func rewriteResponsesSSEEventModel(raw []byte, publicModel string, onEvent func([]byte) error) ([]byte, bool, error) {
	lines := splitSSEEventLines(raw)
	if len(lines) == 0 {
		return raw, false, nil
	}
	dataParts := make([]string, 0, 1)
	firstData := -1
	for i, line := range lines {
		content, _ := splitSSELineEnding(line)
		if data, ok := parseSSELine(content); ok {
			if firstData < 0 {
				firstData = i
			}
			dataParts = append(dataParts, data)
		}
	}
	if firstData < 0 || len(dataParts) == 0 {
		return raw, false, nil
	}
	data := strings.Join(dataParts, "\n")
	if strings.TrimSpace(data) == "[DONE]" {
		return raw, false, nil
	}
	if onEvent != nil {
		if err := onEvent([]byte(data)); err != nil {
			return nil, false, err
		}
	}
	rewritten, changed := rewriteResponsesResponseModelJSON([]byte(data), publicModel)
	if !changed {
		return raw, false, nil
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
	return []byte(out.String()), true, nil
}

func normalizeResponsesStreamBodyWithBinding(h *ProxyHandler, source io.ReadCloser, info explicitRouteResponseInfo) io.ReadCloser {
	return normalizeResponsesStreamBody(source, info.publicID, func(data []byte) error {
		tokens, err := extractExplicitResponsesOutputState(data)
		if err != nil {
			// Streaming headers are already committed. Vendor extensions and
			// malformed events must remain transparent instead of terminating the
			// downstream pipe; only successfully extracted state is bindable.
			return nil
		}
		return h.bindExplicitStateTokens(info, tokens)
	})
}

func splitSSEEventLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	lines := make([]string, 0, 4)
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		lines = append(lines, string(raw[start:i+1]))
		start = i + 1
	}
	if start < len(raw) {
		lines = append(lines, string(raw[start:]))
	}
	return lines
}

func normalizeExplicitModelHeaders(headers http.Header, publicModel string) {
	if headers == nil || strings.TrimSpace(publicModel) == "" {
		return
	}
	for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
		if headers.Get(name) != "" {
			headers.Set(name, publicModel)
		}
	}
}
