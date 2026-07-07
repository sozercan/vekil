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

func (h *ProxyHandler) newInferenceUpstreamContext(streaming bool) (context.Context, context.CancelFunc) {
	return h.newInferenceUpstreamContextFrom(context.Background(), streaming)
}

// newInferenceUpstreamContextFrom builds the upstream request context the same
// way as newInferenceUpstreamContext (background-rooted with a timeout, so a
// client disconnect does not cancel the upstream call) but copies the
// retry-stats tracked marker from the inbound request context when present. The
// background root deliberately strips inherited values, so this explicit copy is
// what lets a tracked inference request's upstream retries be counted while
// non-tracked callers (insight, model-catalog fetch, count-token probes) stay
// uncounted. Pass the inbound r.Context(); a context without the marker (e.g.
// context.Background()) yields an untracked upstream context.
func (h *ProxyHandler) newInferenceUpstreamContextFrom(inbound context.Context, streaming bool) (context.Context, context.CancelFunc) {
	// Use background context with timeout to avoid cancellation from client
	// disconnects while still preventing goroutine leaks on upstream hangs.
	timeout := upstreamTimeout
	if streaming {
		timeout = h.effectiveStreamingUpstreamTimeout()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if isRetryStatsTracked(inbound) {
		ctx = markRetryStatsTracked(ctx)
	}
	if summary := RequestSummaryFromContext(inbound); summary != nil {
		ctx = contextWithRequestSummary(ctx, summary)
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
	model := extractRequestModel(body)
	lookupModel := model
	if endpoint == providerEndpointMessages {
		lookupModel = NormalizeModelName(model)
	}
	provider, owner, known := h.resolveProviderModel(lookupModel, endpoint)
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

	rewrittenBody, err := prepareProviderRequestBody(provider, owner, body, endpoint)
	if err != nil {
		return nil, providerModel{}, nil, err
	}
	return provider, owner, rewrittenBody, nil
}

func applyProviderModelRequestPolicy(body []byte, owner providerModel) []byte {
	if !owner.dropSamplingParams && (owner.parallelToolCalls == nil || *owner.parallelToolCalls) {
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

func (h *ProxyHandler) postJSONEndpoint(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, path, body, nil)
}

func (h *ProxyHandler) postJSONEndpointWithHeaders(ctx context.Context, path string, body []byte, extraHeaders http.Header) (*http.Response, error) {
	resp, selectedOwner, _, err := h.postJSONEndpointWithHeadersTracked(ctx, path, body, extraHeaders)
	if err == nil {
		h.observeSelectedProvider(ctx, selectedOwner)
	}
	return resp, err
}

func (h *ProxyHandler) postJSONEndpointWithHeadersTracked(ctx context.Context, path string, body []byte, extraHeaders http.Header) (*http.Response, providerModel, []byte, error) {
	_, owner, rewrittenBody, err := h.resolveProviderRequest(body, path)
	if err != nil {
		return nil, providerModel{}, nil, err
	}

	attempts := h.providerFallbackAttempts(owner, path)
	if len(attempts) == 0 {
		attempts = []providerModel{owner}
	}
	var lastErr error
	lastOwner := owner
	lastBody := rewrittenBody
	for i, attemptOwner := range attempts {
		attemptCtx := ctx
		var cancel context.CancelFunc
		if i > 0 && ctx.Err() != nil {
			attemptCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), upstreamTimeout)
		}
		if cancel != nil {
			defer cancel()
		}
		attemptProvider := h.providerSetup().providerByID(attemptOwner.providerID)
		if attemptProvider == nil || !attemptProvider.supportsEndpoint(path) || !providerModelSupportsEndpoint(attemptOwner, path) {
			continue
		}
		attemptBody, prepErr := prepareProviderRequestBody(attemptProvider, attemptOwner, body, path)
		if prepErr != nil {
			return nil, providerModel{}, nil, prepErr
		}
		resp, postErr := h.doWithRetry(func() (*http.Request, error) {
			req, err := h.newProviderJSONRequest(attemptCtx, attemptProvider, http.MethodPost, path, attemptBody, extraHeaders, "", attemptOwner)
			if err != nil {
				return nil, err
			}
			return req, nil
		})
		lastOwner = attemptOwner
		lastBody = attemptBody
		if !shouldFallbackResponse(resp, postErr) || i == len(attempts)-1 {
			return resp, attemptOwner, attemptBody, postErr
		}
		lastErr = postErr
		if lastErr == nil && resp != nil {
			lastErr = &upstreamError{statusCode: resp.StatusCode, headers: resp.Header.Clone()}
			drainAndClose(resp.Body)
		}
		h.logProviderFallback(owner.publicID, attemptOwner, attempts[i+1], lastErr)
	}
	if lastErr != nil {
		return nil, lastOwner, lastBody, lastErr
	}
	return nil, providerModel{}, nil, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("no fallback provider available for model %q", owner.publicID)}
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return h.postJSONEndpoint(ctx, providerEndpointChatCompletions, body)
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	resp, _, err := h.postResponsesWithHeadersTracked(ctx, body, extraHeaders)
	return resp, err
}

func (h *ProxyHandler) postResponsesWithHeadersTracked(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, providerModel, error) {
	resp, owner, _, err := h.postJSONEndpointWithHeadersTracked(ctx, providerEndpointResponses, body, extraHeaders)
	if err != nil {
		return nil, providerModel{}, err
	}
	resp, err = h.maybeRetryResponsesWithoutUnverifiableEncryptedContent(ctx, body, extraHeaders, resp)
	return resp, owner, err
}

