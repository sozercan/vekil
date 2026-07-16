package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const responsesChatRetryEncryptedToken = "gAAAAABresponsesChatReplayTokenpQ=="

type observedResponsesChatRetryRequest struct {
	path           string
	authorization  string
	providerHeader string
	model          string
	input          []map[string]any
}

func TestExecuteResponsesBackedChatRetriesEncryptedReplayOnCapturedRoute(t *testing.T) {
	successBody, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}

	var requestsMu sync.Mutex
	requests := make([]observedResponsesChatRetryRequest, 0, 2)
	var fallbackRequests atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == providerEndpointModels {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		fallbackRequests.Add(1)
		http.Error(w, "retry escaped captured provider", http.StatusInternalServerError)
	}))
	t.Cleanup(fallback.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed, decodeErr := decodeObservedResponsesChatRetryRequest(r)
		if decodeErr != nil {
			t.Errorf("decode upstream request: %v", decodeErr)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requestsMu.Lock()
		requests = append(requests, observed)
		attempt := len(requests)
		requestsMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch attempt {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"The encrypted content could not be verified. Reason: Encrypted content could not be decrypted or parsed.","code":"invalid_request_body"}}`)
		case 2:
			_, _ = w.Write(successBody)
		default:
			t.Errorf("unexpected upstream attempt %d", attempt)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(upstream.Close)

	h := newResponsesChatEncryptedRetryTestHandler(t, upstream.URL, fallback.URL)
	result, err := h.executeChatCompletions(context.Background(), responsesChatEncryptedReplayRequestBody(t, h), chatExecutionOptions{})
	if err != nil {
		t.Fatalf("executeChatCompletions() error = %v", err)
	}
	if result.Backend != chatBackendResponses || result.Completion == nil || result.Response != nil {
		t.Fatalf("result = %#v", result)
	}

	requestsMu.Lock()
	gotRequests := append([]observedResponsesChatRetryRequest(nil), requests...)
	requestsMu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("upstream requests = %d, want initial request plus one sanitized retry", len(gotRequests))
	}
	if got := fallbackRequests.Load(); got != 0 {
		t.Fatalf("fallback provider requests = %d, want captured provider reused without route resolution", got)
	}
	for i, request := range gotRequests {
		if request.path != "/captured/responses" {
			t.Fatalf("request %d path = %q, want captured Responses path", i+1, request.path)
		}
		if request.authorization != "Bearer captured-secret" {
			t.Fatalf("request %d Authorization = %q, want captured provider auth", i+1, request.authorization)
		}
		if request.providerHeader != "captured-provider" {
			t.Fatalf("request %d X-Test-Provider = %q, want captured provider", i+1, request.providerHeader)
		}
		if request.model != "captured-deployment" {
			t.Fatalf("request %d model = %q, want captured deployment", i+1, request.model)
		}
	}

	initialInput := gotRequests[0].input
	if len(initialInput) != 3 {
		t.Fatalf("initial input = %#v, want encrypted reasoning, function call, and tool output", initialInput)
	}
	if initialInput[0]["type"] != "reasoning" || initialInput[0]["encrypted_content"] != responsesChatRetryEncryptedToken {
		t.Fatalf("initial encrypted reasoning item = %#v", initialInput[0])
	}
	if got, want := gotRequests[1].input, initialInput[1:]; !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized retry input = %#v, want only encrypted reasoning stripped from %#v", got, initialInput)
	}
	for _, item := range gotRequests[1].input {
		if _, ok := item["encrypted_content"]; ok {
			t.Fatalf("sanitized retry retained encrypted_content: %#v", item)
		}
	}
}

func TestExecuteResponsesBackedChatDoesNotRetryEncryptedReplayOnUnrelatedError(t *testing.T) {
	var requestsMu sync.Mutex
	requests := make([]observedResponsesChatRetryRequest, 0, 1)
	var fallbackRequests atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == providerEndpointModels {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		fallbackRequests.Add(1)
		http.Error(w, "unexpected fallback request", http.StatusInternalServerError)
	}))
	t.Cleanup(fallback.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed, decodeErr := decodeObservedResponsesChatRetryRequest(r)
		if decodeErr != nil {
			t.Errorf("decode upstream request: %v", decodeErr)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requestsMu.Lock()
		requests = append(requests, observed)
		requestsMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"the requested model is unavailable","code":"invalid_request_body"}}`)
	}))
	t.Cleanup(upstream.Close)

	h := newResponsesChatEncryptedRetryTestHandler(t, upstream.URL, fallback.URL)
	result, err := h.executeChatCompletions(context.Background(), responsesChatEncryptedReplayRequestBody(t, h), chatExecutionOptions{})
	if err != nil {
		t.Fatalf("executeChatCompletions() error = %v", err)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusBadRequest {
		t.Fatalf("result response = %#v, want original 400", result.Response)
	}
	defer func() { _ = result.Response.Body.Close() }()

	requestsMu.Lock()
	gotRequests := append([]observedResponsesChatRetryRequest(nil), requests...)
	requestsMu.Unlock()
	if len(gotRequests) != 1 {
		t.Fatalf("upstream requests = %d, want no sanitized retry for unrelated error", len(gotRequests))
	}
	if got := fallbackRequests.Load(); got != 0 {
		t.Fatalf("fallback provider requests = %d, want no retry", got)
	}
	if len(gotRequests[0].input) == 0 || gotRequests[0].input[0]["encrypted_content"] != responsesChatRetryEncryptedToken {
		t.Fatalf("initial replay input did not retain encrypted content: %#v", gotRequests[0].input)
	}
}

