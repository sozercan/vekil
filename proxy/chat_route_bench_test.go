package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const (
	benchmarkChatRoutePublicModel   = "gpt-5.4-chat"
	benchmarkChatRouteUpstreamModel = "gpt-5.4-chat-prod"
	benchmarkChatRouteProviderID    = "azure-chat-benchmark"
	benchmarkChatRouteTargetID      = "azure-chat-primary"
	benchmarkChatRouteID            = "gpt-5.4-chat-route"
	benchmarkChatRouteBaseURL       = "https://benchmark-resource.openai.azure.com/openai/v1"
	benchmarkChatRouteRequestURL    = benchmarkChatRouteBaseURL + providerEndpointChatCompletions
	benchmarkChatRouteAPIKey        = "benchmark-api-key"
)

var benchmarkChatRouteRawRequestBody = []byte(`{
  "model": "gpt-5.4-chat",
  "messages": [
    {
      "role": "system",
      "content": "You are a coding assistant. Inspect the supplied repository context before proposing a change."
    },
    {
      "role": "user",
      "content": [
        {
          "type": "text",
          "text": "Find the request-routing code, explain the safest change, and name the focused tests to run."
        }
      ]
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "read_file",
        "description": "Read a bounded range from a repository file.",
        "parameters": {
          "type": "object",
          "properties": {
            "path": {"type": "string"},
            "start_line": {"type": "integer", "minimum": 1},
            "end_line": {"type": "integer", "minimum": 1}
          },
          "required": ["path"]
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "search_code",
        "description": "Search repository files for a literal or regular expression.",
        "parameters": {
          "type": "object",
          "properties": {
            "query": {"type": "string"},
            "glob": {"type": "string"}
          },
          "required": ["query"]
        }
      }
    }
  ],
  "tool_choice": "auto",
  "temperature": 0.2,
  "top_p": 0.95,
  "max_tokens": 2048,
  "stream": false,
  "metadata": {
    "workload": "phase-0-chat-route-benchmark",
    "repository": "vekil"
  }
}`)

type benchmarkChatRouteBuild struct {
	request *http.Request
	body    []byte
}

var benchmarkChatRouteBuildSink benchmarkChatRouteBuild

func BenchmarkChatRouteLegacyDirectResolutionRequestBuild(b *testing.B) {
	handler := newChatRouteBenchmarkHandler(b, false)
	body := preparedChatRouteBenchmarkBody(b)
	ctx := context.Background()

	built, err := buildLegacyDirectChatBenchmarkRequest(ctx, handler, body)
	if err != nil {
		b.Fatalf("build legacy direct chat request: %v", err)
	}
	validateChatRouteBenchmarkBuild(b, built, false)

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		built, err = buildLegacyDirectChatBenchmarkRequest(ctx, handler, body)
		if err != nil {
			b.Fatalf("build legacy direct chat request: %v", err)
		}
		benchmarkChatRouteBuildSink = built
	}
}

func BenchmarkChatRouteExplicitPriorityOneTargetRequestBuild(b *testing.B) {
	handler := newChatRouteBenchmarkHandler(b, true)
	body := preparedChatRouteBenchmarkBody(b)
	ctx := context.Background()

	built, err := buildExplicitPriorityChatBenchmarkRequest(ctx, handler, body)
	if err != nil {
		b.Fatalf("build explicit priority chat request: %v", err)
	}
	validateChatRouteBenchmarkBuild(b, built, true)

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		built, err = buildExplicitPriorityChatBenchmarkRequest(ctx, handler, body)
		if err != nil {
			b.Fatalf("build explicit priority chat request: %v", err)
		}
		benchmarkChatRouteBuildSink = built
	}
}

func buildLegacyDirectChatBenchmarkRequest(ctx context.Context, handler *ProxyHandler, body []byte) (benchmarkChatRouteBuild, error) {
	provider, owner, preparedBody, err := handler.resolveProviderRequest(body, providerEndpointChatCompletions)
	if err != nil {
		return benchmarkChatRouteBuild{}, err
	}
	req, err := handler.newProviderJSONRequest(ctx, provider, http.MethodPost, providerEndpointChatCompletions, preparedBody, nil, "", owner)
	if err != nil {
		return benchmarkChatRouteBuild{}, err
	}
	return benchmarkChatRouteBuild{request: req, body: preparedBody}, nil
}

