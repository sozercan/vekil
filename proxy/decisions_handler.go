package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

// copilotEndpointDecisions is Copilot's native Decisions API endpoint.
const copilotEndpointDecisions = "/v1/decisions"

// HandleDecisions serves POST /v1/decisions as a passthrough to the upstream
// endpoint of the same path, without translation. Only models whose catalog
// entry lists /v1/decisions in supported_endpoints are accepted, so the
// endpoint never reaches models or providers that did not advertise it.
func (h *ProxyHandler) HandleDecisions(w http.ResponseWriter, r *http.Request) {
	const endpoint, summaryEndpoint = copilotEndpointDecisions, "decisions"
	body, err := readBodyWithLimit(r, maxRequestBodySize)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		writeOpenAIRequestBodyError(w, readBodyStatusCode(err), err)
		return
	}
	defer func() { _ = r.Body.Close() }()

	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	var request struct {
		Model json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error")
		return
	}
	var model string
	if len(request.Model) == 0 || json.Unmarshal(request.Model, &model) != nil || strings.TrimSpace(model) == "" {
		writeOpenAIErrorWithDetails(w, http.StatusBadRequest, "model is required", "invalid_request_error", "model", "")
		return
	}
	// The body is forwarded unchanged, so the gated ID must be the exact one
	// upstream will see.
	if model != strings.TrimSpace(model) {
		writeOpenAIErrorWithDetails(w, http.StatusBadRequest, "model must not have surrounding whitespace", "invalid_request_error", "model", "")
		return
	}
	h.observeRequestSummary(r.Context(), summaryEndpoint, model, false, endpoint)
	if !h.modelAllowedForRequest(model, endpoint) {
		writeOpenAIError(w, http.StatusBadRequest, modelNotAllowedRequestError(model).Error(), "invalid_request_error")
		return
	}

	catalogCtx, catalogCancel := h.newLifecycleUpstreamContext(modelsUpstreamTimeout)
	supported, err := h.catalogModelSupportsEndpoint(catalogCtx, r.Context(), model, endpoint)
	catalogCancel()
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		h.log.Error("model catalog unavailable", logger.F("endpoint", summaryEndpoint), logger.Err(err))
		writeOpenAIUpstreamRequestFailure(w, upstreamStatusCode(err, http.StatusBadGateway), err)
		return
	}
	if !supported {
		writeOpenAIErrorWithDetails(w, http.StatusBadRequest,
			fmt.Sprintf("model %q does not support %s", model, endpoint), "invalid_request_error", "model", "")
		return
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), false)
	defer upstreamCancel()
	upstreamCtx = withCopilotRequestMetadata(upstreamCtx, r.Header)

	resp, err := h.postJSONEndpointWithHeadersForModel(upstreamCtx, endpoint, body, nil, model)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", summaryEndpoint), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		writeOpenAIUpstreamRequestFailure(w, statusCode, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	observeUpstreamHeaders(r.Context(), resp.Header)

	writeErr := writePassthroughSniffingUsage(w, resp, func(responseBody []byte) ([]byte, bool) {
		observeDecisionUsage(r.Context(), responseBody)
		return responseBody, false
	})
	if writeErr != nil {
		h.handleResponseBodyWriteError(w, r, upstreamCtx, summaryEndpoint, writeErr)
	}
}

// observeDecisionUsage records the input/output token usage that decision
// responses report, when present.
func observeDecisionUsage(ctx context.Context, body []byte) {
	var response struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || response.Usage == nil {
		return
	}
	if response.Usage.InputTokens <= 0 && response.Usage.OutputTokens <= 0 {
		return
	}
	observeOpenAIUsage(ctx, &models.OpenAIUsage{
		PromptTokens:     response.Usage.InputTokens,
		CompletionTokens: response.Usage.OutputTokens,
		TotalTokens:      response.Usage.InputTokens + response.Usage.OutputTokens,
	})
}

// catalogModelSupportsEndpoint reports whether the merged /models catalog
// lists model with endpoint in its supported_endpoints.
func (h *ProxyHandler) catalogModelSupportsEndpoint(ctx, waitCtx context.Context, model, endpoint string) (bool, error) {
	entry, ok, err := h.ensureCanonicalModelsCacheEntry(ctx, waitCtx)
	if err != nil && !ok {
		return false, err
	}
	var catalog struct {
		Data []struct {
			ID                 string   `json:"id"`
			SupportedEndpoints []string `json:"supported_endpoints"`
		} `json:"data"`
	}
	if err := json.Unmarshal(entry.body, &catalog); err != nil {
		return false, fmt.Errorf("parse model catalog: %w", err)
	}
	for _, candidate := range catalog.Data {
		if candidate.ID == model {
			return supportsEndpoint(candidate.SupportedEndpoints, endpoint), nil
		}
	}
	return false, nil
}
