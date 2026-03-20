package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkResponsesWebSocketRequestBuildFullReplay_LongHistory(b *testing.B) {
	benchmarkResponsesWebSocketRequestBuild(b, false)
}

func BenchmarkResponsesWebSocketRequestBuildDeltaReplay_LongHistory(b *testing.B) {
	benchmarkResponsesWebSocketRequestBuild(b, true)
}

func benchmarkResponsesWebSocketRequestBuild(b *testing.B, enableDelta bool) {
	b.Helper()

	requestBytes, err := json.Marshal(map[string]interface{}{
		"type":         "response.create",
		"model":        "gpt-5.4",
		"instructions": "You are helpful",
		"tools":        []interface{}{},
		"tool_choice":  "auto",
		"include":      []string{},
		"input": []map[string]interface{}{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{
						"type": "input_text",
						"text": "Continue the task using the latest file changes only.",
					},
				},
			},
		},
		"previous_response_id": "resp-bench-1",
	})
	if err != nil {
		b.Fatalf("marshal request: %v", err)
	}

	request, err := parseResponsesWebSocketCreateRequest(requestBytes)
	if err != nil {
		b.Fatalf("parse request: %v", err)
	}

	history := benchmarkResponsesWebSocketHistory(200, 256)
	cfg := ResponsesWebSocketConfig{
		TurnStateDelta:     enableDelta,
		DisableAutoCompact: true,
	}
	handler := &ProxyHandler{responsesWS: cfg}
	session := &responsesWebSocketSession{
		lastResponseID: "resp-bench-1",
		lastSignature:  request.signature(),
		historyItems:   history,
	}
	if enableDelta {
		session.turnState = "turn-state-bench"
	}

	b.ReportAllocs()
	b.SetBytes(int64(rawMessagesSize(history) + rawMessagesSize(request.Input)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		plan, err := session.planRequest(handler, request)
		if err != nil {
			b.Fatalf("plan request: %v", err)
		}
		body, err := request.upstreamBody(plan.upstreamSegments()...)
		if err != nil {
			b.Fatalf("encode upstream body: %v", err)
		}
		if len(body) == 0 {
			b.Fatal("encoded body is empty")
		}
	}
}

func benchmarkResponsesWebSocketHistory(turns, payloadSize int) []json.RawMessage {
	history := make([]json.RawMessage, 0, turns*2)
	for i := 0; i < turns; i++ {
		history = append(history, benchmarkResponsesWebSocketMessage("user", i, payloadSize))
		history = append(history, benchmarkResponsesWebSocketMessage("assistant", i, payloadSize))
	}
	return history
}

func benchmarkResponsesWebSocketMessage(role string, index, payloadSize int) json.RawMessage {
	text := fmt.Sprintf("%s turn %03d %s", role, index, strings.Repeat("x", payloadSize))
	encoded, err := json.Marshal(map[string]interface{}{
		"type": "message",
		"role": role,
		"content": []map[string]string{
			{
				"type": "input_text",
				"text": text,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}
