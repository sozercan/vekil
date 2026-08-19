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
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/logger"
)

// Upstream error prose quotes the offending request value back, so it must not
// reach the log even though the enumerated type, code and param may.
func TestUpstreamAuthoredErrorMessageStaysOutOfTheLog(t *testing.T) {
	const secret = "SSN 123-45-6789 and the user's prompt"
	upstreamBody := []byte(`{"error":{"type":"invalid_request_error","code":"bad_value","param":"messages","message":"Invalid value for 'messages[0].content': ` + secret + `"}}`)

	for _, tc := range []struct {
		name string
		err  error
		want map[string]string
	}{
		{
			name: "upstream responses error keeps its classifiers but loses the prose",
			err:  responsesChatExecutionErrorFromUpstream(&upstreamError{statusCode: 400, body: upstreamBody}),
			want: map[string]string{"error_type": "invalid_request_error", "error_code": "bad_value", "error_param": "messages"},
		},
		{
			name: "upstream stream termination loses the prose",
			err:  chatExecutionErrorFromStreamTermination(errors.New("upstream said " + secret)),
			want: map[string]string{"error_type": "server_error", "error_code": "responses_stream_failed"},
		},
		{
			name: "vekil's own diagnosis is logged in full",
			err:  replayChatExecutionError(responsesChatReplayProjectionCode, responsesChatReplayProjectionMessage),
			want: map[string]string{
				"error_type":    "invalid_request_error",
				"error_code":    responsesChatReplayProjectionCode,
				"error_param":   "messages",
				"error_message": responsesChatReplayProjectionMessage,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, summary := WithRequestSummary(context.Background())
			var executionErr *chatExecutionError
			if !errors.As(tc.err, &executionErr) {
				t.Fatalf("err = %T, want *chatExecutionError", tc.err)
			}
			observeChatExecutionError(ctx, executionErr)

			got := map[string]string{}
			for _, field := range summary.LoggerFields() {
				if !strings.HasPrefix(field.Key, "error_") {
					continue
				}
				value, ok := field.Value.(string)
				if !ok {
					t.Fatalf("%s = %#v, want a string", field.Key, field.Value)
				}
				if strings.Contains(value, secret) {
					t.Fatalf("%s leaked request content: %q", field.Key, value)
				}
				got[field.Key] = value
			}
			if len(got) != len(tc.want) {
				t.Fatalf("fields = %#v, want %#v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("fields[%s] = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

// The wiring, not the helper. A non-200 upstream reply arrives as a SUCCESSFUL result, so
// the execution-error path that fills these fields never runs and the boundary has to do
// it. Probed against Copilot before this existed: a real 400 logged the prose and recorded
// no classifier at all.
func TestAnthropicUpstreamErrorRecordsClassifiersThroughTheHandler(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// The envelope Copilot actually returned for an invalid tool schema.
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid schema for function 't': ` +
			`'not-a-real-type' is not valid","code":"invalid_value",` +
			`"param":"tools[0].input_schema","type":"invalid_request_error"}}`))
	})

	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)).
		WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.HandleAnthropicMessages(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	got := map[string]string{}
	for _, field := range summary.LoggerFields() {
		if !strings.HasPrefix(field.Key, "error_") {
			continue
		}
		value, _ := field.Value.(string)
		got[field.Key] = value
	}
	if got["error_code"] != "invalid_value" || got["error_param"] != "tools" {
		t.Fatalf("classifiers not recorded through the handler: %#v", got)
	}
	for key, value := range got {
		if strings.Contains(value, "not-a-real-type") || strings.Contains(value, "Invalid schema") {
			t.Fatalf("%s leaked upstream prose: %q", key, value)
		}
	}
}

func TestAnthropicCountTokensErrorLogsOnlySafeClassifiers(t *testing.T) {
	const secret = "SSN 123-45-6789 and the user prompt"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid value quoted from request: ` + secret + `","code":"bad_value","param":"messages[0].content","type":"invalid_request_error"}}`))
	})
	var logs bytes.Buffer
	handler.log = logger.NewWithWriter(logger.LevelDebug, &logs)

	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.HandleAnthropicMessagesCountTokens(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "Invalid value quoted") {
		t.Fatalf("count_tokens logs exposed the upstream error body: %s", logs.String())
	}
	got := map[string]string{}
	for _, field := range summary.LoggerFields() {
		if value, ok := field.Value.(string); ok && strings.HasPrefix(field.Key, "error_") {
			got[field.Key] = value
		}
	}
	if got["error_type"] != "invalid_request_error" || got["error_code"] != "bad_value" || got["error_param"] != "messages" {
		t.Fatalf("classifiers = %#v", got)
	}
}

