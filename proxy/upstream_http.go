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

const upstreamPassthroughProbeSize = 32 << 10

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

func writeUpstreamResponseAndObserve(w http.ResponseWriter, resp *http.Response, observe func(io.Reader)) error {
	defer func() { _ = resp.Body.Close() }()

	probe := make([]byte, upstreamPassthroughProbeSize)
	n, readErr := resp.Body.Read(probe)
	if n == 0 && readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	writer := io.Writer(w)
	var (
		observerDone chan struct{}
		observerPipe *io.PipeWriter
	)
	if observe != nil {
		pr, pw := io.Pipe()
		observerDone = make(chan struct{})
		observerPipe = pw
		go func() {
			defer close(observerDone)
			observe(pr)
			_, _ = io.Copy(io.Discard, pr)
			_ = pr.Close()
		}()
		writer = io.MultiWriter(w, pw)
	}

	closeObserver := func(copyErr error) {
		if observerPipe == nil {
			return
		}
		_ = observerPipe.CloseWithError(copyErr)
		<-observerDone
	}

	if n > 0 {
		if _, err := writer.Write(probe[:n]); err != nil {
			closeObserver(err)
			return nil
		}
	}

	var copyErr error
	switch {
	case readErr == nil:
		_, copyErr = io.Copy(writer, resp.Body)
	case errors.Is(readErr, io.EOF):
		copyErr = nil
	default:
		copyErr = readErr
	}

	closeObserver(copyErr)
	return nil
}

func writeUpstreamResponseAndObserveOpenAIUsage(w http.ResponseWriter, resp *http.Response, obs *requestObservation) error {
	var observe func(io.Reader)
	if obs != nil {
		observe = func(r io.Reader) {
			obs.observeOpenAIUsageFromReader(r)
		}
	}
	return writeUpstreamResponseAndObserve(w, resp, observe)
}

func writeUpstreamResponseAndObserveResponsesUsage(w http.ResponseWriter, resp *http.Response, obs *requestObservation) error {
	var observe func(io.Reader)
	if obs != nil {
		observe = func(r io.Reader) {
			obs.observeResponsesUsageFromReader(r)
		}
	}
	return writeUpstreamResponseAndObserve(w, resp, observe)
}
