package proxy

import (
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
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type completeJSONThenErrorReadCloser struct {
	payload []byte
	offset  int
	closed  atomic.Bool
}

func (r *completeJSONThenErrorReadCloser) Read(p []byte) (int, error) {
	if r.offset >= len(r.payload) {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.offset:])
	r.offset += n
	if r.offset == len(r.payload) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (r *completeJSONThenErrorReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

type cleanupTimeoutStreamBody struct {
	prefix       *strings.Reader
	releaseCh    chan struct{}
	closeStarted chan struct{}
	releaseOnce  sync.Once
	closeOnce    sync.Once
}

func newCleanupTimeoutStreamBody(prefix string) *cleanupTimeoutStreamBody {
	return &cleanupTimeoutStreamBody{
		prefix:       strings.NewReader(prefix),
		releaseCh:    make(chan struct{}),
		closeStarted: make(chan struct{}),
	}
}

func (b *cleanupTimeoutStreamBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	<-b.releaseCh
	return 0, io.ErrClosedPipe
}

func (b *cleanupTimeoutStreamBody) Close() error {
	b.closeOnce.Do(func() { close(b.closeStarted) })
	<-b.releaseCh
	return nil
}

func (b *cleanupTimeoutStreamBody) release() {
	b.releaseOnce.Do(func() { close(b.releaseCh) })
}

func newExplicitRouteSurfaceHandler(t *testing.T, providerKind providerType, endpoint string, primaryURL, secondaryURL string) *ProxyHandler {
	t.Helper()
	provider := func(id, baseURL, key string, primary bool) ProviderConfig {
		cfg := ProviderConfig{ID: id, Type: string(providerKind), BaseURL: baseURL, Default: primary}
		switch providerKind {
		case providerTypeAzureOpenAI:
			cfg.BaseURL = strings.TrimRight(baseURL, "/") + "/openai/v1"
			cfg.APIKey = key
		case providerTypeAnthropicCompatible:
			cfg.AuthType = "none"
		default:
			cfg.AuthType = "none"
		}
		return cfg
	}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion2,
			Providers: []ProviderConfig{
				provider("primary", primaryURL, "primary-key", true),
				provider("secondary", secondaryURL, "secondary-key", false),
			},
			ModelRoutes: []ModelRouteConfig{{
				ID:        "route-public",
				PublicID:  "public-model",
				Endpoints: []string{endpoint},
				Targets: []ModelRouteTargetConfig{
					{ID: "primary-target", Provider: "primary", UpstreamModel: "physical-primary"},
					{ID: "secondary-target", Provider: "secondary", UpstreamModel: "physical-secondary"},
				},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func TestExplicitRouteOpenAIChatNormalizationReadErrorFailsBeforeCommit(t *testing.T) {
	body := &completeJSONThenErrorReadCloser{payload: []byte(`{"id":"chat-primary","object":"chat.completion","model":"physical-primary","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)}
	var calls atomic.Int32
	h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, "http://primary.invalid", "http://secondary.invalid")
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Type": []string{"application/json"}, "Openai-Model": []string{"physical-primary"}},
			Body:          body,
			ContentLength: int64(len(body.payload)),
			Request:       req,
		}, nil
	})}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`))
	h.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadGateway, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	if !body.closed.Load() {
		t.Fatal("upstream response body was not closed after normalization read failure")
	}
	if strings.Contains(w.Body.String(), "physical-primary") {
		t.Fatalf("client response exposed physical model after read failure: %s", w.Body.String())
	}
	if got := w.Header().Get("Openai-Model"); got == "physical-primary" {
		t.Fatalf("client response exposed physical model header %q", got)
	}
}

func TestExplicitRouteAnthropicJSONNormalizationFailuresBeforeCommit(t *testing.T) {
	tests := []struct {
		name string
		body func() io.ReadCloser
	}{
		{
			name: "malformed JSON",
			body: func() io.ReadCloser {
				return io.NopCloser(strings.NewReader(`{"id":"msg-primary","type":"message","model":"physical-primary"`))
			},
		},
		{
			name: "complete body with non-EOF read error",
			body: func() io.ReadCloser {
				return &completeJSONThenErrorReadCloser{payload: []byte(`{"id":"msg-primary","type":"message","role":"assistant","model":"physical-primary","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			upstreamBody := tt.body()
			h := newExplicitRouteSurfaceHandler(t, providerTypeAnthropicCompatible, providerEndpointMessages, "http://primary.invalid", "http://secondary.invalid")
			h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) != 1 {
					return nil, errors.New("unexpected failover after accepted Anthropic response")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header: http.Header{
						"Content-Type":    []string{"application/json"},
						"X-Upstream-Only": []string{"must-not-leak"},
					},
					Body:    upstreamBody,
					Request: req,
				}, nil
			})}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"public-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
			h.HandleAnthropicMessages(w, req)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadGateway, w.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls.Load())
			}
			if strings.Contains(w.Body.String(), "physical-primary") {
				t.Fatalf("client response exposed physical model: %s", w.Body.String())
			}
			if got := w.Header().Get("X-Upstream-Only"); got != "" {
				t.Fatalf("precommit normalization failure copied upstream header %q", got)
			}

			var envelope models.AnthropicError
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode Anthropic error envelope: %v; body=%s", err, w.Body.String())
			}
			if envelope.Type != "error" || envelope.Error.Type != "api_error" || envelope.Error.Message != "failed to read upstream response" {
				t.Fatalf("unexpected Anthropic error envelope: %+v", envelope)
			}
			if tracked, ok := upstreamBody.(*completeJSONThenErrorReadCloser); ok && !tracked.closed.Load() {
				t.Fatal("upstream response body was not closed after normalization read failure")
			}
		})
	}
}

func TestExplicitRouteAnthropicJSONNormalizationRewritesPublicModel(t *testing.T) {
	const upstreamResponse = `{"id":"msg-primary","type":"message","role":"assistant","model":"physical-primary","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"vendor":{"preserved":true}}`
	var calls atomic.Int32
	h := newExplicitRouteSurfaceHandler(t, providerTypeAnthropicCompatible, providerEndpointMessages, "http://primary.invalid", "http://secondary.invalid")
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{fmt.Sprintf("%d", len(upstreamResponse))},
				"Openai-Model":   []string{"physical-primary"},
			},
			Body:    io.NopCloser(strings.NewReader(upstreamResponse)),
			Request: req,
		}, nil
	})}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"public-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	h.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	if strings.Contains(w.Body.String(), "physical-primary") {
		t.Fatalf("client response exposed physical model: %s", w.Body.String())
	}
	if got := w.Header().Get("Openai-Model"); got != "public-model" {
		t.Fatalf("Openai-Model = %q, want public-model", got)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode normalized Anthropic response: %v", err)
	}
	if got := rawJSONString(payload["model"]); got != "public-model" {
		t.Fatalf("response model = %q, want public-model", got)
	}
	if !strings.Contains(string(payload["vendor"]), `"preserved":true`) {
		t.Fatalf("vendor field was not preserved: %s", payload["vendor"])
	}
}

func TestLegacyAnthropicJSONNormalizationRemainsBestEffort(t *testing.T) {
	const upstreamResponse = `{"id":"msg-legacy","type":"message","model":"physical-model"`
	var calls atomic.Int32
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "legacy",
			Type:     string(providerTypeAnthropicCompatible),
			Default:  true,
			BaseURL:  "http://legacy.invalid",
			AuthType: "none",
			Models: []ProviderModelConfig{{
				PublicID:   "public-model",
				Deployment: "physical-model",
				Endpoints:  []string{providerEndpointMessages},
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamResponse)),
			Request:    req,
		}, nil
	})}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"public-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	h.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	if got := w.Body.String(); got != upstreamResponse {
		t.Fatalf("legacy body = %q, want byte-identical %q", got, upstreamResponse)
	}
}