// Character shape is not proof: every rune in `SSN_123-45-6789` is one a classifier may
// contain. These match the grammars instead -- a lowercase identifier, a JSON path.
func TestUpstreamClassifiersMatchTheirGrammarNotJustTheirCharacters(t *testing.T) {
	for _, tc := range []struct {
		name, code, param string
		wantCode          string
		wantParam         string
	}{
		{name: "copilot top-level code", code: "invalid_request_body", param: "", wantCode: "invalid_request_body"},
		{name: "json path keeps only its root", code: "invalid_value", param: "tools[0].input_schema", wantCode: "invalid_value", wantParam: "tools"},
		// The whole reason the root is not enough on its own: a client-chosen key is a
		// perfectly ordinary lower-snake identifier, so the grammar cannot tell it apart.
		{name: "a client's metadata key is not part of the path", code: "", param: "metadata.customer_ssn", wantParam: "metadata"},
		{name: "a client key under a tool schema is not part of the path", code: "", param: "tools[0].input_schema.properties.customer_ssn", wantParam: "tools"},
		{name: "a client-controlled numeric suffix is not logged", code: "", param: "user[123456789]", wantParam: "user"},
		{name: "a bare client key has no recognised root at all", code: "", param: "customer_ssn", wantParam: ""},
		{name: "an unrecognised root is dropped rather than guessed", code: "", param: "not_a_request_field.id", wantParam: ""},
		{name: "uppercase is not a code", code: "SSN_123-45-6789", param: "", wantCode: ""},
		{name: "prose is not a code", code: "Invalid value: the prompt", param: "", wantCode: ""},
		{name: "hyphens are not a code", code: "some-value-here", param: "", wantCode: ""},
		{name: "prose is not a path", code: "", param: "the user said hello", wantParam: ""},
		{name: "unbounded index is not a path", code: "", param: "messages[abc].content", wantParam: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeUpstreamClassifierCode(tc.code); got != tc.wantCode {
				t.Fatalf("code %q -> %q, want %q", tc.code, got, tc.wantCode)
			}
			if got := safeUpstreamClassifierParam(tc.param); got != tc.wantParam {
				t.Fatalf("param %q -> %q, want %q", tc.param, got, tc.wantParam)
			}
		})
	}
}

// The envelope Copilot actually returns from /v1/responses: classifiers at the TOP level,
// no `error` wrapper. Captured by probing the live API.
func TestCopilotTopLevelErrorEnvelopeYieldsItsCode(t *testing.T) {
	body := []byte(`{"code":"invalid_request_body","message":"Invalid value: 'not-a-real-effort'."}`)

	ctx, summary := WithRequestSummary(context.Background())
	observeUpstreamErrorDetail(ctx, 400, body)

	got := map[string]string{}
	for _, field := range summary.LoggerFields() {
		if strings.HasPrefix(field.Key, "error_") {
			got[field.Key], _ = field.Value.(string)
		}
	}
	if got["error_code"] != "invalid_request_body" {
		t.Fatalf("top-level code lost: %#v", got)
	}
	for key, value := range got {
		if strings.Contains(value, "not-a-real-effort") {
			t.Fatalf("%s leaked the rejected value: %q", key, value)
		}
	}
}

