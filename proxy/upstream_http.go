package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func (h *ProxyHandler) newLifecycleUpstreamContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return h.newLifecycleUpstreamContextFrom(h.lifecycleContext(), timeout)
}

func (h *ProxyHandler) newLifecycleUpstreamContextFrom(lifecycle context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	if !h.ShuttingDown() {
		ctx, cancel := context.WithTimeout(lifecycle, timeout)
		// BeginShutdown publishes draining before canceling the lifecycle root.
		// Keep the ordinary path to one child context, but recheck after creating
		// it so a context installed in that ordering window still returns with the
		// proxy-shutdown cause below.
		if !h.ShuttingDown() || lifecycle.Err() != nil {
			return ctx, cancel
		}
		cancel()
	}

	causeCtx, cancelCause := context.WithCancelCause(lifecycle)
	ctx, cancelTimeout := context.WithTimeout(causeCtx, timeout)
	// BeginShutdown publishes draining before canceling the lifecycle root so
	// admission closes first. Close that tiny ordering window for children
	// derived from the still-live root; the parent cancellation covers the
	// opposite ordering where it wins before this recheck.
	if h.ShuttingDown() {
		cancelCause(errProxyLifecycleShutdown)
	}
	return ctx, func() {
		// Preserve the historical caller-cancel semantics: an explicit returned
		// cancel is ordinary context cancellation, while timeout and lifecycle
		// shutdown retain their own causes.
		cancelTimeout()
		cancelCause(context.Canceled)
	}
}

func (h *ProxyHandler) newInferenceUpstreamContext(streaming bool) (context.Context, context.CancelFunc) {
	return h.newInferenceUpstreamContextFrom(context.Background(), streaming)
}

// newInferenceUpstreamContextFrom builds a lifecycle-rooted upstream request
// context with its own timeout. It deliberately does not inherit cancellation
// or arbitrary values from the inbound request, so an ordinary client disconnect
// does not cancel upstream inference. BeginShutdown cancels the lifecycle root,
// which promptly stops both existing work and contexts created after draining
// begins.
//
// The retry-stats tracked marker is copied explicitly when present. A context
// without the marker (for example context.Background()) yields an untracked
// upstream context for internal insight, catalog, count-token, and shim work.
func (h *ProxyHandler) newInferenceUpstreamContextFrom(inbound context.Context, streaming bool) (context.Context, context.CancelFunc) {
	timeout := upstreamTimeout
	if streaming {
		timeout = h.effectiveStreamingUpstreamTimeout()
	}
	lifecycle := h.lifecycleContextForRetryStats(isRetryStatsTracked(inbound))
	return h.newLifecycleUpstreamContextFrom(lifecycle, timeout)
}

func upstreamStatusCode(err error, fallback int) int {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.statusCode
	}
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return providerErr.statusCode
	}
	return fallback
}

func extractRequestModel(body []byte) string {
	if model, _, ok := extractTopLevelJSONStringFast(body, "model", false); ok {
		return model
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}

	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return ""
	}

	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return ""
		}

		key, ok := keyToken.(string)
		if !ok {
			return ""
		}

		if key == "model" {
			var model string
			if err := dec.Decode(&model); err != nil {
				return ""
			}
			return strings.TrimSpace(model)
		}

		if err := skipJSONValue(dec); err != nil {
			return ""
		}
	}

	return ""
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}

	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return nil
	}
}

func mergeHeaderValues(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (h *ProxyHandler) resolveProviderRequest(body []byte, endpoint string) (*providerRuntime, providerModel, []byte, error) {
	return h.resolveProviderRequestForModel(body, endpoint, extractRequestModel(body))
}

func (h *ProxyHandler) resolveProviderRequestForModel(body []byte, endpoint string, model string) (*providerRuntime, providerModel, []byte, error) {
	return h.resolveProviderRequestForModelWithValidation(body, endpoint, model, false)
}

func (h *ProxyHandler) resolveProviderRequestForModelValidated(body []byte, endpoint string, model string) (*providerRuntime, providerModel, []byte, error) {
	return h.resolveProviderRequestForModelWithValidation(body, endpoint, model, true)
}

func (h *ProxyHandler) resolveProviderRequestForModelWithValidation(body []byte, endpoint string, model string, bodyValidated bool) (*providerRuntime, providerModel, []byte, error) {
	if !h.modelAllowedForRequest(model, endpoint) {
		return nil, providerModel{}, nil, modelNotAllowedRequestError(model)
	}
	provider, owner, known := h.resolveProviderModelForRequest(model, endpoint)
	if provider == nil {
		return nil, providerModel{}, nil, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("no provider available for endpoint %s", endpoint)}
	}
	if !provider.supportsEndpoint(endpoint) {
		return nil, providerModel{}, nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("provider %q does not support %s", provider.id, endpoint),
		}
	}
	if known && !providerModelSupportsEndpoint(owner, endpoint) {
		return nil, providerModel{}, nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("model %q does not support %s", model, endpoint),
		}
	}
	if !known && !provider.allowsUnknownModelEndpoint(endpoint) {
		return nil, providerModel{}, nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("model %q does not support %s", model, endpoint),
		}
	}

	rewrittenBody, err := prepareResolvedProviderRequestBodyWithValidation(body, model, endpoint, provider, owner, bodyValidated)
	if err != nil {
		return nil, providerModel{}, nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	return provider, owner, rewrittenBody, nil
}

func prepareResolvedProviderRequestBody(
	body []byte,
	requestModel string,
	endpoint string,
	provider *providerRuntime,
	owner providerModel,
) ([]byte, error) {
	return prepareResolvedProviderRequestBodyWithValidation(body, requestModel, endpoint, provider, owner, false)
}

