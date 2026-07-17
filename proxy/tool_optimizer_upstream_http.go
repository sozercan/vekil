package proxy

import (
	"context"
	"net/http"
)

func (h *ProxyHandler) writeResponsesUpstreamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, store *ToolExecutionContextStore, scope string) error {
	if info, ok := explicitRouteResponseInfoFromResponse(resp); ok {
		return writeExplicitResponsesResponse(ctx, h, w, resp, info, store, scope)
	}
	if h == nil || h.toolOptimizers == nil || !h.toolOptimizers.ShouldInspectNonStreamingResponses() || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		// Optimizers off (the default) or a non-OK/empty response: stream the body
		// through verbatim while sniffing usage tokens for traffic stats.
		return writeResponsesPassthroughObservingUsage(ctx, w, resp)
	}
	return writePassthroughSniffingUsage(w, resp, func(bodyBytes []byte) ([]byte, bool) {
		observeResponsesUsage(ctx, sniffResponsesUsageBody(bodyBytes))
		return h.maybeRewriteResponsesResponseBody(ctx, bodyBytes, store, scope)
	})
}

// writeResponsesPassthroughObservingUsage writes a non-streaming Responses
// body to the client byte-for-byte while sniffing usage tokens for traffic
// stats. It preserves the near-zero-copy contract: on a 200 it reads the body
// to parse usage and writes the same bytes back unchanged (headers and
// Content-Length untouched); any non-2xx or read failure falls back to a plain
// streamed copy. Memory is bounded via usageSniffMaxBuffer: an oversized body
// streams through with the usage parse skipped rather than being buffered whole.
// ctx must be the inbound request context so the usage lands on the right
// RequestSummary.
func writeResponsesPassthroughObservingUsage(ctx context.Context, w http.ResponseWriter, resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return writeUpstreamResponse(w, resp)
	}
	return writePassthroughSniffingUsage(w, resp, func(body []byte) ([]byte, bool) {
		observeResponsesUsage(ctx, sniffResponsesUsageBody(body))
		return body, false
	})
}