// Pins behaviour this change relies on rather than adds: the canonical Responses stream
// already records the upstream classifier and already withholds the prose. A review claimed
// otherwise; a correct fixture showed it does. Without this test the next reader re-adds the
// redundant observe call, as I did.
func TestStreamingResponsesFailureRecordsAClassifier(t *testing.T) {
	// Reuse the real fixture's prelude: a lone response.failed is rejected as
	// invalid_responses_stream, which is vekil's OWN error and records classifiers of
	// its own, hiding what is under test.
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_one_tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	created, _, ok := strings.Cut(string(fixture), "\n\n")
	if !ok {
		t.Fatal("fixture has no first event")
	}
	failed := created + "\n\n" + "event: response.failed\n" +
		`data: {"type":"response.failed","sequence_number":1,"response":{"status":"failed",` +
		`"error":{"code":"insufficient_quota","message":"quota exceeded for this key"}}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(failed))
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})

	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-public","max_tokens":128,"stream":true,`+
			`"messages":[{"role":"user","content":"go"}]}`)).WithContext(ctx)

	h.HandleAnthropicMessages(httptest.NewRecorder(), req)

	got := map[string]string{}
	for _, field := range summary.LoggerFields() {
		if strings.HasPrefix(field.Key, "error_") {
			got[field.Key], _ = field.Value.(string)
		}
	}
	if got["error_code"] != "insufficient_quota" {
		t.Fatalf("upstream classifier not recorded from a failed stream: %#v", got)
	}
	for key, value := range got {
		if strings.Contains(value, "quota exceeded") {
			t.Fatalf("%s leaked upstream prose: %q", key, value)
		}
	}
}