func prepareResolvedProviderRequestBodyWithValidation(
	body []byte,
	requestModel string,
	endpoint string,
	provider *providerRuntime,
	owner providerModel,
	bodyValidated bool,
) ([]byte, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	rewrittenBody := body
	if !providerUsesAzureClassicDeploymentPath(provider, endpoint) {
		var err error
		if bodyValidated {
			rewrittenBody, _, err = rewriteRequestModelForProviderFromModelJSONValidated(body, requestModel, owner.upstreamModel, owner.upstreamModelJSON)
		} else {
			rewrittenBody, _, err = rewriteRequestModelForProviderFromModelJSON(body, requestModel, owner.upstreamModel, owner.upstreamModelJSON)
		}
		if err != nil {
			return nil, err
		}
	}
	return applyProviderModelRequestPolicy(rewrittenBody, endpoint, owner), nil
}

func applyProviderModelRequestPolicy(body []byte, endpoint string, owner providerModel) []byte {
	rewriteMaxTokens := endpoint == providerEndpointChatCompletions && owner.useMaxCompletionTokens
	dropStopSequences := endpoint == providerEndpointChatCompletions && owner.dropStopSequences
	if !owner.dropSamplingParams && !dropStopSequences && !rewriteMaxTokens && (owner.parallelToolCalls == nil || *owner.parallelToolCalls) {
		return body
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	changed := false
	if owner.dropSamplingParams {
		for _, field := range []string{"temperature", "top_p"} {
			if _, ok := payload[field]; ok {
				delete(payload, field)
				changed = true
			}
		}
	}
	if dropStopSequences {
		if _, ok := payload["stop"]; ok {
			delete(payload, "stop")
			changed = true
		}
	}
	if rewriteMaxTokens {
		if maxTokens, ok := payload["max_tokens"]; ok {
			maxCompletionTokens, exists := payload["max_completion_tokens"]
			if !exists || bytes.Equal(bytes.TrimSpace(maxCompletionTokens), []byte("null")) {
				payload["max_completion_tokens"] = maxTokens
			}
			delete(payload, "max_tokens")
			changed = true
		}
	}
	if owner.parallelToolCalls != nil && !*owner.parallelToolCalls {
		if _, ok := payload["parallel_tool_calls"]; ok {
			delete(payload, "parallel_tool_calls")
			changed = true
		}
	}
	if !changed {
		return body
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return rewritten
}

func (h *ProxyHandler) postResolvedProviderRequest(
	ctx context.Context,
	provider *providerRuntime,
	owner providerModel,
	endpoint string,
	body []byte,
	extraHeaders http.Header,
) (*http.Response, error) {
	return h.postResolvedProviderRequestForModel(ctx, provider, owner, endpoint, body, extraHeaders, extractRequestModel(body))
}

func (h *ProxyHandler) postResolvedProviderRequestForModel(
	ctx context.Context,
	provider *providerRuntime,
	owner providerModel,
	endpoint string,
	body []byte,
	extraHeaders http.Header,
	requestModel string,
) (*http.Response, error) {
	if provider == nil {
		return nil, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider is required")}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	preparedBody, err := prepareResolvedProviderRequestBody(body, requestModel, endpoint, provider, owner)
	if err != nil {
		return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}

	return h.doInferenceWithRetry(func() (*http.Request, error) {
		return h.newProviderJSONInferenceRequest(ctx, provider, http.MethodPost, endpoint, preparedBody, extraHeaders, "", owner)
	})
}

// maybeRetryResolvedResponsesWithoutUnverifiableEncryptedContent mirrors the
// native Responses retry contract while retaining an already-resolved provider,
// model owner, and endpoint. Re-resolving from requestBody would route on the
// already-rewritten upstream model instead of the public model that selected the
// original route.
func (h *ProxyHandler) maybeRetryResolvedResponsesWithoutUnverifiableEncryptedContent(
	ctx context.Context,
	provider *providerRuntime,
	owner providerModel,
	endpoint string,
	requestBody []byte,
	extraHeaders http.Header,
	resp *http.Response,
) (*http.Response, error) {
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		return resp, nil
	}

	respBodyPrefix, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(compactUpstreamErrorBodySize)+1))
	if readErr != nil {
		_ = resp.Body.Close()
		if len(respBodyPrefix) > compactUpstreamErrorBodySize {
			respBodyPrefix = respBodyPrefix[:compactUpstreamErrorBodySize]
		}
		cloned := cloneHTTPResponseWithBody(resp, respBodyPrefix)
		cloned.Header.Del("Content-Length")
		return cloned, nil
	}
	classificationBody := respBodyPrefix
	if len(classificationBody) > compactUpstreamErrorBodySize {
		classificationBody = classificationBody[:compactUpstreamErrorBodySize]
	}
	restoreOriginalResp := func() *http.Response {
		cloned := new(http.Response)
		*cloned = *resp
		if resp.Header != nil {
			cloned.Header = resp.Header.Clone()
		}
		cloned.Body = prefixedReadCloser{
			Reader: io.MultiReader(bytes.NewReader(respBodyPrefix), resp.Body),
			close:  resp.Body.Close,
		}
		return cloned
	}

	if !isUnverifiableEncryptedContentError(resp.StatusCode, classificationBody) {
		return restoreOriginalResp(), nil
	}

	retryBody, strippedItems := sanitizeResponsesUnverifiableEncryptedContentBody(requestBody)
	if strippedItems == 0 {
		return restoreOriginalResp(), nil
	}

	providerID := ""
	if provider != nil {
		providerID = provider.id
	}
	h.log.Info("retrying resolved Responses request without unverifiable encrypted content",
		logger.F("encrypted_items_stripped", strippedItems),
		logger.F("provider", providerID),
	)
	retryResp, retryErr := h.postResolvedProviderRequest(ctx, provider, owner, endpoint, retryBody, extraHeaders)
	if retryErr != nil {
		h.log.Debug("resolved Responses encrypted-content retry request failed", logger.Err(retryErr))
		return restoreOriginalResp(), nil
	}
	_ = resp.Body.Close()
	return retryResp, nil
}

func (h *ProxyHandler) postJSONEndpoint(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, path, body, nil)
}

func (h *ProxyHandler) postJSONEndpointWithHeaders(ctx context.Context, path string, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithHeadersForModel(ctx, path, body, extraHeaders, extractRequestModel(body))
}

