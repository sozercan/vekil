package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCompactionContract_CompactEndpointReturnsCodexCompatibleHistory(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("expected compact fallback to call /responses, got %q", r.URL.Path)
		}
		if got := r.Header.Get("session-id"); got != "sess-compact" {
			t.Fatalf("expected compact request to forward session-id, got %q", got)
		}
		if got := r.Header.Get("thread-id"); got != "thread-compact" {
			t.Fatalf("expected compact request to forward thread-id, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Installation-Id"); got != "install-compact" {
			t.Fatalf("expected compact request to forward installation id, got %q", got)
		}
		body := decodeJSONBodyForContract(t, r.Body)
		if !strings.Contains(rawJSONToStringForContract(t, body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
			t.Fatalf("expected compact prompt instructions, got %s", body["instructions"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary from upstream"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItemForContract("system", "system compact context"),
			messageItemForContract("user", "hello remote compact"),
			messageItemForContract("assistant", "FIRST_REMOTE_REPLY"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "sess-compact")
	req.Header.Set("thread-id", "thread-compact")
	req.Header.Set("X-Codex-Installation-Id", "install-compact")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	body := decodeJSONBodyForContract(t, resp.Body)
	output := rawJSONArrayForContract(t, body["output"])
	if len(output) != 3 {
		t.Fatalf("expected retained system/user messages plus compaction item, got %d output items: %s", len(output), body["output"])
	}
	if got := contractMessageText(t, output[0], "system"); got != "system compact context" {
		t.Fatalf("expected compact response to retain original system history, got %q", got)
	}
	if got := contractMessageText(t, output[1], "user"); got != "hello remote compact" {
		t.Fatalf("expected compact response to retain original user history, got %q", got)
	}
	if contractItemType(t, output[2]) != "compaction" {
		t.Fatalf("expected final compact response item to be compaction, got %s", output[2])
	}
	for _, item := range output {
		if contractItemRole(t, item) == "assistant" {
			t.Fatalf("compact response must not include assistant summary messages that duplicate the checkpoint: %s", item)
		}
	}
}

func TestCompactionContract_ReducesInternalOutputLimitToProviderMaximum(t *testing.T) {
	var mu sync.Mutex
	var limits []int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("expected compact fallback to call /responses, got %q", r.URL.Path)
		}
		body := decodeJSONBodyForContract(t, r.Body)
		var limit int
		if err := json.Unmarshal(body["max_output_tokens"], &limit); err != nil {
			t.Fatalf("decode max_output_tokens: %v", err)
		}
		mu.Lock()
		limits = append(limits, limit)
		mu.Unlock()
		if limit > 4096 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"Invalid 'max_output_tokens': integer above maximum value. Expected a value <= 4096, but got %d instead.","type":"invalid_request_error","code":"invalid_value","param":"max_output_tokens"}}`, limit)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"bounded checkpoint summary"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-4-turbo",
		"input": []interface{}{messageItemForContract("user", "history to compact")},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	mu.Lock()
	gotLimits := append([]int(nil), limits...)
	mu.Unlock()
	if got, want := fmt.Sprint(gotLimits), "[16384 8192 4096]"; got != want {
		t.Fatalf("max_output_tokens attempts = %s, want %s", got, want)
	}
}

func TestIsCompactOutputTokenLimitExceededError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "param and upper bound", status: http.StatusBadRequest, body: `{"error":{"message":"Expected a value <= 4096","param":"max_output_tokens"}}`, want: true},
		{name: "message only", status: http.StatusBadRequest, body: `{"error":{"message":"max_output_tokens exceeds the maximum for this model"}}`, want: true},
		{name: "minimum rejection", status: http.StatusBadRequest, body: `{"error":{"message":"max_output_tokens must be at least 16","param":"max_output_tokens"}}`},
		{name: "unsupported field", status: http.StatusBadRequest, body: `{"error":{"message":"max_output_tokens is not supported","param":"max_output_tokens"}}`},
		{name: "context overflow mentioning output tokens", status: http.StatusBadRequest, body: `{"error":{"message":"maximum context length exceeded: input plus max_output_tokens exceeds the model limit","code":"context_length_exceeded","param":"max_output_tokens"}}`},
		{name: "wrong status", status: http.StatusRequestEntityTooLarge, body: `{"error":{"message":"max_output_tokens exceeds the maximum","param":"max_output_tokens"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCompactOutputTokenLimitExceededError(tt.status, []byte(tt.body)); got != tt.want {
				t.Fatalf("isCompactOutputTokenLimitExceededError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompactBudgetPersistsLearnedOutputTokenLimit(t *testing.T) {
	budget := newCompactBudget(10)
	budget.recordLearnedOutputTokens(4096)
	budget.recordLearnedOutputTokens(8192)
	if got := budget.learnedOutputTokensValue(); got != 4096 {
		t.Fatalf("learned output-token cap = %d, want 4096", got)
	}

	original := map[string]json.RawMessage{"max_output_tokens": json.RawMessage("16384")}
	rewritten := applyLearnedCompactOutputTokenLimit(original, budget)
	if got, ok := compactOutputTokenLimit(rewritten); !ok || got != 4096 {
		t.Fatalf("rewritten output-token cap = %d, %v, want 4096, true", got, ok)
	}
	if got, _ := compactOutputTokenLimit(original); got != 16384 {
		t.Fatalf("original output-token cap mutated to %d", got)
	}

	budget.recordLearnedOutputTokens(2048)
	rewritten = applyLearnedCompactOutputTokenLimit(original, budget)
	if got, ok := compactOutputTokenLimit(rewritten); !ok || got != 2048 {
		t.Fatalf("ratcheted output-token cap = %d, %v, want 2048, true", got, ok)
	}
}

func TestCompactRetryCancellation(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := compactRetryCancellation(canceledCtx, errors.New("retry failed")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v, want context.Canceled", err)
	}

	wrappedDeadline := fmt.Errorf("retry transport: %w", context.DeadlineExceeded)
	if err := compactRetryCancellation(context.Background(), wrappedDeadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrapped deadline error = %v, want context.DeadlineExceeded", err)
	}
	if err := compactRetryCancellation(context.Background(), errors.New("ordinary failure")); err != nil {
		t.Fatalf("ordinary retry error classified as cancellation: %v", err)
	}
}

func TestCompactionContract_CompactEndpointRejectsInvalidSummaryResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "incomplete with partial text",
			body: `{"id":"resp-incomplete","object":"response","status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial checkpoint"}]}]}`,
		},
		{
			name: "refusal only",
			body: `{"id":"resp-refusal","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"cannot summarize"}]}]}`,
		},
		{
			name: "function call only",
			body: `{"id":"resp-tool","object":"response","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"summarize","arguments":"{}"}]}`,
		},
		{
			name: "empty output",
			body: `{"id":"resp-empty","object":"response","status":"completed","output":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			})

			reqBody := mustMarshalContractJSON(t, map[string]interface{}{
				"model": "gpt-5.4",
				"input": []interface{}{messageItemForContract("user", "history to compact")},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.HandleCompact(w, req)

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("expected invalid compaction response to fail with 502, got %d: %s", resp.StatusCode, body)
			}
			if bytes.Contains(body, []byte(syntheticCompactionPrefix)) {
				t.Fatalf("invalid compaction response must not emit a synthetic checkpoint: %s", body)
			}
		})
	}
}

func TestCompactionContract_InternalCompactionNormalizesCallerControls(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		invoke func(*ProxyHandler, http.ResponseWriter, *http.Request)
		input  []interface{}
	}{
		{
			name:   "compact endpoint",
			path:   "/v1/responses/compact",
			invoke: (*ProxyHandler).HandleCompact,
			input:  []interface{}{messageItemForContract("user", "compact controls")},
		},
		{
			name:   "remote compaction trigger",
			path:   "/v1/responses",
			invoke: (*ProxyHandler).HandleResponses,
			input: []interface{}{
				messageItemForContract("user", "compact controls"),
				map[string]interface{}{"type": "compaction_trigger"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstream map[string]json.RawMessage
			handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				upstream = decodeJSONBodyForContract(t, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp-controls","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"normalized checkpoint"}]}]}`)
			})

			reqBody := mustMarshalContractJSON(t, map[string]interface{}{
				"model":                "gpt-5.4",
				"previous_response_id": "resp-upstream-lineage",
				"input":                tt.input,
				"tools": []interface{}{
					map[string]interface{}{"type": "function", "name": "must_not_run"},
				},
				"tool_choice":         "required",
				"parallel_tool_calls": true,
				"text": map[string]interface{}{
					"format": map[string]interface{}{
						"type": "json_schema",
						"name": "caller_schema",
						"schema": map[string]interface{}{
							"type": "object",
						},
					},
					"verbosity": "low",
				},
				"response_format":       map[string]interface{}{"type": "json_object"},
				"max_output_tokens":     1,
				"max_tokens":            2,
				"max_completion_tokens": 3,
				"stream":                true,
				"stream_options":        map[string]interface{}{"include_usage": true},
				"background":            true,
				"service_tier":          "priority",
				"prompt_cache_key":      "route-sentinel",
				"metadata":              map[string]interface{}{"route": "ROUTING_SENTINEL"},
			})
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			tt.invoke(handler, w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
			}
			if upstream == nil {
				t.Fatal("expected an internal upstream compaction request")
			}

			for _, field := range []string{
				"tools",
				"tool_choice",
				"parallel_tool_calls",
				"response_format",
				"max_tokens",
				"max_completion_tokens",
				"stream",
				"stream_options",
				"background",
			} {
				if _, ok := upstream[field]; ok {
					t.Fatalf("internal compaction request must remove caller field %q: %s", field, upstream[field])
				}
			}

			if got := rawJSONToIntForContract(t, upstream["max_output_tokens"]); got != internalCompactionMaxOutputTokens {
				t.Fatalf("internal max_output_tokens = %d, want proxy cap %d", got, internalCompactionMaxOutputTokens)
			}

			text := rawJSONObjectForContract(t, upstream["text"])
			if _, ok := text["format"]; ok {
				t.Fatalf("internal compaction request must remove text.format: %s", upstream["text"])
			}
			if got := rawJSONToStringForContract(t, text["verbosity"]); got != "low" {
				t.Fatalf("expected non-format text controls to survive, got %q", got)
			}
			if got := rawJSONToStringForContract(t, upstream["model"]); got != "gpt-5.4" {
				t.Fatalf("expected model to survive normalization, got %q", got)
			}
			if got := rawJSONToStringForContract(t, upstream["previous_response_id"]); got != "resp-upstream-lineage" {
				t.Fatalf("expected ordinary upstream lineage to survive normalization, got %q", got)
			}
			if got := rawJSONToStringForContract(t, upstream["service_tier"]); got != "priority" {
				t.Fatalf("expected routing service tier to survive, got %q", got)
			}
			if got := rawJSONToStringForContract(t, upstream["prompt_cache_key"]); got != "route-sentinel" {
				t.Fatalf("expected prompt cache routing key to survive, got %q", got)
			}
			metadata := rawJSONObjectForContract(t, upstream["metadata"])
			if got := rawJSONToStringForContract(t, metadata["route"]); got != "ROUTING_SENTINEL" {
				t.Fatalf("expected routing metadata to survive, got %q", got)
			}
		})
	}
}