// vekil authors these, but interpolates the client's own text into them: an unknown field's
// NAME becomes the param, and an unsupported value is echoed into the message. Neither is
// ours to log. The grammar gate alone does not catch it -- a key like "customer_ssn" is a
// perfectly well-formed lower-snake segment.
func TestClientDerivedErrorFieldsStayOutOfTheLog(t *testing.T) {
	const clientKey = "customer_ssn"

	for _, tc := range []struct {
		name string
		err  *chatExecutionError
		want map[string]string
	}{
		{
			name: "an unknown field's name is dropped, the path that found it is kept",
			err:  newChatInvalidRequestClientField("metadata", clientKey, "unsupported JSON field"),
			want: map[string]string{"error_type": "invalid_request_error", "error_param": "metadata"},
		},
		{
			name: "a top-level unknown field leaves no param at all",
			err:  newChatInvalidRequestClientField("", clientKey, "unsupported JSON field"),
			want: map[string]string{"error_type": "invalid_request_error"},
		},
		{
			// A JSON key may contain dots. Trimming the joined path at its last dot leaves
			// half the key behind, and the remainder still passes the grammar filter.
			name: "a client key containing dots leaves nothing of itself behind",
			err:  newChatInvalidRequestClientField("metadata", clientKey+".value", "unsupported JSON field"),
			want: map[string]string{"error_type": "invalid_request_error", "error_param": "metadata"},
		},
		{
			// The param vekil built is kept. The message is not, even though this one happens
			// to be a literal: newChatInvalidRequest is also called with a client value
			// quoted into the message, so the constructor cannot vouch for what it was given.
			name: "vekil's own static param survives, its message does not",
			err:  newChatInvalidRequest("messages[0].content", "message content is required"),
			want: map[string]string{"error_type": "invalid_request_error", "error_param": "messages[0].content"},
		},
		{
			// The shape that made the default matter, from policy_responses_translate.go:
			// vekil's own prose with a client value quoted into it. Logging local messages by
			// default put arbitrary request content in the logs.
			name: "a client value quoted into vekil's prose goes with the message",
			err: newChatInvalidRequest("input[0].type",
				fmt.Sprintf("Responses input item type %q is not supported for policy models", clientKey)),
			want: map[string]string{"error_type": "invalid_request_error", "error_param": "input[0].type"},
		},
		{
			// Opting in is what makes the field useful: this is the diagnosis the carrier
			// work exists to surface, and its constructor owns the whole string.
			name: "a constructor that declares its message constant still logs it",
			err:  missingResponsesChatReplayError(),
			want: map[string]string{
				"error_type":    "invalid_request_error",
				"error_code":    "responses_replay_state_missing",
				"error_param":   "messages",
				"error_message": "Responses-backed tool state is no longer available; restart the assistant tool-call turn.",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Guard: a fixture that never carries the key would pass for the wrong reason.
			if tc.err.clientDerived && !strings.Contains(tc.err.Param, clientKey) {
				t.Fatalf("fixture param %q does not carry the client key; this proves nothing", tc.err.Param)
			}
			ctx, summary := WithRequestSummary(context.Background())
			observeChatExecutionError(ctx, tc.err)

			got := map[string]string{}
			for _, field := range summary.LoggerFields() {
				if !strings.HasPrefix(field.Key, "error_") {
					continue
				}
				value, _ := field.Value.(string)
				if strings.Contains(value, clientKey) {
					t.Fatalf("%s leaked the client's field name: %q", field.Key, value)
				}
				got[field.Key] = value
			}
			if len(got) != len(tc.want) {
				t.Fatalf("fields = %#v, want %#v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("fields[%s] = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

// The same wiring on the Chat surface, which had it only on the Anthropic one. A
// Responses-backed non-2xx is canonicalized into a SUCCESSFUL chatExecutionResult, so no
// execution error is built here either -- and this path logged a warn with no reason at all.
func TestChatUpstreamErrorRecordsClassifiersThroughTheHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid schema for function 't': ` +
			`'not-a-real-type' is not valid","code":"invalid_value",` +
			`"param":"tools[0].input_schema","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-public","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	got := map[string]string{}
	for _, field := range summary.LoggerFields() {
		if !strings.HasPrefix(field.Key, "error_") {
			continue
		}
		value, _ := field.Value.(string)
		got[field.Key] = value
	}
	if got["error_code"] != "invalid_value" || got["error_param"] != "tools" {
		t.Fatalf("classifiers not recorded through the Chat handler: %#v", got)
	}
	for key, value := range got {
		if strings.Contains(value, "not-a-real-type") || strings.Contains(value, "Invalid schema") {
			t.Fatalf("%s leaked upstream prose: %q", key, value)
		}
	}
}

func TestNativeChatUpstreamErrorRecordsClassifiersAndPreservesBody(t *testing.T) {
	errorJSON := `{"error":{"message":"Invalid schema for function 't': 'not-a-real-type' is not valid",` +
		`"code":"invalid_value","param":"tools[0].input_schema","type":"invalid_request_error"}}`
	upstreamBody := errorJSON + strings.Repeat(" ", upstreamErrorDetailMaxBodyBytes)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != providerEndpointChatCompletions {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, providerEndpointChatCompletions)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	for _, testCase := range []struct {
		name  string
		model string
		new   func(*testing.T) *ProxyHandler
	}{
		{
			name:  "default provider route",
			model: "gpt-public",
			new: func(t *testing.T) *ProxyHandler {
				return newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointChatCompletions})
			},
		},
		{
			name:  "declared model route",
			model: "public-model",
			new: func(t *testing.T) *ProxyHandler {
				return newGeminiCountTokensRouteTestHandler(t,
					[]ProviderConfig{{ID: "primary", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: upstream.URL, AuthType: "none"}},
					[]ModelRouteTargetConfig{{ID: "target-primary", Provider: "primary", UpstreamModel: "physical-primary"}},
					ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 1, MaxUpstreamSends: 1},
				)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"`+testCase.model+`","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			testCase.new(t).HandleOpenAIChatCompletions(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Body.String() != upstreamBody {
				t.Fatalf("passthrough body changed: got %d bytes, want %d", recorder.Body.Len(), len(upstreamBody))
			}
			got := map[string]string{}
			for _, field := range summary.LoggerFields() {
				if !strings.HasPrefix(field.Key, "error_") {
					continue
				}
				value, _ := field.Value.(string)
				got[field.Key] = value
			}
			if got["error_type"] != "invalid_request_error" || got["error_code"] != "invalid_value" || got["error_param"] != "tools" {
				t.Fatalf("classifiers = %#v", got)
			}
			for key, value := range got {
				if strings.Contains(value, "not-a-real-type") || strings.Contains(value, "Invalid schema") {
					t.Fatalf("%s leaked upstream prose: %q", key, value)
				}
			}
		})
	}
}

func TestExplicitNativeChatStreamOptionsRecoveryClearsDiscardedClassifiers(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request %d: %v", call, err)
			http.Error(w, "invalid test request", http.StatusInternalServerError)
			return
		}
		switch call {
		case 1:
			if _, ok := payload["stream_options"]; !ok {
				t.Errorf("first request is missing injected stream_options: %v", payload)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field stream_options","type":"invalid_request_error","code":"unsupported_parameter","param":"stream_options"}}`))
		case 2:
			if _, ok := payload["stream_options"]; ok {
				t.Errorf("protocol recovery retained stream_options: %v", payload)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-recovery\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		default:
			t.Errorf("unexpected upstream request %d", call)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	handler := newGeminiCountTokensRouteTestHandler(t,
		[]ProviderConfig{{ID: "primary", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: upstream.URL, AuthType: "none"}},
		[]ModelRouteTargetConfig{{ID: "target-primary", Provider: "primary", UpstreamModel: "physical-primary"}},
		ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 1, MaxUpstreamSends: 2},
	)
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after protocol recovery; body = %s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	for _, field := range summary.LoggerFields() {
		if strings.HasPrefix(field.Key, "error_") {
			t.Fatalf("successful recovery retained discarded classifier %s=%#v", field.Key, field.Value)
		}
	}
}

func TestExplicitForcedStreamFailoverRecapturesFinalResponseClassifiers(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n")
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid client tool schema","type":"invalid_request_error","code":"bad_value","param":"tools[0].function.parameters"}}`)
	}))
	defer secondary.Close()

	handler := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"public-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(recorder, req)

	if recorder.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusTeapot, recorder.Body.String())
	}
	got := map[string]string{}
	for _, field := range summary.LoggerFields() {
		if value, ok := field.Value.(string); ok && strings.HasPrefix(field.Key, "error_") {
			got[field.Key] = value
		}
	}
	if got["error_type"] != "invalid_request_error" || got["error_code"] != "bad_value" || got["error_param"] != "tools" {
		t.Fatalf("final response classifiers = %#v", got)
	}
}

