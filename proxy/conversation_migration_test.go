package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newConversationAPIHandler(t *testing.T, transport http.RoundTripper, saved *ProvidersConfig) (*ProxyHandler, ProvidersConfig) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("conversation migration requires Linux or macOS durable storage")
	}
	var cfg ProvidersConfig
	if saved != nil {
		cfg = *saved
	} else {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg = ProvidersConfig{
			SchemaVersion:         2,
			StateBindings:         &StateBindingsConfig{Mode: "durable", File: filepath.Join(dir, "bindings.db")},
			ConversationMigration: &ConversationMigrationConfig{Routes: []string{"azure"}},
			Providers: []ProviderConfig{
				{ID: "east", Type: "azure-openai", BaseURL: "https://east.openai.azure.com/openai/v1", APIKey: "east-test-key"},
				{ID: "west", Type: "azure-openai", BaseURL: "https://west.openai.azure.com/openai/v1", APIKey: "west-test-key"},
			},
			ModelRoutes: []ModelRouteConfig{{
				ID: "azure", PublicID: "coding", Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{{ID: "east", Provider: "east", UpstreamModel: "deployment-east"}, {ID: "west", Provider: "west", UpstreamModel: "deployment-west"}},
				Routing: ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 2, MaxUpstreamSends: 3},
			}},
		}
	}
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg), WithResponsesWebSocketConfig(ResponsesWebSocketConfig{Enabled: true}),
		func(h *ProxyHandler) {
			h.client = &http.Client{Transport: transport}
			h.streamingUpstreamTimeout = 3 * time.Second
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopConversationAPIHandler(t, h) })
	return h, cfg
}

func stopConversationAPIHandler(t *testing.T, h *ProxyHandler) {
	t.Helper()
	h.BeginShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.WaitLifecycleWorkers(ctx); err != nil {
		t.Error(err)
	}
}

func conversationPOST(t *testing.T, h *ProxyHandler, fields map[string]any, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	fields["model"] = "coding"
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, req)
	return recorder
}

func conversationText(text string) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func conversationResponse(t *testing.T, req *http.Request, id string, output ...any) *http.Response {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &input)
	payload := map[string]any{"id": id, "model": "physical-model", "status": "completed", "output": output}
	encoded, _ := json.Marshal(payload)
	headers := http.Header{"Content-Type": {"application/json"}, "X-Codex-Turn-State": {"turn-" + id}}
	if input.Stream {
		headers.Set("Content-Type", "text/event-stream")
		encoded = []byte("event: response.completed\ndata: " + `{"type":"response.completed","response":` + string(encoded) + "}\n\ndata: [DONE]\n\n")
	}
	return routeExecutorTestResponse(req, http.StatusOK, headers, string(encoded))
}