func newResponsesChatEncryptedRetryTestHandler(t *testing.T, baseURL, fallbackURL string) *ProxyHandler {
	t.Helper()
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fallback-copilot-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:      "fallback-provider",
				Type:    string(providerTypeCopilot),
				Default: true,
			},
			{
				ID:             "captured-provider",
				Type:           string(providerTypeOpenAICompatible),
				BaseURL:        baseURL,
				AuthType:       string(providerAuthTypeBearer),
				APIKey:         "captured-secret",
				ExtraHeaders:   map[string]string{"X-Test-Provider": "captured-provider"},
				ResponsesPath:  "/captured/responses",
				ModelDiscovery: string(providerModelDiscoveryStatic),
				Models: []ProviderModelConfig{
					{
						PublicID:   "gpt-public",
						Deployment: "captured-deployment",
						Endpoints:  []string{providerEndpointResponses},
					},
				},
			},
		}}),
		WithCopilotBaseURL(fallbackURL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	h.maxRetries = 1
	t.Cleanup(h.BeginShutdown)
	return h
}

func responsesChatEncryptedReplayRequestBody(t *testing.T, h *ProxyHandler) []byte {
	t.Helper()
	route := responsesChatReplayRoute{
		ProviderID:    "captured-provider",
		PublicModel:   "gpt-public",
		UpstreamModel: "captured-deployment",
	}
	published, err := h.responsesChatReplayStore().Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`null`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"reasoning-retry","encrypted_content":"` + responsesChatRetryEncryptedToken + `"}`),
			json.RawMessage(`{"type":"function_call","id":"item-retry","call_id":"upstream-call-retry","name":"lookup","arguments":"{}","status":"completed"}`),
		},
		Calls: []responsesChatReplayPublishCall{
			{
				UpstreamCallID:   "upstream-call-retry",
				Name:             "lookup",
				VisibleArguments: `{}`,
				OutputItemIndex:  1,
			},
		},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	call := published.Projection.Calls[0]
	body, err := json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":   call.ID,
						"type": "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						},
					},
				},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": call.ID,
				"content":      "tool result",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal Chat request: %v", err)
	}
	return body
}

func decodeObservedResponsesChatRetryRequest(r *http.Request) (observedResponsesChatRetryRequest, error) {
	var body struct {
		Model string           `json:"model"`
		Input []map[string]any `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return observedResponsesChatRetryRequest{}, err
	}
	return observedResponsesChatRetryRequest{
		path:           r.URL.Path,
		authorization:  r.Header.Get("Authorization"),
		providerHeader: r.Header.Get("X-Test-Provider"),
		model:          body.Model,
		input:          body.Input,
	}, nil
}