// Gemini's non-200 branches read the body to build their detail line in addition to the shared
// execution-boundary classification. Keep both route shapes covered so refactoring either
// translation branch cannot silently drop the safe classifiers again.
func TestGeminiUpstreamErrorRecordsClassifiersThroughTheHandler(t *testing.T) {
	upstreamFail := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid schema for function 't': ` +
			`'not-a-real-type' is not valid","code":"invalid_value",` +
			`"param":"tools[0].input_schema","type":"invalid_request_error"}}`))
	}

	for _, tc := range []struct {
		name    string
		model   string
		handler func(t *testing.T) *ProxyHandler
	}{
		{
			name:  "no route operation",
			model: "gemini-3-pro-preview",
			handler: func(t *testing.T) *ProxyHandler {
				return newTestProxyHandler(t, upstreamFail)
			},
		},
		{
			name:  "explicit route operation",
			model: "public-model",
			handler: func(t *testing.T) *ProxyHandler {
				upstream := httptest.NewServer(http.HandlerFunc(upstreamFail))
				t.Cleanup(upstream.Close)
				return newGeminiCountTokensRouteTestHandler(t,
					[]ProviderConfig{{ID: "primary", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: upstream.URL, AuthType: "none"}},
					[]ModelRouteTargetConfig{{ID: "target-primary", Provider: "primary", UpstreamModel: "physical-primary"}},
					ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 1, MaxUpstreamSends: 1},
				)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+tc.model+":generateContent",
				strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			tc.handler(t).HandleGeminiModels(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			got := map[string]string{}
			for _, field := range summary.LoggerFields() {
				if !strings.HasPrefix(field.Key, "error_") {
					continue
				}
				value, _ := field.Value.(string)
				got[field.Key] = value
			}
			if got["error_code"] != "invalid_value" || got["error_param"] != "tools" {
				t.Fatalf("classifiers not recorded through the Gemini handler: %#v", got)
			}
			for key, value := range got {
				if strings.Contains(value, "not-a-real-type") || strings.Contains(value, "Invalid schema") {
					t.Fatalf("%s leaked upstream prose: %q", key, value)
				}
			}
		})
	}
}