func (h *ProxyHandler) postJSONEndpointWithHeadersForModel(ctx context.Context, path string, body []byte, extraHeaders http.Header, model string) (*http.Response, error) {
	return h.postJSONEndpointWithHeadersForModelValidation(ctx, path, body, extraHeaders, model, false)
}

func (h *ProxyHandler) postJSONEndpointWithHeadersForModelValidated(ctx context.Context, path string, body []byte, extraHeaders http.Header, model string) (*http.Response, error) {
	return h.postJSONEndpointWithHeadersForModelValidation(ctx, path, body, extraHeaders, model, true)
}

func (h *ProxyHandler) postJSONEndpointWithHeadersForModelValidation(ctx context.Context, path string, body []byte, extraHeaders http.Header, model string, bodyValidated bool) (*http.Response, error) {
	operation := routeOperationFromContext(ctx)
	if operation == nil {
		requestedModel := strings.TrimSpace(model)
		if requestedModel != "" && !h.modelAllowedForRequest(requestedModel, path) {
			return nil, modelNotAllowedRequestError(requestedModel)
		}
	}
	if operation != nil && operation.route != nil && !operation.route.legacy {
		route := operation.route
		requestedModel := strings.TrimSpace(model)
		if requestedModel == "" {
			requestedModel = route.public.id
		}
		if requestedModel != route.public.id {
			resolvedAlias := false
			if plan, planned := operation.policyPlan(); planned {
				if entry, known := h.providerSetup().lookupPublicModelEntry(requestedModel); known && entry != nil && entry.kind == publicEntryPolicy && entry.id == plan.publicID {
					// Keep the request alias as the rewrite source while authorizing it
					// against the sealed policy public identity.
					resolvedAlias = true
				}
			} else if resolved, known := h.resolveModelRouteForRequest(requestedModel, path); known && resolved == route {
				// Keep the actual alias as the rewrite source. Using the canonical
				// public ID here can leave the alias in the immutable request body
				// when the target upstream model is already canonical.
				resolvedAlias = true
			}
			attemptKind := routeAttemptKindFromContext(ctx)
			if !resolvedAlias && attemptKind != routeAttemptCompatibilityFallback && attemptKind != routeAttemptCompaction {
				return nil, &providerRequestError{
					statusCode: http.StatusBadRequest,
					err:        fmt.Errorf("explicit model route %q cannot change model to %q", route.public.id, requestedModel),
				}
			}
			if !resolvedAlias {
				upstreamModel := requestedModel
				if pinned := operation.pinnedTarget(); pinned != "" {
					if target, ok := route.targetByID(pinned); ok && target.provider != nil {
						if configured, exists := target.provider.staticModels[upstreamModel]; exists && configured.upstreamModel != "" {
							upstreamModel = configured.upstreamModel
						}
					}
				}
				// Keep requestedModel as the model present in the immutable fallback
				// body. The override carries only the physical model selected for the
				// pinned target, so prepareRouteTargetBody can still rewrite from the
				// actual fallback model to that upstream model.
				ctx = withRouteUpstreamModelOverride(ctx, upstreamModel)
			}
		}
		if !route.supportsEndpoint(path) {
			return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("model %q does not support %s", route.public.id, path)}
		}
		stream := path == providerEndpointResponses && parseResponsesRequestMetadata(body).Stream
		return h.executeExplicitRouteRequest(ctx, route, path, body, extraHeaders, requestedModel, stream)
	}

	route, known := h.resolveModelRouteForRequest(model, path)
	if known && route != nil && !route.legacy {
		if err := rejectDuplicateJSONMappingKeys(body); err != nil {
			return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("invalid ambiguous JSON request: %w", err)}
		}
		if !route.supportsEndpoint(path) {
			return nil, &providerRequestError{
				statusCode: http.StatusBadRequest,
				err:        fmt.Errorf("model %q does not support %s", model, path),
			}
		}
		stream := path == providerEndpointResponses && parseResponsesRequestMetadata(body).Stream
		return h.executeExplicitRouteRequest(ctx, route, path, body, extraHeaders, model, stream)
	}

	var provider *providerRuntime
	var owner providerModel
	var rewrittenBody []byte
	var err error
	if bodyValidated {
		provider, owner, rewrittenBody, err = h.resolveProviderRequestForModelValidated(body, path, model)
	} else {
		provider, owner, rewrittenBody, err = h.resolveProviderRequestForModel(body, path, model)
	}
	if err != nil {
		return nil, err
	}

	return h.doInferenceWithRetry(func() (*http.Request, error) {
		req, err := h.newProviderJSONInferenceRequest(ctx, provider, http.MethodPost, path, rewrittenBody, extraHeaders, "", owner)
		if err != nil {
			return nil, err
		}
		return req, nil
	})
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return h.postJSONEndpoint(ctx, providerEndpointChatCompletions, body)
}

func (h *ProxyHandler) postChatCompletionsForModel(ctx context.Context, body []byte, model string) (*http.Response, error) {
	return h.postJSONEndpointWithHeadersForModel(ctx, providerEndpointChatCompletions, body, nil, model)
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postResponsesWithHeadersForModel(ctx, body, extraHeaders, extractRequestModel(body))
}

func (h *ProxyHandler) postResponsesWithHeadersForModel(ctx context.Context, body []byte, extraHeaders http.Header, model string) (*http.Response, error) {
	return h.postResponsesWithHeadersForModelValidation(ctx, body, extraHeaders, model, false)
}

func (h *ProxyHandler) postResponsesWithHeadersForModelValidation(ctx context.Context, body []byte, extraHeaders http.Header, model string, bodyValidated bool) (*http.Response, error) {
	resp, err := h.postJSONEndpointWithHeadersForModelValidation(ctx, providerEndpointResponses, body, extraHeaders, model, bodyValidated)
	if err != nil {
		return nil, err
	}
	return h.maybeRetryResponsesWithoutUnverifiableEncryptedContent(ctx, body, extraHeaders, resp)
}

