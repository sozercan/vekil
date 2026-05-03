package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (h *ProxyHandler) newInferenceUpstreamContext(streaming bool) (context.Context, context.CancelFunc) {
	// Use background context with timeout to avoid cancellation from client
	// disconnects while still preventing goroutine leaks on upstream hangs.
	timeout := upstreamTimeout
	if streaming {
		timeout = h.effectiveStreamingUpstreamTimeout()
	}
	return context.WithTimeout(context.Background(), timeout)
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

func (h *ProxyHandler) resolveProviderRequest(body []byte, endpoint string) (*providerRuntime, []byte, error) {
	model := extractRequestModel(body)
	provider, owner, known := h.resolveProviderModel(model, endpoint)
	if provider == nil {
		return nil, nil, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("no provider available for endpoint %s", endpoint)}
	}
	if !providerSupportsEndpoint(provider, endpoint) {
		return nil, nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("provider %q does not support %s", provider.id, endpoint),
		}
	}
	if known && !providerModelSupportsEndpoint(owner, endpoint) {
		return nil, nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("model %q does not support %s", model, endpoint),
		}
	}

	rewrittenBody, _, err := rewriteRequestModelForProvider(body, owner.upstreamModel)
	if err != nil {
		return nil, nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	return provider, rewrittenBody, nil
}

func providerSupportsEndpoint(provider *providerRuntime, endpoint string) bool {
	if provider == nil {
		return false
	}
	if provider.kind == providerTypeOpenAICodex {
		return supportsEndpoint(openAICodexProviderEndpoints, endpoint)
	}
	return true
}

func (h *ProxyHandler) postJSONEndpoint(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, path, body, nil)
}

func (h *ProxyHandler) postJSONEndpointWithHeaders(ctx context.Context, path string, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithMetrics(ctx, path, body, extraHeaders, nil)
}

func (h *ProxyHandler) postJSONEndpointWithMetrics(ctx context.Context, path string, body []byte, extraHeaders http.Header, tracker *requestMetricsTracker) (*http.Response, error) {
	requestedModel := extractRequestModel(body)
	provider, rewrittenBody, err := h.resolveProviderRequest(body, path)
	if err != nil {
		return nil, err
	}
	h.populateRequestMetricsRouteLabels(ctx, tracker, requestedModel, path, provider)

	return h.doWithRetryWithMetrics(tracker, func() (*http.Request, error) {
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodPost, path, rewrittenBody, extraHeaders, "")
		if err != nil {
			return nil, err
		}
		return req, nil
	})
}

func (h *ProxyHandler) populateRequestMetricsRouteLabels(_ context.Context, tracker *requestMetricsTracker, requestedModel, endpoint string, provider *providerRuntime) {
	if tracker == nil || provider == nil {
		return
	}

	tracker.setProvider(provider.id)
	if tracker.publicModelLabel() != metricsUnknownLabel {
		return
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return
	}

	resolvedProvider, owner, known := h.resolveProviderModel(requestedModel, endpoint)
	if known && resolvedProvider != nil && resolvedProvider.id == provider.id {
		tracker.setPublicModel(owner.publicID)
	}
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return h.postChatCompletionsWithMetrics(ctx, body, nil)
}

func (h *ProxyHandler) postChatCompletionsWithMetrics(ctx context.Context, body []byte, tracker *requestMetricsTracker) (*http.Response, error) {
	return h.postJSONEndpointWithMetrics(ctx, "/chat/completions", body, nil, tracker)
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postResponsesWithHeadersAndMetrics(ctx, body, extraHeaders, nil)
}

func (h *ProxyHandler) postResponsesWithHeadersAndMetrics(ctx context.Context, body []byte, extraHeaders http.Header, tracker *requestMetricsTracker) (*http.Response, error) {
	return h.postJSONEndpointWithMetrics(ctx, "/responses", body, extraHeaders, tracker)
}

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