func conversationCompleted(t *testing.T, recorder *httptest.ResponseRecorder, stream bool) map[string]json.RawMessage {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]json.RawMessage
	if !stream {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	} else {
		err := consumeResponsesSSEMessages(bytes.NewReader(recorder.Body.Bytes()), func(message responsesSSEMessage) error {
			if strings.TrimSpace(message.data) == "[DONE]" {
				return nil
			}
			var envelope map[string]json.RawMessage
			if json.Unmarshal([]byte(message.data), &envelope) == nil && rawJSONString(envelope["type"]) == "response.completed" {
				return json.Unmarshal(envelope["response"], &response)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if response == nil || rawJSONString(response["status"]) != "completed" || !bytes.Contains(response["vekil"], []byte(`"history":"saved"`)) {
		t.Fatalf("completion is not durably saved: %s", recorder.Body.String())
	}
	if response["response"] != nil || response["type"] != nil {
		t.Fatalf("SSE envelope leaked into response: %s", recorder.Body.String())
	}
	return response
}

func TestConversationMigrationPreservesToolsBranchesAndRestart(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var failed atomic.Bool
			var east, west atomic.Int32
			var westBodies []string
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "east.") {
					if failed.Load() {
						return nil, errors.New("dial failed before request write")
					}
					number := east.Add(1)
					if number == 1 {
						return conversationResponse(t, req, "east-1",
							map[string]any{"type": "reasoning", "id": "reason-east-1", "encrypted_content": "private-east-1", "summary": []any{}},
							map[string]any{"type": "function_call", "id": "item-edit", "call_id": "call-edit", "name": "edit_file", "arguments": `{"text":"cedar"}`}), nil
					}
					return conversationResponse(t, req, fmt.Sprintf("east-%d", number), conversationText("The local edit completed.")), nil
				}
				body, _ := io.ReadAll(req.Body)
				westBodies = append(westBodies, string(body))
				if strings.Contains(string(body), "private-east") || strings.Contains(string(body), `"previous_response_id":"east-`) || req.Header.Get("X-Codex-Turn-State") != "" {
					t.Error("west received east-owned state")
				}
				if req.Header.Get("api-key") != "west-test-key" {
					t.Error("west received the wrong authentication")
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				number := west.Add(1)
				return conversationResponse(t, req, fmt.Sprintf("west-%d", number), conversationText(fmt.Sprintf("west answer %d", number))), nil
			})
			h, cfg := newConversationAPIHandler(t, transport, nil)
			first := conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"instructions": "Preserve the cedar file and the original user instruction.",
				"input":        "Edit the local file once.", "stream": stream,
				"tools": []any{map[string]any{"type": "function", "name": "edit_file", "parameters": map[string]any{"type": "object"}}},
			}, nil), stream)
			if rawJSONString(first["id"]) != "east-1" {
				t.Fatal("initial turn did not use east")
			}
			// The client performs a local side effect once and supplies its result.
			sideEffect := filepath.Join(t.TempDir(), "side-effect.txt")
			if err := os.WriteFile(sideEffect, []byte("cedar\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			second := conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"previous_response_id": "east-1", "stream": stream,
				"input": []any{map[string]any{"type": "function_call_output", "call_id": "call-edit", "output": "wrote cedar exactly once"}},
			}, http.Header{"X-Codex-Turn-State": {"turn-east-1"}}), stream)
			stopConversationAPIHandler(t, h)
			h, _ = newConversationAPIHandler(t, transport, &cfg)
			failed.Store(true)
			third := conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"previous_response_id": rawJSONString(second["id"]), "input": "Continue the task.", "stream": stream,
			}, http.Header{"X-Codex-Turn-State": {"turn-east-2"}}), stream)
			if rawJSONString(third["id"]) != "west-1" || !bytes.Contains(third["vekil"], []byte(`"migration":"completed"`)) {
				t.Fatal("migration was not reported on the completed west turn")
			}
			for _, want := range []string{"original user instruction", "Edit the local file once.", "function_call", "function_call_output", "call-edit", "wrote cedar exactly once", "The local edit completed.", "Continue the task.", "edit_file"} {
				if !strings.Contains(westBodies[0], want) {
					t.Errorf("reconstructed history is missing %q", want)
				}
			}
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"previous_response_id": "west-1", "input": "Next west turn.", "stream": stream}, nil), stream)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"previous_response_id": "east-2", "input": "An older branch.", "stream": stream}, nil), stream)
			if len(westBodies) != 3 || strings.Contains(westBodies[2], "west answer 1") || strings.Contains(westBodies[2], "Next west turn.") {
				t.Fatal("older branch acquired newer west history")
			}
			oldOwner := h.stateBindings.lookup(stateBindingTypeResponseID, "east-2")
			route, _ := h.resolveModelRouteForRequest("coding", providerEndpointResponses)
			if oldOwner.outcome != stateBindingLookupKnown || h.stateBindings.ownerTarget(oldOwner.owner, route) != "east" {
				t.Fatal("migration relabeled old ownership")
			}
			contents, _ := os.ReadFile(sideEffect)
			if string(contents) != "cedar\n" || east.Load() != 2 || west.Load() != 3 {
				t.Fatalf("unexpected replay counts: east=%d west=%d file=%q", east.Load(), west.Load(), contents)
			}
		})
	}
}

func TestConversationMigrationFullHistoryAndIsolation(t *testing.T) {
	var outage atomic.Bool
	var calls atomic.Int32
	var westBody string
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		if outage.Load() && strings.HasPrefix(req.URL.Host, "east.") {
			return nil, errors.New("prewrite failure")
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if strings.HasPrefix(req.URL.Host, "west.") {
			westBody = string(body)
		}
		return conversationResponse(t, req, fmt.Sprintf("full-%d", calls.Add(1)), conversationText("Hello.")), nil
	})
	h, _ := newConversationAPIHandler(t, transport, nil)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"instructions": "private instructions for A", "input": "Hi."}, http.Header{"Session_id": {"client-a"}}), false)
	full := []any{map[string]any{"role": "user", "content": "Hi."}, conversationText("Hello."), map[string]any{"role": "user", "content": "Continue."}}
	before := calls.Load()
	missing := conversationPOST(t, h, map[string]any{"input": full}, http.Header{"Session_id": {"client-b"}})
	if missing.Code != 400 || calls.Load() != before || !strings.Contains(missing.Body.String(), "conversation_history_unavailable") {
		t.Fatalf("unrelated full history acquired A: %d %s", missing.Code, missing.Body.String())
	}
	outage.Store(true)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": full}, http.Header{"Session_id": {"client-a"}}), false)
	if !strings.Contains(westBody, "private instructions for A") {
		t.Fatal("matching scoped full history lost instructions")
	}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": full}, http.Header{"X-Vekil-History-Complete": {"true"}, "Session_id": {"client-b"}}), false)
	if strings.Contains(westBody, "private instructions for A") {
		t.Fatal("explicit import inherited another client's instructions")
	}
	partial := []any{conversationText("Hello."), map[string]any{"role": "user", "content": "Continue."}}
	if got := conversationPOST(t, h, map[string]any{"input": partial}, http.Header{"Session_id": {"client-a"}}); got.Code != 400 {
		t.Fatalf("partial history accepted: %d %s", got.Code, got.Body.String())
	}
}