func (h *ProxyHandler) postAnthropicMessages(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	resp, _, err := h.postAnthropicMessagesTracked(ctx, body, extraHeaders)
	return resp, err
}

func (h *ProxyHandler) postAnthropicMessagesTracked(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, providerModel, error) {
	resp, owner, _, err := h.postJSONEndpointWithHeadersTracked(ctx, providerEndpointMessages, body, extraHeaders)
	return resp, owner, err
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

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// writeOpenAIChatCompletionResponse writes a non-streaming OpenAI chat response,
// normalizing missing required Chat Completions fields for strict SDK clients
// while preserving vendor-specific fields. It only rewrites successful JSON
// responses that fit in usageSniffMaxBuffer; errors, invalid JSON, and oversized
// responses fail open to passthrough behavior.
func (h *ProxyHandler) writeOpenAIChatCompletionResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, requestedModel string) {
	writePassthroughSniffingUsage(w, resp, func(body []byte) ([]byte, bool) {
		out, changed, err := normalizeOpenAIChatCompletionResponse(body, requestedModel, time.Now())
		if err != nil {
			observeOpenAIUsage(ctx, sniffOpenAIUsage(body))
			return body, false
		}
		observeOpenAIUsage(ctx, sniffOpenAIUsage(out))
		return out, changed
	})
}

func (h *ProxyHandler) writeOpenAIPassthroughObservingUsage(ctx context.Context, w http.ResponseWriter, resp *http.Response) {
	writePassthroughSniffingUsage(w, resp, func(body []byte) ([]byte, bool) {
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
func writePassthroughSniffingUsage(w http.ResponseWriter, resp *http.Response, transform func([]byte) ([]byte, bool)) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		copyPassthroughHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Read one byte past the cap so we can tell a full body from an oversized one.
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, usageSniffMaxBuffer+1))
	copyPassthroughHeaders(w.Header(), resp.Header)
	if err != nil {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(prefix)
		return
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
		_, _ = w.Write(out)
		return
	}

	// Oversized: skip the usage parse and stream prefix + remainder so memory
	// stays bounded. Total bytes written equal the full body, so any
	// Content-Length header copied above remains correct.
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(prefix)
	_, _ = io.Copy(w, resp.Body)
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

func writeDirectAnthropicJSONResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) error {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
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
	_, _ = w.Write(rewritten)
	return nil
}

func writeDirectAnthropicStreamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) {
	defer func() { _ = resp.Body.Close() }()

	copyPassthroughHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	setSSEHeaders(w)
	w.WriteHeader(resp.StatusCode)
	streamAnthropicPassthroughBody(ctx, w, resp.Body, publicModel, upstreamModel)
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

func prepareProviderRequestBody(provider *providerRuntime, owner providerModel, body []byte, endpoint string) ([]byte, error) {
	rewrittenBody := body
	if provider == nil {
		return nil, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider is required")}
	}
	if provider.kind != providerTypeAzureOpenAI || len(provider.endpoints) == 0 {
		if !providerUsesAzureClassicDeploymentPath(provider, endpoint) {
			var err error
			rewrittenBody, _, err = rewriteRequestModelForProvider(body, owner.upstreamModel)
			if err != nil {
				return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
			}
		}
	}
	return applyProviderModelRequestPolicy(rewrittenBody, owner), nil
}

func (h *ProxyHandler) providerFallbackAttempts(owner providerModel, endpoint string) []providerModel {
	setup := h.providerSetup()
	chain := setup.fallbackChain(owner.publicID)
	if len(chain) == 0 {
		return nil
	}
	attempts := make([]providerModel, 0, len(chain)+1)
	seenCurrent := false
	for _, candidate := range chain {
		if candidate.providerID == owner.providerID && candidate.publicID == owner.publicID {
			seenCurrent = true
		}
		if !seenCurrent {
			continue
		}
		if providerModelSupportsEndpoint(candidate, endpoint) {
			attempts = append(attempts, candidate)
		}
	}
	if len(attempts) == 0 {
		attempts = append(attempts, owner)
		for _, candidate := range chain {
			if candidate.providerID == owner.providerID && candidate.publicID == owner.publicID {
				continue
			}
			if providerModelSupportsEndpoint(candidate, endpoint) {
				attempts = append(attempts, candidate)
			}
		}
	}
	return attempts
}

func shouldFallbackResponse(resp *http.Response, err error) bool {
	if err != nil {
		return shouldFallbackToNextProvider(err)
	}
	return resp != nil && resp.StatusCode >= http.StatusInternalServerError
}

func shouldFallbackToNextProvider(err error) bool {
	if err == nil {
		return false
	}
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return providerErr.statusCode >= http.StatusInternalServerError && strings.Contains(strings.ToLower(providerErr.err.Error()), "no healthy endpoints")
	}
	if permanentTransportError(err) {
		return false
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return retryable(upstreamErr.statusCode)
	}
	return true
}

func (h *ProxyHandler) logProviderFallback(requested string, from, to providerModel, cause error) {
	if h == nil || h.log == nil {
		return
	}
	fields := []logger.Field{
		logger.F("requested_model", requested),
		logger.F("from_provider", from.providerID),
		logger.F("from_model", from.publicID),
		logger.F("to_provider", to.providerID),
		logger.F("to_model", to.publicID),
	}
	if cause != nil {
		fields = append(fields, logger.Err(cause))
	}
	h.log.Info("provider fallback", fields...)
}
