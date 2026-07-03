package proxy

import (
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

var (
	benchmarkOpenAIStreamError   *openAIStreamError
	benchmarkOpenAIStreamErrorOK bool
	benchmarkSSEEventType        string
	benchmarkSSEData             string
	benchmarkOpenAIStreamChunks  int
)

func BenchmarkParseOpenAIStreamErrorOrdinaryChunk(b *testing.B) {
	data := `{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":123,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello world"}}]}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkOpenAIStreamError, benchmarkOpenAIStreamErrorOK = parseOpenAIStreamError("", data)
	}
}

func BenchmarkSSEDataAccumulatorDispatchSingleDataLine(b *testing.B) {
	data := `{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":123,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello world"}}]}`
	acc := sseDataAccumulator{dataLines: make([]string, 0, 1)}
	onData := func(eventType, data string) bool {
		benchmarkSSEEventType = eventType
		benchmarkSSEData = data
		return true
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		acc.eventType = "chat.completion.chunk"
		acc.dataLines = append(acc.dataLines, data)
		if !acc.dispatch(onData) {
			b.Fatal("dispatch stopped")
		}
	}
}

func BenchmarkConsumeOpenAIStreamChunksOrdinary(b *testing.B) {
	chunk := `{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":123,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello world"}}]}`
	input := strings.Repeat("data: "+chunk+"\n\n", 64) + "data: [DONE]\n\n"
	onChunk := func(models.OpenAIStreamChunk) bool {
		benchmarkOpenAIStreamChunks++
		return true
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkOpenAIStreamChunks = 0
		sawDone, err := consumeOpenAIStreamChunks(strings.NewReader(input), onChunk)
		if err != nil {
			b.Fatalf("consumeOpenAIStreamChunks returned error: %v", err)
		}
		if !sawDone || benchmarkOpenAIStreamChunks != 64 {
			b.Fatalf("sawDone=%v chunks=%d, want true/64", sawDone, benchmarkOpenAIStreamChunks)
		}
	}
}
