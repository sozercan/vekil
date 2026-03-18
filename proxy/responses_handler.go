package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/copilot-proxy/logger"
)

// HandleResponses handles POST /v1/responses by forwarding the request to
// Copilot's responses endpoint with only auth headers injected.
func (h *ProxyHandler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	if rewrittenBody, rewriteCount := rewriteSyntheticCompactionRequest(bodyBytes); rewriteCount > 0 {
		bodyBytes = rewrittenBody
		resumePromptInjected := false
		if resumedBody, injected := injectSyntheticCompactionResumePrompt(bodyBytes); injected {
			bodyBytes = resumedBody
			resumePromptInjected = true
		}
		h.log.Debug("rewrote compaction items",
			logger.F("endpoint", "responses"),
			logger.F("count", rewriteCount),
			logger.F("resume_prompt_injected", resumePromptInjected),
		)
	}

	var partial struct {
		Stream *bool `json:"stream,omitempty"`
	}
	_ = json.Unmarshal(bodyBytes, &partial)
	isStreaming := partial.Stream != nil && *partial.Stream

	upstreamCtx, upstreamCancel := newInferenceUpstreamContext()
	defer upstreamCancel()

	resp, err := h.postResponses(upstreamCtx, token, bodyBytes)
	if err != nil {
		writeOpenAIError(w, upstreamStatusCode(err, http.StatusBadGateway), fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	if isStreaming && resp.StatusCode == http.StatusOK {
		copyPassthroughHeaders(w.Header(), resp.Header)
		StreamOpenAIPassthrough(w, resp.Body)
		return
	}

	writeUpstreamResponse(w, resp)
}

// compactPrompt is the system instruction used when the upstream does not
// support the /responses/compact endpoint natively. The proxy converts the
// compact request into a regular /responses call with this prompt so the
// model produces a summarized handoff. The resulting compaction item is a
// proxy-owned opaque token rather than a real upstream-encrypted payload.
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
	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	if rewrittenBody, rewriteCount := rewriteSyntheticCompactionRequest(bodyBytes); rewriteCount > 0 {
		bodyBytes = rewrittenBody
		h.log.Debug("rewrote compaction items",
			logger.F("endpoint", "responses/compact"),
			logger.F("count", rewriteCount),
		)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	prompt, _ := json.Marshal(compactPrompt)
	body["instructions"] = prompt
	bodyBytes, _ = json.Marshal(body)

	upstreamCtx, upstreamCancel := newInferenceUpstreamContext()
	defer upstreamCancel()

	resp, err := h.postResponsesWithFallback(upstreamCtx, token, bodyBytes)
	if err != nil {
		writeOpenAIError(w, upstreamStatusCode(err, http.StatusBadGateway), fmt.Sprintf("upstream request failed: %v", err), "server_error")
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
	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	bodyBytes, err := readBody(r)
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

	upstreamCtx, upstreamCancel := newInferenceUpstreamContext()
	defer upstreamCancel()

	resp, err := h.postResponsesWithFallback(upstreamCtx, token, reqBody)
	if err != nil {
		writeOpenAIError(w, upstreamStatusCode(err, http.StatusBadGateway), fmt.Sprintf("upstream request failed: %v", err), "server_error")
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

	var text string
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
				text += content.Text
			}
		}
	}

	return sanitizeProxySummaryText(text), nil
}

func (h *ProxyHandler) postResponsesWithFallback(ctx context.Context, token string, bodyBytes []byte) (*http.Response, error) {
	resp, err := h.postResponses(ctx, token, bodyBytes)
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
	fallbackModel, fallbackErr := h.pickResponsesCompatibleModel(ctx, token, requestedModel)
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

	retryResp, retryErr := h.postResponses(ctx, token, fallbackBody)
	if retryErr != nil {
		h.log.Debug("responses fallback request failed", logger.Err(retryErr))
		return resp, nil
	}

	return retryResp, nil
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

func (h *ProxyHandler) pickResponsesCompatibleModel(ctx context.Context, token, exclude string) (string, error) {
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.copilotURL+"/models", nil)
		if err != nil {
			return nil, err
		}
		h.setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected /models status %d: %s", resp.StatusCode, string(body))
	}

	var upstream struct {
		Data []struct {
			ID                 string   `json:"id"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			Policy             struct {
				State string `json:"state"`
			} `json:"policy"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&upstream); err != nil {
		return "", err
	}

	supported := make(map[string]struct{})
	firstAvailable := ""
	for _, model := range upstream.Data {
		if model.ID == "" || model.ID == exclude {
			continue
		}
		if !supportsEndpoint(model.SupportedEndpoints, "/responses") {
			continue
		}
		if strings.EqualFold(model.Policy.State, "disabled") {
			continue
		}
		supported[model.ID] = struct{}{}
		if firstAvailable == "" {
			firstAvailable = model.ID
		}
	}

	for _, preferred := range preferredResponsesFallbackModels {
		if _, ok := supported[preferred]; ok {
			return preferred, nil
		}
	}

	return firstAvailable, nil
}
