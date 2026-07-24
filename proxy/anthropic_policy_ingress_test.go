package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type policyCloseTrackingReader struct {
	io.Reader
	closed bool
}

func (r *policyCloseTrackingReader) Close() error {
	r.closed = true
	return nil
}

type policyErrorReadCloser struct {
	err    error
	closed bool
}

func (r *policyErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *policyErrorReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestPolicySanitizedOpenAIStreamBoundsOneEvent(t *testing.T) {
	upstream := &policyCloseTrackingReader{Reader: strings.NewReader("data: one\ndata: two\ndata: three\ndata: four\ndata: five\n\n")}
	stream := newPolicySanitizedOpenAIStream(upstream).(*policySanitizedOpenAIStream)
	stream.maxEventBytes = 32
	recorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(recorder, stream, "coding-economy", "msg-policy-oversize")
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("oversized policy stream was not sanitized: %s", body)
	}
	for _, leaked := range []string{"exceeds maximum buffer", "upstream stream read failed", "data: one"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("oversized policy stream leaked %q: %s", leaked, body)
		}
	}
	if !upstream.closed {
		t.Fatal("oversized policy SSE event did not close upstream body")
	}
}

func TestPolicySanitizedOpenAIStreamTransportErrorMapsToAnthropicError(t *testing.T) {
	upstream := &policyErrorReadCloser{err: errors.New("secret transport detail")}
	recorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(recorder, newPolicySanitizedOpenAIStream(upstream), "coding-economy", "msg-policy-transport")
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("transport failure was not sanitized: %s", body)
	}
	if strings.Contains(body, "secret transport detail") || strings.Contains(body, "upstream stream read failed") {
		t.Fatalf("transport failure detail leaked: %s", body)
	}
	if !upstream.closed {
		t.Fatal("transport failure did not close upstream body")
	}
}

func TestPolicySanitizedOpenAIStreamAllowsFoundryFilterAnnotations(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"","object":"","created":0,"model":"","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],"choices":[],"usage":null}`,
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],"usage":null}`,
		`data: {"id":"","object":"","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":"content_filter","content_filter_results":{},"content_filter_offsets":{"check_offset":4,"start_offset":0,"end_offset":2}}],"usage":null}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	recorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(recorder, newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))), "coding-economy", "msg-foundry-filter")
	body := recorder.Body.String()
	if !strings.Contains(body, `"text":"ok"`) || !strings.Contains(body, "event: message_stop") || strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("Foundry filter annotations were not preserved as a valid stream: %s", body)
	}
}

func TestPolicySanitizedOpenAIStreamAllowsOpenAIModerationChunk(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[],"usage":null,"moderation":{"input":{"type":"moderation_results","model":"omni-moderation-latest","results":[]},"output":{"type":"moderation_results","model":"omni-moderation-latest","results":[]}}}`,
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}],"usage":null}`,
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	recorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(recorder, newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))), "coding-economy", "msg-openai-moderation")
	body := recorder.Body.String()
	if !strings.Contains(body, `"text":"ok"`) || !strings.Contains(body, "event: message_stop") || strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("OpenAI moderation chunk was not preserved as a valid stream: %s", body)
	}
}

func TestPolicySanitizedOpenAIStreamRejectsMultipleChoices(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"role":"assistant","content":"first"},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":"second"},"finish_reason":null}],"usage":null}`,
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":null}],"usage":null}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	recorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(recorder, newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))), "coding-economy", "msg-unfinished-choice")
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) || strings.Contains(body, "event: message_stop") {
		t.Fatalf("unfinished policy stream was accepted: %s", body)
	}
}