func TestExplicitRouteOpenAIChatHTTPRejectionPolicyAndPublicIdentity(t *testing.T) {
	t.Run("authoritative 429 switches with fresh target auth", func(t *testing.T) {
		var primaryCalls, secondaryCalls atomic.Int32
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			primaryCalls.Add(1)
			if got := r.Header.Get("api-key"); got != "primary-key" {
				t.Errorf("primary api-key = %q", got)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`)
		}))
		defer primary.Close()
		secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secondaryCalls.Add(1)
			if got := r.Header.Get("api-key"); got != "secondary-key" {
				t.Errorf("secondary api-key = %q", got)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("secondary leaked Authorization = %q", got)
			}
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			if got := request["model"]; got != "physical-secondary" {
				t.Errorf("secondary model = %#v", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Vekil-Request-ID", "upstream-spoof")
			_, _ = io.WriteString(w, `{"id":"chat-secondary","object":"chat.completion","model":"physical-secondary","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}))
		defer secondary.Close()

		h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
		ctx, summary := WithRequestSummary(context.Background())
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		w := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
			t.Fatalf("calls primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
		}
		if !strings.Contains(w.Body.String(), `"model":"public-model"`) || strings.Contains(w.Body.String(), "physical-secondary") {
			t.Fatalf("public model identity was not normalized: %s", w.Body.String())
		}
		if got := w.Header().Get("X-Vekil-Request-ID"); got == "" || got == "upstream-spoof" || got != summary.OperationID() {
			t.Fatalf("X-Vekil-Request-ID=%q summary=%q", got, summary.OperationID())
		}
	})

	t.Run("bare 503 is ambiguous and does not switch", func(t *testing.T) {
		var secondaryCalls atomic.Int32
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"gateway unavailable"}}`)
		}))
		defer primary.Close()
		secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			secondaryCalls.Add(1)
			_, _ = io.WriteString(w, `{"id":"unexpected"}`)
		}))
		defer secondary.Close()

		h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if secondaryCalls.Load() != 0 {
			t.Fatalf("secondary calls = %d, want 0", secondaryCalls.Load())
		}
	})

	t.Run("coded overload switches only after bounded certification", func(t *testing.T) {
		var secondaryCalls atomic.Int32
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"overloaded_error","code":"model_overloaded","message":"capacity"}}`)
		}))
		defer primary.Close()
		secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			secondaryCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat-secondary","object":"chat.completion","model":"physical-secondary","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}))
		defer secondary.Close()

		h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if secondaryCalls.Load() != 1 {
			t.Fatalf("secondary calls = %d, want 1", secondaryCalls.Load())
		}
	})
}

