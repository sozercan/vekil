package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
)

func (h *ProxyHandler) writeResponsesUpstreamResponse(w http.ResponseWriter, resp *http.Response, store *ToolExecutionContextStore, scope string) {
	if h == nil || h.toolOptimizers == nil || !h.toolOptimizers.ShouldInspectNonStreamingResponses() || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		writeUpstreamResponse(w, resp)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		copyPassthroughHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, bytes.NewReader(bodyBytes))
		return
	}
	ctx := context.Background()
	if resp.Request != nil {
		ctx = resp.Request.Context()
	}
	rewritten, changed := h.maybeRewriteResponsesResponseBody(ctx, bodyBytes, store, scope)
	copyPassthroughHeaders(w.Header(), resp.Header)
	if changed {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)
}