func TestCompactionContract_UnknownCompactionTokenIsPreserved(t *testing.T) {
	const upstreamOpaqueToken = "opaque+server/token/with/slashes=="
	var upstreamInput []json.RawMessage
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		upstreamInput = rawJSONArrayForContract(t, body["input"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-replay","object":"response","status":"completed","output":[]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": upstreamOpaqueToken,
			},
			messageItemForContract("user", "new request wins"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if len(upstreamInput) != 2 {
		t.Fatalf("expected two upstream input items, got %d: %s", len(upstreamInput), upstreamInput)
	}
	first := rawJSONObjectForContract(t, upstreamInput[0])
	if rawJSONToStringForContract(t, first["type"]) != "compaction" {
		t.Fatalf("expected unknown token to remain a compaction item, got %s", upstreamInput[0])
	}
	if rawJSONToStringForContract(t, first["encrypted_content"]) != upstreamOpaqueToken {
		t.Fatalf("expected unknown token to be preserved, got %s", first["encrypted_content"])
	}
}

func TestCompactionContract_RemoteCompactionV2TriggerProducesCompactionItem(t *testing.T) {
	var mu sync.Mutex
	var upstreamRequests []map[string]json.RawMessage
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		mu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		mu.Unlock()

		if _, ok := body["instructions"]; !ok {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-normal\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"normal response\"}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-normal\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
			return
		}

		input := rawJSONArrayForContract(t, body["input"])
		for _, item := range input {
			if contractItemType(t, item) == "compaction_trigger" {
				t.Fatalf("compact fallback request must not forward compaction_trigger: %s", body["input"])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-v2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"remote v2 checkpoint"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItemForContract("user", "USER_ONE"),
			messageItemForContract("assistant", "FIRST_REMOTE_REPLY"),
			map[string]interface{}{"type": "compaction_trigger"},
		},
		"tools":               []interface{}{},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"stream":              true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE response, got Content-Type %q", got)
	}
	events := parseSSEDataForContract(t, resp.Body)
	if !sseHasCompactionOutputForContract(t, events) {
		t.Fatalf("expected remote compaction v2 SSE to contain a compaction output item, got %#v", events)
	}
	mu.Lock()
	gotRequests := len(upstreamRequests)
	mu.Unlock()
	if gotRequests != 1 {
		t.Fatalf("expected only the compact fallback upstream request, got %d upstream requests", gotRequests)
	}
}

func TestCompactionContract_RemoteCompactionV2PreservesPreviousResponseID(t *testing.T) {
	var upstreamRequest map[string]json.RawMessage
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		upstreamRequest = body
		if !strings.Contains(rawJSONToStringForContract(t, body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
			t.Fatalf("expected compact prompt instructions, got %s", body["instructions"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-v2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"server-side checkpoint"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev",
		"input":                []interface{}{map[string]interface{}{"type": "compaction_trigger"}},
		"stream":               true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := rawJSONToStringForContract(t, upstreamRequest["previous_response_id"]); got != "resp-prev" {
		t.Fatalf("expected compact fallback to preserve previous_response_id, got %q", got)
	}
	input := rawJSONArrayForContract(t, upstreamRequest["input"])
	if len(input) != 0 {
		t.Fatalf("expected compact fallback to strip only the trigger from delta input, got %s", upstreamRequest["input"])
	}
}

func TestCompactionContract_HTTPRemoteCompactionFollowUpResetsSyntheticLineage(t *testing.T) {
	const historySentinel = "HISTORY_SENTINEL_BATCH_4"
	const followUpSentinel = "FOLLOW_UP_SENTINEL_BATCH_4"

	var upstreamCalls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		body := decodeJSONBodyForContract(t, r.Body)

		switch call {
		case 1:
			if !strings.Contains(rawJSONToStringForContract(t, body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
				t.Fatalf("expected first upstream call to be compaction, got %s", body["instructions"])
			}
			input := rawJSONArrayForContract(t, body["input"])
			if len(input) != 2 {
				t.Fatalf("expected compaction input history without trigger, got %d items: %s", len(input), body["input"])
			}
			if got := contractMessageText(t, input[0], "user"); got != historySentinel {
				t.Fatalf("expected history sentinel in compaction input, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"resp-compact-upstream","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint preserves %s"}]}]}`, historySentinel)
		case 2:
			if _, ok := body["previous_response_id"]; ok {
				t.Fatalf("follow-up must not forward proxy synthetic previous_response_id: %s", body["previous_response_id"])
			}
			if got := r.Header.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("follow-up must not forward stale turn state after proxy compaction, got %q", got)
			}
			input := rawJSONArrayForContract(t, body["input"])
			if len(input) != 2 {
				t.Fatalf("expected expanded checkpoint plus new input, got %d items: %s", len(input), body["input"])
			}
			if got := contractMessageText(t, input[0], "developer"); !strings.Contains(got, historySentinel) {
				t.Fatalf("expected expanded checkpoint to retain history sentinel, got %q", got)
			}
			if got := contractMessageText(t, input[1], "user"); got != followUpSentinel {
				t.Fatalf("expected follow-up input to survive, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-follow-up","object":"response","status":"completed","output":[]}`)
		default:
			t.Fatalf("unexpected upstream request %d", call)
		}
	})

	compactBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItemForContract("user", historySentinel),
			messageItemForContract("assistant", "history answer"),
			map[string]interface{}{"type": "compaction_trigger"},
		},
	})
	compactReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compactBody))
	compactReq.Header.Set("Content-Type", "application/json")
	compactW := httptest.NewRecorder()
	handler.HandleResponses(compactW, compactReq)

	compactResp := compactW.Result()
	if compactResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(compactResp.Body)
		t.Fatalf("expected compact trigger 200, got %d: %s", compactResp.StatusCode, body)
	}
	compactResult := decodeJSONBodyForContract(t, compactResp.Body)
	syntheticID := rawJSONToStringForContract(t, compactResult["id"])
	if !strings.HasPrefix(syntheticID, syntheticCompactionResponseIDPrefix) {
		t.Fatalf("expected proxy synthetic response id, got %q", syntheticID)
	}
	compactOutput := rawJSONArrayForContract(t, compactResult["output"])
	if len(compactOutput) != 1 || contractItemType(t, compactOutput[0]) != "compaction" {
		t.Fatalf("expected one proxy compaction item, got %s", compactResult["output"])
	}

	followUpBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": syntheticID,
		"input": []interface{}{
			compactOutput[0],
			messageItemForContract("user", followUpSentinel),
		},
	})
	followUpReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(followUpBody))
	followUpReq.Header.Set("Content-Type", "application/json")
	followUpReq.Header.Set("X-Codex-Turn-State", "stale-turn-state-from-synthetic-response")
	followUpW := httptest.NewRecorder()
	handler.HandleResponses(followUpW, followUpReq)

	followUpResp := followUpW.Result()
	if followUpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(followUpResp.Body)
		t.Fatalf("expected follow-up 200, got %d: %s", followUpResp.StatusCode, body)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("expected compact and follow-up upstream calls, got %d", got)
	}
}

func TestCompactionContract_HTTPProxyCheckpointPreservesOrdinaryUpstreamLineage(t *testing.T) {
	const upstreamResponseID = "resp-real-upstream"
	const turnState = "real-upstream-turn-state"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		if got := rawJSONToStringForContract(t, body["previous_response_id"]); got != upstreamResponseID {
			t.Fatalf("expected ordinary upstream response id to be preserved, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Turn-State"); got != turnState {
			t.Fatalf("expected ordinary upstream turn state to be preserved, got %q", got)
		}
		input := rawJSONArrayForContract(t, body["input"])
		if got := contractMessageText(t, input[0], "developer"); !strings.Contains(got, "ordinary lineage checkpoint") {
			t.Fatalf("expected proxy checkpoint to still be expanded, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-next","object":"response","status":"completed","output":[]}`)
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": upstreamResponseID,
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction("ordinary lineage checkpoint"),
			},
			messageItemForContract("user", "continue"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", turnState)
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	if resp := w.Result(); resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestCompactionContract_Responses413PreservesOriginalFailureWhenCompactionIsIncomplete(t *testing.T) {
	const original413Body = `{"error":{"message":"ORIGINAL_413_SENTINEL","code":"payload_too_large"}}`

	var normalCalls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		instructions := ""
		if len(body["instructions"]) > 0 {
			instructions = rawJSONToStringForContract(t, body["instructions"])
		}
		if strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-incomplete-413","object":"response","status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial checkpoint must not be used"}]}]}`)
			return
		}

		if normalCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, original413Body)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-should-not-exist","object":"response","status":"completed","output":[]}`)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 1,
	}

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItemForContract("assistant", "older answer"),
			messageItemForContract("user", "latest request"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected original 413 when compaction is incomplete, got %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("ORIGINAL_413_SENTINEL")) {
		t.Fatalf("expected original 413 body to survive, got %s", body)
	}
	if got := normalCalls.Load(); got != 1 {
		t.Fatalf("incomplete compaction must not produce a compacted retry, got %d normal calls", got)
	}
}

func TestCompactionContract_WebSocketResponseProcessedIsControlOnly(t *testing.T) {
	var upstreamRequests atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber := upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-%d\"}}\n\n", requestNumber)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-%d\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n", requestNumber)
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "hello")})); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if err := conn.WriteJSON(map[string]interface{}{"type": "response.processed", "response_id": "resp-1"}); err != nil {
		t.Fatalf("failed to write response.processed frame: %v", err)
	}
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "next")})); err != nil {
		t.Fatalf("failed to write second websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	if got := upstreamRequests.Load(); got != 2 {
		t.Fatalf("expected response.processed not to create an upstream request, got %d", got)
	}
}

