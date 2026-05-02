package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	obs := h.newRequestObservation(metricsEndpointResponses)
	statusCode := http.StatusOK
	defer func() {
		obs.finish(statusCode)
	}()

	bodyBytes, err := readBody(r)
	if err != nil {
		statusCode = readBodyStatusCode(err)
		writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()
	obs.bindProxyRequest(h, extractResponsesRequestModel(bodyBytes), "/responses")

	bodyBytes = h.rewriteResponsesRequestBody(bodyBytes, "responses", true)

	var partial struct {
		Stream *bool `json:"stream,omitempty"`
	}
	_ = json.Unmarshal(bodyBytes, &partial)
	isStreaming := partial.Stream != nil && *partial.Stream

	extraHeaders := responsesExtraHeadersFromRequest(r)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(isStreaming)
	upstreamCtx = obs.withContext(upstreamCtx)
	defer upstreamCancel()

	resp, err := h.postResponsesWithHeaders(upstreamCtx, bodyBytes, extraHeaders)
	if err != nil {
		statusCode = upstreamStatusCode(err, http.StatusBadGateway)
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
		statusCode = upstreamStatusCode(err, http.StatusBadGateway)
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
		statusCode = peekAndForwardResponsesObserved(h, w, r, resp, upstreamCancel, model, obs)
		return
	}

	if err := writeUpstreamResponseAndObserveResponsesUsage(w, resp, obs); err != nil {
		statusCode = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}
	statusCode = resp.StatusCode
}

