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

func (h *ProxyHandler) resolveProviderRequest(body []byte, endpoint string, routingHeaders ...http.Header) (*providerRuntime, providerModel, []byte, error) {
	model := extractRequestModel(body)
	provider, owner, known, forcedSpeedAlias := h.resolveProviderModelWithFastAlias(model, endpoint)
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

	routingHeader := http.Header(nil)
	if len(routingHeaders) > 0 {
		routingHeader = routingHeaders[0]
	}
	owner, body = h.maybeApplySpeedTierRouting(body, endpoint, routingHeader, provider, owner, known, forcedSpeedAlias)

	rewrittenBody := body
	if !providerUsesAzureClassicDeploymentPath(provider, endpoint) {
		var err error
		rewrittenBody, _, err = rewriteRequestModelForProvider(body, owner.upstreamModel)
		if err != nil {
			return nil, providerModel{}, nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
		}
	}
	rewrittenBody = applyProviderModelRequestPolicy(rewrittenBody, owner)
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

func (h *ProxyHandler) postJSONEndpointWithHeaders(ctx context.Context, path string, body []byte, extraHeaders http.Header, routingHeaders ...http.Header) (*http.Response, error) {
	resp, _, _, err := h.postJSONEndpointWithHeadersTracked(ctx, path, body, extraHeaders, routingHeaders...)
	return resp, err
}

func (h *ProxyHandler) postJSONEndpointWithHeadersTracked(ctx context.Context, path string, body []byte, extraHeaders http.Header, routingHeaders ...http.Header) (*http.Response, providerModel, []byte, error) {
	provider, owner, rewrittenBody, err := h.resolveProviderRequest(body, path, routingHeaders...)
	if err != nil {
		return nil, providerModel{}, nil, err
	}

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodPost, path, rewrittenBody, extraHeaders, "", owner)
		if err != nil {
			return nil, err
		}
		return req, nil
	})
	return resp, owner, rewrittenBody, err
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return h.postChatCompletionsWithHeaders(ctx, body, nil)
}

func (h *ProxyHandler) postChatCompletionsWithHeaders(ctx context.Context, body []byte, routingHeaders http.Header) (*http.Response, error) {
	resp, _, err := h.postChatCompletionsWithHeadersTracked(ctx, body, routingHeaders)
	return resp, err
}

func (h *ProxyHandler) postChatCompletionsWithHeadersTracked(ctx context.Context, body []byte, routingHeaders http.Header) (*http.Response, providerModel, error) {
	resp, owner, _, err := h.postJSONEndpointWithHeadersTracked(ctx, providerEndpointChatCompletions, body, nil, routingHeaders)
	return resp, owner, err
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header, routingHeaders ...http.Header) (*http.Response, error) {
	resp, owner, _, err := h.postJSONEndpointWithHeadersTracked(ctx, providerEndpointResponses, body, extraHeaders, routingHeaders...)
	if err != nil {
		return nil, err
	}
	retryBody := body
	if owner.publicID != "" {
		if selectedBody, _, rewriteErr := rewriteRequestModelForProvider(body, owner.publicID); rewriteErr == nil {
			retryBody = selectedBody
		}
	}
	return h.maybeRetryResponsesWithoutUnverifiableEncryptedContent(ctx, retryBody, extraHeaders, resp, noSpeedTierRoutingHeaders())
}

func (h *ProxyHandler) postAnthropicMessagesTracked(ctx context.Context, body []byte, extraHeaders http.Header, routingHeaders ...http.Header) (*http.Response, providerModel, error) {
	resp, owner, _, err := h.postJSONEndpointWithHeadersTracked(ctx, providerEndpointMessages, body, extraHeaders, routingHeaders...)
	return resp, owner, err
}

func (h *ProxyHandler) postAnthropicMessagesCountTokens(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	provider, owner, rewrittenBody, err := h.resolveProviderRequest(body, providerEndpointMessages, noSpeedTierRoutingHeaders())
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