func (h *ProxyHandler) postAnthropicMessagesCountTokens(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	model := extractRequestModel(body)
	operation := routeOperationFromContext(ctx)
	if operation == nil {
		requestedModel := strings.TrimSpace(model)
		if requestedModel != "" && !h.modelAllowedForRequest(requestedModel, providerEndpointMessages) {
			return nil, modelNotAllowedRequestError(requestedModel)
		}
	}
	if operation != nil && operation.route != nil && !operation.route.legacy {
		return h.executeExplicitRouteRequestPath(ctx, operation.route, providerEndpointMessages, providerEndpointMessagesCount, body, extraHeaders, model, false)
	}
	if route, known := h.resolveModelRouteForRequest(model, providerEndpointMessages); known && route != nil && !route.legacy {
		return h.executeExplicitRouteRequestPath(ctx, route, providerEndpointMessages, providerEndpointMessagesCount, body, extraHeaders, model, false)
	}

	provider, owner, rewrittenBody, err := h.resolveProviderRequest(body, providerEndpointMessages)
	if err != nil {
		return nil, err
	}
	if provider.kind != providerTypeAnthropicCompatible {
		return nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("provider %q does not support %s", provider.id, providerEndpointMessagesCount),
		}
	}

	return h.doInferenceWithRetry(func() (*http.Request, error) {
		req, err := h.newProviderJSONInferenceRequest(ctx, provider, http.MethodPost, providerEndpointMessagesCount, rewrittenBody, extraHeaders, "", owner)
		if err != nil {
			return nil, err
		}
		return req, nil
	})
}

type bodyCopyWriter struct {
	w        http.ResponseWriter
	writeErr error
	prepared bool
}

func (w *bodyCopyWriter) Write(p []byte) (int, error) {
	if !w.prepared {
		w.w.Header().Set("Content-Type", "application/json")
		w.w.Header().Set("X-Content-Type-Options", "nosniff")
		w.prepared = true
	}
	n, err := w.w.Write(p)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

type responseBodyWriteError struct {
	err                   error
	committed             bool
	upstream              bool
	statusCode            int
	cancellationAtFailure bool
}

func (e *responseBodyWriteError) Error() string { return e.err.Error() }
func (e *responseBodyWriteError) Unwrap() error { return e.err }

func newResponseBodyWriteError(resp *http.Response, err error, committed, upstream, cancellationAtFailure bool) *responseBodyWriteError {
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	return &responseBodyWriteError{
		err:                   err,
		committed:             committed,
		upstream:              upstream,
		statusCode:            statusCode,
		cancellationAtFailure: cancellationAtFailure,
	}
}

func responseRequestContext(resp *http.Response) context.Context {
	if resp != nil && resp.Request != nil {
		return resp.Request.Context()
	}
	return context.Background()
}

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return &responseBodyWriteError{err: fmt.Errorf("upstream response body is unavailable"), upstream: true}
	}
	body := newLifecycleAwareReadCloser(resp.Body, responseRequestContext(resp))
	defer func() { _ = body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	tracked := &bodyCopyWriter{w: w}
	_, err := io.Copy(tracked, body)
	if body.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, true, true, body.canceledAtFailure())
	}
	if err == nil {
		return nil
	}
	return newResponseBodyWriteError(resp, err, true, tracked.writeErr == nil, body.canceledAtFailure())
}

// writeOpenAIChatCompletionResponse writes a non-streaming OpenAI chat response,
// normalizing missing required Chat Completions fields for strict SDK clients
// while preserving vendor-specific fields. It only rewrites successful JSON
// responses that fit in usageSniffMaxBuffer; errors, invalid JSON, and oversized
// responses fail open to passthrough behavior.
func (h *ProxyHandler) writeOpenAIChatCompletionResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, requestedModel string) error {
	return writePassthroughSniffingUsage(w, resp, func(body []byte) ([]byte, bool) {
		if usage, canonical := inspectCanonicalOpenAIChatCompletionResponse(body, requestedModel); canonical {
			observeOpenAIUsage(ctx, &usage)
			return body, false
		}
		out, changed, err := normalizeOpenAIChatCompletionResponse(body, requestedModel, time.Now())
		if err != nil {
			observeOpenAIUsage(ctx, sniffOpenAIUsage(body))
			return body, false
		}
		observeOpenAIUsage(ctx, sniffOpenAIUsage(out))
		return out, changed
	})
}

func (h *ProxyHandler) writeOpenAIPassthroughObservingUsage(ctx context.Context, w http.ResponseWriter, resp *http.Response) error {
	return writePassthroughSniffingUsage(w, resp, func(body []byte) ([]byte, bool) {
		observeOpenAIUsage(ctx, sniffOpenAIUsage(body))
		return body, false
	})
}

// usageSniffMaxBuffer bounds how much of a non-streaming upstream response the
// proxy buffers in memory solely to parse its usage block for traffic stats.
// Real LLM JSON responses — even very large completions — sit well under this,
// so usage is captured for all realistic traffic; the cap only prevents a
// single pathological multi-megabyte body (or many concurrent ones) from being
// buffered whole. Oversized bodies stream through with usage stats skipped.
const usageSniffMaxBuffer = 4 << 20 // 4 MiB

const (
	usageSniffTinyBufferSize  = 512
	usageSniffSmallBufferSize = 4 << 10
)

var usageSniffTinyBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, usageSniffTinyBufferSize)
	return &buffer
}}

var usageSniffSmallBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, usageSniffSmallBufferSize)
	return &buffer
}}

