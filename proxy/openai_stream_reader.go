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
	maxBytes  int
	dataBytes int
	overflow  bool
}

func (a *sseDataAccumulator) consumeLine(line string, onData func(string, string) bool) bool {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return a.dispatch(onData)
	}
	if a.overflow {
		return true
	}
	if data, ok := parseSSELine(line); ok {
		if a.maxBytes > 0 {
			if len(line)+1 > a.maxBytes-a.dataBytes {
				clear(a.dataLines)
				a.dataLines = a.dataLines[:0]
				a.overflow = true
				return false
			}
			a.dataBytes += len(line) + 1
		}
		a.dataLines = append(a.dataLines, data)
		return true
	}
	if eventType, ok := parseSSEEventLine(line); ok {
		a.eventType = eventType
	}
	return true
}

func (a *sseDataAccumulator) dispatch(onData func(string, string) bool) bool {
	a.dataBytes = 0
	a.overflow = false
	if len(a.dataLines) == 0 {
		a.eventType = ""
		return true
	}
	data := a.dataLines[0]
	if len(a.dataLines) > 1 {
		data = strings.Join(a.dataLines, "\n")
	}
	eventType := a.eventType
	clear(a.dataLines)
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

var errOpenAISSELineTooLong = errors.New("SSE line exceeds maximum buffer")
var errOpenAISSEEventTooLong = errors.New("SSE event exceeds maximum buffer")

func readOpenAISSELine(reader *bufio.Reader) (string, error) {
	var line strings.Builder
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if line.Len()+len(fragment) > openAIStreamScannerMaxBuffer {
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
	headers http.Header
}

func (e *openAIStreamError) httpStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	switch strings.ToLower(strings.TrimSpace(e.Code)) {
	case "too_many_requests", "rate_limit_exceeded", "rate_limit_error":
		return http.StatusTooManyRequests
	case "user_model_rate_limited", "user_global_rate_limited", "user_weekly_rate_limited", "integration_rate_limited":
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

func shouldParseOpenAIStreamError(eventType, data string) bool {
	if strings.EqualFold(strings.TrimSpace(eventType), "error") {
		return true
	}
	data = strings.TrimLeft(data, " \t\r\n")
	if data == "" || data[0] != '{' {
		return false
	}
	return strings.Contains(data, `"error"`)
}

func parseOpenAIStreamError(eventType, data string) (*openAIStreamError, bool) {
	if !shouldParseOpenAIStreamError(eventType, data) {
		return nil, false
	}

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

func consumeOpenAIStreamChunks(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool) (bool, error) {
	return consumeOpenAIStreamChunksWithDecodeMode(r, onChunk, false)
}

var errOpenAIStreamMalformedChunk = errors.New("upstream sent a malformed Chat stream chunk")

func consumeOpenAIStreamChunksStrict(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool) (bool, error) {
	return consumeOpenAIStreamChunksWithDecodeMode(r, onChunk, true)
}

func consumeOpenAIStreamChunksWithDecodeMode(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool, strict bool) (bool, error) {
	reader := bufio.NewReaderSize(r, openAIStreamScannerInitialBuffer)

	sawDone := false
	var streamReadErr error
	var accumulator sseDataAccumulator
	if strict {
		accumulator.maxBytes = openAIStreamScannerMaxBuffer
	}
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
			if strict && strings.TrimSpace(data) != "" {
				streamReadErr = errOpenAIStreamMalformedChunk
				return false
			}
			return true
		}

		return onChunk == nil || onChunk(chunk)
	}

	for {
		line, err := readOpenAISSELine(reader)
		if len(line) > 0 {
			if !accumulator.consumeLine(line, processData) {
				if accumulator.overflow {
					return false, errOpenAISSEEventTooLong
				}
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
