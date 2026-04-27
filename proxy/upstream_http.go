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
)

func (h *ProxyHandler) newInferenceUpstreamContext(streaming bool) (context.Context, context.CancelFunc) {
	return h.newInferenceUpstreamContextFromParent(nil, streaming)
}

func (h *ProxyHandler) newInferenceUpstreamContextFromParent(parent context.Context, streaming bool) (context.Context, context.CancelFunc) {
	// Use a cancellation-detached parent context so request-scoped values such as
	// metrics labels still flow through, without aborting upstream work on client
	// disconnects.
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}

	timeout := upstreamTimeout
	if streaming {
		timeout = h.effectiveStreamingUpstreamTimeout()
	}
	return context.WithTimeout(base, timeout)
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

type resolvedProviderRequest struct {
	provider      *providerRuntime
	rewrittenBody []byte
	publicModel   string
}

func (r resolvedProviderRequest) applyMetricsScope(scope *requestMetricsScope) {
	if scope == nil {
		return
	}
	scope.SetPublicModel(r.publicModel)
	if r.provider != nil {
		scope.SetProvider(r.provider.id)
	}
}

func (h *ProxyHandler) resolveProviderRequestDetailed(body []byte, endpoint string) (*resolvedProviderRequest, error) {
	model := extractRequestModel(body)
	provider, owner, known := h.resolveProviderModel(model, endpoint)
	resolved := &resolvedProviderRequest{
		provider:    provider,
		publicModel: model,
	}
	if provider == nil {
		return resolved, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("no provider available for endpoint %s", endpoint)}
	}
	if !providerSupportsEndpoint(provider, endpoint) {
		return resolved, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("provider %q does not support %s", provider.id, endpoint),
		}
	}
	if known && !providerModelSupportsEndpoint(owner, endpoint) {
		return resolved, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("model %q does not support %s", model, endpoint),
		}
	}

	rewrittenBody, _, err := rewriteRequestModelForProvider(body, owner.upstreamModel)
	if err != nil {
		return resolved, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	resolved.rewrittenBody = rewrittenBody
	return resolved, nil
}

func (h *ProxyHandler) resolveProviderRequest(body []byte, endpoint string) (*providerRuntime, []byte, error) {
	resolved, err := h.resolveProviderRequestDetailed(body, endpoint)
	if err != nil {
		return nil, nil, err
	}
	return resolved.provider, resolved.rewrittenBody, nil
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
	scope := requestMetricsFromContext(ctx)

	resolved, err := h.resolveProviderRequestDetailed(body, path)
	if resolved != nil {
		resolved.applyMetricsScope(scope)
	}
	if err != nil {
		return nil, err
	}

	resp, err := h.doWithRetry(ctx, func() (*http.Request, error) {
		req, err := h.newProviderJSONRequest(ctx, resolved.provider, http.MethodPost, path, resolved.rewrittenBody, extraHeaders, "")
		if err != nil {
			return nil, err
		}
		return req, nil
	})
	if err != nil {
		scope.observeUpstreamError(classifyUpstreamMetricsReason(err), classifyUpstreamMetricsCode(err))
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		scope.observeUpstreamError("status_code", strconv.Itoa(resp.StatusCode))
	}
	return resp, nil
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return h.postJSONEndpoint(ctx, "/chat/completions", body)
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, "/responses", body, extraHeaders)
}

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeUpstreamResponseWithBody(w http.ResponseWriter, resp *http.Response, observeBody func([]byte)) error {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if observeBody != nil {
		observeBody(body)
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(body)
	return err
}