// writePassthroughSniffingUsage writes a non-streaming upstream response to the
// client while buffering at most usageSniffMaxBuffer bytes so the supplied
// transform can sniff usage and optionally rewrite the complete body. A 2xx body
// that fits the cap is buffered and passed to transform; if transform reports a
// rewrite, Content-Length is adjusted. Oversized bodies stream through without a
// transform so proxy memory stays bounded. Non-2xx responses and read errors
// fall back to a plain copy.
func writePassthroughSniffingUsage(w http.ResponseWriter, resp *http.Response, transform func([]byte) ([]byte, bool)) error {
	if resp == nil || resp.Body == nil {
		return &responseBodyWriteError{err: fmt.Errorf("upstream response body is unavailable"), upstream: true}
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices &&
		resp.ContentLength >= 0 && resp.ContentLength < usageSniffSmallBufferSize {
		return writeSmallKnownLengthPassthroughSniffingUsage(w, resp, transform)
	}

	body := newLifecycleAwareReadCloser(resp.Body, responseRequestContext(resp))
	defer func() { _ = body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		copyPassthroughHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		tracked := &bodyCopyWriter{w: w}
		_, err := io.Copy(tracked, body)
		if body.canceledAtFailure() {
			return newResponseBodyWriteError(resp, context.Canceled, true, true, body.canceledAtFailure())
		}
		if err != nil {
			return newResponseBodyWriteError(resp, err, true, tracked.writeErr == nil, body.canceledAtFailure())
		}
		return nil
	}

	// Read one byte past the cap so we can tell a full body from an oversized one.
	// Known small Content-Length responses use one exact allocation instead of
	// constructing a limiter and growing io.ReadAll's default buffer.
	prefix, pooledBuffer, err := readUsageSniffPrefix(body, resp.ContentLength)
	if pooledBuffer != nil {
		defer releaseUsageSniffBuffer(pooledBuffer)
	}
	if body.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, body.canceledAtFailure())
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, false, true, body.canceledAtFailure())
	}
	copyPassthroughHeaders(w.Header(), resp.Header)
	if resp.ContentLength >= 0 && int64(len(prefix)) > resp.ContentLength {
		w.Header().Del("Content-Length")
	}

	if len(prefix) <= usageSniffMaxBuffer {
		// Whole body fits: parse/transform it, then write the result.
		out := prefix
		changed := false
		if transform != nil {
			out, changed = transform(prefix)
		}
		if changed {
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(out); err != nil {
			return newResponseBodyWriteError(resp, err, true, false, false)
		}
		return nil
	}

	// Oversized: skip the usage parse and stream prefix + remainder so memory
	// stays bounded. A proven length mismatch above removes the stale advertised
	// length before the larger body is written.
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(prefix); err != nil {
		return newResponseBodyWriteError(resp, err, true, false, false)
	}
	tracked := &bodyCopyWriter{w: w}
	_, err = io.Copy(tracked, body)
	if body.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, true, true, body.canceledAtFailure())
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, true, tracked.writeErr == nil, body.canceledAtFailure())
	}
	return nil
}

func writeSmallKnownLengthPassthroughSniffingUsage(w http.ResponseWriter, resp *http.Response, transform func([]byte) ([]byte, bool)) error {
	ctx := responseRequestContext(resp)
	defer func() { _ = resp.Body.Close() }()

	pooledBuffer := borrowUsageSniffBuffer(resp.ContentLength)
	defer releaseUsageSniffBuffer(pooledBuffer)
	prefix := (*pooledBuffer)[:int(resp.ContentLength)+1]
	n, canceledAtFailure, err := readFullObservingLifecycle(resp.Body, ctx, prefix)
	longerThanAdvertised := false
	if canceledAtFailure {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, true)
	}
	switch err {
	case nil:
		longerThanAdvertised = true
		// The body exceeded its advertised length. Continue to the normal cap so
		// a malformed Content-Length cannot truncate the downstream response.
		body := newLifecycleAwareReadCloser(resp.Body, ctx)
		rest, readErr := io.ReadAll(io.LimitReader(body, int64(usageSniffMaxBuffer+1-len(prefix))))
		prefix = append(prefix, rest...)
		if body.canceledAtFailure() {
			return newResponseBodyWriteError(resp, context.Canceled, false, true, true)
		}
		if readErr != nil {
			return newResponseBodyWriteError(resp, readErr, false, true, false)
		}
		if len(prefix) > usageSniffMaxBuffer {
			copyPassthroughHeaders(w.Header(), resp.Header)
			w.Header().Del("Content-Length")
			w.WriteHeader(resp.StatusCode)
			if _, writeErr := w.Write(prefix); writeErr != nil {
				return newResponseBodyWriteError(resp, writeErr, true, false, false)
			}
			tracked := &bodyCopyWriter{w: w}
			_, copyErr := io.Copy(tracked, body)
			if body.canceledAtFailure() {
				return newResponseBodyWriteError(resp, context.Canceled, true, true, true)
			}
			if copyErr != nil {
				return newResponseBodyWriteError(resp, copyErr, true, tracked.writeErr == nil, false)
			}
			return nil
		}
	case io.EOF, io.ErrUnexpectedEOF:
		if int64(n) != resp.ContentLength {
			return newResponseBodyWriteError(resp, io.ErrUnexpectedEOF, false, true, false)
		}
		prefix = prefix[:n]
	default:
		return newResponseBodyWriteError(resp, err, false, true, false)
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	if longerThanAdvertised {
		w.Header().Del("Content-Length")
	}
	out := prefix
	changed := false
	if transform != nil {
		out, changed = transform(prefix)
	}
	if changed {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	}
	w.WriteHeader(resp.StatusCode)
	if _, writeErr := w.Write(out); writeErr != nil {
		return newResponseBodyWriteError(resp, writeErr, true, false, false)
	}
	return nil
}

func readFullObservingLifecycle(body io.Reader, ctx context.Context, buffer []byte) (n int, canceledAtFailure bool, err error) {
	for n < len(buffer) && err == nil {
		var read int
		read, err = body.Read(buffer[n:])
		n += read
		if lifecycleCancellationAtReadFailure(ctx, err) {
			canceledAtFailure = true
		}
	}
	if n >= len(buffer) {
		err = nil
	} else if n > 0 && errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, canceledAtFailure, err
}

