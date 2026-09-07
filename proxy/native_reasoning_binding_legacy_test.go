package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func newLegacyNativeReasoningBindingHandler(t *testing.T, configured bool, inference http.HandlerFunc) *ProxyHandler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case providerEndpointModels:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"physical-primary","supported_endpoints":["/chat/completions"]},{"id":"physical-secondary","supported_endpoints":["/chat/completions"]},{"id":"physical-responses","supported_endpoints":["/responses"]}]}`)
		case providerEndpointChatCompletions, providerEndpointResponses:
			inference(w, r)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	options := []Option{WithCopilotBaseURL(upstream.URL)}
	if configured {
		options = append(options, WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion1,
			Providers:     []ProviderConfig{{ID: "native", Type: string(providerTypeCopilot), Default: true}},
		}))
	}
	h, err := NewProxyHandler(auth.NewTestAuthenticator("initial-token"), logger.NewWithWriter(logger.LevelError, io.Discard), options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func TestNativeReasoningBindingLegacyCredentialRotation(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, mode := range []struct {
			name             string
			streaming, tools bool
		}{
			{name: "json"},
			{name: "forced aggregation", tools: true},
			{name: "streaming", streaming: true, tools: true},
		} {
			t.Run(fmt.Sprintf("configured=%t/%s", configured, mode.name), func(t *testing.T) {
				var calls atomic.Int32
				h := newLegacyNativeReasoningBindingHandler(t, configured, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request models.OpenAIRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					signature := "opaque-initial"
					if r.Header.Get("Authorization") == "Bearer rotated-token" {
						signature = "opaque-rotated"
					}
					if len(request.Messages) > 1 && request.Messages[1].ReasoningOpaque != signature {
						t.Errorf("upstream received state from another credential: %+v", request.Messages[1])
					}
					writeNativeReasoningBindingResponse(t, w, request, signature)
				})
				first := httptest.NewRecorder()
				h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "physical-primary", mode.streaming, mode.tools))
				history := nativeReasoningBindingContent(t, first)
				if len(history) != 2 || history[0].Signature != "opaque-initial" || calls.Load() != 1 {
					t.Fatalf("initial response did not preserve reasoning: calls=%d content=%+v", calls.Load(), history)
				}
				sameAccount := httptest.NewRecorder()
				h.HandleAnthropicMessages(sameAccount, nativeReasoningBindingRequest(t, "physical-primary", mode.streaming, mode.tools, history))
				if sameAccount.Code != http.StatusOK || calls.Load() != 2 {
					t.Fatalf("same-account continuation failed: status=%d calls=%d body=%s", sameAccount.Code, calls.Load(), sameAccount.Body.String())
				}
				h.auth = auth.NewTestAuthenticator("rotated-token")
				replay := httptest.NewRecorder()
				h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "physical-primary", mode.streaming, mode.tools, history))
				if replay.Code != http.StatusBadRequest || calls.Load() != 2 {
					t.Fatalf("credential rotation replayed old state: status=%d calls=%d body=%s", replay.Code, calls.Load(), replay.Body.String())
				}
				for _, secret := range []string{"initial-token", "rotated-token", "opaque-initial"} {
					if strings.Contains(replay.Body.String(), secret) {
						t.Fatalf("credential rejection exposed state: %s", replay.Body.String())
					}
				}
				fresh := httptest.NewRecorder()
				h.HandleAnthropicMessages(fresh, nativeReasoningBindingRequest(t, "physical-primary", mode.streaming, mode.tools))
				freshHistory := nativeReasoningBindingContent(t, fresh)
				if len(freshHistory) != 2 || freshHistory[0].Signature != "opaque-rotated" || calls.Load() != 3 {
					t.Fatalf("stateless request did not use the new credential: calls=%d content=%+v", calls.Load(), freshHistory)
				}
			})
		}
	}
}

func TestNativeReasoningBindingLegacyRejectsUnknownConflictingAndCrossModelState(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%t", configured), func(t *testing.T) {
			var calls atomic.Int32
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				writeNativeReasoningBindingResponse(t, w, request, "opaque-"+request.Model)
			})
			h := newLegacyNativeReasoningBindingHandler(t, configured, upstream)
			first := httptest.NewRecorder()
			h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "physical-primary", false, false))
			primaryHistory := nativeReasoningBindingContent(t, first)
			second := httptest.NewRecorder()
			h.HandleAnthropicMessages(second, nativeReasoningBindingRequest(t, "physical-secondary", false, false))
			secondaryHistory := nativeReasoningBindingContent(t, second)
			unknownHistory := []models.ContentBlock{{Type: "thinking", Thinking: stringPtr("inspect"), Signature: "opaque-unknown"}}
			otherProcess := newLegacyNativeReasoningBindingHandler(t, configured, upstream)
			for _, tc := range []struct {
				name    string
				handler *ProxyHandler
				model   string
				history [][]models.ContentBlock
			}{
				{name: "unknown", handler: h, model: "physical-primary", history: [][]models.ContentBlock{unknownHistory}},
				{name: "mixed known and unknown", handler: h, model: "physical-primary", history: [][]models.ContentBlock{primaryHistory, unknownHistory}},
				{name: "conflicting physical owners", handler: h, model: "physical-primary", history: [][]models.ContentBlock{primaryHistory, secondaryHistory}},
				{name: "changed physical model", handler: h, model: "physical-secondary", history: [][]models.ContentBlock{primaryHistory}},
				{name: "changed native endpoint", handler: h, model: "physical-responses", history: [][]models.ContentBlock{primaryHistory}},
				{name: "another process", handler: otherProcess, model: "physical-primary", history: [][]models.ContentBlock{primaryHistory}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					response := httptest.NewRecorder()
					tc.handler.HandleAnthropicMessages(response, nativeReasoningBindingRequest(t, tc.model, false, false, tc.history...))
					if response.Code != http.StatusBadRequest || calls.Load() != 2 {
						t.Fatalf("unbound history reached inference: status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
					}
				})
			}
		})
	}
}

func TestNativeReasoningBindingLegacyRawChatReplay(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, mode := range []struct {
			name             string
			streaming, tools bool
		}{
			{name: "json"},
			{name: "forced aggregation", tools: true},
			{name: "streaming", streaming: true, tools: true},
		} {
			t.Run(fmt.Sprintf("configured=%t/%s", configured, mode.name), func(t *testing.T) {
				var calls atomic.Int32
				h := newLegacyNativeReasoningBindingHandler(t, configured, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request models.OpenAIRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if len(request.Messages) > 1 && request.Messages[1].ReasoningOpaque != "opaque-initial" {
						t.Errorf("raw Chat replay changed native state: %+v", request.Messages[1])
					}
					writeNativeReasoningBindingResponse(t, w, request, "opaque-initial")
				})
				first := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(first, nativeReasoningBindingRawRequest(t, mode.streaming, mode.tools, nil, "physical-primary"))
				message := nativeReasoningBindingRawMessage(t, first)
				if message.ReasoningOpaque != "opaque-initial" || calls.Load() != 1 {
					t.Fatalf("raw Chat response lost native state: calls=%d message=%+v", calls.Load(), message)
				}
				sameAccount := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(sameAccount, nativeReasoningBindingRawRequest(t, mode.streaming, mode.tools, &message, "physical-primary"))
				if sameAccount.Code != http.StatusOK || calls.Load() != 2 {
					t.Fatalf("raw Chat same-account continuation failed: status=%d calls=%d body=%s", sameAccount.Code, calls.Load(), sameAccount.Body.String())
				}
				h.auth = auth.NewTestAuthenticator("rotated-token")
				replay := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(replay, nativeReasoningBindingRawRequest(t, mode.streaming, mode.tools, &message, "physical-primary"))
				if replay.Code != http.StatusBadRequest || calls.Load() != 2 {
					t.Fatalf("raw Chat credential rotation replayed old state: status=%d calls=%d body=%s", replay.Code, calls.Load(), replay.Body.String())
				}
			})
		}
	}
}

func legacyNativeReasoningJSONBody(t *testing.T, model, signature string, textBytes int) []byte {
	t.Helper()
	content, err := json.Marshal(strings.Repeat("x", textBytes))
	if err != nil {
		t.Fatal(err)
	}
	finish := "stop"
	body, err := json.Marshal(models.OpenAIResponse{
		ID: "chatcmpl-legacy", Object: "chat.completion", Created: 1, Model: model,
		Choices: []models.OpenAIChoice{{Message: models.OpenAIMessage{
			Role: "assistant", Content: content, ReasoningText: "inspect", ReasoningOpaque: signature,
		}, FinishReason: &finish}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestNativeReasoningBindingLegacyOversizedRawJSONPassthrough(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, signed := range []bool{false, true} {
			t.Run(fmt.Sprintf("configured=%t/signed=%t", configured, signed), func(t *testing.T) {
				signature := ""
				if signed {
					signature = "opaque-oversized"
				}
				body := legacyNativeReasoningJSONBody(t, "physical-primary", signature, usageSniffMaxBuffer)
				var calls atomic.Int32
				h := newLegacyNativeReasoningBindingHandler(t, configured, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					_, _ = w.Write(body)
				})
				first := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(first, nativeReasoningBindingRawRequest(t, false, false, nil, "physical-primary"))
				if first.Code != http.StatusOK || calls.Load() != 1 || !bytes.Equal(first.Body.Bytes(), body) {
					t.Fatalf("oversized JSON passthrough changed: status=%d calls=%d bytes=%d want=%d", first.Code, calls.Load(), first.Body.Len(), len(body))
				}
				if signed {
					message := models.OpenAIMessage{Role: "assistant", ReasoningText: "inspect", ReasoningOpaque: signature}
					replay := httptest.NewRecorder()
					h.HandleOpenAIChatCompletions(replay, nativeReasoningBindingRawRequest(t, false, false, &message, "physical-primary"))
					if replay.Code != http.StatusBadRequest || calls.Load() != 1 {
						t.Fatalf("oversized unbound state reached inference: status=%d calls=%d", replay.Code, calls.Load())
					}
				}
			})
		}
	}
}

func TestNativeReasoningBindingLegacyLargeTranslatedResponseReplay(t *testing.T) {
	// Anthropic non-streaming output is force-aggregated from native SSE.
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_text\":\"inspect\",\"reasoning_opaque\":\"opaque-large-translated\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", usageSniffMaxBuffer) + "\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			var calls atomic.Int32
			h := newLegacyNativeReasoningBindingHandler(t, false, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			})
			first := httptest.NewRecorder()
			h.HandleAnthropicMessages(first, nativeReasoningBindingRequest(t, "physical-primary", streaming, false))
			history := nativeReasoningBindingContent(t, first)
			if len(history) != 2 || history[0].Signature != "opaque-large-translated" || history[1].Text == nil || len(*history[1].Text) != usageSniffMaxBuffer {
				t.Fatalf("large translated output changed: blocks=%d bytes=%d", len(history), first.Body.Len())
			}
			replay := httptest.NewRecorder()
			h.HandleAnthropicMessages(replay, nativeReasoningBindingRequest(t, "physical-primary", streaming, false, history))
			if replay.Code != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("large translated response state was not bound: status=%d calls=%d", replay.Code, calls.Load())
			}
		})
	}
}

func TestNativeReasoningBindingLegacyRawJSONConflictDoesNotCopyContentLength(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%t", configured), func(t *testing.T) {
			var calls atomic.Int32
			var upstreamLength atomic.Int64
			h := newLegacyNativeReasoningBindingHandler(t, configured, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				body := legacyNativeReasoningJSONBody(t, request.Model, "opaque-shared", usageSniffSmallBufferSize+1)
				upstreamLength.Store(int64(len(body)))
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body)
			})
			first := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(first, nativeReasoningBindingRawRequest(t, false, false, nil, "physical-primary"))
			if message := nativeReasoningBindingRawMessage(t, first); message.ReasoningOpaque != "opaque-shared" {
				t.Fatalf("initial response lost state: %+v", message)
			}
			downstream := httptest.NewServer(http.HandlerFunc(h.HandleOpenAIChatCompletions))
			defer downstream.Close()
			request := nativeReasoningBindingRawRequest(t, false, false, nil, "physical-secondary")
			response, err := downstream.Client().Post(downstream.URL, "application/json", request.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("conflicting-state error has invalid framing: %v (status=%d length=%d body=%s)", err, response.StatusCode, response.ContentLength, body)
			}
			if response.StatusCode != http.StatusBadGateway || calls.Load() != 2 || !json.Valid(body) {
				t.Fatalf("conflicting output was not rejected: status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
			if response.ContentLength == upstreamLength.Load() || (response.ContentLength >= 0 && response.ContentLength != int64(len(body))) {
				t.Fatalf("error retained upstream length: length=%d body=%d upstream=%d", response.ContentLength, len(body), upstreamLength.Load())
			}
			if bytes.Contains(body, []byte("opaque-shared")) {
				t.Fatalf("error exposed conflicting state: %s", body)
			}
		})
	}
}
