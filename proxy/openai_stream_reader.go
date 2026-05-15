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
	openAIStreamScannerMaxBuffer     = 1024 * 1024
)

type sseDataAccumulator struct {
	dataLines []string
}

func (a *sseDataAccumulator) consumeLine(line string, onData func(string) bool) bool {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return a.dispatch(onData)
	}

	data, ok := parseSSELine(line)
	if !ok {
		return true
	}
	a.dataLines = append(a.dataLines, data)
	return true
}

func (a *sseDataAccumulator) dispatch(onData func(string) bool) bool {
	if len(a.dataLines) == 0 {
		return true
	}
	data := strings.Join(a.dataLines, "\n")
	a.dataLines = a.dataLines[:0]
	if onData == nil {
		return true
	}
	return onData(data)
}

// consumeOpenAIStreamChunks scans an upstream OpenAI SSE stream, ignores
// non-data events and malformed JSON chunks, and reports whether the stream
// terminated with the expected [DONE] sentinel. Multi-line SSE data fields are
// joined according to the SSE event model before JSON decoding.
func consumeOpenAIStreamChunks(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, openAIStreamScannerInitialBuffer), openAIStreamScannerMaxBuffer)

	sawDone := false
	var accumulator sseDataAccumulator
	processData := func(data string) bool {
		if data == "[DONE]" {
			sawDone = true
			return false
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}

		return onChunk == nil || onChunk(chunk)
	}

	for scanner.Scan() {
		if !accumulator.consumeLine(scanner.Text(), processData) {
			return sawDone, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("reading SSE stream: %w", err)
	}

	if !accumulator.dispatch(processData) {
		return sawDone, nil
	}

	return sawDone, nil
}