func readUsageSniffPrefix(body io.Reader, contentLength int64) ([]byte, *[]byte, error) {
	const limit = usageSniffMaxBuffer + 1
	if contentLength < 0 || contentLength > usageSniffMaxBuffer {
		prefix, err := io.ReadAll(io.LimitReader(body, limit))
		return prefix, nil, err
	}

	var pooledBuffer *[]byte
	var prefix []byte
	if contentLength < usageSniffSmallBufferSize {
		pooledBuffer = borrowUsageSniffBuffer(contentLength)
		prefix = (*pooledBuffer)[:int(contentLength)+1]
	} else {
		prefix = make([]byte, int(contentLength)+1)
	}
	n, err := io.ReadFull(body, prefix)
	switch err {
	case nil:
		// The body exceeded its advertised length. Continue to the normal cap so
		// a malformed Content-Length cannot truncate the downstream response.
		if len(prefix) >= limit {
			return prefix, pooledBuffer, nil
		}
		rest, readErr := io.ReadAll(io.LimitReader(body, int64(limit-len(prefix))))
		return append(prefix, rest...), pooledBuffer, readErr
	case io.EOF, io.ErrUnexpectedEOF:
		if int64(n) != contentLength {
			return nil, pooledBuffer, io.ErrUnexpectedEOF
		}
		return prefix[:n], pooledBuffer, nil
	default:
		return nil, pooledBuffer, err
	}
}

func borrowUsageSniffBuffer(contentLength int64) *[]byte {
	if contentLength < usageSniffTinyBufferSize {
		return usageSniffTinyBufferPool.Get().(*[]byte)
	}
	return usageSniffSmallBufferPool.Get().(*[]byte)
}

func releaseUsageSniffBuffer(buffer *[]byte) {
	if buffer == nil {
		return
	}
	switch cap(*buffer) {
	case usageSniffTinyBufferSize:
		*buffer = (*buffer)[:usageSniffTinyBufferSize]
		usageSniffTinyBufferPool.Put(buffer)
	case usageSniffSmallBufferSize:
		*buffer = (*buffer)[:usageSniffSmallBufferSize]
		usageSniffSmallBufferPool.Put(buffer)
	}
}