// buildExplicitPriorityChatBenchmarkRequest mirrors the deterministic,
// pre-dispatch portion of executeExplicitRouteRequest. It stops before send
// budgeting, telemetry, httptrace, and http.Client.Do so benchmark results do
// not include network or scheduler variability.
func buildExplicitPriorityChatBenchmarkRequest(ctx context.Context, handler *ProxyHandler, body []byte) (benchmarkChatRouteBuild, error) {
	requestedModel := extractRequestModel(body)
	route, known := handler.resolveModelRouteForRequest(requestedModel, providerEndpointChatCompletions)
	if !known || route == nil || route.legacy {
		return benchmarkChatRouteBuild{}, fmt.Errorf("explicit route for model %q is unavailable", requestedModel)
	}
	if !route.supportsEndpoint(providerEndpointChatCompletions) {
		return benchmarkChatRouteBuild{}, fmt.Errorf("explicit route %q does not support chat completions", route.public.routeID)
	}

	targets := orderedRouteTargets(route, nil, providerEndpointChatCompletions)
	if len(targets) != 1 {
		return benchmarkChatRouteBuild{}, fmt.Errorf("explicit route %q selected %d targets, want 1", route.public.routeID, len(targets))
	}
	target := targets[0]
	owner := providerModelFromRouteTarget(route, target)
	preparedBody, err := prepareRouteTargetBody(body, requestedModel, providerEndpointChatCompletions, route, target, owner)
	if err != nil {
		return benchmarkChatRouteBuild{}, err
	}

	req, err := handler.newProviderJSONRequest(ctx, target.provider, http.MethodPost, providerEndpointChatCompletions, preparedBody, cloneSanitizedRouteHeaders(nil), "", owner)
	if err != nil {
		return benchmarkChatRouteBuild{}, err
	}
	req = req.WithContext(withExplicitRouteResponseInfo(req.Context(), explicitRouteResponseInfo{
		routeID:    route.public.routeID,
		publicID:   route.public.id,
		targetID:   target.id,
		providerID: target.provider.id,
	}))
	// Explicit route sends disable automatic request replay before dispatch.
	req.GetBody = nil
	return benchmarkChatRouteBuild{request: req, body: preparedBody}, nil
}

func newChatRouteBenchmarkHandler(b *testing.B, explicit bool, modes ...routeMode) *ProxyHandler {
	b.Helper()

	parallelToolCalls := false
	dropSamplingParams := true
	useMaxCompletionTokens := true
	provider := ProviderConfig{
		ID:      benchmarkChatRouteProviderID,
		Type:    string(providerTypeAzureOpenAI),
		Default: true,
		BaseURL: benchmarkChatRouteBaseURL,
		APIKey:  benchmarkChatRouteAPIKey,
	}
	cfg := ProvidersConfig{Providers: []ProviderConfig{provider}}
	if explicit {
		mode := routeModePriorityFailover
		if len(modes) > 0 && modes[0] != "" {
			mode = modes[0]
		}
		cfg.SchemaVersion = ProvidersConfigSchemaVersion2
		cfg.ModelRoutes = []ModelRouteConfig{{
			ID:                 benchmarkChatRouteID,
			PublicID:           benchmarkChatRoutePublicModel,
			Name:               "GPT-5.4 Chat Benchmark",
			Endpoints:          []string{providerEndpointChatCompletions},
			ParallelToolCalls:  &parallelToolCalls,
			DropSamplingParams: &dropSamplingParams,
			Targets: []ModelRouteTargetConfig{{
				ID:                     benchmarkChatRouteTargetID,
				Provider:               benchmarkChatRouteProviderID,
				UpstreamModel:          benchmarkChatRouteUpstreamModel,
				UseMaxCompletionTokens: &useMaxCompletionTokens,
			}},
			Routing: ModelRouteRoutingConfig{
				Mode:              string(mode),
				MaxTargetAttempts: 1,
				MaxUpstreamSends:  1,
			},
		}}
	} else {
		cfg.Providers[0].Models = []ProviderModelConfig{{
			PublicID:               benchmarkChatRoutePublicModel,
			Deployment:             benchmarkChatRouteUpstreamModel,
			Name:                   "GPT-5.4 Chat Benchmark",
			Endpoints:              []string{providerEndpointChatCompletions},
			ParallelToolCalls:      &parallelToolCalls,
			DropSamplingParams:     &dropSamplingParams,
			UseMaxCompletionTokens: &useMaxCompletionTokens,
		}}
	}

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("benchmark-copilot-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
	)
	if err != nil {
		b.Fatalf("create chat route benchmark handler: %v", err)
	}
	b.Cleanup(handler.BeginShutdown)
	return handler
}

func preparedChatRouteBenchmarkBody(b *testing.B) []byte {
	b.Helper()
	body, mode := prepareOpenAIChatCompletionsRequest(benchmarkChatRouteRawRequestBody)
	if !mode.forceUpstreamStream || mode.clientRequestedStream {
		b.Fatalf("benchmark fixture mode = %+v, want forced upstream stream", mode)
	}
	return body
}