// compactPrompt is the system instruction used when the upstream does not
// support the /responses/compact endpoint natively. The proxy converts the
// compact request into a regular /responses call with this prompt so the
// model produces a summarized handoff. The resulting compaction item is a
// proxy-owned opaque token rather than a real upstream-encrypted payload.
// compactUpstreamChunkBodySize is measured against the serialized upstream
// request body size, not the model-visible token budget.
const (
	compactUpstreamChunkBodySize = 8 << 20
	// compactUpstreamErrorBodySize caps upstream error bodies that the compact
	// fallback buffers only so it can replay the original failure if chunking fails.
	compactUpstreamErrorBodySize = 1 << 20
)

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
	obs := h.newRequestObservation(metricsEndpointResponsesCompact)
	statusCode := http.StatusOK
	defer func() {
		obs.finish(statusCode)
	}()

	bodyBytes, err := readBodyWithLimit(r, maxLargeRequestBodySize)
	if err != nil {
		statusCode = readBodyStatusCode(err)
		writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()
	obs.bindProxyRequest(h, extractResponsesRequestModel(bodyBytes), "/responses")

	bodyBytes = h.rewriteResponsesRequestBody(bodyBytes, "responses/compact", false)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		statusCode = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	upstreamCtx = obs.withContext(upstreamCtx)
	defer upstreamCancel()

	summaryText, resp, err := h.compactResponsesRequest(upstreamCtx, body, responsesExtraHeadersFromRequest(r))
	if err != nil {
		statusCode = upstreamStatusCode(err, http.StatusBadGateway)
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
		if err := writeUpstreamResponseAndObserveResponsesUsage(w, resp, obs); err != nil {
			statusCode = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
		statusCode = resp.StatusCode
		return
	}

	writeCompactResponse(w, summaryText)
	statusCode = http.StatusOK
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
	obs := h.newRequestObservation(metricsEndpointMemoryTraceSummarize)
	statusCode := http.StatusOK
	defer func() {
		obs.finish(statusCode)
	}()

	bodyBytes, err := readBodyWithLimit(r, maxLargeRequestBodySize)
	if err != nil {
		statusCode = readBodyStatusCode(err)
		writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	var memReq struct {
		Model     string            `json:"model"`
		Traces    []json.RawMessage `json:"traces"`
		Reasoning json.RawMessage   `json:"reasoning,omitempty"`
	}
	if err := json.Unmarshal(bodyBytes, &memReq); err != nil {
		statusCode = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	obs.bindProxyRequest(h, memReq.Model, "/responses")

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
	upstreamCtx = obs.withContext(upstreamCtx)
	defer upstreamCancel()

	resp, err := h.postResponsesWithFallbackHeaders(upstreamCtx, reqBody, responsesExtraHeadersFromRequest(r))
	if err != nil {
		statusCode = upstreamStatusCode(err, http.StatusBadGateway)
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
		statusCode = resp.StatusCode
		if err := writeUpstreamResponseAndObserveResponsesUsage(w, resp, obs); err != nil {
			statusCode = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		statusCode = http.StatusBadGateway
		writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}
	obs.observeResponsesUsageFromBody(respBody)

	summaryText, err := extractResponsesOutputText(respBody)
	if err != nil {
		statusCode = http.StatusBadGateway
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
	return h.compactResponsesRequestDepth(ctx, requestFields, extraHeaders, 0)
}

func (h *ProxyHandler) compactResponsesRequestDepth(ctx context.Context, requestFields map[string]json.RawMessage, extraHeaders http.Header, depth int) (string, *http.Response, error) {
	if depth > 8 {
		return "", nil, fmt.Errorf("compaction chunk recursion limit exceeded")
	}

	bodyBytes, err := marshalCompactResponsesRequest(requestFields, nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := h.postResponsesWithFallbackHeaders(ctx, bodyBytes, extraHeaders)
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

	summary, err := h.compactResponsesRequestInChunks(ctx, requestFields, extraHeaders, depth+1)
	if err != nil {
		h.log.Debug("chunked compact request failed", logger.Err(err))
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

func (h *ProxyHandler) compactResponsesRequestInChunks(ctx context.Context, requestFields map[string]json.RawMessage, extraHeaders http.Header, depth int) (string, error) {
	var input []json.RawMessage
	if err := json.Unmarshal(requestFields["input"], &input); err != nil {
		return "", err
	}
	if len(input) < 2 {
		return "", fmt.Errorf("compact request input cannot be split")
	}

	chunks, err := splitCompactInputByBodySize(requestFields, input, compactUpstreamChunkBodySize)
	if err != nil {
		return "", err
	}
	if len(chunks) < 2 {
		return "", fmt.Errorf("compact request input cannot be split below upstream payload limit")
	}

	h.log.Info("retrying compact request with chunked history after 413",
		logger.F("original_items", len(input)),
		logger.F("chunks", len(chunks)),
		logger.F("original_bytes", rawMessagesSize(input)),
	)

	summaries := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		chunkFields := copyResponsesRequestFieldsWithInput(requestFields, chunk)
		summary, resp, err := h.compactResponsesRequestDepth(ctx, chunkFields, extraHeaders, depth)
		if err != nil {
			return "", err
		}
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, compactUpstreamErrorBodySize))
			_ = resp.Body.Close()
			return "", fmt.Errorf("compact chunk %d returned %d: %s", i+1, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		summaries = append(summaries, summary)
	}

	return h.mergeCompactionSummaries(ctx, requestFields, summaries, extraHeaders, depth)
}

func copyResponsesRequestFieldsWithInput(requestFields map[string]json.RawMessage, input []json.RawMessage) map[string]json.RawMessage {
	copied := make(map[string]json.RawMessage, len(requestFields)+1)
	for key, value := range requestFields {
		copied[key] = value
	}
	inputRaw, err := json.Marshal(input)
	if err == nil {
		copied["input"] = inputRaw
	}
	return copied
}

func splitCompactInputByBodySize(requestFields map[string]json.RawMessage, input []json.RawMessage, maxBodySize int) ([][]json.RawMessage, error) {
	if maxBodySize <= 0 {
		return nil, fmt.Errorf("invalid compact chunk size %d", maxBodySize)
	}

	emptyBody, err := marshalCompactResponsesRequest(requestFields, []json.RawMessage{})
	if err != nil {
		return nil, err
	}
	// The rest of the compact request is stable while splitting. Track only the
	// encoded JSON array size for input so each item is marshaled once instead of
	// re-marshaling the whole candidate body for every append.
	fixedBodySize := len(emptyBody) - len("[]")

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

func encodedRawMessageSize(raw json.RawMessage) (int, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}

func (h *ProxyHandler) mergeCompactionSummaries(ctx context.Context, requestFields map[string]json.RawMessage, summaries []string, extraHeaders http.Header, depth int) (string, error) {
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
	summary, resp, err := h.compactResponsesRequestDepth(ctx, mergeFields, extraHeaders, depth)
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
	resp, err := h.postResponsesWithHeaders(ctx, bodyBytes, extraHeaders)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	if !isUnsupportedResponsesModelError(resp.StatusCode, respBody) {
		return resp, nil
	}

	requestedModel := extractResponsesRequestModel(bodyBytes)
	provider, _, _ := h.resolveProviderModel(requestedModel, "/responses")
	fallbackModel, fallbackErr := h.pickResponsesCompatibleModel(ctx, provider, requestedModel)
	if fallbackErr != nil {
		h.log.Debug("responses fallback lookup failed", logger.Err(fallbackErr))
		return resp, nil
	}
	if fallbackModel == "" || fallbackModel == requestedModel {
		return resp, nil
	}

	fallbackBody, changed, fallbackErr := rewriteResponsesRequestModel(bodyBytes, fallbackModel)
	if fallbackErr != nil {
		h.log.Debug("responses fallback rewrite failed", logger.Err(fallbackErr))
		return resp, nil
	}
	if !changed {
		return resp, nil
	}

	h.log.Info("retrying responses request with fallback model",
		logger.F("requested_model", requestedModel),
		logger.F("fallback_model", fallbackModel),
	)

	retryResp, retryErr := h.postResponsesWithHeaders(ctx, fallbackBody, extraHeaders)
	if retryErr != nil {
		h.log.Debug("responses fallback request failed", logger.Err(retryErr))
		return resp, nil
	}

	return retryResp, nil
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