// sniffOpenAIUsage extracts the usage block from a non-streaming OpenAI chat
// completion body without disturbing the rest of the payload. It returns nil
// when the body is not valid JSON or carries no usage, so callers can pass the
// result straight to observeOpenAIUsage.
func sniffOpenAIUsage(body []byte) *models.OpenAIUsage {
	var parsed struct {
		Usage *models.OpenAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	return parsed.Usage
}

func writeDirectAnthropicJSONResponse(ctx, upstreamCtx context.Context, w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) error {
	info, explicitRoute := explicitRouteResponseInfoFromResponse(resp)
	if explicitRoute {
		publicModel = info.publicID
		upstreamModel = ""
	}
	bodyReader := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
	defer func() { _ = bodyReader.Close() }()

	maxBodySize := int64(0)
	if explicitRoute {
		maxBodySize = maxLargeRequestBodySize
	}
	var body []byte
	var pooledBody *[]byte
	var err error
	if resp.ContentLength >= 0 && resp.ContentLength < usageSniffSmallBufferSize {
		body, pooledBody, err = readDirectAnthropicJSONBodyKnownLength(bodyReader, resp.ContentLength, maxBodySize)
		if pooledBody != nil {
			defer releaseUsageSniffBuffer(pooledBody)
		}
	} else {
		body, err = readDirectAnthropicJSONBody(bodyReader, maxBodySize)
	}
	if bodyReader.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, bodyReader.canceledAtFailure())
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, false, true, bodyReader.canceledAtFailure())
	}
	contentLengthMismatch := resp.ContentLength >= 0 && int64(len(body)) != resp.ContentLength

	var rewritten []byte
	var changed bool
	var fastUsage models.AnthropicUsage
	fastUsageParsed := false
	if explicitRoute {
		rewritten, changed, err = normalizeExplicitAnthropicResponseModelJSON(body, publicModel)
		if err != nil {
			return newResponseBodyWriteError(resp, err, false, true, false)
		}
	} else {
		if pooledBody != nil {
			if inspection, ok := inspectAnthropicResponseJSONFast(body); ok {
				fastUsage = inspection.usage
				fastUsageParsed = inspection.usageParsed
				rewritten, changed, err = rewriteAnthropicResponseModelJSONInPlaceInspected(body, publicModel, upstreamModel, inspection)
				if err != nil {
					return newResponseBodyWriteError(resp, err, false, true, false)
				}
			}
		}
		if rewritten == nil {
			rewritten, changed = rewriteAnthropicResponseModelJSON(body, publicModel, upstreamModel)
		}
	}
	if resp.StatusCode == http.StatusOK {
		if fastUsageParsed {
			observeAnthropicUsage(ctx, fastUsage)
		} else {
			observeAnthropicUsageBody(ctx, rewritten)
		}
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	if changed || contentLengthMismatch {
		w.Header().Del("Content-Length")
	}
	if explicitRoute {
		markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(rewritten); err != nil {
		return newResponseBodyWriteError(resp, err, true, false, false)
	}
	return nil
}

func readDirectAnthropicJSONBodyKnownLength(body io.Reader, contentLength, maxBodySize int64) ([]byte, *[]byte, error) {
	if maxBodySize > 0 && contentLength > maxBodySize {
		return nil, nil, errors.New("explicit route anthropic response exceeds model-normalization limit")
	}
	pooledBuffer := borrowUsageSniffBuffer(contentLength)
	prefix := (*pooledBuffer)[:int(contentLength)+1]
	n, err := io.ReadFull(body, prefix)
	switch err {
	case nil:
		if maxBodySize > 0 {
			remaining := maxBodySize + 1 - int64(len(prefix))
			if remaining <= 0 {
				return nil, pooledBuffer, errors.New("explicit route anthropic response exceeds model-normalization limit")
			}
			rest, readErr := io.ReadAll(io.LimitReader(body, remaining))
			data := append(prefix, rest...)
			if readErr != nil {
				return data, pooledBuffer, readErr
			}
			if int64(len(data)) > maxBodySize {
				return nil, pooledBuffer, errors.New("explicit route anthropic response exceeds model-normalization limit")
			}
			return data, pooledBuffer, nil
		}
		rest, readErr := io.ReadAll(body)
		return append(prefix, rest...), pooledBuffer, readErr
	case io.EOF, io.ErrUnexpectedEOF:
		if int64(n) != contentLength {
			return nil, pooledBuffer, io.ErrUnexpectedEOF
		}
		return prefix[:n], pooledBuffer, nil
	default:
		return nil, pooledBuffer, err
	}
}

// readDirectAnthropicJSONBody keeps the historical unbounded legacy read when
// maxBodySize is zero. Explicit routes pass a positive bound so a successful
// response cannot bypass public-model normalization by exceeding the buffer.
func readDirectAnthropicJSONBody(body io.Reader, maxBodySize int64) ([]byte, error) {
	if maxBodySize <= 0 {
		return io.ReadAll(body)
	}

	data, err := io.ReadAll(io.LimitReader(body, maxBodySize+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > maxBodySize {
		return nil, errors.New("explicit route anthropic response exceeds model-normalization limit")
	}
	return data, nil
}

// normalizeExplicitAnthropicResponseModelJSON is the fail-closed counterpart
// to rewriteAnthropicResponseModelJSON. Explicit routes must not forward a
// successful body unless it is a complete JSON object that can be normalized.
func normalizeExplicitAnthropicResponseModelJSON(body []byte, publicModel string) ([]byte, bool, error) {
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return body, false, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("malformed explicit route anthropic response: %w", err)
	}
	if payload == nil {
		return nil, false, errors.New("malformed explicit route anthropic response: expected JSON object")
	}

	changed := rewriteAnthropicModelFields(payload, publicModel)
	if rawJSONString(payload["model"]) != publicModel {
		rawPublicModel, err := json.Marshal(publicModel)
		if err != nil {
			return nil, false, fmt.Errorf("normalize explicit route anthropic response model: %w", err)
		}
		payload["model"] = rawPublicModel
		changed = true
	}
	if !changed {
		return body, false, nil
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("normalize explicit route anthropic response model: %w", err)
	}
	return rewritten, true, nil
}

func (h *ProxyHandler) writeDirectAnthropicStreamResponse(ctx, upstreamCtx context.Context, w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) {
	if info, ok := explicitRouteResponseInfoFromResponse(resp); ok {
		publicModel = info.publicID
		upstreamModel = ""
	}
	defer func() { _ = resp.Body.Close() }()

	copyPassthroughHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	setSSEHeaders(w)
	w.WriteHeader(resp.StatusCode)
	body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
	streamAnthropicPassthroughBody(ctx, w, body, publicModel, upstreamModel, h.lifecycleStreamHooks(ctx, body.canceledAtFailure))
}

func rewriteAnthropicResponseModelJSON(body []byte, publicModel, upstreamModel string) ([]byte, bool) {
	publicModel = strings.TrimSpace(publicModel)
	upstreamModel = strings.TrimSpace(upstreamModel)
	if publicModel == "" || publicModel == upstreamModel {
		return body, false
	}
	rawPublicModel, err := json.Marshal(publicModel)
	if err == nil {
		if rewritten, changed, ok := rewriteAnthropicResponseModelJSONFast(body, rawPublicModel); ok {
			return rewritten, changed
		}
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}

	changed := rewriteAnthropicModelFields(payload, publicModel)
	if !changed {
		return body, false
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

func rewriteAnthropicResponseModelJSONInPlace(body []byte, publicModel, upstreamModel string) ([]byte, bool, error) {
	inspection, ok := inspectAnthropicResponseJSONFast(body)
	if !ok {
		return nil, false, nil
	}
	return rewriteAnthropicResponseModelJSONInPlaceInspected(body, publicModel, upstreamModel, inspection)
}

func rewriteAnthropicResponseModelJSONInPlaceInspected(body []byte, publicModel, upstreamModel string, inspection anthropicResponseJSONInspection) ([]byte, bool, error) {
	publicModel = strings.TrimSpace(publicModel)
	upstreamModel = strings.TrimSpace(upstreamModel)
	if publicModel == "" || publicModel == upstreamModel {
		return body, false, nil
	}
	rawPublicModel, err := json.Marshal(publicModel)
	if err != nil {
		return nil, false, err
	}
	if !inspection.rewriteModel {
		return body, false, nil
	}
	rewritten, ok := replaceRawJSONRangeInPlace(body, inspection.modelStart, inspection.modelEnd, rawPublicModel)
	if !ok {
		return nil, false, nil
	}
	return rewritten, true, nil
}

// rewriteAnthropicResponseModelJSONFast handles the ordinary Messages response
// without rebuilding the complete JSON object. It declines ambiguous shapes so
// the map-based path above retains duplicate-key, escaped-key, and nested
// message.model behavior.
func rewriteAnthropicResponseModelJSONFast(body []byte, rawPublicModel json.RawMessage) ([]byte, bool, bool) {
	if !json.Valid(rawPublicModel) {
		return body, false, false
	}
	modelStart, modelEnd, rewrite, ok := anthropicResponseModelRewriteRange(body)
	if !ok {
		return body, false, false
	}
	if !rewrite {
		return body, false, true
	}

	if modelStart < 0 || modelEnd < modelStart || modelEnd > len(body) {
		return body, false, false
	}
	baseLen := len(body) - (modelEnd - modelStart)
	maxInt := int(^uint(0) >> 1)
	if len(rawPublicModel) > maxInt-baseLen {
		return body, false, false
	}
	out := make([]byte, 0, baseLen+len(rawPublicModel))
	out = append(out, body[:modelStart]...)
	out = append(out, rawPublicModel...)
	out = append(out, body[modelEnd:]...)
	return out, true, true
}

func anthropicResponseModelRewriteRange(body []byte) (int, int, bool, bool) {
	inspection, ok := inspectAnthropicResponseJSONFast(body)
	if !ok {
		return 0, 0, false, false
	}
	return inspection.modelStart, inspection.modelEnd, inspection.rewriteModel, true
}

type anthropicResponseJSONInspection struct {
	modelStart   int
	modelEnd     int
	rewriteModel bool
	usage        models.AnthropicUsage
	usageParsed  bool
}

func inspectAnthropicResponseJSONFast(body []byte) (anthropicResponseJSONInspection, bool) {
	object, ok := newStrictRawJSONObjectScanner(body)
	if !ok {
		return anthropicResponseJSONInspection{}, false
	}

	inspection := anthropicResponseJSONInspection{modelStart: -1, modelEnd: -1, usageParsed: true}
	modelSeen := false
	messageSeen := false
	usageSeen := false
	for {
		key, start, end, done, scanOK := object.next()
		if !scanOK {
			return anthropicResponseJSONInspection{}, false
		}
		if done {
			break
		}
		switch {
		case rawJSONKeyEqual(key, "model"):
			if modelSeen {
				return anthropicResponseJSONInspection{}, false
			}
			modelSeen = true
			rawModel := bytes.TrimSpace(body[start:end])
			contentStart, contentEnd, valueEnd, escaped, stringOK := scanStrictRawJSONString(rawModel, 0)
			if !stringOK || escaped || valueEnd != len(rawModel) {
				return anthropicResponseJSONInspection{}, false
			}
			if len(bytes.TrimSpace(rawModel[contentStart:contentEnd])) > 0 {
				inspection.modelStart = start
				inspection.modelEnd = end
				inspection.rewriteModel = true
			}
		case rawJSONKeyEqualFold(key, "model"):
			return anthropicResponseJSONInspection{}, false
		case rawJSONKeyEqual(key, "message"):
			if messageSeen {
				return anthropicResponseJSONInspection{}, false
			}
			messageSeen = true
			rawMessage := bytes.TrimSpace(body[start:end])
			if len(rawMessage) > 0 && rawMessage[0] == '{' {
				return anthropicResponseJSONInspection{}, false
			}
		case rawJSONKeyEqual(key, "usage"):
			if usageSeen {
				inspection.usageParsed = false
				continue
			}
			usageSeen = true
			usage, usageOK := parseAnthropicUsageFast(body[start:end])
			if !usageOK {
				inspection.usageParsed = false
				continue
			}
			inspection.usage = usage
		case rawJSONKeyEqualFold(key, "usage"):
			inspection.usageParsed = false
		}
	}
	return inspection, true
}

func parseAnthropicUsageFast(raw []byte) (models.AnthropicUsage, bool) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return models.AnthropicUsage{}, true
	}
	if len(raw) == 0 || raw[0] != '{' {
		return models.AnthropicUsage{}, true
	}
	object, ok := newStrictRawJSONObjectScanner(raw)
	if !ok {
		return models.AnthropicUsage{}, false
	}

	var usage models.AnthropicUsage
	var seen uint8
	const (
		usageFieldInput uint8 = 1 << iota
		usageFieldOutput
		usageFieldCacheCreation
		usageFieldCacheRead
	)
	for {
		key, start, end, done, scanOK := object.next()
		if !scanOK {
			return models.AnthropicUsage{}, false
		}
		if done {
			break
		}
		var field uint8
		switch {
		case rawJSONKeyEqual(key, "input_tokens"):
			field = usageFieldInput
		case rawJSONKeyEqual(key, "output_tokens"):
			field = usageFieldOutput
		case rawJSONKeyEqual(key, "cache_creation_input_tokens"):
			field = usageFieldCacheCreation
		case rawJSONKeyEqual(key, "cache_read_input_tokens"):
			field = usageFieldCacheRead
		case rawJSONKeyEqualFold(key, "input_tokens"),
			rawJSONKeyEqualFold(key, "output_tokens"),
			rawJSONKeyEqualFold(key, "cache_creation_input_tokens"),
			rawJSONKeyEqualFold(key, "cache_read_input_tokens"):
			return models.AnthropicUsage{}, false
		default:
			continue
		}
		if seen&field != 0 {
			return models.AnthropicUsage{}, false
		}
		seen |= field
		valueRaw := bytes.TrimSpace(raw[start:end])
		value := 0
		if !bytes.Equal(valueRaw, []byte("null")) {
			var valueOK bool
			value, valueOK = rawJSONInt(valueRaw)
			if !valueOK {
				return models.AnthropicUsage{}, false
			}
		}
		switch field {
		case usageFieldInput:
			usage.InputTokens = value
		case usageFieldOutput:
			usage.OutputTokens = value
		case usageFieldCacheCreation:
			usage.CacheCreationInputTokens = value
		case usageFieldCacheRead:
			usage.CacheReadInputTokens = value
		}
	}
	return usage, true
}

func replaceRawJSONRangeInPlace(body []byte, start, end int, replacement []byte) ([]byte, bool) {
	if start < 0 || end < start || end > len(body) {
		return body, false
	}
	newLen := len(body) - (end - start)
	maxInt := int(^uint(0) >> 1)
	if len(replacement) > maxInt-newLen {
		return body, false
	}
	newLen += len(replacement)
	if newLen > cap(body) {
		return body, false
	}

	oldLen := len(body)
	replacementEnd := start + len(replacement)
	if newLen > oldLen {
		body = body[:newLen]
		copy(body[replacementEnd:], body[end:oldLen])
	} else if newLen < oldLen {
		copy(body[replacementEnd:], body[end:oldLen])
		clear(body[newLen:oldLen])
		body = body[:newLen]
	}
	copy(body[start:replacementEnd], replacement)
	return body, true
}

func rewriteAnthropicModelFields(payload map[string]json.RawMessage, publicModel string) bool {
	if len(payload) == 0 || strings.TrimSpace(publicModel) == "" {
		return false
	}
	rawModel, err := json.Marshal(publicModel)
	if err != nil {
		return false
	}

	changed := rewriteAnthropicModelField(payload, rawModel)

	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload["message"], &message); err == nil && len(message) > 0 && rewriteAnthropicModelField(message, rawModel) {
		if rawMessage, err := json.Marshal(message); err == nil {
			payload["message"] = rawMessage
			changed = true
		}
	}

	return changed
}

func rewriteAnthropicModelField(payload map[string]json.RawMessage, rawModel json.RawMessage) bool {
	for key, value := range payload {
		if strings.EqualFold(key, "model") && rawJSONString(value) != "" {
			payload["model"] = rawModel
			return true
		}
	}
	return false
}