func TestConversationMigrationBlocksUnsafeRequestsAndRetries(t *testing.T) {
	for _, scenario := range []string{"unknown ID", "compaction", "hosted tool", "unmatched result", "pending tool", "ambiguous write", "partial stream", "DONE without completion", "encrypted rejection"} {
		t.Run(scenario, func(t *testing.T) {
			var sends, west atomic.Int32
			var fail atomic.Bool
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				sends.Add(1)
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
				}
				if fail.Load() {
					switch scenario {
					case "ambiguous write":
						if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
							trace.WroteHeaders()
						}
						return nil, io.ErrUnexpectedEOF
					case "partial stream":
						return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\n"), nil
					case "DONE without completion":
						return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\ndata: [DONE]\n\n"), nil
					case "encrypted rejection":
						return routeExecutorTestResponse(req, 400, nil, `{"error":{"message":"encrypted content could not be verified"},"usage":{"output_tokens":1}}`), nil
					}
					return nil, errors.New("prewrite failure")
				}
				if scenario == "pending tool" {
					return conversationResponse(t, req, "seed", map[string]any{"type": "function_call", "call_id": "call-pending", "name": "edit", "arguments": "{}"}), nil
				}
				return conversationResponse(t, req, "seed", map[string]any{"type": "reasoning", "encrypted_content": "seed-encrypted", "summary": []any{}}, conversationText("Known earlier answer.")), nil
			})
			h, _ := newConversationAPIHandler(t, transport, nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			fail.Store(true)
			fields := map[string]any{"previous_response_id": "seed", "input": "Next."}
			switch scenario {
			case "unknown ID":
				fields["previous_response_id"] = "unknown"
			case "compaction":
				fields["input"] = []any{map[string]any{"type": "compaction", "encrypted_content": "opaque"}}
			case "hosted tool":
				fields["tools"] = []any{map[string]any{"type": "code_interpreter", "container": "provider-owned"}}
			case "unmatched result":
				fields["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "unknown", "output": "done"}}
			case "partial stream", "DONE without completion":
				fields["stream"] = true
			case "encrypted rejection":
				delete(fields, "previous_response_id")
				fields["input"] = []any{map[string]any{"role": "user", "content": "Seed."}, map[string]any{"type": "reasoning", "encrypted_content": "seed-encrypted", "summary": []any{}}, conversationText("Known earlier answer."), map[string]any{"role": "user", "content": "Next."}}
			}
			result := conversationPOST(t, h, fields, nil)
			expectedSends := int32(1)
			if scenario == "ambiguous write" || scenario == "partial stream" || scenario == "DONE without completion" || scenario == "encrypted rejection" {
				expectedSends = 2
			}
			if sends.Load() != expectedSends || west.Load() != 0 {
				t.Fatalf("unsafe retry: sends=%d west=%d response=%d %s", sends.Load(), west.Load(), result.Code, result.Body.String())
			}
			if scenario != "partial stream" && scenario != "DONE without completion" && result.Code < 400 {
				t.Fatalf("unsafe turn accepted: %d %s", result.Code, result.Body.String())
			}
			if scenario == "ambiguous write" || scenario == "partial stream" || scenario == "DONE without completion" || scenario == "encrypted rejection" {
				if !strings.Contains(result.Body.String(), "conversation_execution_uncertain") {
					t.Fatalf("missing execution uncertainty diagnostic: %d %s", result.Code, result.Body.String())
				}
				before := sends.Load()
				retry := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Try again."}, nil)
				if retry.Code != 409 || sends.Load() != before || !strings.Contains(retry.Body.String(), "conversation_execution_uncertain") {
					t.Fatalf("uncertain turn retried: %d %s", retry.Code, retry.Body.String())
				}
			}
		})
	}
}

func TestConversationMigrationSerializesOneConversation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		call := calls.Add(1)
		if call == 2 {
			close(entered)
			<-release
		}
		return conversationResponse(t, req, fmt.Sprintf("serial-%d", call), conversationText("answer")), nil
	})
	h, _ := newConversationAPIHandler(t, transport, nil)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result := conversationPOST(t, h, map[string]any{"previous_response_id": "serial-1", "input": "First."}, nil)
		if result.Code != 200 {
			t.Errorf("first turn: %d %s", result.Code, result.Body.String())
		}
	}()
	<-entered
	second := conversationPOST(t, h, map[string]any{"previous_response_id": "serial-1", "input": "Simultaneous."}, nil)
	close(release)
	wg.Wait()
	if second.Code != 409 || calls.Load() != 2 || !strings.Contains(second.Body.String(), "conversation_turn_in_progress") {
		t.Fatalf("same conversation raced: %d %s calls=%d", second.Code, second.Body.String(), calls.Load())
	}
}