func TestPolicySanitizedOpenAIStreamLatchesIncompleteDone(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}],"usage":null}`,
		`data: [DONE]`,
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"content":"late"},"finish_reason":"stop"}],"usage":null}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	filtered, err := io.ReadAll(newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))))
	if err != nil {
		t.Fatal(err)
	}
	body := string(filtered)
	if !strings.Contains(body, `"message":"upstream request failed"`) || strings.Contains(body, `"content":"late"`) || strings.Contains(body, "[DONE]") {
		t.Fatalf("incomplete terminal did not latch the sanitized stream: %s", body)
	}
}

func TestPolicySanitizedOpenAIStreamRejectsEOFBeforeDone(t *testing.T) {
	stream := "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}],\"usage\":null}\n\n"
	filtered, err := io.ReadAll(newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(filtered); !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("EOF before [DONE] was not sanitized: %s", body)
	}
}

func TestPolicySanitizedOpenAIStreamRejectsUnterminatedEventAtEOF(t *testing.T) {
	stream := `data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}],"usage":null}`
	filtered, err := io.ReadAll(newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))))
	if err != nil {
		t.Fatal(err)
	}
	body := string(filtered)
	if !strings.HasPrefix(body, "event: error\ndata: ") || strings.Contains(body, `"content":"partial"`) {
		t.Fatalf("unterminated EOF event was not replaced by a framed error: %s", body)
	}
}

func TestPolicySanitizedOpenAIStreamRejectsOutputAfterFinish(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"content":"first"},"finish_reason":"stop"}],"usage":null}`,
		`data: {"id":"chat","object":"chat.completion.chunk","created":1,"model":"light-model","choices":[{"index":0,"delta":{"content":"late"},"finish_reason":null}],"usage":null}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	filtered, err := io.ReadAll(newPolicySanitizedOpenAIStream(io.NopCloser(strings.NewReader(stream))))
	if err != nil {
		t.Fatal(err)
	}
	body := string(filtered)
	if !strings.Contains(body, `"message":"upstream request failed"`) || strings.Contains(body, `"content":"late"`) || strings.Contains(body, "[DONE]") {
		t.Fatalf("post-finish output was accepted: %s", body)
	}
}

func TestHandleAnthropicMessagesPolicyIngressPreservesForcedStreamAndPublicContract(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
	parallelUnsupported := false
	cfg.ModelRoutes[1].ParallelToolCalls = &parallelUnsupported

	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, summary := WithRequestSummary(t.Context())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"stream":false,
		"messages":[{"role":"user","content":"inspect the repository"}],
		"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],
		"tool_choice":{"type":"none"}
	}`)).WithContext(ctx)
	h.HandleAnthropicMessages(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type=%q, want non-streaming Anthropic JSON", got)
	}
	operationID := recorder.Header().Get("X-Vekil-Request-ID")
	if operationID == "" || operationID != summary.OperationID() {
		t.Fatalf("request ID/header = %q/%q", operationID, summary.OperationID())
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic response: %v; body=%s", err, recorder.Body.String())
	}
	if response.Model != "coding-economy" {
		t.Fatalf("response model=%q, want policy public id", response.Model)
	}
	if len(response.Content) != 1 || response.Content[0].Text == nil || *response.Content[0].Text != "ok" {
		t.Fatalf("response content=%+v, want text ok", response.Content)
	}

	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v, want one terminal request", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
	h.RecordRequest(summary, recorder.Code, "claude", 0)
	snapshot := h.stats.snapshot()
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].OperationID != operationID {
		t.Fatalf("request stats operation ID = %+v, want %q", snapshot.Recent, operationID)
	}
	if len(snapshot.RecentAttempts) != 1 || snapshot.RecentAttempts[0].OperationID != operationID {
		t.Fatalf("route attempt operation ID = %+v, want %q", snapshot.RecentAttempts, operationID)
	}
	parallel := light.parallelToolCallsSnapshot()
	if len(parallel) != 1 || string(parallel[0]) != "false" {
		t.Fatalf("parallel_tool_calls=%q, want policy contract false", parallel)
	}
}

func TestHandleAnthropicMessagesPolicyIngressUsesProfileTierReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name         string
		outputConfig string
		wantEffort   string
	}{
		{name: "explicit Claude effort is overridden", outputConfig: `,"output_config":{"effort":"max"}`, wantEffort: `"low"`},
		{name: "omitted Claude effort uses profile tier", wantEffort: `"low"`},
		{name: "output config without effort uses profile tier", outputConfig: `,"output_config":{"format":{"type":"json_schema"}}`, wantEffort: `"low"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
			cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
			cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
			cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
			cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"

			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(cfg),
				WithPolicyRoutingMode(PolicyRoutingModeOff),
			)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"model":"coding-economy","max_tokens":64,"messages":[{"role":"user","content":"hello"}]` + tc.outputConfig + `}`
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			reasoning := light.reasoningEffortSnapshot()
			if len(reasoning) != 1 || string(reasoning[0]) != tc.wantEffort {
				t.Fatalf("reasoning_effort=%q, want %q", reasoning, tc.wantEffort)
			}
		})
	}
}