func validateChatRouteBenchmarkBuild(b *testing.B, built benchmarkChatRouteBuild, explicit bool) {
	b.Helper()
	if built.request == nil {
		b.Fatal("benchmark request is nil")
	}
	if built.request.Method != http.MethodPost {
		b.Fatalf("benchmark request method = %q, want POST", built.request.Method)
	}
	if got := built.request.URL.String(); got != benchmarkChatRouteRequestURL {
		b.Fatalf("benchmark request URL = %q, want %q", got, benchmarkChatRouteRequestURL)
	}
	if got := built.request.Header.Get("api-key"); got != benchmarkChatRouteAPIKey {
		b.Fatalf("benchmark api-key = %q, want configured key", got)
	}
	if got := built.request.Header.Get("Content-Type"); got != "application/json" {
		b.Fatalf("benchmark Content-Type = %q, want application/json", got)
	}
	if got := built.request.ContentLength; got != int64(len(built.body)) {
		b.Fatalf("benchmark ContentLength = %d, want %d", got, len(built.body))
	}

	requestBody, err := io.ReadAll(built.request.Body)
	if err != nil {
		b.Fatalf("read benchmark request body: %v", err)
	}
	if err := built.request.Body.Close(); err != nil {
		b.Fatalf("close benchmark request body: %v", err)
	}
	if !bytes.Equal(requestBody, built.body) {
		b.Fatal("benchmark request body differs from prepared body")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		b.Fatalf("decode benchmark request body: %v", err)
	}
	if got := rawJSONString(payload["model"]); got != benchmarkChatRouteUpstreamModel {
		b.Fatalf("benchmark upstream model = %q, want %q", got, benchmarkChatRouteUpstreamModel)
	}
	for _, field := range []string{"temperature", "top_p", "parallel_tool_calls", "max_tokens"} {
		if _, ok := payload[field]; ok {
			b.Fatalf("benchmark provider policy retained %q", field)
		}
	}
	var maxCompletionTokens int
	if err := json.Unmarshal(payload["max_completion_tokens"], &maxCompletionTokens); err != nil || maxCompletionTokens != 2048 {
		b.Fatalf("benchmark max_completion_tokens = %d, err=%v, want 2048", maxCompletionTokens, err)
	}
	var stream bool
	if err := json.Unmarshal(payload["stream"], &stream); err != nil || !stream {
		b.Fatalf("benchmark stream = %t, err=%v, want true", stream, err)
	}

	providerInfo, ok := built.request.Context().Value(providerRouteContextKey{}).(providerRouteInfo)
	if !ok || providerInfo.id != benchmarkChatRouteProviderID {
		b.Fatalf("benchmark provider route = %+v, ok=%t", providerInfo, ok)
	}
	responseInfo, hasResponseInfo := built.request.Context().Value(explicitRouteResponseContextKey{}).(explicitRouteResponseInfo)
	if explicit {
		if built.request.GetBody != nil {
			b.Fatal("explicit benchmark request exposes automatic replay through GetBody")
		}
		if !hasResponseInfo || responseInfo.routeID != benchmarkChatRouteID || responseInfo.targetID != benchmarkChatRouteTargetID {
			b.Fatalf("explicit benchmark response metadata = %+v, ok=%t", responseInfo, hasResponseInfo)
		}
		return
	}
	if built.request.GetBody == nil {
		b.Fatal("legacy benchmark request unexpectedly has nil GetBody")
	}
	if hasResponseInfo {
		b.Fatalf("legacy benchmark request has explicit response metadata: %+v", responseInfo)
	}
}

type benchmarkChatRouteTransport struct{}

func (benchmarkChatRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := []byte(`{"id":"chatcmpl-bench","object":"chat.completion","created":1,"model":"physical","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func BenchmarkChatRouteLegacyDirectTransport(b *testing.B) {
	handler := newChatRouteBenchmarkHandler(b, false)
	handler.client = &http.Client{Transport: benchmarkChatRouteTransport{}}
	body := preparedChatRouteBenchmarkBody(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		resp, err := handler.postChatCompletions(ctx, body)
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}

func BenchmarkChatRouteExplicitPrimaryOnlyTransport(b *testing.B) {
	handler := newChatRouteBenchmarkHandler(b, true, routeModePrimaryOnly)
	handler.client = &http.Client{Transport: benchmarkChatRouteTransport{}}
	body := preparedChatRouteBenchmarkBody(b)
	route, known := handler.resolveModelRouteForRequest(benchmarkChatRoutePublicModel, providerEndpointChatCompletions)
	if !known || route == nil || route.legacy {
		b.Fatal("explicit benchmark route unavailable")
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		op := newRouteOperation(route, context.Background())
		ctx := withRouteOperation(context.Background(), op)
		resp, err := handler.postChatCompletions(ctx, body)
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}
