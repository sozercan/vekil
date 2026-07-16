package proxy

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func TestResponsesChatStreamTerminalValidationErrorsRetainUsage(t *testing.T) {
	state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
	state.createdSeen = true
	state.itemsByIndex[0] = "message"
	state.doneByIndex[0] = true
	state.messagesByIndex[0] = &responsesChatMessageState{
		outputIndex: 0,
		done:        true,
		parts: map[int]*responsesChatTextPart{
			0: {outputIndex: 0, contentIndex: 0, kind: "output_text", digest: sha256.New()},
		},
	}
	part := state.messagesByIndex[0].parts[0]
	_, _ = part.digest.Write([]byte("streamed"))
	part.bytes = len("streamed")
	part.valueDone = true
	part.partDone = true

	transition, err := state.handleCompleted([]byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp-terminal-usage",
			"status":"completed",
			"output":[{"type":"message","id":"message-1","role":"assistant","content":[{"type":"output_text","text":"different"}]}],
			"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}
		}
	}`))
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("error = %#v, want chatExecutionError", err)
	}
	wantUsage := &models.OpenAIUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
	if executionErr.Usage == nil || executionErr.Usage.PromptTokens != wantUsage.PromptTokens || executionErr.Usage.CompletionTokens != wantUsage.CompletionTokens || executionErr.Usage.TotalTokens != wantUsage.TotalTokens {
		t.Fatalf("error usage = %#v, want %#v", executionErr.Usage, wantUsage)
	}
	if len(transition.chunks) != 1 || transition.chunks[0].Usage == nil || transition.chunks[0].Usage.TotalTokens != wantUsage.TotalTokens {
		t.Fatalf("transition = %#v", transition)
	}
}

func TestResponsesChatStreamMalformedTerminalEventRetainsDecodedUsage(t *testing.T) {
	state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
	state.createdSeen = true

	transition, err := state.handleCompleted([]byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp-malformed-terminal",
			"status":"incomplete",
			"output":[],
			"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}
		}
	}`))
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Usage == nil || executionErr.Usage.TotalTokens != 15 {
		t.Fatalf("error = %#v", err)
	}
	if len(transition.chunks) != 1 || transition.chunks[0].Usage == nil || transition.chunks[0].Usage.TotalTokens != 15 {
		t.Fatalf("transition = %#v", transition)
	}
}
