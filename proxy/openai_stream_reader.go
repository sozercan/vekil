package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/models"
)

const (
	openAIStreamScannerInitialBuffer = 64 * 1024
	openAIStreamScannerMaxBuffer     = 8 * 1024 * 1024
)

type sseDataAccumulator struct {
	eventType string
	dataLines []string
}

func (a *sseDataAccumulator) consumeLine(line string, onData func(string, string) bool) bool {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return a.dispatch(onData)
	}
	if data, ok := parseSSELine(line); ok {
		a.dataLines = append(a.dataLines, data)
		return true
	}
	if eventType, ok := parseSSEEventLine(line); ok {
		a.eventType = eventType
	}
	return true
}

func (a *sseDataAccumulator) dispatch(onData func(string, string) bool) bool {
	if len(a.dataLines) == 0 {
		a.eventType = ""
		return true
	}
	data := strings.Join(a.dataLines, "\n")
	eventType := a.eventType
	a.dataLines = a.dataLines[:0]
	a.eventType = ""
	if onData == nil {
		return true
	}
	return onData(eventType, data)
}

func parseSSEEventLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "event:") {
		return "", false
	}
	eventType := strings.TrimPrefix(line, "event:")
	eventType = strings.TrimPrefix(eventType, " ")
	return strings.TrimSpace(eventType), true
}

// errOpenAISSELineTooLong is returned by readOpenAISSELine when a single SSE
// line exceeds openAIStreamScannerMaxBuffer. The accumulated bytes read so far
// are returned alongside the error so a forwarding caller can write them to the
// client and fall back to raw passthrough of the remainder (the bytes are valid,
// just too large to buffer for parsing) rather than treating it as a failure.
var errOpenAISSELineTooLong = errors.New("SSE line exceeds maximum buffer")

func readOpenAISSELine(reader *bufio.Reader) (string, error) {
	var line strings.Builder
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if line.Len()+len(fragment) > openAIStreamScannerMaxBuffer {
				// Return what we have so the caller can still forward it; the rest of
				// this line remains in the reader and is drained by the caller's raw
				// fallback copy.
				line.Write(fragment)
				return line.String(), errOpenAISSELineTooLong
			}
			line.Write(fragment)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if line.Len() > 0 {
			return line.String(), err
		}
		return "", err
	}
}

type openAIStreamError struct {
	Type    string
	Code    string
	Message string
}

// httpStatus maps an OpenAI-style stream error to an HTTP status so a
// post-commit stream failure is recorded with its real semantic status (e.g.
// 429 for a rate limit, 503 for an overload) rather than a generic bad gateway.
func (e *openAIStreamError) httpStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	switch strings.ToLower(strings.TrimSpace(e.Code)) {
	case "too_many_requests", "rate_limit_exceeded", "rate_limit_error":
		return http.StatusTooManyRequests
	case "model_overloaded", "engine_overloaded", "overloaded_error", "service_unavailable":
		return http.StatusServiceUnavailable
	case "gateway_timeout", "timeout":
		return http.StatusGatewayTimeout
	case "bad_gateway":
		return http.StatusBadGateway
	}
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "rate_limit_error", "rate_limit_exceeded":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func (e *openAIStreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	if e.Type != "" {
		return e.Type
	}
	return "upstream stream error"
}

func parseOpenAIStreamError(eventType, data string) (*openAIStreamError, bool) {
	var envelope struct {
		Error *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err == nil {
		if envelope.Error != nil {
			return &openAIStreamError{
				Type:    strings.TrimSpace(envelope.Error.Type),
				Code:    strings.TrimSpace(envelope.Error.Code),
				Message: strings.TrimSpace(envelope.Error.Message),
			}, true
		}
		if strings.EqualFold(strings.TrimSpace(eventType), "error") {
			return &openAIStreamError{
				Type:    strings.TrimSpace(envelope.Type),
				Code:    strings.TrimSpace(envelope.Code),
				Message: strings.TrimSpace(envelope.Message),
			}, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(eventType), "error") {
		return &openAIStreamError{Message: sanitizeUpstreamErrorText(data)}, true
	}
	return nil, false
}

// consumeOpenAIStreamChunks scans an upstream OpenAI SSE stream, ignores
// malformed JSON chunks, and reports whether the stream terminated with the
// expected [DONE] sentinel. Multi-line SSE data fields are joined according to
// the SSE event model before JSON decoding.
func consumeOpenAIStreamChunks(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool) (bool, error) {
	reader := bufio.NewReaderSize(r, openAIStreamScannerInitialBuffer)

	sawDone := false
	var streamReadErr error
	var accumulator sseDataAccumulator
	processData := func(eventType, data string) bool {
		if data == "[DONE]" {
			sawDone = true
			return false
		}
		if streamErr, ok := parseOpenAIStreamError(eventType, data); ok {
			streamReadErr = streamErr
			return false
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}

		return onChunk == nil || onChunk(chunk)
	}

	for {
		line, err := readOpenAISSELine(reader)
		if len(line) > 0 {
			if !accumulator.consumeLine(line, processData) {
				return sawDone, streamReadErr
			}
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			break
		}
		return false, fmt.Errorf("reading SSE stream: %w", err)
	}

	if !accumulator.dispatch(processData) {
		return sawDone, streamReadErr
	}

	return sawDone, nil
}
