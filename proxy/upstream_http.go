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
	lookupModel := model
	if endpoint == providerEndpointMessages {
		lookupModel = NormalizeModelName(model)
	}
	provider, owner, known := h.resolveProviderModel(lookupModel, endpoint)
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
	if !known && !providerAllowsUnknownModelEndpoint(provider, endpoint) {
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
	switch provider.kind {
	case providerTypeOpenAICodex:
		return supportsEndpoint(openAICodexProviderEndpoints, endpoint)
	case providerTypeOpenAICompatible:
		return endpoint == providerEndpointChatCompletions || endpoint == providerEndpointResponses
	case providerTypeAnthropicCompatible:
		return endpoint == providerEndpointMessages
	default:
		return true
	}
}

func providerAllowsUnknownModelEndpoint(provider *providerRuntime, endpoint string) bool {
	if provider == nil {
		return false
	}
	switch provider.kind {
	case providerTypeOpenAICompatible:
		return providerUsesDynamicModels(provider) && endpoint == providerEndpointChatCompletions
	case providerTypeAnthropicCompatible:
		return providerUsesDynamicModels(provider) && endpoint == providerEndpointMessages
	default:
		return true
	}
}

func (h *ProxyHandler) postJSONEndpoint(ctx context.Context, path string, body []byte) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, path, body, nil)
}

func (h *ProxyHandler) postJSONEndpointWithHeaders(ctx context.Context, path string, body []byte, extraHeaders http.Header) (*http.Response, error) {
	provider, rewrittenBody, err := h.resolveProviderRequest(body, path)
	if err != nil {
		return nil, err
	}

	return h.doWithRetry(func() (*http.Request, error) {
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodPost, path, rewrittenBody, extraHeaders, "")
		if err != nil {
			return nil, err
		}
		return req, nil
	})
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, body []byte) (*http.Response, error) {
	return h.postJSONEndpoint(ctx, providerEndpointChatCompletions, body)
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, providerEndpointResponses, body, extraHeaders)
}

func (h *ProxyHandler) postAnthropicMessages(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, providerEndpointMessages, body, extraHeaders)
}

func (h *ProxyHandler) postAnthropicMessagesCountTokens(ctx context.Context, body []byte, extraHeaders http.Header) (*http.Response, error) {
	provider, rewrittenBody, err := h.resolveProviderRequest(body, providerEndpointMessages)
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
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodPost, providerEndpointMessagesCount, rewrittenBody, extraHeaders, "")
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

func writeDirectAnthropicJSONResponse(w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) error {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
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

func writeDirectAnthropicStreamResponse(w http.ResponseWriter, resp *http.Response, publicModel, upstreamModel string) {
	defer func() { _ = resp.Body.Close() }()

	copyPassthroughHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	setSSEHeaders(w)
	w.WriteHeader(resp.StatusCode)
	streamAnthropicPassthroughBody(w, resp.Body, publicModel, upstreamModel)
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
