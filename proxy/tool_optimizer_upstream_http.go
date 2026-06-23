package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
)

func (h *ProxyHandler) writeResponsesUpstreamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, store *ToolExecutionContextStore, scope string) {
	if h == nil || h.toolOptimizers == nil || !h.toolOptimizers.ShouldInspectNonStreamingResponses() || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		// Optimizers off (the default) or a non-OK/empty response: stream the body
		// through verbatim while sniffing usage tokens for traffic stats.
		writeResponsesPassthroughObservingUsage(ctx, w, resp)
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
	observeResponsesUsage(ctx, sniffResponsesUsageBody(bodyBytes))
	rewritten, changed := h.maybeRewriteResponsesResponseBody(ctx, bodyBytes, store, scope)
	copyPassthroughHeaders(w.Header(), resp.Header)
	if changed {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)
}

// writeResponsesPassthroughObservingUsage writes a non-streaming Responses
// body to the client byte-for-byte while sniffing usage tokens for traffic
// stats. It preserves the near-zero-copy contract: on a 200 it reads the body
// to parse usage and writes the same bytes back unchanged (headers and
// Content-Length untouched); any non-200 or read failure falls back to a plain
// streamed copy. Memory is bounded via usageSniffMaxBuffer: an oversized body
// streams through with the usage parse skipped rather than being buffered whole.
// ctx must be the inbound request context so the usage lands on the right
// RequestSummary.
func writeResponsesPassthroughObservingUsage(ctx context.Context, w http.ResponseWriter, resp *http.Response) {
	if resp == nil || resp.Body == nil {
		writeUpstreamResponse(w, resp)
		return
	}
	writePassthroughSniffingUsage(w, resp, func(body []byte) {
		observeResponsesUsage(ctx, sniffResponsesUsageBody(body))
	})
}
