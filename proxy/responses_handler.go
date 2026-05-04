package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sozercan/vekil/logger"
)

func responsesExtraHeadersFromRequest(r *http.Request) http.Header {
	var headers http.Header

	for _, name := range []string{
		"X-OpenAI-Subagent",
		"OpenAI-Beta",
		"session_id",
		"X-Client-Request-Id",
		"X-Codex-Beta-Features",
		"X-Codex-Turn-State",
		"X-Codex-Turn-Metadata",
		"X-Codex-Parent-Thread-Id",
		"X-Codex-Window-Id",
	} {
		for _, value := range r.Header.Values(name) {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				if headers == nil {
					headers = make(http.Header, 2)
				}
				headers.Add(name, trimmed)
			}
		}
	}

	return headers
}

// HandleResponses handles POST /v1/responses by forwarding the request to
// Copilot's responses endpoint with only auth headers injected.
func (h *ProxyHandler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	bodyBytes = h.rewriteResponsesRequestBody(bodyBytes, "responses", true)

	var partial struct {
		Stream *bool `json:"stream,omitempty"`
	}
	_ = json.Unmarshal(bodyBytes, &partial)
	isStreaming := partial.Stream != nil && *partial.Stream

	extraHeaders := responsesExtraHeadersFromRequest(r)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(isStreaming)
	defer upstreamCancel()

	resp, err := h.postResponsesWithHeaders(upstreamCtx, bodyBytes, extraHeaders)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "responses"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		if statusCode == http.StatusInternalServerError {
			writeOpenAIError(w, statusCode, "authentication failed", "server_error")
			return
		}
		writeOpenAIError(w, statusCode, "upstream request failed", "server_error")
		return
	}
	resp, err = h.maybeRetryCompactedResponsesRequest(upstreamCtx, bodyBytes, extraHeaders, resp)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "responses"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		if statusCode == http.StatusInternalServerError {
			writeOpenAIError(w, statusCode, "authentication failed", "server_error")
			return
		}
		writeOpenAIError(w, statusCode, "upstream request failed", "server_error")
		return
	}

	if isStreaming && resp.StatusCode == http.StatusOK {
		model := extractRequestModel(bodyBytes)
		peekAndForwardResponses(h, w, r, resp, upstreamCancel, model)
		return
	}

	writeUpstreamResponse(w, resp)
}

// compactPrompt is the system instruction used when the upstream does not
// support the /responses/compact endpoint natively. The proxy converts the
// compact request into a regular /responses call with this prompt so the
// model produces a summarized handoff. The resulting compaction item is a
// proxy-owned opaque token rather than a real upstream-encrypted payload.
// compactUpstreamChunkBodySize is the default target body size for chunked
// compact retries. It is measured against the serialized upstream request body
// size, not the model-visible token budget. Picked conservatively because
// observed Copilot /responses payload caps sit around 1 MiB; per-provider
// limits differ (Azure and OpenAI Codex tolerate larger bodies). When the
// upstream still returns 413 at this size, the chunker halves the target on
// each recursive retry until it hits compactUpstreamChunkBodyFloor.
const (
	compactUpstreamChunkBodySize  = 1 << 20
	compactUpstreamChunkBodyFloor = 64 << 10
	// compactUpstreamErrorBodySize caps upstream error bodies that the compact
	// fallback buffers only so it can replay the original failure if chunking fails.
	compactUpstreamErrorBodySize = 1 << 20
	// compactUpstreamMaxAttempts caps the number of logical compaction calls
	// the compact-413 fallback may issue per inbound request. Each logical
	// call may make up to one extra HTTP POST if the configured model is
	// rejected as unsupported (model-fallback) and may be retried by the
	// shared transport-retry policy in retry.go on transient upstream
	// failures (429/502/503/504). The cap is a runaway-fanout safety net,
	// not a precise HTTP-POST limit.
	//
	// The default is sized to the documented inbound ceiling so the budget
	// does not gatekeep legitimate large requests:
	//   ceil(maxLargeRequestBodySize / compactUpstreamChunkBodySize)  ← worst-case chunks
	//   * 2                                                            ← one round of halving
	//   + initial 413 + merge call + small headroom
	// = 64 * 2 + 16 = 144. We round up to 160 to leave room for sibling
	// re-splits when learnedTarget contracts mid-flight.
	compactUpstreamMaxAttempts = 160
)

// compactBudget bounds the total upstream attempts the compact-413 fallback may
// consume per inbound request. It is threaded through the recursive chunking
// path so siblings, retries, and merge calls all share the same allowance.
//
// learnedTarget records the smallest target body size we have observed working
// (or are about to retry with) after an upstream 413. The sibling fanout loop
// in compactResponsesRequestInChunks reads this between iterations so once one
// chunk forces a halving, every remaining sibling drops to that new target
// instead of repeating the same known-doomed POST.
//
// resolvedModel records the substitute model picked by the model-fallback path
// the first time the configured model is rejected as unsupported. Subsequent
// compact calls in the same fanout pre-rewrite their request body to this
// model so they never trigger another fallback probe (which would otherwise
// double the real upstream POST count per logical compaction call).
type compactBudget struct {
	attempts      int
	max           int
	learnedTarget int
	resolvedModel string
}

func newCompactBudget(max int) *compactBudget {
	if max <= 0 {
		max = compactUpstreamMaxAttempts
	}
	return &compactBudget{max: max}
}

// consume bumps the attempt counter and returns true if the call is still
// within budget. A nil receiver is treated as unbounded so the helper is safe
// to use in code paths that do not enforce a budget.
func (b *compactBudget) consume() bool {
	if b == nil {
		return true
	}
	b.attempts++
	return b.attempts <= b.max
}

func (b *compactBudget) exhausted() bool {
	if b == nil {
		return false
	}
	return b.attempts >= b.max
}