func TestAnthropicPolicyIngressUnmappedReasoningEffortContract(t *testing.T) {
	endpoints := []struct {
		name      string
		path      string
		maxTokens string
		handle    func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{name: "messages", path: providerEndpointMessages, maxTokens: `,"max_tokens":64`, handle: (*ProxyHandler).HandleAnthropicMessages},
		{name: "count_tokens", path: providerEndpointMessagesCount, handle: (*ProxyHandler).HandleAnthropicMessagesCountTokens},
	}
	requests := []struct {
		name         string
		outputConfig string
		wantStatus   int
		wantError    string
	}{
		{name: "effort omitted", wantStatus: http.StatusOK},
		{name: "output config without effort", outputConfig: `,"output_config":{"format":{"type":"json_schema"}}`, wantStatus: http.StatusOK},
		{name: "explicit effort", outputConfig: `,"output_config":{"effort":"max"}`, wantStatus: http.StatusBadRequest, wantError: "reasoning_effort is not supported"},
	}

	for _, endpoint := range endpoints {
		for _, requestCase := range requests {
			t.Run(endpoint.name+"/"+requestCase.name, func(t *testing.T) {
				light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
				powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
				h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
					WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
					WithPolicyRoutingMode(PolicyRoutingModeOff),
				)
				if err != nil {
					t.Fatal(err)
				}
				body := `{"model":"coding-economy"` + endpoint.maxTokens + `,"messages":[{"role":"user","content":"hello"}]` + requestCase.outputConfig + `}`
				recorder := httptest.NewRecorder()
				endpoint.handle(h, recorder, httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(body)))
				if recorder.Code != requestCase.wantStatus {
					t.Fatalf("status=%d, want %d; body=%s", recorder.Code, requestCase.wantStatus, recorder.Body.String())
				}
				if requestCase.wantError != "" && !strings.Contains(recorder.Body.String(), requestCase.wantError) {
					t.Fatalf("body=%s, want error containing %q", recorder.Body.String(), requestCase.wantError)
				}
				sends, _ := light.snapshot()
				if requestCase.wantStatus == http.StatusOK {
					if sends != 1 {
						t.Fatalf("light sends=%d, want one", sends)
					}
					efforts := light.reasoningEffortSnapshot()
					if len(efforts) != 1 || len(efforts[0]) != 0 {
						t.Fatalf("reasoning efforts=%q, want one omitted value", efforts)
					}
				} else if sends != 0 {
					t.Fatalf("light sends=%d, want zero", sends)
				}
			})
		}
	}
}

func TestAnthropicPolicyIngressRejectsBlankOutputConfigEffort(t *testing.T) {
	endpoints := []struct {
		name      string
		path      string
		maxTokens string
		handle    func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{name: "messages", path: providerEndpointMessages, maxTokens: `,"max_tokens":64`, handle: (*ProxyHandler).HandleAnthropicMessages},
		{name: "count_tokens", path: providerEndpointMessagesCount, handle: (*ProxyHandler).HandleAnthropicMessagesCountTokens},
	}
	invalidEfforts := []struct {
		name string
		raw  string
	}{
		{name: "null", raw: "null"},
		{name: "empty", raw: `""`},
		{name: "whitespace", raw: `" \t\n "`},
	}

	for _, mapped := range []bool{false, true} {
		mapping := "unmapped"
		if mapped {
			mapping = "mapped"
		}
		for _, endpoint := range endpoints {
			for _, invalidEffort := range invalidEfforts {
				t.Run(mapping+"/"+endpoint.name+"/"+invalidEffort.name, func(t *testing.T) {
					light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
					powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
					cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
					if mapped {
						cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
						cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
						cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
						cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"
					}
					h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
						WithProvidersConfig(cfg),
						WithPolicyRoutingMode(PolicyRoutingModeOff),
					)
					if err != nil {
						t.Fatal(err)
					}
					body := `{"model":"coding-economy"` + endpoint.maxTokens + `,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":` + invalidEffort.raw + `}}`
					recorder := httptest.NewRecorder()
					endpoint.handle(h, recorder, httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(body)))
					if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "output_config.effort must be a non-empty string") {
						t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					if sends, _ := light.snapshot(); sends != 0 {
						t.Fatalf("light sends=%d, want zero", sends)
					}
					if sends, _ := powerful.snapshot(); sends != 0 {
						t.Fatalf("powerful sends=%d, want zero", sends)
					}
				})
			}
		}
	}
}