// The leak this closes came in through a path no constructor could vouch for:
// walkPolicyResponsesJSON joins the CLIENT's object keys into the path it reports, then hands
// it to newChatInvalidRequest, which takes its param as an argument. Gating only the
// constructor that knows it was given a client field name left this one wide open, so the gate
// is on the segments instead -- a name vekil does not own ends the path, whatever built it.
func TestLocalParamsKeepOnlyTheSegmentsVekilOwns(t *testing.T) {
	const clientKey = "customer_ssn"

	t.Run("through the real JSON walker", func(t *testing.T) {
		// A duplicate key nested under metadata: the walker reports the full path it built.
		body := []byte(`{"metadata":{"` + clientKey + `.value":1,"` + clientKey + `.value":2}}`)
		err := validatePolicyResponsesJSON(body)
		if err == nil {
			t.Fatal("walker accepted a duplicate key; this test would prove nothing")
		}
		var executionErr *chatExecutionError
		if !errors.As(err, &executionErr) {
			t.Fatalf("err = %T, want *chatExecutionError", err)
		}
		if !strings.Contains(executionErr.Param, clientKey) {
			t.Fatalf("walker no longer builds the path from the client key (%q); this test is obsolete", executionErr.Param)
		}

		ctx, summary := WithRequestSummary(context.Background())
		observeChatExecutionError(ctx, executionErr)
		for _, field := range summary.LoggerFields() {
			value, _ := field.Value.(string)
			if strings.Contains(value, clientKey) {
				t.Fatalf("%s leaked a client JSON key: %q", field.Key, value)
			}
		}
		got := map[string]string{}
		for _, field := range summary.LoggerFields() {
			if strings.HasPrefix(field.Key, "error_") {
				got[field.Key], _ = field.Value.(string)
			}
		}
		// Nothing, not a prefix: this walker traverses the CLIENT's JSON, so no segment of
		// the path it built is vekil's to log.
		if got["error_param"] != "" {
			t.Fatalf("error_param = %q, want nothing from a wholly client-authored path", got["error_param"])
		}
	})

	// A key that SPELLS an owned root with a numeric subscript defeats a segment allowlist on
	// looks alone -- "user[123456789]" passes ownedParamSegment and carried nine client-chosen
	// digits into the log. The walker no longer offers the path for logging at all, which is
	// what actually closes this; the allowlist below is the second line, not the first.
	t.Run("a client key that spells an owned segment", func(t *testing.T) {
		body := []byte(`{"metadata":{"user[123456789]":1,"user[123456789]":2}}`)
		err := validatePolicyResponsesJSON(body)
		var executionErr *chatExecutionError
		if !errors.As(err, &executionErr) {
			t.Fatalf("err = %T, want *chatExecutionError", err)
		}
		if !strings.Contains(executionErr.Param, "123456789") {
			t.Fatalf("walker no longer reports the client key (%q); this test is obsolete", executionErr.Param)
		}
		if ok := ownedParamSegment("user[123456789]"); !ok {
			t.Fatal("the allowlist now rejects this segment; this test no longer covers the bypass it was written for")
		}
		ctx, summary := WithRequestSummary(context.Background())
		observeChatExecutionError(ctx, executionErr)
		for _, field := range summary.LoggerFields() {
			if value, _ := field.Value.(string); strings.Contains(value, "123456789") {
				t.Fatalf("%s leaked client-chosen digits: %q", field.Key, value)
			}
		}
	})

	// The same bypass, reached through the Chat validators rather than the policy walker: an
	// unknown top-level key and an unknown stream option are both the client's spelling, and
	// "user[123456789]" spells an owned root with a numeric subscript.
	for _, tc := range []struct {
		name string
		run  func(map[string]json.RawMessage) error
	}{
		{"unknown top-level Chat field", func(raw map[string]json.RawMessage) error {
			return validateChatResponsesTopLevel(raw)
		}},
		{"unknown stream option", func(raw map[string]json.RawMessage) error {
			_, err := parseChatStreamOptions(map[string]json.RawMessage{
				"stream_options": json.RawMessage(`{"user[123456789]":true}`),
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(map[string]json.RawMessage{"user[123456789]": json.RawMessage(`1`)})
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) {
				t.Fatalf("err = %v (%T), want *chatExecutionError", err, err)
			}
			if !strings.Contains(executionErr.Param, "123456789") {
				t.Fatalf("validator no longer reports the client key (%q); this test is obsolete", executionErr.Param)
			}
			ctx, summary := WithRequestSummary(context.Background())
			observeChatExecutionError(ctx, executionErr)
			for _, field := range summary.LoggerFields() {
				if value, _ := field.Value.(string); strings.Contains(value, "123456789") {
					t.Fatalf("%s leaked client-chosen digits: %q", field.Key, value)
				}
			}
		})
	}

	for _, tc := range []struct{ name, param, want string }{
		{"a path vekil built survives whole", "messages[0].content", "messages[0].content"},
		{"an owned nested field survives", "input[3].type", "input[3].type"},
		{"a client key ends the path", "metadata." + clientKey, "metadata"},
		{"and takes its own children with it", "metadata." + clientKey + ".value", "metadata"},
		{"a client key under a tool schema ends it too", "tools[0].parameters." + clientKey, "tools[0].parameters"},
		{"a bare client key leaves nothing", clientKey, ""},
		{"a quoted subscript is not an index", `metadata["` + clientKey + `"]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeLocalClassifierParam(tc.param); got != tc.want {
				t.Fatalf("safeLocalClassifierParam(%q) = %q, want %q", tc.param, got, tc.want)
			}
		})
	}
}

// A stream that fails AFTER committing bytes never reaches the execution-error path: the
// handler is already past it, and the stream callbacks recorded usage and a failure status but
// not the typed error. So the warn this branch added carried a stats_status and no reason,
// even though responses_chat_stream.go had just finished classifying the failure.
func TestPostCommitStreamFailureRecordsClassifiers(t *testing.T) {
	text, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	cut := strings.LastIndex(string(text), "event: response.completed")
	if cut < 0 {
		t.Fatal("fixture no longer ends with response.completed; cannot build a post-commit failure")
	}
	// Same stream, but the terminal event fails instead of completing: the text deltas above
	// have already been written downstream by then, which is what makes it post-commit.
	sse := string(text)[:cut] + "event: response.failed\n" +
		`data: {"type":"response.failed","sequence_number":9,"response":` +
		`{"id":"resp_synth_text_stream_001","object":"response","created_at":1700000000,` +
		`"status":"failed","model":"gpt-synthetic-responses","output":[],"parallel_tool_calls":true,` +
		`"error":{"type":"invalid_request_error","code":"invalid_value","param":"tools[0].input_schema",` +
		`"message":"Invalid schema for function 't': 'not-a-real-type' is not valid"}}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name string
		run  func(h *ProxyHandler, w http.ResponseWriter, r *http.Request)
		path string
		body string
	}{
		{
			name: "anthropic", path: "/v1/messages",
			body: `{"model":"gpt-public","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			run:  func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleAnthropicMessages(w, r) },
		},
		{
			name: "openai chat", path: "/v1/chat/completions",
			body: `{"model":"gpt-public","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			run:  func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleOpenAIChatCompletions(w, r) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			tc.run(h, rec, req)

			got := map[string]string{}
			for _, field := range summary.LoggerFields() {
				if strings.HasPrefix(field.Key, "error_") {
					got[field.Key], _ = field.Value.(string)
				}
			}
			if got["error_code"] != "invalid_value" || got["error_type"] != "invalid_request_error" {
				t.Fatalf("post-commit failure recorded no classifiers: %#v (status %d)", got, rec.Code)
			}
			if got["error_param"] != "tools" {
				t.Fatalf("error_param = %q, want the owned root", got["error_param"])
			}
			for key, value := range got {
				if strings.Contains(value, "not-a-real-type") || strings.Contains(value, "Invalid schema") {
					t.Fatalf("%s leaked upstream prose: %q", key, value)
				}
			}
		})
	}
}
