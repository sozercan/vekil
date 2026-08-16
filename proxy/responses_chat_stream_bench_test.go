package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func BenchmarkChatOverResponsesTextStream(b *testing.B) {
	fixture := benchmarkResponsesChatTextFixture(10_000)
	benchmarkResponsesChatStream(b, fixture, false)
}

func BenchmarkChatOverResponsesFunctionArguments(b *testing.B) {
	fixture := benchmarkResponsesChatToolFixture(1, 1_000)
	benchmarkResponsesChatStream(b, fixture, true)
}

func BenchmarkChatOverResponsesParallelTools(b *testing.B) {
	fixture := benchmarkResponsesChatToolFixture(8, 128)
	benchmarkResponsesChatStream(b, fixture, true)
}

func benchmarkResponsesChatStream(b *testing.B, fixture []byte, withReplay bool) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(fixture)))
	ctx := context.Background()
	route := responsesChatReplayRoute{ProviderID: "bench-provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	var chunkCount int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var store *responsesChatReplayStore
		if withReplay {
			store = newResponsesChatReplayStore()
		}
		stream, err := prepareResponsesChatStream(ctx, io.NopCloser(bytes.NewReader(fixture)), responsesChatStreamConfig{
			PublicModel: "gpt-public", ReplayStore: store, ReplayRoute: route, PrecommitTimeout: time.Second,
		})
		if err != nil {
			b.Fatal(err)
		}
		err = consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
			chunkCount += len(chunk.Choices)
			return nil
		}, nil)
		if err != nil {
			b.Fatal(err)
		}
		if store != nil {
			_ = store.Close()
		}
	}
	benchmarkResponsesChatChunkCount = chunkCount
}

var benchmarkResponsesChatChunkCount int

func benchmarkResponsesChatTextFixture(deltaCount int) []byte {
	var stream strings.Builder
	sequence := int64(0)
	writeBenchmarkResponsesEvent(&stream, "response.created", map[string]any{
		"type": "response.created", "sequence_number": sequence,
		"response": map[string]any{"id": "resp_bench_text", "created_at": int64(1_700_000_000), "status": "in_progress"},
	})
	sequence++
	writeBenchmarkResponsesEvent(&stream, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": sequence, "output_index": 0,
		"item": map[string]any{"type": "message", "id": "msg_bench_text", "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	sequence++
	writeBenchmarkResponsesEvent(&stream, "response.content_part.added", map[string]any{
		"type": "response.content_part.added", "sequence_number": sequence, "item_id": "msg_bench_text", "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	})
	sequence++
	var text strings.Builder
	for i := 0; i < deltaCount; i++ {
		delta := fmt.Sprintf("d%05d ", i)
		text.WriteString(delta)
		writeBenchmarkResponsesEvent(&stream, "response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "sequence_number": sequence, "item_id": "msg_bench_text", "output_index": 0, "content_index": 0, "delta": delta,
		})
		sequence++
	}
	fullText := text.String()
	writeBenchmarkResponsesEvent(&stream, "response.output_text.done", map[string]any{
		"type": "response.output_text.done", "sequence_number": sequence, "item_id": "msg_bench_text", "output_index": 0, "content_index": 0, "text": fullText,
	})
	sequence++
	writeBenchmarkResponsesEvent(&stream, "response.content_part.done", map[string]any{
		"type": "response.content_part.done", "sequence_number": sequence, "item_id": "msg_bench_text", "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": fullText},
	})
	sequence++
	item := map[string]any{"type": "message", "id": "msg_bench_text", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": fullText}}}
	writeBenchmarkResponsesEvent(&stream, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": sequence, "output_index": 0, "item": item,
	})
	sequence++
	writeBenchmarkResponsesEvent(&stream, "response.completed", map[string]any{
		"type": "response.completed", "sequence_number": sequence,
		"response": map[string]any{"id": "resp_bench_text", "created_at": int64(1_700_000_000), "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": deltaCount, "total_tokens": deltaCount + 10}},
	})
	return []byte(stream.String())
}

func benchmarkResponsesChatToolFixture(toolCount, fragments int) []byte {
	var stream strings.Builder
	sequence := int64(0)
	writeBenchmarkResponsesEvent(&stream, "response.created", map[string]any{
		"type": "response.created", "sequence_number": sequence,
		"response": map[string]any{"id": "resp_bench_tools", "created_at": int64(1_700_000_000), "status": "in_progress"},
	})
	sequence++
	arguments := make([]strings.Builder, toolCount)
	for tool := 0; tool < toolCount; tool++ {
		writeBenchmarkResponsesEvent(&stream, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": sequence, "output_index": tool,
			"item": map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_bench_%d", tool), "call_id": fmt.Sprintf("call_bench_%d", tool), "name": fmt.Sprintf("tool_%d", tool), "arguments": "", "status": "in_progress"},
		})
		sequence++
	}
	for fragment := 0; fragment < fragments; fragment++ {
		for tool := 0; tool < toolCount; tool++ {
			piece := fmt.Sprintf("%q", fmt.Sprintf("p%d_%d", tool, fragment))
			if fragment == 0 {
				piece = "[" + piece
			} else {
				piece = "," + piece
			}
			if fragment == fragments-1 {
				piece += "]"
			}
			arguments[tool].WriteString(piece)
			writeBenchmarkResponsesEvent(&stream, "response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "sequence_number": sequence, "item_id": fmt.Sprintf("fc_bench_%d", tool), "output_index": tool, "delta": piece,
			})
			sequence++
		}
	}
	output := make([]any, 0, toolCount)
	for tool := toolCount - 1; tool >= 0; tool-- {
		args := arguments[tool].String()
		writeBenchmarkResponsesEvent(&stream, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "sequence_number": sequence, "item_id": fmt.Sprintf("fc_bench_%d", tool), "output_index": tool, "arguments": args,
		})
		sequence++
		item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_bench_%d", tool), "call_id": fmt.Sprintf("call_bench_%d", tool), "name": fmt.Sprintf("tool_%d", tool), "arguments": args, "status": "completed"}
		writeBenchmarkResponsesEvent(&stream, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": sequence, "output_index": tool, "item": item,
		})
		sequence++
	}
	for tool := 0; tool < toolCount; tool++ {
		output = append(output, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_bench_%d", tool), "call_id": fmt.Sprintf("call_bench_%d", tool), "name": fmt.Sprintf("tool_%d", tool), "arguments": arguments[tool].String(), "status": "completed"})
	}
	writeBenchmarkResponsesEvent(&stream, "response.completed", map[string]any{
		"type": "response.completed", "sequence_number": sequence,
		"response": map[string]any{"id": "resp_bench_tools", "created_at": int64(1_700_000_000), "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": toolCount * fragments, "total_tokens": 10 + toolCount*fragments}},
	})
	return []byte(stream.String())
}

func writeBenchmarkResponsesEvent(stream *strings.Builder, event string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	stream.WriteString("event: ")
	stream.WriteString(event)
	stream.WriteString("\ndata: ")
	stream.Write(encoded)
	stream.WriteString("\n\n")
}