func TestHandleAnthropicMessagesPolicyIngressOverridesEffortOutsideRouteIntersection(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
	cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
	cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
	cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
	cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"

	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}],
		"output_config":{"effort":"max"}
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if efforts := light.reasoningEffortSnapshot(); len(efforts) != 1 || string(efforts[0]) != `"low"` {
		t.Fatalf("light reasoning efforts=%q, want low", efforts)
	}
	if requests, _ := powerful.snapshot(); requests != 0 {
		t.Fatalf("powerful requests=%d, want zero", requests)
	}
}

func TestHandleAnthropicMessagesPolicyIngressDoesNotExposePublicTerminalID(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
	cfg.ModelRoutes[0].Exposure = modelRouteExposurePublic
	cfg.ModelRoutes[0].PublicID = "light-public"
	cfg.ModelRoutes[0].Name = "Public Light Terminal"

	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "coding-economy" || strings.Contains(recorder.Body.String(), "light-public") {
		t.Fatalf("policy response exposed terminal identity: model=%q body=%s", response.Model, recorder.Body.String())
	}
}

func TestHandleAnthropicMessagesCountTokensPolicyIngressUsesPlannedProbe(t *testing.T) {
	var probe models.OpenAIRequest
	lightRequests := 0
	light := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		lightRequests++
		if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"count-probe","object":"chat.completion","model":"light-model","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer light.Close()
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.URL, powerful.server.URL, policyConfigModeOff)
	parallelUnsupported := false
	cfg.ModelRoutes[0].ParallelToolCalls = &parallelUnsupported
	cfg.ModelRoutes[1].ParallelToolCalls = &parallelUnsupported
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessagesCount, strings.NewReader(`{
		"model":"coding-economy",
		"messages":[{"role":"user","content":"count this request"}],
		"tools":[{"name":"lookup","description":"Look up data","input_schema":{"type":"object","properties":{}}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("missing X-Vekil-Request-ID")
	}
	var response models.AnthropicCountTokensResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.InputTokens != 2 {
		t.Fatalf("input_tokens=%d, want terminal prompt usage", response.InputTokens)
	}
	if lightRequests != 1 || probe.Model != "light-model" {
		t.Fatalf("light requests/model=%d/%q, want one light-model probe", lightRequests, probe.Model)
	}
	if probe.Stream == nil || *probe.Stream || probe.StreamOptions != nil {
		t.Fatalf("probe stream/stream_options=%v/%+v, want false/nil", probe.Stream, probe.StreamOptions)
	}
	if probe.Temperature == nil || *probe.Temperature != 0 || probe.MaxCompletionTokens == nil || *probe.MaxCompletionTokens != responsesChatMinimumOutputTokens || probe.MaxTokens != nil {
		t.Fatalf("probe temperature/max_completion/max=%v/%v/%v, want 0/%d/nil", probe.Temperature, probe.MaxCompletionTokens, probe.MaxTokens, responsesChatMinimumOutputTokens)
	}
	if probe.ParallelToolCalls != nil {
		t.Fatalf("probe parallel_tool_calls=%v, want omitted for unsupported terminal", probe.ParallelToolCalls)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
	assertNoRouteAttemptStats(t, h)
}

func TestAnthropicPolicyLocalTranslationErrorsPreserveIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name: "messages", path: providerEndpointMessages,
			body:   `{"model":"coding-economy","max_tokens":64,"messages":[{"role":"user","content":[{"type":"unknown"}]}]}`,
			handle: (*ProxyHandler).HandleAnthropicMessages,
		},
		{
			name: "count_tokens", path: providerEndpointMessagesCount,
			body:   `{"model":"coding-economy","messages":[{"role":"user","content":[{"type":"unknown"}]}]}`,
			handle: (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
				WithPolicyRoutingMode(PolicyRoutingModeOff),
			)
			if err != nil {
				t.Fatal(err)
			}
			ctx, summary := WithRequestSummary(t.Context())
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			recorder := httptest.NewRecorder()
			tc.handle(h, recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "translation error") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" || got != summary.OperationID() {
				t.Fatalf("request ID/header = %q/%q", got, summary.OperationID())
			}
			for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
				if got := recorder.Header().Get(name); got != "coding-economy" {
					t.Fatalf("%s = %q, want policy public ID", name, got)
				}
			}
			if sends, _ := light.snapshot(); sends != 0 {
				t.Fatalf("light sends = %d, want zero", sends)
			}
		})
	}
}

func TestHandleAnthropicMessagesPolicyIngressKeepsPrewarmNonStreaming(t *testing.T) {
	var probe models.OpenAIRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"prewarm","object":"chat.completion","model":"light-model","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":0,
		"messages":[{"role":"user","content":"warm cache"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}]
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if probe.Stream != nil || probe.StreamOptions != nil {
		t.Fatalf("prewarm stream/stream_options = %v/%+v, want omitted", probe.Stream, probe.StreamOptions)
	}
}

func TestHandleAnthropicMessagesPolicyIngressAllowsDeterministicReplayContinuation(t *testing.T) {
	const replayID = "call_vekil_AAAAAAAAAAAAAAAAAAAAAA"
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"look up the result"},
			{"role":"assistant","content":[{"type":"tool_use","id":"`+replayID+`","name":"lookup","input":{"query":"status"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+replayID+`","content":"ready"}]}
		],
		"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic response: %v; body=%s", err, recorder.Body.String())
	}
	if response.Model != "coding-economy" {
		t.Fatalf("response model=%q, want policy public id", response.Model)
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v, want deterministic baseline continuation", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
}