func TestCompactionContract_WebSocketRemoteCompactionFollowUpUsesCompactionHistory(t *testing.T) {
	var mu sync.Mutex
	var upstreamRequests []map[string]interface{}
	var normalRequests int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		mu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		mu.Unlock()

		if strings.Contains(fmt.Sprint(body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
			input := body["input"].([]interface{})
			if !strings.Contains(fmt.Sprint(input), "first") {
				t.Fatalf("compact upstream request must use full websocket history, got %v", input)
			}
			for _, item := range input {
				itemObject := item.(map[string]interface{})
				if itemObject["type"] == "compaction_trigger" {
					t.Fatalf("compact upstream request must not forward compaction_trigger: %v", body["input"])
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"resp-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}]}`)
			return
		}

		normalRequests++
		if normalRequests == 2 && r.Header.Get("X-Codex-Turn-State") != "" {
			t.Fatalf("follow-up after synthetic compaction must not use stale turn state, got %q", r.Header.Get("X-Codex-Turn-State"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if normalRequests == 1 {
			w.Header().Set("X-Codex-Turn-State", "turn-state-1")
		}
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-normal-%d\"}}\n\n", normalRequests)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-normal-%d\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n", normalRequests)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     true,
		DisableAutoCompact: true,
	}

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	first := newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "first")})
	if err := conn.WriteJSON(first); err != nil {
		t.Fatalf("failed to write first websocket request: %v", err)
	}
	firstCreated := mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)
	firstID := websocketResponseID(t, firstCreated)

	compact := newResponsesWebSocketCreateRequest([]interface{}{map[string]interface{}{"type": "compaction_trigger"}})
	compact["previous_response_id"] = firstID
	if err := conn.WriteJSON(compact); err != nil {
		t.Fatalf("failed to write websocket compaction request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	compactionOutput := mustReadWebSocketJSON(t, conn)
	if compactionOutput["type"] != "response.output_item.done" {
		t.Fatalf("expected compaction output event, got %v", compactionOutput["type"])
	}
	compactCompleted := mustReadWebSocketJSON(t, conn)
	compactionID := websocketResponseID(t, compactCompleted)

	if err := conn.WriteJSON(map[string]interface{}{"type": "response.processed", "response_id": compactionID}); err != nil {
		t.Fatalf("failed to write response.processed websocket frame: %v", err)
	}
	followUp := newResponsesWebSocketCreateRequest([]interface{}{messageItemForContract("user", "after")})
	followUp["previous_response_id"] = compactionID
	if err := conn.WriteJSON(followUp); err != nil {
		t.Fatalf("failed to write follow-up websocket request: %v", err)
	}
	followUpCreated := mustReadWebSocketJSON(t, conn)
	if followUpCreated["type"] != "response.created" {
		t.Fatalf("expected normal follow-up response.created, got %v", followUpCreated["type"])
	}
	followUpCompleted := mustReadWebSocketJSON(t, conn)
	if followUpCompleted["type"] != "response.completed" {
		t.Fatalf("expected normal follow-up response.completed, got %v", followUpCompleted["type"])
	}

	mu.Lock()
	requests := append([]map[string]interface{}(nil), upstreamRequests...)
	mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("expected first turn, compaction, and normal follow-up upstream requests; got %d", len(requests))
	}
	if strings.Contains(fmt.Sprint(requests[2]["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("follow-up request should not be another compaction request: %#v", requests[2])
	}
	followUpInput := requests[2]["input"].([]interface{})
	if len(followUpInput) != 2 {
		t.Fatalf("expected follow-up replay to contain compaction plus new user message, got %d: %#v", len(followUpInput), followUpInput)
	}
	checkpoint := followUpInput[0].(map[string]interface{})
	if checkpoint["type"] != "message" || checkpoint["role"] != "developer" || !strings.Contains(fmt.Sprint(checkpoint["content"]), "checkpoint summary") {
		t.Fatalf("expected follow-up replay to start with developer checkpoint, got %#v", followUpInput[0])
	}
	followUpUser := followUpInput[1].(map[string]interface{})
	if followUpUser["type"] == "compaction_trigger" {
		t.Fatalf("follow-up replay must not include stale compaction_trigger: %#v", followUpInput)
	}
	if followUpUser["role"] != "user" || !strings.Contains(fmt.Sprint(followUpUser["content"]), "after") {
		t.Fatalf("expected follow-up replay to include latest user message, got %#v", followUpInput[1])
	}
}

func TestCompactionContract_ResponsesSanitizesContextCompaction(t *testing.T) {
	const summary = "context checkpoint summary from codex"
	const upstreamOpaqueToken = "opaque+server/context/token=="
	var upstreamInput []json.RawMessage

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeJSONBodyForContract(t, r.Body)
		upstreamInput = rawJSONArrayForContract(t, body["input"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-context-compaction","object":"response","status":"completed","output":[]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":    "context_compaction",
				"summary": summary,
			},
			map[string]interface{}{
				"type":              "context_compaction",
				"encrypted_content": upstreamOpaqueToken,
			},
			messageItemForContract("user", "continue after checkpoint"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if len(upstreamInput) != 3 {
		t.Fatalf("expected three upstream input items, got %d: %s", len(upstreamInput), upstreamInput)
	}
	if got := contractMessageText(t, upstreamInput[0], "developer"); !strings.Contains(got, summary) {
		t.Fatalf("expected context_compaction summary to become developer checkpoint, got %q", got)
	}

	opaque := rawJSONObjectForContract(t, upstreamInput[1])
	if got := rawJSONToStringForContract(t, opaque["type"]); got != "context_compaction" {
		t.Fatalf("expected opaque context_compaction to be preserved, got %s", upstreamInput[1])
	}
	if got := rawJSONToStringForContract(t, opaque["encrypted_content"]); got != upstreamOpaqueToken {
		t.Fatalf("expected opaque context_compaction token to be preserved, got %q", got)
	}

	if got := contractMessageText(t, upstreamInput[2], "user"); got != "continue after checkpoint" {
		t.Fatalf("expected later user message to be preserved, got %q", got)
	}
}

func TestCompactionContract_CompactEndpointSanitizesContextCompaction(t *testing.T) {
	const summary = "compact endpoint checkpoint summary"
	var upstreamInput []json.RawMessage

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("expected compact fallback to call /responses, got %q", r.URL.Path)
		}
		body := decodeJSONBodyForContract(t, r.Body)
		upstreamInput = rawJSONArrayForContract(t, body["input"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-context-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"new compact summary"}]}]}`))
	})

	reqBody := mustMarshalContractJSON(t, map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "context_compaction",
				"content": []interface{}{
					map[string]interface{}{
						"type": "input_text",
						"text": summary,
					},
				},
			},
			messageItemForContract("user", "compact this follow-up"),
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if len(upstreamInput) != 2 {
		t.Fatalf("expected sanitized checkpoint plus user message upstream, got %d: %s", len(upstreamInput), upstreamInput)
	}
	if got := contractMessageText(t, upstreamInput[0], "developer"); !strings.Contains(got, summary) {
		t.Fatalf("expected context_compaction summary to become developer checkpoint, got %q", got)
	}
	if got := contractMessageText(t, upstreamInput[1], "user"); got != "compact this follow-up" {
		t.Fatalf("expected compact request user message to be preserved, got %q", got)
	}
}

func TestCompactionContract_WebSocketSanitizesContextCompaction(t *testing.T) {
	const summary = "websocket checkpoint summary"
	var mu sync.Mutex
	var upstreamRequest map[string]interface{}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}
		mu.Lock()
		upstreamRequest = body
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-ws-context\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-ws-context\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
	})

	server := startResponsesWebSocketProxyServer(t, handler)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	req := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type":               "context_compaction",
			"checkpoint_summary": summary,
		},
		messageItemForContract("user", "after websocket checkpoint"),
	})
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	_ = mustReadWebSocketJSON(t, conn)
	_ = mustReadWebSocketJSON(t, conn)

	mu.Lock()
	body := upstreamRequest
	mu.Unlock()
	if body == nil {
		t.Fatal("expected upstream websocket proxy request")
	}
	input := upstreamInputItems(t, body)
	if len(input) != 2 {
		t.Fatalf("expected sanitized checkpoint plus user message upstream, got %d: %#v", len(input), input)
	}
	if got := requireMessageTextWithRole(t, input[0], "developer"); !strings.Contains(got, summary) {
		t.Fatalf("expected websocket context_compaction summary to become developer checkpoint, got %q", got)
	}
	if got := requireMessageTextWithRole(t, input[1], "user"); got != "after websocket checkpoint" {
		t.Fatalf("expected websocket user message to be preserved, got %q", got)
	}
}

func messageItemForContract(role, text string) map[string]interface{} {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]interface{}{
		"type": "message",
		"role": role,
		"content": []map[string]string{
			{"type": contentType, "text": text},
		},
	}
}

func mustMarshalContractJSON(t testing.TB, value interface{}) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return body
}

func decodeJSONBodyForContract(t testing.TB, r io.Reader) map[string]json.RawMessage {
	t.Helper()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read JSON body: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode JSON body %s: %v", string(body), err)
	}
	return decoded
}

func rawJSONArrayForContract(t testing.TB, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode JSON array %s: %v", string(raw), err)
	}
	return items
}

func rawJSONObjectForContract(t testing.TB, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode JSON object %s: %v", string(raw), err)
	}
	return item
}

func rawJSONToIntForContract(t testing.TB, raw json.RawMessage) int {
	t.Helper()
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode JSON integer %s: %v", string(raw), err)
	}
	return value
}

func rawJSONToStringForContract(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode JSON string %s: %v", string(raw), err)
	}
	return value
}

func contractItemType(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	item := rawJSONObjectForContract(t, raw)
	return rawJSONToStringForContract(t, item["type"])
}

func contractItemRole(t testing.TB, raw json.RawMessage) string {
	t.Helper()
	item := rawJSONObjectForContract(t, raw)
	if len(item["role"]) == 0 {
		return ""
	}
	return rawJSONToStringForContract(t, item["role"])
}

func contractMessageText(t testing.TB, raw json.RawMessage, wantRole string) string {
	t.Helper()
	item := rawJSONObjectForContract(t, raw)
	if gotType := rawJSONToStringForContract(t, item["type"]); gotType != "message" {
		t.Fatalf("expected message item, got %q in %s", gotType, raw)
	}
	if gotRole := rawJSONToStringForContract(t, item["role"]); gotRole != wantRole {
		t.Fatalf("expected role %q, got %q in %s", wantRole, gotRole, raw)
	}
	content := rawJSONArrayForContract(t, item["content"])
	var pieces []string
	for _, rawPart := range content {
		part := rawJSONObjectForContract(t, rawPart)
		if len(part["text"]) == 0 {
			continue
		}
		pieces = append(pieces, rawJSONToStringForContract(t, part["text"]))
	}
	return strings.Join(pieces, "\n")
}

func parseSSEDataForContract(t testing.TB, r io.Reader) []map[string]interface{} {
	t.Helper()
	scanner := bufio.NewScanner(r)
	var dataLines []string
	var events []map[string]interface{}
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("decode SSE data %s: %v", data, err)
		}
		events = append(events, event)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	flush()
	return events
}

func sseHasCompactionOutputForContract(t testing.TB, events []map[string]interface{}) bool {
	t.Helper()
	for _, event := range events {
		if event["type"] != "response.output_item.done" {
			continue
		}
		item, ok := event["item"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected output_item.done item object, got %#v", event["item"])
		}
		if item["type"] == "compaction" && strings.TrimSpace(fmt.Sprint(item["encrypted_content"])) != "" {
			return true
		}
	}
	return false
}
