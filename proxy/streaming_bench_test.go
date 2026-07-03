package proxy

import (
	"strconv"
	"testing"

	"github.com/sozercan/vekil/models"
)

var benchmarkOpenAIResponse *models.OpenAIResponse

func BenchmarkOpenAIResponseAggregatorToolCallArguments(b *testing.B) {
	idx := 0
	chunks := make([]models.OpenAIStreamChunk, 0, 130)
	chunks = append(chunks, models.OpenAIStreamChunk{
		ID:      "chatcmpl-bench-tools",
		Object:  "chat.completion.chunk",
		Created: 123,
		Model:   "gpt-4o",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{Role: "assistant", ToolCalls: []models.OpenAIToolCall{{
				ID:    "call_shell",
				Type:  "function",
				Index: &idx,
				Function: models.OpenAIFunctionCall{
					Name:      "shell_command",
					Arguments: `{"parts":[`,
				},
			}}},
		}},
	})
	for i := 0; i < 128; i++ {
		fragment := strconv.Quote("arg-" + strconv.Itoa(i))
		if i < 127 {
			fragment += ","
		}
		chunks = append(chunks, models.OpenAIStreamChunk{
			ID:      "chatcmpl-bench-tools",
			Object:  "chat.completion.chunk",
			Created: 123,
			Model:   "gpt-4o",
			Choices: []models.OpenAIStreamChoice{{
				Index: 0,
				Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
					Index:    &idx,
					Function: models.OpenAIFunctionCall{Arguments: fragment},
				}}},
			}},
		})
	}
	chunks = append(chunks, models.OpenAIStreamChunk{
		ID:      "chatcmpl-bench-tools",
		Object:  "chat.completion.chunk",
		Created: 123,
		Model:   "gpt-4o",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
				Index:    &idx,
				Function: models.OpenAIFunctionCall{Arguments: `]}`},
			}}},
		}},
	})

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		agg := newOpenAIResponseAggregator()
		for _, chunk := range chunks {
			agg.addChunk(chunk)
		}
		benchmarkOpenAIResponse = agg.buildResponse()
		if got := len(benchmarkOpenAIResponse.Choices[0].Message.ToolCalls); got != 1 {
			b.Fatalf("tool calls = %d, want 1", got)
		}
	}
}