func TestAnthropicSplitMixedAssistantReplayMatchesInnerStore(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	route := responsesChatReplayRoute{ProviderID: "bridge", PublicModel: "light-model", UpstreamModel: "gpt-5.6-luna"}
	assistantText := "Baseline diagnosis is reproducible; I will add the regression first."
	arguments := `{"file_path":"/private/tmp/calc_test.go","old_string":"old","new_string":"new"}`
	clientArguments := map[string]any{
		"replace_all": false,
		"file_path":   "/private/tmp/calc_test.go",
		"old_string":  "old",
		"new_string":  "new",
	}
	outputItem, _ := json.Marshal(map[string]any{
		"type": "function_call", "id": "fc-edit", "call_id": "upstream-edit", "name": "Edit", "arguments": arguments,
	})
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route: route, AssistantContent: json.RawMessage(`"` + assistantText + `"`),
		OutputItems: []json.RawMessage{outputItem},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-edit", Name: "Edit", VisibleArguments: arguments, OutputItemIndex: 0,
			OptionalDefaults: responsesChatReplayOptionalDefaults{"replace_all": json.RawMessage("false")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayID := published.Projection.Calls[0].ID
	var req models.AnthropicRequest
	requestBody, _ := json.Marshal(map[string]any{
		"model": "light-model",
		"tools": []any{map[string]any{
			"name": "Edit",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path":  map[string]any{"type": "string"},
					"old_string": map[string]any{"type": "string"},
					"new_string": map[string]any{"type": "string"},
					"replace_all": map[string]any{
						"type": "boolean", "default": false,
					},
				},
				"required": []string{"file_path", "old_string", "new_string"},
			},
		}},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": assistantText}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": replayID, "name": "Edit", "input": clientArguments,
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": replayID, "content": "String to replace not found", "is_error": true,
			}}},
		},
	})
	if err := json.Unmarshal(requestBody, &req); err != nil {
		t.Fatal(err)
	}
	chat, err := TranslateAnthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || string(chat.Messages[0].Content) != `"`+assistantText+`"` || len(chat.Messages[0].ToolCalls) != 1 {
		t.Fatalf("split Anthropic assistant turn was not re-merged: %+v", chat.Messages)
	}
	chatBody, _ := json.Marshal(chat)
	if _, err := translateChatRequestToResponses(chatBody, responsesChatRequestOptions{
		ReplayStore: store,
		ReplayRoute: route,
	}); err != nil {
		t.Fatalf("split Anthropic mixed turn did not match inner replay store: %v; chat=%s", err, chatBody)
	}
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesTerminalHTTPError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalFailureStatus.Store(http.StatusBadGateway)

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"inspect the repository"}]
	}`)))

	assertSanitizedAnthropicPolicyError(t, recorder, http.StatusBadGateway)
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesTerminalStreamError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalStreamFailure.Store(true)

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"stream":true,
		"messages":[{"role":"user","content":"inspect the repository"}]
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"model":"coding-economy"`) || !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("policy stream did not preserve public identity and sanitized error: %s", body)
	}
	for _, leaked := range []string{"power-model", "power-route", "power-provider"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("policy stream leaked %q: %s", leaked, body)
		}
	}
}