// recordLearnedTarget shrinks the shared adaptive target when a new lower
// upper-bound on the upstream's payload cap is discovered. Larger values are
// ignored so the target only ratchets downward.
func (b *compactBudget) recordLearnedTarget(target int) {
	if b == nil || target <= 0 {
		return
	}
	if b.learnedTarget == 0 || target < b.learnedTarget {
		b.learnedTarget = target
	}
}

// adjustTarget returns the smaller of the caller's target and any learned
// target, so siblings inherit shrinkage observed by an earlier chunk in the
// same fanout.
func (b *compactBudget) adjustTarget(target int) int {
	if b == nil || b.learnedTarget == 0 {
		return target
	}
	if b.learnedTarget < target {
		return b.learnedTarget
	}
	return target
}

// recordResolvedModel memoizes the substitute model picked by the
// model-fallback path so subsequent compact calls in the same fanout can
// pre-rewrite their request body and skip the unsupported-model probe.
// First-write-wins: a later resolution to a different name (e.g. across
// providers) is ignored to keep the fanout coherent.
func (b *compactBudget) recordResolvedModel(model string) {
	if b == nil || model == "" {
		return
	}
	if b.resolvedModel == "" {
		b.resolvedModel = model
	}
}

const compactPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION for an interrupted coding-agent session. Create a handoff summary for another LLM that must continue the same task seamlessly.

Write the summary so the next assistant can resume work without asking the user to restate the task.

Include:
- Current objective and task status: IN_PROGRESS, BLOCKED_ON_USER, or COMPLETE
- Completed work and key decisions already made
- The last concrete action taken and any important intermediate results
- The next exact step the next assistant should take first
- Critical context, constraints, user preferences, files, commands, errors, or references needed to continue

Be concise, structured, and action-oriented. Do not chat with the user. Do not ask follow-up questions unless the task status is BLOCKED_ON_USER.`

// HandleCompact handles POST /v1/responses/compact by forwarding the request
// to the upstream /responses endpoint with a compaction system prompt injected.
// The upstream response is then transformed into the compact response format
// that Codex expects. The returned compaction item is a proxy-owned token that
// this proxy can later expand back into summarized context for /responses.
func (h *ProxyHandler) HandleCompact(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBodyWithLimit(r, maxLargeRequestBodySize)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	bodyBytes = h.rewriteResponsesRequestBody(bodyBytes, "responses/compact", false)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	summaryText, resp, err := h.compactResponsesRequest(upstreamCtx, body, responsesExtraHeadersFromRequest(r))
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "compact"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		if statusCode == http.StatusInternalServerError {
			writeOpenAIError(w, statusCode, "authentication failed", "server_error")
			return
		}
		writeOpenAIError(w, statusCode, "upstream request failed", "server_error")
		return
	}
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
		if contentType := resp.Header.Get("Content-Type"); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	writeCompactResponse(w, summaryText)
}

// memorySummarizePrompt is the system instruction used to summarize conversation
// traces into memory entries when the upstream does not support the
// /memories/trace_summarize endpoint natively.
const memorySummarizePrompt = `You are summarizing a past coding session trace for future reference.

For each trace provided, produce TWO outputs:
1. "trace_summary": A detailed summary of what happened in the session — key actions, decisions, files modified, errors encountered, and outcomes.
2. "memory_summary": A concise, high-level summary (1-3 sentences) suitable for injecting into a future session as context.