func TestExplicitRouteOpenAIChatStreamProtectsProxyOperationID(t *testing.T) {
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Vekil-Request-ID", "upstream-spoof")
		w.Header().Set("X-Request-Id", "chat-stream-request-id")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-primary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		http.Error(w, "unexpected secondary request", http.StatusInternalServerError)
	}))
	defer secondary.Close()

	h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if secondaryCalls.Load() != 0 {
		t.Fatalf("secondary calls = %d, want 0", secondaryCalls.Load())
	}
	if got := w.Header().Get("X-Vekil-Request-ID"); got == "" || got == "upstream-spoof" || got != summary.OperationID() {
		t.Fatalf("X-Vekil-Request-ID=%q summary=%q", got, summary.OperationID())
	}
	if got := w.Header().Get("X-Request-Id"); got != "chat-stream-request-id" {
		t.Fatalf("X-Request-Id = %q, want chat-stream-request-id", got)
	}
}

func TestExplicitRouteCertifiedStreamCleanupTimeoutSuppressesFailover(t *testing.T) {
	tests := []struct {
		name          string
		providerKind  providerType
		endpoint      string
		requestPath   string
		requestBody   string
		primaryStream string
		secondaryBody string
		handle        func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:          "OpenAI Chat",
			providerKind:  providerTypeAzureOpenAI,
			endpoint:      providerEndpointChatCompletions,
			requestPath:   "/v1/chat/completions",
			requestBody:   `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			primaryStream: "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			secondaryBody: "data: [DONE]\n\n",
			handle:        (*ProxyHandler).HandleOpenAIChatCompletions,
		},
		{
			name:          "Anthropic Messages",
			providerKind:  providerTypeAnthropicCompatible,
			endpoint:      providerEndpointMessages,
			requestPath:   "/v1/messages",
			requestBody:   `{"model":"public-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			primaryStream: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n",
			secondaryBody: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			handle:        (*ProxyHandler).HandleAnthropicMessages,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := newCleanupTimeoutStreamBody(tt.primaryStream)
			defer body.release()
			var calls atomic.Int32
			h := newExplicitRouteSurfaceHandler(t, tt.providerKind, tt.endpoint, "http://primary.invalid", "http://secondary.invalid")
			h.streamingUpstreamTimeout = 3 * time.Second
			h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       body,
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(tt.secondaryBody)),
					Request:    req,
				}, nil
			})}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tt.requestPath, strings.NewReader(tt.requestBody))
			started := time.Now()
			tt.handle(h, w, req)
			elapsed := time.Since(started)

			select {
			case <-body.closeStarted:
			default:
				t.Fatal("rejected stream cleanup did not attempt to close the upstream body")
			}
			body.release()
			if elapsed >= 1500*time.Millisecond {
				t.Fatalf("handler remained blocked for %v, want bounded cleanup well below the 3s inference timeout", elapsed)
			}
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadGateway, w.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want cleanup timeout to suppress failover", calls.Load())
			}
			if strings.Contains(w.Body.String(), "secondary") {
				t.Fatalf("client response unexpectedly included secondary output: %s", w.Body.String())
			}
		})
	}
}