func TestHandleAnthropicMessagesCountTokensPolicyIngressSanitizesTerminalError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalFailureStatus.Store(http.StatusBadGateway)

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessagesCount, strings.NewReader(`{
		"model":"coding-economy",
		"messages":[{"role":"user","content":"count this request"}]
	}`)))

	assertSanitizedAnthropicPolicyError(t, recorder, http.StatusBadGateway)
	assertNoRouteAttemptStats(t, h)
}

func assertSanitizedAnthropicPolicyError(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicError
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic error: %v; body=%s", err, recorder.Body.String())
	}
	if response.Error.Message != "upstream request failed" {
		t.Fatalf("error message=%q, want sanitized upstream failure", response.Error.Message)
	}
	for _, leaked := range []string{"terminal unavailable", "light-model", "power-model", "power-route", "power-provider"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("policy error leaked %q: %s", leaked, recorder.Body.String())
		}
	}
	for _, header := range []string{"X-Request-ID", "X-Azure-Request-ID", "OpenAI-Processing-Ms", "X-Vekil-Internal-Route", "X-RateLimit-Model", "RateLimit-Policy"} {
		if got := recorder.Header().Get(header); got != "" {
			t.Fatalf("%s=%q, want omitted", header, got)
		}
	}
	for _, header := range []string{"Openai-Model", "X-Openai-Model"} {
		if got := recorder.Header().Get(header); got != "coding-economy" {
			t.Fatalf("%s=%q, want policy public id", header, got)
		}
	}
}

func TestHandleAnthropicMessagesPolicyIngressPreservesClientStream(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"stream":true,
		"messages":[{"role":"user","content":"inspect the repository"}]
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q, want Anthropic SSE", got)
	}
	body := recorder.Body.String()
	for _, want := range []string{`event: message_start`, `"model":"coding-economy"`, `"text":"ok"`, `event: message_stop`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `"model":"light-model"`) {
		t.Fatalf("stream leaked terminal model: %s", body)
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v, want one streamed terminal request", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesMalformedStreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Openai-Model", "light-model")
		_, _ = io.WriteString(w, "data: not-json\n\n")
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicError
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Message != "upstream request failed" || strings.Contains(recorder.Body.String(), "invalid character") || strings.Contains(recorder.Body.String(), "not-json") {
		t.Fatalf("malformed upstream detail leaked: %s", recorder.Body.String())
	}
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesMalformedClientStreamError(t *testing.T) {
	for _, malformed := range []string{
		"not-json",
		`{}`,
		`{"choices":null}`,
		`{"choices":[]}`,
		`{"choices":[{}]}`,
		`{"choices":"not-an-array"}`,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null,"Finish_Reason":"stop"}]}`,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":"stop","Finish_Reason":null}]}`,
	} {
		t.Run(malformed, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", malformed)
			}))
			defer upstream.Close()
			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
				WithPolicyRoutingMode(PolicyRoutingModeOff),
			)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
				"model":"coding-economy",
				"max_tokens":64,
				"stream":true,
				"messages":[{"role":"user","content":"hello"}]
			}`)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"message":"upstream request failed"`) || strings.Contains(body, malformed) || strings.Contains(body, "invalid character") || strings.Contains(body, "cannot unmarshal") || strings.Contains(body, "event: message_stop") {
				t.Fatalf("malformed client stream detail leaked: %s", body)
			}
		})
	}
}

func TestAnthropicPolicyPlannerUnavailableUsesSanitizedServerError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "messages",
			path:   providerEndpointMessages,
			body:   `{"model":"coding-economy","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			handle: (*ProxyHandler).HandleAnthropicMessages,
		},
		{
			name:   "count_tokens",
			path:   providerEndpointMessagesCount,
			body:   `{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`,
			handle: (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
				WithPolicyRoutingMode(PolicyRoutingModeEnforce),
			)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			tc.handle(h, recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response models.AnthropicError
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Message != "upstream request failed" || strings.Contains(recorder.Body.String(), "preflight") {
				t.Fatalf("planner detail leaked: %s", recorder.Body.String())
			}
		})
	}
}
