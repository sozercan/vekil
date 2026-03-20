package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

func newInferenceUpstreamContext() (context.Context, context.CancelFunc) {
	// Use background context with timeout to avoid cancellation from client
	// disconnects while still preventing goroutine leaks on upstream hangs.
	return context.WithTimeout(context.Background(), upstreamTimeout)
}

func upstreamStatusCode(err error, fallback int) int {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.statusCode
	}
	return fallback
}

func (h *ProxyHandler) postJSONEndpoint(ctx context.Context, token, path string, body []byte) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, token, path, body, nil)
}

func mergeHeaderValues(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (h *ProxyHandler) postJSONEndpointWithHeaders(ctx context.Context, token, path string, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.copilotURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		h.setCopilotHeaders(req, token)
		if len(extraHeaders) > 0 {
			mergeHeaderValues(req.Header, extraHeaders)
		}
		return req, nil
	})
}

func (h *ProxyHandler) postChatCompletions(ctx context.Context, token string, body []byte) (*http.Response, error) {
	return h.postJSONEndpoint(ctx, token, "/chat/completions", body)
}

func (h *ProxyHandler) postResponses(ctx context.Context, token string, body []byte) (*http.Response, error) {
	return h.postJSONEndpoint(ctx, token, "/responses", body)
}

func (h *ProxyHandler) postResponsesWithHeaders(ctx context.Context, token string, body []byte, extraHeaders http.Header) (*http.Response, error) {
	return h.postJSONEndpointWithHeaders(ctx, token, "/responses", body, extraHeaders)
}

func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