func TestExplicitRouteForcedStreamProgressControlsFailover(t *testing.T) {
	requestBody := `{"model":"public-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	tests := []struct {
		name          string
		primaryStream string
		wantStatus    int
		wantSecondary int32
		wantText      string
	}{
		{
			name: "preamble-only rate limit switches",
			primaryStream: "data: {\"id\":\"primary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantStatus:    http.StatusOK,
			wantSecondary: 1,
			wantText:      "secondary",
		},
		{
			name: "coded overload before progress switches",
			primaryStream: "data: {\"id\":\"primary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"code\":\"model_overloaded\",\"message\":\"busy\"}}\n\n",
			wantStatus:    http.StatusOK,
			wantSecondary: 1,
			wantText:      "secondary",
		},
		{
			name: "uncoded overload is ambiguous",
			primaryStream: "data: {\"id\":\"primary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			wantStatus:    http.StatusServiceUnavailable,
			wantSecondary: 0,
			wantText:      "busy",
		},
		{
			name: "text before reset never switches",
			primaryStream: "data: {\"id\":\"primary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantStatus:    http.StatusTooManyRequests,
			wantSecondary: 0,
			wantText:      "slow down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var secondaryCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.primaryStream)
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"secondary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-secondary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"secondary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-secondary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"secondary\"},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer secondary.Close()

			h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
			w := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(w, req)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d want %d body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if secondaryCalls.Load() != tt.wantSecondary {
				t.Fatalf("secondary calls = %d want %d", secondaryCalls.Load(), tt.wantSecondary)
			}
			if !strings.Contains(w.Body.String(), tt.wantText) {
				t.Fatalf("body = %s, want %q", w.Body.String(), tt.wantText)
			}
		})
	}
}

func TestExplicitRouteTranslatedClientStreamsDoNotDuplicatePreambles(t *testing.T) {
	newUpstreams := func(t *testing.T) (*httptest.Server, *httptest.Server, *atomic.Int32) {
		t.Helper()
		primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"primary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-primary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n")
		}))
		var secondaryCalls atomic.Int32
		secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			secondaryCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"secondary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-secondary\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"secondary\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-secondary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"secondary\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		t.Cleanup(primary.Close)
		t.Cleanup(secondary.Close)
		return primary, secondary, &secondaryCalls
	}

	t.Run("Anthropic message_start", func(t *testing.T) {
		primary, secondary, secondaryCalls := newUpstreams(t)
		h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"public-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		h.HandleAnthropicMessages(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if secondaryCalls.Load() != 1 {
			t.Fatalf("secondary calls = %d", secondaryCalls.Load())
		}
		if got := strings.Count(w.Body.String(), "event: message_start"); got != 1 {
			t.Fatalf("message_start count = %d body=%s", got, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "slow down") || !strings.Contains(w.Body.String(), "secondary") {
			t.Fatalf("unexpected translated stream: %s", w.Body.String())
		}
	})

	t.Run("Gemini first frame", func(t *testing.T) {
		primary, secondary, secondaryCalls := newUpstreams(t)
		h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/public-model:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		w := httptest.NewRecorder()
		h.HandleGeminiModels(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if secondaryCalls.Load() != 1 {
			t.Fatalf("secondary calls = %d", secondaryCalls.Load())
		}
		if strings.Contains(w.Body.String(), "slow down") || strings.Count(w.Body.String(), "secondary") != 1 {
			t.Fatalf("unexpected Gemini stream: %s", w.Body.String())
		}
	})
}

func TestExplicitRouteDirectAnthropicStreamFailoverHasOneMessageStart(t *testing.T) {
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-primary\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"physical-primary\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n")
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-secondary\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"physical-secondary\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"secondary\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer secondary.Close()

	h := newExplicitRouteSurfaceHandler(t, providerTypeAnthropicCompatible, providerEndpointMessages, primary.URL, secondary.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"public-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.HandleAnthropicMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if secondaryCalls.Load() != 1 {
		t.Fatalf("secondary calls = %d", secondaryCalls.Load())
	}
	if got := strings.Count(w.Body.String(), "event: message_start"); got != 1 {
		t.Fatalf("message_start count = %d body=%s", got, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "physical-secondary") || !strings.Contains(w.Body.String(), `"model":"public-model"`) {
		t.Fatalf("direct stream model was not normalized: %s", w.Body.String())
	}
}

func TestExplicitRouteSurfacesRejectAmbiguousJSONBeforeTranslation(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		run  func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name: "OpenAI Chat",
			path: "/v1/chat/completions",
			body: `{"model":"shadow-model","model":"public-model","messages":[{"role":"user","content":"hi"}]}`,
			run:  func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleOpenAIChatCompletions(w, r) },
		},
		{
			name: "Anthropic Messages",
			path: "/v1/messages",
			body: `{"model":"shadow-model","model":"public-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			run:  func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleAnthropicMessages(w, r) },
		},
		{
			name: "Anthropic count_tokens",
			path: "/v1/messages/count_tokens",
			body: `{"model":"shadow-model","model":"public-model","messages":[{"role":"user","content":"hi"}]}`,
			run: func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) {
				h.HandleAnthropicMessagesCountTokens(w, r)
			},
		},
		{
			name: "Gemini translation",
			path: "/v1beta/models/public-model:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"contents":[]}`,
			run:  func(h *ProxyHandler, w http.ResponseWriter, r *http.Request) { h.HandleGeminiModels(w, r) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer secondary.Close()

			h := newExplicitRouteSurfaceHandler(t, providerTypeAzureOpenAI, providerEndpointChatCompletions, primary.URL, secondary.URL)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			tt.run(h, w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if upstreamCalls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
			}
			if !strings.Contains(w.Body.String(), "duplicate") {
				t.Fatalf("body = %s, want duplicate-key detail", w.Body.String())
			}
		})
	}
}

