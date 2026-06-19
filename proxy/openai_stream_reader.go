package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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

func readOpenAISSELine(reader *bufio.Reader) (string, error) {
	var line strings.Builder
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if line.Len()+len(fragment) > openAIStreamScannerMaxBuffer {
				return "", fmt.Errorf("SSE line exceeds %d bytes", openAIStreamScannerMaxBuffer)
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
