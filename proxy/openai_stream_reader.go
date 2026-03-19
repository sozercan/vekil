package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/sozercan/copilot-proxy/models"
)

const (
	openAIStreamScannerInitialBuffer = 64 * 1024
	openAIStreamScannerMaxBuffer     = 1024 * 1024
)

// consumeOpenAIStreamChunks scans an upstream OpenAI SSE stream, ignores
// non-data lines and malformed JSON chunks, and reports whether the stream
// terminated with the expected [DONE] sentinel.
func consumeOpenAIStreamChunks(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, openAIStreamScannerInitialBuffer), openAIStreamScannerMaxBuffer)

	for scanner.Scan() {
		data, ok := parseSSELine(scanner.Text())
		if !ok {
			continue
		}
		if data == "[DONE]" {
			return true, nil
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if onChunk != nil && !onChunk(chunk) {
			return false, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("reading SSE stream: %w", err)
	}

	return false, nil
}