func TestAnthropicDuplicateModelSelectionPreservesLegacyForwarding(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         string
		responseBody string
		handle       func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:         "Messages",
			path:         "/v1/messages",
			body:         `{"model":"public-model","model":"legacy-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			responseBody: `{"id":"msg_legacy","type":"message","role":"assistant","model":"legacy-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			handle:       (*ProxyHandler).HandleAnthropicMessages,
		},
		{
			name:         "count_tokens",
			path:         "/v1/messages/count_tokens",
			body:         `{"model":"public-model","model":"legacy-model","messages":[{"role":"user","content":"hi"}]}`,
			responseBody: `{"input_tokens":3}`,
			handle:       (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seenModel := make(chan string, 1)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				seenModel <- payload.Model
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.responseBody)
			}))
			defer upstream.Close()

			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
				SchemaVersion: ProvidersConfigSchemaVersion2,
				Providers: []ProviderConfig{
					{
						ID:       "legacy",
						Type:     string(providerTypeAnthropicCompatible),
						Default:  true,
						BaseURL:  upstream.URL,
						AuthType: "none",
						Models: []ProviderModelConfig{{
							PublicID:   "legacy-model",
							Deployment: "legacy-model",
							Endpoints:  []string{providerEndpointMessages},
						}},
					},
					{ID: "explicit", Type: string(providerTypeAnthropicCompatible), BaseURL: upstream.URL, AuthType: "none"},
				},
				ModelRoutes: []ModelRouteConfig{{
					ID:        "explicit-route",
					PublicID:  "public-model",
					Endpoints: []string{providerEndpointMessages},
					Targets: []ModelRouteTargetConfig{{
						ID: "target", Provider: "explicit", UpstreamModel: "physical-model",
					}},
				}},
			}))
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			defer h.BeginShutdown()

			w := httptest.NewRecorder()
			tt.handle(h, w, httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls.Load())
			}
			select {
			case model := <-seenModel:
				if model != "legacy-model" {
					t.Fatalf("forwarded model = %q, want last duplicate value legacy-model", model)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream request model was not observed")
			}
		})
	}
}