Respond with a JSON array where each element has "trace_summary" and "memory_summary" fields. Output ONLY valid JSON, no markdown fences.`

// HandleMemorySummarize handles POST /v1/memories/trace_summarize by sending
// the traces to the upstream /responses endpoint with a summarization prompt,
// then transforming the response into the format Codex expects.
func (h *ProxyHandler) HandleMemorySummarize(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBodyWithLimit(r, maxLargeRequestBodySize)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	var memReq struct {
		Model     string            `json:"model"`
		Traces    []json.RawMessage `json:"traces"`
		Reasoning json.RawMessage   `json:"reasoning,omitempty"`
	}
	if err := json.Unmarshal(bodyBytes, &memReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}

	tracesJSON, _ := json.Marshal(memReq.Traces)
	userContent := "Summarize the following session traces:\n\n" + string(tracesJSON)

	responsesReq := map[string]interface{}{
		"model":        memReq.Model,
		"instructions": memorySummarizePrompt,
		"input": []map[string]interface{}{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": userContent},
				},
			},
		},
	}
	if len(memReq.Reasoning) > 0 && string(memReq.Reasoning) != "null" {
		responsesReq["reasoning"] = json.RawMessage(memReq.Reasoning)
	}
	reqBody, _ := json.Marshal(responsesReq)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	resp, err := h.postResponsesWithFallbackHeaders(upstreamCtx, reqBody, responsesExtraHeadersFromRequest(r))
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "memory_summarize"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		if statusCode == http.StatusInternalServerError {
			writeOpenAIError(w, statusCode, "authentication failed", "server_error")
			return
		}
		writeOpenAIError(w, statusCode, "upstream request failed", "server_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		if contentType := resp.Header.Get("Content-Type"); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}

	summaryText, err := extractResponsesOutputText(respBody)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to parse upstream response", "server_error")
		return
	}

	cleaned := strings.TrimSpace(summaryText)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	type memorySummary struct {
		TraceSummary  string `json:"trace_summary"`
		MemorySummary string `json:"memory_summary"`
	}
	var summaries []memorySummary
	if err := json.Unmarshal([]byte(cleaned), &summaries); err != nil {
		summaries = make([]memorySummary, len(memReq.Traces))
		for i := range summaries {
			summaries[i] = memorySummary{
				TraceSummary:  cleaned,
				MemorySummary: cleaned,
			}
		}
	}

	for len(summaries) < len(memReq.Traces) {
		summaries = append(summaries, memorySummary{
			TraceSummary:  "No summary available.",
			MemorySummary: "No summary available.",
		})
	}
	summaries = summaries[:len(memReq.Traces)]

	memResp := struct {
		Output []memorySummary `json:"output"`
	}{
		Output: summaries,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(memResp)
}

func extractResponsesOutputText(body []byte) (string, error) {
	var upstream struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, item := range upstream.Output {
		var outputItem struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(item, &outputItem); err != nil {
			continue
		}
		if outputItem.Type != "message" {
			continue
		}
		for _, content := range outputItem.Content {
			if (content.Type == "output_text" || content.Type == "text") && content.Text != "" {
				sb.WriteString(content.Text)
			}
		}
	}

	return sanitizeProxySummaryText(sb.String()), nil
}

func writeCompactResponse(w http.ResponseWriter, summaryText string) {
	type contentPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type outputItem struct {
		Type             string        `json:"type"`
		Role             string        `json:"role,omitempty"`
		Content          []contentPart `json:"content,omitempty"`
		EncryptedContent string        `json:"encrypted_content,omitempty"`
	}
	compactResp := struct {
		Output []outputItem `json:"output"`
	}{
		Output: []outputItem{
			{
				Type:    "message",
				Role:    "assistant",
				Content: []contentPart{{Type: "output_text", Text: summaryText}},
			},
			{
				Type:             "compaction",
				EncryptedContent: encodeSyntheticCompaction(summaryText),
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(compactResp)
}

func (h *ProxyHandler) compactResponsesRequest(ctx context.Context, requestFields map[string]json.RawMessage, extraHeaders http.Header) (string, *http.Response, error) {
	budget := newCompactBudget(h.effectiveCompactMaxAttempts())
	return h.compactResponsesRequestDepth(ctx, requestFields, extraHeaders, 0, h.effectiveCompactChunkBodyBytes(), budget)
}

func (h *ProxyHandler) compactResponsesRequestDepth(ctx context.Context, requestFields map[string]json.RawMessage, extraHeaders http.Header, depth int, targetBodySize int, budget *compactBudget) (string, *http.Response, error) {
	if depth > 8 {
		return "", nil, fmt.Errorf("compaction chunk recursion limit exceeded")
	}

	if !budget.consume() {
		h.log.Info("compact upstream attempt budget exhausted",
			logger.F("attempts", budget.attempts-1),
			logger.F("max_attempts", budget.max),
			logger.F("depth", depth),
		)
		return "", nil, fmt.Errorf("compact upstream attempt budget exhausted (max=%d)", budget.max)
	}

	bodyBytes, err := marshalCompactResponsesRequest(requestFields, nil)
	if err != nil {
		return "", nil, err
	}

	// If a previous chunk in this fanout already discovered that the
	// configured model is unsupported, rewrite to the resolved fallback
	// model up front so we don't make the model-fallback probe on every
	// chunk (which would double the real upstream POST count per logical
	// compaction call).
	bodyBytes = applyResolvedCompactModel(bodyBytes, budget)

	resp, err := h.postResponsesCompactWithFallback(ctx, bodyBytes, extraHeaders, budget)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode == http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", nil, err
		}
		summary, err := extractResponsesOutputText(respBody)
		if err != nil {
			return "", nil, err
		}
		return summary, nil, nil
	}

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		return "", resp, nil
	}

	respBody, truncated, readErr := readBodyWithCap(resp.Body, compactUpstreamErrorBodySize)
	_ = resp.Body.Close()
	if readErr != nil {
		return "", nil, readErr
	}
	originalResp := cloneHTTPResponseWithBody(resp, respBody)
	if truncated {
		originalResp.Header.Del("Content-Length")
		h.log.Debug("truncated upstream 413 response body for compact fallback",
			logger.F("max_bytes", compactUpstreamErrorBodySize),
		)
	}

	// Decide the next target body size. We halve eagerly when the rejected
	// request was already at or below the current target, because that means
	// our target overestimates the actual upstream cap. Without this, a request
	// that fits in one chunk at the configured target but still 413s would
	// abort instead of shrinking. We also halve unconditionally on recursive
	// passes so sibling chunks contribute pressure on the target too.
	nextTarget := targetBodySize
	if depth > 0 || len(bodyBytes) <= targetBodySize {
		nextTarget = targetBodySize / 2
	}
	if nextTarget < compactUpstreamChunkBodyFloor {
		h.log.Debug("compact chunk size hit floor; returning original 413",
			logger.F("target_body_size", targetBodySize),
			logger.F("floor", compactUpstreamChunkBodyFloor),
			logger.F("depth", depth),
		)
		return "", originalResp, nil
	}

	// Record the smaller target on the shared budget so sibling chunks in the
	// outer fanout (if any) shrink to this value before they POST and burn
	// their own discovery 413 at the larger size.
	budget.recordLearnedTarget(nextTarget)

	summary, err := h.compactResponsesRequestInChunks(ctx, requestFields, extraHeaders, depth+1, nextTarget, budget)
	if err != nil {
		h.log.Debug("chunked compact request failed",
			logger.F("target_body_size", nextTarget),
			logger.F("depth", depth),
			logger.F("attempts", budget.attempts),
			logger.F("max_attempts", budget.max),
			logger.Err(err),
		)
		return "", originalResp, nil
	}
	return summary, nil, nil
}

func marshalCompactResponsesRequest(requestFields map[string]json.RawMessage, input []json.RawMessage) ([]byte, error) {
	body := make(map[string]json.RawMessage, len(requestFields)+1)
	for key, value := range requestFields {
		body[key] = value
	}

	prompt, err := json.Marshal(compactPrompt)
	if err != nil {
		return nil, err
	}
	body["instructions"] = prompt

	if input != nil {
		inputRaw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body["input"] = inputRaw
	}

	return json.Marshal(body)
}

func cloneHTTPResponseWithBody(resp *http.Response, body []byte) *http.Response {
	if resp == nil {
		return nil
	}
	cloned := new(http.Response)
	*cloned = *resp
	if resp.Header != nil {
		cloned.Header = resp.Header.Clone()
	}
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.ContentLength = int64(len(body))
	return cloned
}

func readBodyWithCap(r io.Reader, maxBytes int) ([]byte, bool, error) {
	if maxBytes < 0 {
		return nil, false, fmt.Errorf("invalid body cap %d", maxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > maxBytes {
		return body[:maxBytes], true, nil
	}
	return body, false, nil
}

func (h *ProxyHandler) compactResponsesRequestInChunks(ctx context.Context, requestFields map[string]json.RawMessage, extraHeaders http.Header, depth int, targetBodySize int, budget *compactBudget) (string, error) {
	if targetBodySize <= 0 {
		targetBodySize = h.effectiveCompactChunkBodyBytes()
	}
	// Honor any previously learned target so re-entries (e.g. after a sibling
	// 413 forces re-splitting) inherit prior shrinkage instead of replanning
	// at the original too-large size.
	targetBodySize = budget.adjustTarget(targetBodySize)

	originalInput, err := compactInputAsRawMessages(requestFields["input"])
	if err != nil {
		return "", err
	}
	originalItems := len(originalInput)
	originalBytes := rawMessagesSize(originalInput)

	fallbackFields, strippedFixedFields, err := compactFallbackRequestFieldsForBodySize(requestFields, targetBodySize)
	if err != nil {
		return "", err
	}

	input, oversizedItemsSplit, err := splitOversizedCompactInputItemsByBodySize(fallbackFields, originalInput, targetBodySize)
	if err != nil {
		return "", err
	}

	chunks, err := splitCompactInputByBodySize(fallbackFields, input, targetBodySize)
	if err != nil {
		return "", err
	}
	if len(chunks) == 0 && len(input) == 0 && len(strippedFixedFields) > 0 {
		chunks = [][]json.RawMessage{{}}
	}
	// If the fallback can't synthesize any chunk to send (e.g. the inbound
	// request had input:[] and no fixed fields were stripped), refuse to
	// pretend the compaction succeeded. Returning an error here lets the
	// caller surface the original upstream 413 rather than a 200 with an
	// empty summary token.
	if len(chunks) == 0 {
		return "", fmt.Errorf("compact request has no chunks to send after fallback splitting")
	}
	// Allow a single chunk to proceed; if upstream rejects it, the recursive
	// halving + floor + budget guards in compactResponsesRequestDepth will
	// shrink the target until either it fits or we exhaust the budget.

	// Cheap pre-flight against the budget so we don't enter a fanout we can't
	// afford to finish. +1 for the merge call when there are multiple chunks.
	expectedAttempts := len(chunks)
	if len(chunks) > 1 {
		expectedAttempts++
	}
	if budget != nil && budget.attempts+expectedAttempts > budget.max {
		return "", fmt.Errorf("compact upstream attempt budget would be exceeded by %d-chunk fanout (have=%d, max=%d)", len(chunks), budget.attempts, budget.max)
	}

	fields := []logger.Field{
		logger.F("original_items", originalItems),
		logger.F("chunks", len(chunks)),
		logger.F("original_bytes", originalBytes),
		logger.F("target_body_size", targetBodySize),
	}
	if oversizedItemsSplit {
		fields = append(fields, logger.F("split_oversized_items", true), logger.F("expanded_items", len(input)))
	}
	if len(strippedFixedFields) > 0 {
		fields = append(fields, logger.F("stripped_fixed_fields", strippedFixedFields))
	}
	if budget != nil {
		fields = append(fields, logger.F("attempts_used", budget.attempts), logger.F("attempts_max", budget.max))
	}
	h.log.Info("retrying compact request with chunked history after 413", fields...)

	summaries := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		chunkFields := copyResponsesRequestFieldsWithInput(fallbackFields, chunk)
		summary, resp, err := h.compactResponsesRequestDepth(ctx, chunkFields, extraHeaders, depth, targetBodySize, budget)
		if err != nil {
			return "", err
		}
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, compactUpstreamErrorBodySize))
			_ = resp.Body.Close()
			return "", fmt.Errorf("compact chunk %d returned %d: %s", i+1, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		summaries = append(summaries, summary)

		// If the just-finished chunk forced the shared target to shrink, abandon
		// the planned chunk layout and re-split the remaining input at the
		// learned smaller target. Otherwise every remaining sibling would burn
		// its own discovery 413 at the known-doomed larger size before recursing.
		if i+1 < len(chunks) && budget != nil && budget.learnedTarget > 0 && budget.learnedTarget < targetBodySize {
			remaining := flattenCompactChunks(chunks[i+1:])
			remainingFields := copyResponsesRequestFieldsWithInput(fallbackFields, remaining)
			h.log.Info("re-splitting remaining compact chunks at learned smaller target",
				logger.F("learned_target", budget.learnedTarget),
				logger.F("prior_target", targetBodySize),
				logger.F("remaining_chunks", len(chunks)-(i+1)),
			)
			tail, err := h.compactResponsesRequestInChunks(ctx, remainingFields, extraHeaders, depth, budget.learnedTarget, budget)
			if err != nil {
				return "", err
			}
			summaries = append(summaries, tail)
			break
		}
	}

	return h.mergeCompactionSummaries(ctx, fallbackFields, summaries, extraHeaders, depth, targetBodySize, budget)
}

// flattenCompactChunks concatenates a list of chunked input slices back into
// one flat slice so the caller can re-split at a different target size.
func flattenCompactChunks(chunks [][]json.RawMessage) []json.RawMessage {
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	out := make([]json.RawMessage, 0, total)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func compactFallbackRequestFieldsForBodySize(requestFields map[string]json.RawMessage, maxBodySize int) (map[string]json.RawMessage, []string, error) {
	if maxBodySize <= 0 {
		return nil, nil, fmt.Errorf("invalid compact chunk size %d", maxBodySize)
	}

	probeInput, err := compactFallbackProbeInput()
	if err != nil {
		return nil, nil, err
	}

	if fits, _, err := compactRequestFieldsFitBodySize(requestFields, probeInput, maxBodySize); err != nil {
		return nil, nil, err
	} else if fits {
		return copyResponsesRequestFields(requestFields), nil, nil
	}

	stripCandidates := [][]string{
		{"tools", "tool_choice"},
		{"text"},
		{"tools", "tool_choice", "text"},
	}
	lastSize := 0
	for _, fields := range stripCandidates {
		candidate, stripped := copyResponsesRequestFieldsWithout(requestFields, fields...)
		if len(stripped) == 0 {
			continue
		}

		fits, size, err := compactRequestFieldsFitBodySize(candidate, probeInput, maxBodySize)
		if err != nil {
			return nil, nil, err
		}
		lastSize = size
		if fits {
			return candidate, stripped, nil
		}
	}

	if lastSize == 0 {
		_, lastSize, err = compactRequestFieldsFitBodySize(requestFields, probeInput, maxBodySize)
		if err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("compact request fixed fields exceed upstream payload limit after fallback minimization: %d > %d", lastSize, maxBodySize)
}

func compactFallbackProbeInput() ([]json.RawMessage, error) {
	message, err := compactTextInputRawMessage("")
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{message}, nil
}

func compactRequestFieldsFitBodySize(requestFields map[string]json.RawMessage, input []json.RawMessage, maxBodySize int) (bool, int, error) {
	body, err := marshalCompactResponsesRequest(requestFields, input)
	if err != nil {
		return false, 0, err
	}
	return len(body) <= maxBodySize, len(body), nil
}

func copyResponsesRequestFields(requestFields map[string]json.RawMessage) map[string]json.RawMessage {
	copied := make(map[string]json.RawMessage, len(requestFields))
	for key, value := range requestFields {
		copied[key] = value
	}
	return copied
}

func copyResponsesRequestFieldsWithout(requestFields map[string]json.RawMessage, fields ...string) (map[string]json.RawMessage, []string) {
	copied := copyResponsesRequestFields(requestFields)
	stripped := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, ok := copied[field]; !ok {
			continue
		}
		delete(copied, field)
		stripped = append(stripped, field)
	}
	return copied, stripped
}

func copyResponsesRequestFieldsWithInput(requestFields map[string]json.RawMessage, input []json.RawMessage) map[string]json.RawMessage {
	copied := copyResponsesRequestFields(requestFields)
	inputRaw, err := json.Marshal(input)
	if err == nil {
		copied["input"] = inputRaw
	}
	return copied
}

func compactInputAsRawMessages(rawInput json.RawMessage) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(rawInput)) == 0 {
		return nil, fmt.Errorf("compact request missing input")
	}

	var input []json.RawMessage
	if err := json.Unmarshal(rawInput, &input); err == nil {
		return input, nil
	}

	var text string
	if err := json.Unmarshal(rawInput, &text); err == nil {
		message, err := compactTextInputRawMessage(text)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{message}, nil
	}

	// The public Responses API accepts strings and arrays, but preserve any
	// unexpected JSON value as historical context so an oversized compact request
	// can still be reduced instead of replaying the upstream 413 unchanged.
	return []json.RawMessage{rawInput}, nil
}

func compactTextInputRawMessage(text string) (json.RawMessage, error) {
	return json.Marshal(map[string]interface{}{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{
				"type": "input_text",
				"text": text,
			},
		},
	})
}

func splitOversizedCompactInputItemsByBodySize(requestFields map[string]json.RawMessage, input []json.RawMessage, maxBodySize int) ([]json.RawMessage, bool, error) {
	if maxBodySize <= 0 {
		return nil, false, fmt.Errorf("invalid compact chunk size %d", maxBodySize)
	}

	fixedBodySize, err := compactRequestFixedBodySize(requestFields)
	if err != nil {
		return nil, false, err
	}

	out := make([]json.RawMessage, 0, len(input))
	var splitAny bool
	for _, item := range input {
		itemSize, err := encodedRawMessageSize(item)
		if err != nil {
			return nil, false, err
		}
		if fixedBodySize+len("[]")+itemSize <= maxBodySize {
			out = append(out, item)
			continue
		}

		splitItems, err := splitOversizedCompactInputItemByBodySize(requestFields, item, maxBodySize)
		if err != nil {
			return nil, false, err
		}
		out = append(out, splitItems...)
		splitAny = true
	}

	return out, splitAny, nil
}

func splitOversizedCompactInputItemByBodySize(requestFields map[string]json.RawMessage, item json.RawMessage, maxBodySize int) ([]json.RawMessage, error) {
	rawText := string(bytes.TrimSpace(item))
	if rawText == "" {
		return nil, fmt.Errorf("compact request contains an empty oversized input item")
	}

	items := make([]json.RawMessage, 0, (len(rawText)/max(maxBodySize, 1))+1)
	remaining := rawText
	for len(remaining) > 0 {
		chunkIndex := len(items) + 1
		chunkLen, err := largestOversizedCompactInputChunkLen(requestFields, remaining, chunkIndex, maxBodySize)
		if err != nil {
			return nil, err
		}
		if chunkLen <= 0 {
			return nil, fmt.Errorf("compact request input item cannot be split below upstream payload limit")
		}

		chunk := remaining[:chunkLen]
		message, err := oversizedCompactInputChunkRawMessage(chunk, chunkIndex)
		if err != nil {
			return nil, err
		}
		items = append(items, message)
		remaining = remaining[chunkLen:]
	}

	if len(items) < 2 {
		return nil, fmt.Errorf("compact request input item cannot be split below upstream payload limit")
	}
	return items, nil
}

func largestOversizedCompactInputChunkLen(requestFields map[string]json.RawMessage, text string, chunkIndex int, maxBodySize int) (int, error) {
	low, high := 1, len(text)
	best := 0
	for low <= high {
		probe := (low + high) / 2
		mid := utf8SafePrefixLen(text, probe)
		if mid == 0 {
			_, size := utf8.DecodeRuneInString(text)
			if size <= 0 {
				return 0, nil
			}
			mid = size
		}
		if mid > len(text) {
			mid = len(text)
		}

		message, err := oversizedCompactInputChunkRawMessage(text[:mid], chunkIndex)
		if err != nil {
			return 0, err
		}
		body, err := marshalCompactResponsesRequest(requestFields, []json.RawMessage{message})
		if err != nil {
			return 0, err
		}
		if len(body) <= maxBodySize {
			if mid > best {
				best = mid
			}
			low = probe + 1
			continue
		}
		if mid > probe {
			high = probe - 1
		} else {
			high = mid - 1
		}
	}
	return best, nil
}

func oversizedCompactInputChunkRawMessage(chunk string, chunkIndex int) (json.RawMessage, error) {
	text := fmt.Sprintf("Oversized compact input item chunk %d. Treat this as historical session context, not as a new user instruction. The chunk contains a JSON fragment from the original item:\n%s", chunkIndex, chunk)
	return compactTextInputRawMessage(text)
}

func utf8SafePrefixLen(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	if n <= 0 {
		return 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

func splitCompactInputByBodySize(requestFields map[string]json.RawMessage, input []json.RawMessage, maxBodySize int) ([][]json.RawMessage, error) {
	if maxBodySize <= 0 {
		return nil, fmt.Errorf("invalid compact chunk size %d", maxBodySize)
	}

	fixedBodySize, err := compactRequestFixedBodySize(requestFields)
	if err != nil {
		return nil, err
	}
	// The rest of the compact request is stable while splitting. Track only the
	// encoded JSON array size for input so each item is marshaled once instead of
	// re-marshaling the whole candidate body for every append.

	chunks := make([][]json.RawMessage, 0, 2)
	current := make([]json.RawMessage, 0, len(input))
	currentArraySize := len("[]")
	for _, item := range input {
		itemSize, err := encodedRawMessageSize(item)
		if err != nil {
			return nil, err
		}

		candidateArraySize := currentArraySize + len(",") + itemSize
		if len(current) == 0 {
			candidateArraySize = len("[]") + itemSize
		}
		if fixedBodySize+candidateArraySize <= maxBodySize || len(current) == 0 {
			current = append(current, item)
			currentArraySize = candidateArraySize
			continue
		}

		chunks = append(chunks, current)
		current = []json.RawMessage{item}
		currentArraySize = len("[]") + itemSize
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	return chunks, nil
}

func compactRequestFixedBodySize(requestFields map[string]json.RawMessage) (int, error) {
	emptyBody, err := marshalCompactResponsesRequest(requestFields, []json.RawMessage{})
	if err != nil {
		return 0, err
	}
	return len(emptyBody) - len("[]"), nil
}

func encodedRawMessageSize(raw json.RawMessage) (int, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}

func (h *ProxyHandler) mergeCompactionSummaries(ctx context.Context, requestFields map[string]json.RawMessage, summaries []string, extraHeaders http.Header, depth int, targetBodySize int, budget *compactBudget) (string, error) {
	switch len(summaries) {
	case 0:
		return "", nil
	case 1:
		return summaries[0], nil
	}

	input := make([]json.RawMessage, 0, len(summaries))
	for i, summary := range summaries {
		message, err := json.Marshal(map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{
					"type": "input_text",
					"text": fmt.Sprintf("Partial checkpoint summary %d of %d:\n%s", i+1, len(summaries), summary),
				},
			},
		})
		if err != nil {
			return "", err
		}
		input = append(input, json.RawMessage(message))
	}

	mergeFields := copyResponsesRequestFieldsWithInput(requestFields, input)
	summary, resp, err := h.compactResponsesRequestDepth(ctx, mergeFields, extraHeaders, depth, targetBodySize, budget)
	if err != nil {
		return "", err
	}
	if resp != nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, compactUpstreamErrorBodySize))
		_ = resp.Body.Close()
		return "", fmt.Errorf("compact summary merge returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return summary, nil
}

func (h *ProxyHandler) rewriteResponsesRequestBody(bodyBytes []byte, endpoint string, injectResumePrompt bool) []byte {
	requestedModel := extractResponsesRequestModel(bodyBytes)
	provider, _, _ := h.resolveProviderModel(requestedModel, "/responses")

	if rewrittenBody, strippedFields := stripUnsupportedResponsesRequestFields(bodyBytes, provider); len(strippedFields) > 0 {
		bodyBytes = rewrittenBody
		h.log.Debug("stripped unsupported responses request fields",
			logger.F("endpoint", endpoint),
			logger.F("fields", strippedFields),
		)
	}

	if rewrittenBody, rewriteCount := rewriteSyntheticCompactionRequest(bodyBytes); rewriteCount > 0 {
		bodyBytes = rewrittenBody
		resumePromptInjected := false
		if injectResumePrompt {
			if resumedBody, injected := injectSyntheticCompactionResumePrompt(bodyBytes); injected {
				bodyBytes = resumedBody
				resumePromptInjected = true
			}
		}

		fields := []logger.Field{
			logger.F("endpoint", endpoint),
			logger.F("count", rewriteCount),
		}
		if injectResumePrompt {
			fields = append(fields, logger.F("resume_prompt_injected", resumePromptInjected))
		}
		h.log.Debug("rewrote compaction items", fields...)
	}

	return bodyBytes
}

func stripUnsupportedResponsesRequestFields(bodyBytes []byte, provider *providerRuntime) ([]byte, []string) {
	if provider == nil {
		return bodyBytes, nil
	}

	unsupportedToolTypes := unsupportedResponsesToolTypes(provider)
	if len(unsupportedToolTypes) == 0 {
		return bodyBytes, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return bodyBytes, nil
	}

	rawTools, ok := payload["tools"]
	if !ok {
		return bodyBytes, nil
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return bodyBytes, nil
	}

	filteredTools := make([]json.RawMessage, 0, len(tools))
	strippedFields := make([]string, 0, len(tools)+1)
	strippedToolTypes := make(map[string]struct{})
	for i, rawTool := range tools {
		toolType := responsesToolType(rawTool)
		if _, unsupported := unsupportedToolTypes[toolType]; unsupported {
			strippedFields = append(strippedFields, fmt.Sprintf("tools[%d]", i))
			strippedToolTypes[toolType] = struct{}{}
			continue
		}
		filteredTools = append(filteredTools, rawTool)
	}

	if len(strippedFields) == 0 {
		return bodyBytes, nil
	}

	rewrittenTools, err := json.Marshal(filteredTools)
	if err != nil {
		return bodyBytes, nil
	}
	payload["tools"] = rewrittenTools

	if rawToolChoice, ok := payload["tool_choice"]; ok {
		if _, stripped := stripUnsupportedResponsesToolChoice(rawToolChoice, len(filteredTools) == 0, strippedToolTypes); stripped {
			delete(payload, "tool_choice")
			strippedFields = append(strippedFields, "tool_choice")
		}
	}

	rewrittenBody, err := json.Marshal(payload)
	if err != nil {
		return bodyBytes, nil
	}

	return rewrittenBody, strippedFields
}

func unsupportedResponsesToolTypes(provider *providerRuntime) map[string]struct{} {
	if provider == nil {
		return nil
	}

	switch provider.kind {
	case providerTypeCopilot, providerTypeAzureOpenAI:
		return map[string]struct{}{
			"image_generation": {},
		}
	default:
		return nil
	}
}

func responsesToolType(rawTool json.RawMessage) string {
	var tool struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawTool, &tool); err != nil {
		return ""
	}
	return strings.TrimSpace(tool.Type)
}

func stripUnsupportedResponsesToolChoice(rawToolChoice json.RawMessage, noRemainingTools bool, strippedToolTypes map[string]struct{}) (json.RawMessage, bool) {
	if noRemainingTools {
		return nil, true
	}

	var toolChoiceString string
	if err := json.Unmarshal(rawToolChoice, &toolChoiceString); err == nil {
		return rawToolChoice, false
	}

	toolType := responsesToolType(rawToolChoice)
	if _, unsupported := strippedToolTypes[toolType]; unsupported {
		return nil, true
	}

	return rawToolChoice, false
}

func (h *ProxyHandler) postResponsesWithFallbackHeaders(ctx context.Context, bodyBytes []byte, extraHeaders http.Header) (*http.Response, error) {
	resp, _, err := h.postResponsesWithFallbackHeadersTracked(ctx, bodyBytes, extraHeaders)
	return resp, err
}

// postResponsesWithFallbackHeadersTracked behaves like
// postResponsesWithFallbackHeaders, but also returns the model the request was
// ultimately served by — empty unless the model-fallback path engaged. The
// compact fanout uses this to memoize the resolved fallback so siblings don't
// each re-pay the unsupported-model probe.
func (h *ProxyHandler) postResponsesWithFallbackHeadersTracked(ctx context.Context, bodyBytes []byte, extraHeaders http.Header) (*http.Response, string, error) {
	resp, err := h.postResponsesWithHeaders(ctx, bodyBytes, extraHeaders)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, "", nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, "", err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	if !isUnsupportedResponsesModelError(resp.StatusCode, respBody) {
		return resp, "", nil
	}

	requestedModel := extractResponsesRequestModel(bodyBytes)
	provider, _, _ := h.resolveProviderModel(requestedModel, "/responses")
	fallbackModel, fallbackErr := h.pickResponsesCompatibleModel(ctx, provider, requestedModel)
	if fallbackErr != nil {
		h.log.Debug("responses fallback lookup failed", logger.Err(fallbackErr))
		return resp, "", nil
	}
	if fallbackModel == "" || fallbackModel == requestedModel {
		return resp, "", nil
	}

	fallbackBody, changed, fallbackErr := rewriteResponsesRequestModel(bodyBytes, fallbackModel)
	if fallbackErr != nil {
		h.log.Debug("responses fallback rewrite failed", logger.Err(fallbackErr))
		return resp, "", nil
	}
	if !changed {
		return resp, "", nil
	}

	h.log.Info("retrying responses request with fallback model",
		logger.F("requested_model", requestedModel),
		logger.F("fallback_model", fallbackModel),
	)

	retryResp, retryErr := h.postResponsesWithHeaders(ctx, fallbackBody, extraHeaders)
	if retryErr != nil {
		h.log.Debug("responses fallback request failed", logger.Err(retryErr))
		return resp, "", nil
	}

	return retryResp, fallbackModel, nil
}

// postResponsesCompactWithFallback wraps the model-fallback POST so that the
// resolved fallback model is captured on the shared compact budget. Subsequent
// compact calls in the same fanout pre-rewrite their body via
// applyResolvedCompactModel and skip the unsupported-model probe entirely.
func (h *ProxyHandler) postResponsesCompactWithFallback(ctx context.Context, bodyBytes []byte, extraHeaders http.Header, budget *compactBudget) (*http.Response, error) {
	resp, fallbackModel, err := h.postResponsesWithFallbackHeadersTracked(ctx, bodyBytes, extraHeaders)
	if err != nil {
		return nil, err
	}
	if fallbackModel != "" {
		budget.recordResolvedModel(fallbackModel)
	}
	return resp, nil
}

// applyResolvedCompactModel rewrites bodyBytes' "model" field to the budget's
// resolved fallback model when one has been recorded. Failures fall back to
// the original body so a malformed request still gets the prior fallback path.
func applyResolvedCompactModel(bodyBytes []byte, budget *compactBudget) []byte {
	if budget == nil || budget.resolvedModel == "" {
		return bodyBytes
	}
	current := extractResponsesRequestModel(bodyBytes)
	if current == "" || current == budget.resolvedModel {
		return bodyBytes
	}
	rewritten, changed, err := rewriteResponsesRequestModel(bodyBytes, budget.resolvedModel)
	if err != nil || !changed {
		return bodyBytes
	}
	return rewritten
}

func (h *ProxyHandler) maybeRetryCompactedResponsesRequest(ctx context.Context, bodyBytes []byte, extraHeaders http.Header, resp *http.Response) (*http.Response, error) {
	if resp == nil || resp.StatusCode != http.StatusRequestEntityTooLarge {
		return resp, nil
	}

	var requestFields map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &requestFields); err != nil {
		return resp, nil
	}

	var previousResponseID string
	if err := json.Unmarshal(requestFields["previous_response_id"], &previousResponseID); err != nil || strings.TrimSpace(previousResponseID) == "" {
		return resp, nil
	}

	var model string
	if err := json.Unmarshal(requestFields["model"], &model); err != nil || strings.TrimSpace(model) == "" {
		return resp, nil
	}

	var input []json.RawMessage
	if err := json.Unmarshal(requestFields["input"], &input); err != nil {
		return resp, nil
	}

	keepTail := h.responsesWebSocketConfig().AutoCompactKeepTail
	if keepTail <= 0 || len(input) <= keepTail {
		return resp, nil
	}

	prefixLen := len(input) - keepTail
	summary, err := h.compactResponsesInput(ctx, model, input[:prefixLen], extraHeaders)
	if err != nil {
		h.log.Debug("responses 413 compaction failed", logger.Err(err))
		return resp, nil
	}

	checkpoint, err := proxyCompactionContextRawMessage(summary)
	if err != nil {
		h.log.Debug("responses 413 compaction checkpoint build failed", logger.Err(err))
		return resp, nil
	}

	compactedInput := make([]json.RawMessage, 0, keepTail+1)
	compactedInput = append(compactedInput, checkpoint)
	compactedInput = append(compactedInput, input[prefixLen:]...)

	compactedInputRaw, err := json.Marshal(compactedInput)
	if err != nil {
		h.log.Debug("responses 413 compaction marshal failed", logger.Err(err))
		return resp, nil
	}
	requestFields["input"] = compactedInputRaw

	retryBody, err := json.Marshal(requestFields)
	if err != nil {
		h.log.Debug("responses 413 retry body marshal failed", logger.Err(err))
		return resp, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	h.log.Info("retrying responses request with compacted history after 413",
		logger.F("model", model),
		logger.F("previous_response_id", previousResponseID),
		logger.F("original_items", len(input)),
		logger.F("compacted_items", len(compactedInput)),
		logger.F("original_bytes", rawMessagesSize(input)),
		logger.F("compacted_bytes", rawMessagesSize(compactedInput)),
	)

	retryResp, retryErr := h.postResponsesWithHeaders(ctx, retryBody, extraHeaders)
	if retryErr != nil {
		h.log.Debug("responses 413 retry request failed", logger.Err(retryErr))
		return resp, nil
	}

	return retryResp, nil
}

func (h *ProxyHandler) compactResponsesInput(ctx context.Context, model string, input []json.RawMessage, extraHeaders http.Header) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("missing model for websocket compaction")
	}

	modelRaw, err := json.Marshal(model)
	if err != nil {
		return "", err
	}
	inputRaw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}

	requestFields := map[string]json.RawMessage{
		"model": modelRaw,
		"input": inputRaw,
	}
	summary, resp, err := h.compactResponsesRequest(ctx, requestFields, extraHeaders)
	if err != nil {
		return "", err
	}
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, compactUpstreamErrorBodySize))
		return "", fmt.Errorf("compaction request returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return summary, nil
}

func isUnsupportedResponsesModelError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}

	switch envelope.Error.Code {
	case "model_not_supported", "unsupported_api_for_model":
		return true
	}

	message := strings.ToLower(envelope.Error.Message)
	return envelope.Error.Param == "model" &&
		strings.Contains(message, "model") &&
		strings.Contains(message, "not supported")
}

func extractResponsesRequestModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

func rewriteResponsesRequestModel(body []byte, model string) ([]byte, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, err
	}

	current := extractResponsesRequestModel(body)
	if current == model {
		return body, false, nil
	}

	rawModel, err := json.Marshal(model)
	if err != nil {
		return nil, false, err
	}
	payload["model"] = rawModel

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}

func (h *ProxyHandler) pickResponsesCompatibleModel(ctx context.Context, provider *providerRuntime, exclude string) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("provider is required")
	}

	result, err := h.fetchProviderModels(ctx, provider, "", "")
	if err != nil {
		return "", err
	}

	supported := make(map[string]struct{})
	firstAvailable := ""
	for _, model := range filterProviderModels(provider, result.models) {
		if model.publicID == "" || model.publicID == exclude {
			continue
		}
		if !providerModelSupportsEndpoint(model, "/responses") {
			continue
		}
		if model.disabled {
			continue
		}
		supported[model.publicID] = struct{}{}
		if firstAvailable == "" {
			firstAvailable = model.publicID
		}
	}

	for _, preferred := range preferredResponsesFallbackModels {
		if _, ok := supported[preferred]; ok {
			return preferred, nil
		}
	}

	return firstAvailable, nil
}
