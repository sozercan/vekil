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
		return writeResponsesPassthroughObservingUsage(ctx, w, resp)
	}
	return writePassthroughSniffingUsage(w, resp, func(bodyBytes []byte) ([]byte, bool) {
		observeResponsesUsage(ctx, sniffResponsesUsageBody(bodyBytes))
		return h.maybeRewriteResponsesResponseBody(ctx, bodyBytes, store, scope)
	})
}

func writeResponsesPassthroughObservingUsage(ctx context.Context, w http.ResponseWriter, resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return writeUpstreamResponse(w, resp)
	}
	return writePassthroughSniffingUsage(w, resp, func(body []byte) ([]byte, bool) {
		observeResponsesUsage(ctx, sniffResponsesUsageBody(body))
		return body, false
	})
}
