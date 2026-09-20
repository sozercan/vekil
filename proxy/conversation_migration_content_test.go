package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConversationMigrationContentContract(t *testing.T) {
	cases := []struct {
		name    string
		content string
		allowed bool
	}{
		{"plain text", `{"type":"output_text","text":"Answer."}`, true},
		{"empty annotations", `{"type":"output_text","text":"Answer.","annotations":[],"logprobs":[]}`, true},
		{"null annotations", `{"type":"output_text","text":"Answer.","annotations":null,"logprobs":null}`, true},
		{"token probabilities", `{"type":"output_text","text":"Answer.","logprobs":[{"token":"Answer","logprob":-0.1,"bytes":[65],"top_logprobs":[]}]}`, true},
		{"URL citation", `{"type":"output_text","text":"Answer.","annotations":[{"type":"url_citation","url":"https://example.com","title":"Source","start_index":0,"end_index":6}]}`, false},
		{"file citation", `{"type":"input_text","text":"Answer.","annotations":[{"type":"file_citation","file_id":"file-private","index":0}]}`, false},
		{"invalid annotations", `{"type":"text","text":"Answer.","annotations":{}}`, false},
		{"unknown text field", `{"type":"output_text","text":"Answer.","reference":"file-private"}`, false},
		{"refusal", `{"type":"refusal","refusal":"Cannot answer."}`, true},
		{"unknown refusal field", `{"type":"refusal","refusal":"Cannot answer.","reference":"file-private"}`, false},
	}
	for _, tc := range cases {
		for _, output := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/output=%t/stream=%t", tc.name, output, stream), func(t *testing.T) {
					var sends atomic.Int32
					message := json.RawMessage(`{"type":"message","role":"assistant","content":[` + tc.content + `]}`)
					h, _ := newConversationAPIHandler(t, routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						if strings.HasSuffix(req.URL.Path, "/models") {
							return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
						}
						sends.Add(1)
						if output {
							return conversationResponse(t, req, "content-response", message), nil
						}
						return conversationResponse(t, req, "content-response", conversationText("Continued.")), nil
					}), nil)
					fields := map[string]any{"input": "Question.", "stream": stream}
					if !output {
						fields["input"] = []any{map[string]any{"role": "user", "content": "Question."}, message}
					}
					response := conversationPOST(t, h, fields, http.Header{"X-Vekil-History-Complete": {"true"}})
					if tc.allowed {
						conversationCompleted(t, response, stream)
					} else {
						body := response.Body.String()
						if !strings.Contains(body, "conversation_state_unsupported") || strings.Contains(body, `"history":"saved"`) || strings.Contains(body, `"status":"completed"`) {
							t.Fatalf("unsupported content accepted: %d %s", response.Code, body)
						}
						if _, err := h.conversationHistory.lookupResponse("azure", "content-response"); !errors.Is(err, errConversationHistoryMissing) {
							t.Fatalf("unsupported content was saved: %v", err)
						}
					}
					wantSends := int32(1)
					if !output && !tc.allowed {
						wantSends = 0
					}
					if sends.Load() != wantSends {
						t.Fatalf("sends = %d, want %d", sends.Load(), wantSends)
					}
				})
			}
		}
	}
}
