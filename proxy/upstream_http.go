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
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func (h *ProxyHandler) newLifecycleUpstreamContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	causeCtx, cancelCause := context.WithCancelCause(h.lifecycleContext())
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
	ctx, cancel := h.newLifecycleUpstreamContext(timeout)
	if isRetryStatsTracked(inbound) {
		ctx = markRetryStatsTracked(ctx)
	}
	// Propagate the request summary so retry-logging can read provider/model
	// labels without needing a separate plumbing path.
	if summary := RequestSummaryFromContext(inbound); summary != nil {
		ctx = context.WithValue(ctx, requestSummaryContextKey{}, summary)
	}
	return ctx, cancel
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

	rewrittenBody, err := prepareResolvedProviderRequestBody(body, model, endpoint, provider, owner)
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
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	rewrittenBody := body
	if !providerUsesAzureClassicDeploymentPath(provider, endpoint) {
		var err error
		rewrittenBody, _, err = rewriteRequestModelForProviderFromModel(body, requestModel, owner.upstreamModel)
		if err != nil {
			return nil, err
		}
	}
	return applyProviderModelRequestPolicy(rewrittenBody, endpoint, owner), nil
}

func applyProviderModelRequestPolicy(body []byte, endpoint string, owner providerModel) []byte {
	rewriteMaxTokens := endpoint == providerEndpointChatCompletions && owner.useMaxCompletionTokens
	if !owner.dropSamplingParams && !rewriteMaxTokens && (owner.parallelToolCalls == nil || *owner.parallelToolCalls) {
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
	if provider == nil {
		return nil, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider is required")}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	preparedBody, err := prepareResolvedProviderRequestBody(body, extractRequestModel(body), endpoint, provider, owner)
	if err != nil {
		return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}

	return h.doWithRetry(func() (*http.Request, error) {
		return h.newProviderJSONRequest(ctx, provider, http.MethodPost, endpoint, preparedBody, extraHeaders, "", owner)
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
	provider, owner, rewrittenBody, err := h.resolveProviderRequestForModel(body, path, model)
	if err != nil {
		return nil, err
	}

	return h.doWithRetry(func() (*http.Request, error) {
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodPost, path, rewrittenBody, extraHeaders, "", owner)
		if err != nil {
			return nil, err
		}
		return req, nil
	})
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postResponsesWithHeadersForModel(ctx, body, extraHeaders, extractRequestModel(body))
}

func (h *ProxyHandler) postResponsesWithHeadersForModel(ctx context.Context, body []byte, extraHeaders http.Header, model string) (*http.Response, error) {
	resp, err := h.postJSONEndpointWithHeadersForModel(ctx, providerEndpointResponses, body, extraHeaders, model)
	if err != nil {
		return nil, err
	}
	return h.maybeRetryResponsesWithoutUnverifiableEncryptedContent(ctx, body, extraHeaders, resp)
}

func (h *ProxyHandler) postAnthropicMessages(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, providerEndpointMessages, body, extraHeaders)
}

func (h *ProxyHandler) postAnthropicMessagesCountTokens(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
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

	return h.doWithRetry(func() (*http.Request, error) {
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodPost, providerEndpointMessagesCount, rewrittenBody, extraHeaders, "", owner)
		if err != nil {
			return nil, err
		}
		return req, nil
	})
}

type bodyCopyWriter struct {
	w        http.ResponseWriter
	writeErr error
}

func (w *bodyCopyWriter) Write(p []byte) (int, error) {
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
	prefix, err := io.ReadAll(io.LimitReader(body, usageSniffMaxBuffer+1))
	if body.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, body.canceledAtFailure())
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, false, true, body.canceledAtFailure())
	}
	copyPassthroughHeaders(w.Header(), resp.Header)

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
	// stays bounded. Total bytes written equal the full body, so any
	// Content-Length header copied above remains correct.
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
	bodyReader := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
	defer func() { _ = bodyReader.Close() }()

	body, err := io.ReadAll(bodyReader)
	if bodyReader.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, bodyReader.canceledAtFailure())
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, false, true, bodyReader.canceledAtFailure())
	}
	if resp.StatusCode == http.StatusOK {
		observeAnthropicUsageBody(ctx, body)
	}
	rewritten, changed := rewriteAnthropicResponseModelJSON(body, publicModel, upstreamModel)

	copyPassthroughHeaders(w.Header(), resp.Header)
	if changed {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(rewritten); err != nil {
		return newResponseBodyWriteError(resp, err, true, false, false)
	}
	return nil
}

func (h *ProxyHandler) writeDirectAnthropicStreamResponse(ctx, upstreamCtx context.Context, w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) {
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

func rewriteAnthropicModelFields(payload map[string]json.RawMessage, publicModel string) bool {
	if len(payload) == 0 || strings.TrimSpace(publicModel) == "" {
		return false
	}

	changed := false
	if rawJSONString(payload["model"]) != "" {
		rawModel, err := json.Marshal(publicModel)
		if err == nil {
			payload["model"] = rawModel
			changed = true
		}
	}

	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload["message"], &message); err == nil && len(message) > 0 && rawJSONString(message["model"]) != "" {
		rawModel, err := json.Marshal(publicModel)
		if err == nil {
			message["model"] = rawModel
			if rawMessage, err := json.Marshal(message); err == nil {
				payload["message"] = rawMessage
				changed = true
			}
		}
	}

	return changed
}