func TestMixedRouteConfigScopesAmbiguousJSONValidationToExplicitRoutes(t *testing.T) {
	const (
		legacyModel        = "legacy-model"
		explicitModel      = "claude-sonnet-4.5"
		explicitModelAlias = "claude-sonnet-4-5-20250514"
	)

	var legacyCalls, explicitCalls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode legacy request: %v", err)
		} else if body["model"] != legacyModel {
			t.Errorf("legacy model = %#v, want %q", body["model"], legacyModel)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-legacy","object":"response","status":"completed","model":"legacy-model","output":[]}`)
	}))
	defer legacy.Close()

	explicit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		explicitCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode explicit request: %v", err)
		} else if body["model"] != "physical-explicit" {
			t.Errorf("explicit model = %#v, want physical-explicit", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-explicit","object":"response","status":"completed","model":"physical-explicit","output":[]}`)
	}))
	defer explicit.Close()

	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{
			{
				ID:       "legacy",
				Type:     string(providerTypeOpenAICompatible),
				Default:  true,
				BaseURL:  legacy.URL,
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:  legacyModel,
					Endpoints: []string{providerEndpointResponses},
				}},
			},
			{ID: "explicit", Type: string(providerTypeOpenAICompatible), BaseURL: explicit.URL, AuthType: "none"},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "explicit-route",
			PublicID:  explicitModel,
			Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{{
				ID: "target", Provider: "explicit", UpstreamModel: "physical-explicit",
			}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()

	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
		return w
	}

	t.Run("legacy request keeps provider-owned duplicate-key behavior", func(t *testing.T) {
		w := request(`{"model":"legacy-model","input":"first","input":"second"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if legacyCalls.Load() != 1 || explicitCalls.Load() != 0 {
			t.Fatalf("legacy calls = %d, explicit calls = %d", legacyCalls.Load(), explicitCalls.Load())
		}
	})

	t.Run("explicit route rejects before dispatch", func(t *testing.T) {
		w := request(`{"model":"claude-sonnet-4.5","input":"first","input":"second"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if legacyCalls.Load() != 1 || explicitCalls.Load() != 0 {
			t.Fatalf("legacy calls = %d, explicit calls = %d", legacyCalls.Load(), explicitCalls.Load())
		}
		if !strings.Contains(w.Body.String(), "duplicate") {
			t.Fatalf("body = %s, want duplicate-key detail", w.Body.String())
		}
	})

	t.Run("explicit route alias rejects before dispatch", func(t *testing.T) {
		w := request(`{"model":"` + explicitModelAlias + `","input":"first","input":"second"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if legacyCalls.Load() != 1 || explicitCalls.Load() != 0 {
			t.Fatalf("legacy calls = %d, explicit calls = %d", legacyCalls.Load(), explicitCalls.Load())
		}
		if !strings.Contains(w.Body.String(), "duplicate") {
			t.Fatalf("body = %s, want duplicate-key detail", w.Body.String())
		}
	})
}

func TestMixedRouteConfigOpenAIChatDuplicateModelValidationUsesForwardingSemantics(t *testing.T) {
	const (
		firstLegacyModel = "legacy-first"
		lastLegacyModel  = "legacy-last"
		explicitModel    = "explicit-model"
	)

	var legacyCalls, explicitCalls atomic.Int32
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode legacy request: %v", err)
		}
		model, _ := body["model"].(string)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"chat-legacy","object":"chat.completion.chunk","model":"`+model+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-legacy","object":"chat.completion","model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer legacy.Close()

	explicit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		explicitCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode explicit request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-explicit\",\"object\":\"chat.completion.chunk\",\"model\":\"physical-explicit\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"unexpected\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer explicit.Close()

	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{
			{
				ID:       "legacy",
				Type:     string(providerTypeOpenAICompatible),
				Default:  true,
				BaseURL:  legacy.URL,
				AuthType: "none",
				Models: []ProviderModelConfig{
					{PublicID: firstLegacyModel, Endpoints: []string{providerEndpointChatCompletions}},
					{PublicID: lastLegacyModel, Endpoints: []string{providerEndpointChatCompletions}},
				},
			},
			{ID: "explicit", Type: string(providerTypeOpenAICompatible), BaseURL: explicit.URL, AuthType: "none"},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "explicit-route",
			PublicID:  explicitModel,
			Endpoints: []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{{
				ID: "target", Provider: "explicit", UpstreamModel: "physical-explicit",
			}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()

	tests := []struct {
		name              string
		models            string
		wantStatus        int
		wantLegacyCalls   int32
		wantExplicitCalls int32
		wantResponseModel string
		stream            bool
	}{
		{
			name:       "first legacy last explicit rejects",
			models:     `"model":"legacy-first","model":"explicit-model"`,
			wantStatus: http.StatusBadRequest,
			stream:     true,
		},
		{
			name:              "first explicit last legacy follows last decoded model",
			models:            `"model":"explicit-model","model":"legacy-last"`,
			wantStatus:        http.StatusOK,
			wantLegacyCalls:   1,
			wantResponseModel: lastLegacyModel,
		},
		{
			name:       "same explicit duplicates reject",
			models:     `"model":"explicit-model","model":"explicit-model"`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:              "purely legacy duplicates keep provider behavior",
			models:            `"model":"legacy-first","model":"legacy-last"`,
			wantStatus:        http.StatusOK,
			wantLegacyCalls:   1,
			wantResponseModel: lastLegacyModel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyBefore := legacyCalls.Load()
			explicitBefore := explicitCalls.Load()
			stream := ""
			if tt.stream {
				stream = `,"stream":true`
			}
			body := `{` + tt.models + stream + `,"messages":[{"role":"user","content":"hi"}]}`
			w := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", w.Code, w.Body.String(), tt.wantStatus)
			}
			if got := legacyCalls.Load() - legacyBefore; got != tt.wantLegacyCalls {
				t.Fatalf("legacy calls = %d, want %d", got, tt.wantLegacyCalls)
			}
			if got := explicitCalls.Load() - explicitBefore; got != tt.wantExplicitCalls {
				t.Fatalf("explicit calls = %d, want %d", got, tt.wantExplicitCalls)
			}
			if tt.wantStatus == http.StatusBadRequest {
				if !strings.Contains(w.Body.String(), "duplicate") {
					t.Fatalf("body = %s, want duplicate-key detail", w.Body.String())
				}
				return
			}
			if !strings.Contains(w.Body.String(), `"model":"`+tt.wantResponseModel+`"`) {
				t.Fatalf("body = %s, want response model %q", w.Body.String(), tt.wantResponseModel)
			}
		})
	}
}

func TestExplicitRouteGeminiCompressionAliasUsesCanonicalRouteOperation(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"}}\n\n")
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "physical-secondary" {
			t.Errorf("secondary model = %#v", body["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-2\",\"model\":\"physical-secondary\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer secondary.Close()

	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: 2,
		Providers: []ProviderConfig{
			{ID: "primary", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: primary.URL, AuthType: "none"},
			{ID: "secondary", Type: string(providerTypeOpenAICompatible), BaseURL: secondary.URL, AuthType: "none"},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID: "gemini-route", PublicID: "gemini-3-pro-preview", Endpoints: []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{
				{ID: "primary-target", Provider: "primary", UpstreamModel: "physical-primary"},
				{ID: "secondary-target", Provider: "secondary", UpstreamModel: "physical-secondary"},
			},
			Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/chat-compression-3-pro:streamGenerateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	w := httptest.NewRecorder()
	h.HandleGeminiModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Vekil-Request-ID") == "" {
		t.Fatal("missing X-Vekil-Request-ID")
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("calls primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
	}
}

func TestExplicitNativeAnthropicRouteAcceptsNormalizedAlias(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "physical-claude" {
			t.Errorf("upstream model = %#v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"physical-claude","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: 2,
		Providers:     []ProviderConfig{{ID: "anthropic", Type: string(providerTypeAnthropicCompatible), Default: true, BaseURL: upstream.URL, AuthType: "none"}},
		ModelRoutes: []ModelRouteConfig{{
			ID: "claude-route", PublicID: "claude-sonnet-4.5", Endpoints: []string{providerEndpointMessages},
			Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "anthropic", UpstreamModel: "physical-claude"}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()

	w := httptest.NewRecorder()
	h.HandleAnthropicMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250514","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 || !strings.Contains(w.Body.String(), `"model":"claude-sonnet-4.5"`) {
		t.Fatalf("calls=%d body=%s", calls.Load(), w.Body.String())
	}
}

func TestExplicitRouteOpenAISurfacesAcceptDatedNormalizedAlias(t *testing.T) {
	const (
		publicModel    = "claude-sonnet-4.5"
		requestedModel = "claude-sonnet-4-5-20250514"
		upstreamModel  = "physical-claude"
	)

	tests := []struct {
		name             string
		endpoint         string
		path             string
		requestBody      string
		upstreamResponse string
		handle           func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:             "chat completions",
			endpoint:         providerEndpointChatCompletions,
			path:             "/v1/chat/completions",
			requestBody:      `{"model":"` + requestedModel + `","messages":[{"role":"user","content":"hi"}]}`,
			upstreamResponse: `{"id":"chat-alias","object":"chat.completion","model":"physical-claude","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
			handle:           (*ProxyHandler).HandleOpenAIChatCompletions,
		},
		{
			name:             "responses",
			endpoint:         providerEndpointResponses,
			path:             "/v1/responses",
			requestBody:      `{"model":"` + requestedModel + `","input":"hi"}`,
			upstreamResponse: `{"id":"resp-alias","object":"response","status":"completed","model":"physical-claude","output":[]}`,
			handle:           (*ProxyHandler).HandleResponses,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fallbackCalls, explicitCalls atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.upstreamResponse)
			}))
			defer fallback.Close()

			explicit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				explicitCalls.Add(1)
				if r.URL.Path != tt.endpoint {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, tt.endpoint)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream request: %v", err)
				} else if body["model"] != upstreamModel {
					t.Errorf("upstream model = %#v, want %q", body["model"], upstreamModel)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.upstreamResponse)
			}))
			defer explicit.Close()

			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
				SchemaVersion: ProvidersConfigSchemaVersion2,
				Providers: []ProviderConfig{
					{
						ID:       "fallback",
						Type:     string(providerTypeOpenAICompatible),
						Default:  true,
						BaseURL:  fallback.URL,
						AuthType: "none",
						Models: []ProviderModelConfig{{
							PublicID:  "fallback-model",
							Endpoints: []string{providerEndpointChatCompletions},
						}},
					},
					{ID: "explicit", Type: string(providerTypeOpenAICompatible), BaseURL: explicit.URL, AuthType: "none"},
				},
				ModelRoutes: []ModelRouteConfig{{
					ID:        "claude-route",
					PublicID:  publicModel,
					Endpoints: []string{tt.endpoint},
					Targets: []ModelRouteTargetConfig{{
						ID: "target", Provider: "explicit", UpstreamModel: upstreamModel,
					}},
				}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer h.BeginShutdown()

			w := httptest.NewRecorder()
			tt.handle(h, w, httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.requestBody)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if explicitCalls.Load() != 1 || fallbackCalls.Load() != 0 {
				t.Fatalf("explicit calls = %d, fallback calls = %d", explicitCalls.Load(), fallbackCalls.Load())
			}
			if !strings.Contains(w.Body.String(), `"model":"`+publicModel+`"`) {
				t.Fatalf("response did not preserve public model identity: %s", w.Body.String())
			}
		})
	}
}

func TestExplicitRouteCountTokenRecoveryRespectsOneSendBudget(t *testing.T) {
	t.Run("Gemini", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"use max_tokens"}}`)
		}))
		defer upstream.Close()
		h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
			SchemaVersion: 2,
			Providers:     []ProviderConfig{{ID: "openai", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: upstream.URL, AuthType: "none"}},
			ModelRoutes: []ModelRouteConfig{{
				ID: "route", PublicID: "gemini-3-pro-preview", Endpoints: []string{providerEndpointChatCompletions},
				Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "openai", UpstreamModel: "physical"}},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePrimaryOnly), MaxTargetAttempts: 1, MaxUpstreamSends: 1},
			}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer h.BeginShutdown()
		w := httptest.NewRecorder()
		h.HandleGeminiModels(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3-pro-preview:countTokens", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`)))
		if calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", calls.Load())
		}
	})

	t.Run("translated Anthropic", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"use max_tokens"}}`)
		}))
		defer upstream.Close()
		h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(ProvidersConfig{
			SchemaVersion: 2,
			Providers:     []ProviderConfig{{ID: "openai", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: upstream.URL, AuthType: "none"}},
			ModelRoutes: []ModelRouteConfig{{
				ID: "route", PublicID: "claude-route", Endpoints: []string{providerEndpointChatCompletions},
				Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "openai", UpstreamModel: "physical"}},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePrimaryOnly), MaxTargetAttempts: 1, MaxUpstreamSends: 1},
			}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer h.BeginShutdown()
		w := httptest.NewRecorder()
		h.HandleAnthropicMessagesCountTokens(w, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-route","messages":[{"role":"user","content":"hi"}]}`)))
		if calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", calls.Load())
		}
	})
}
