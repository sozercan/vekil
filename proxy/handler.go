// Package proxy implements HTTP handlers that forward requests to GitHub
// Copilot's backend. It provides Anthropic-to-OpenAI translation for the
// /v1/messages endpoint and near zero-copy passthrough for OpenAI endpoints.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/models"
)

const (
	// maxRequestBodySize is the maximum allowed request body size (10MB).
	maxRequestBodySize = 10 << 20
	// upstreamTimeout is the timeout for LLM inference requests.
	upstreamTimeout = 5 * time.Minute
	// modelsUpstreamTimeout is the timeout for the /models metadata request.
	modelsUpstreamTimeout = 30 * time.Second
	// modelsCacheTTL is how long the /models response is cached.
	modelsCacheTTL = 5 * time.Minute
)

// modelsCache holds a cached /models response to avoid repeated upstream calls.
type modelsCache struct {
	mu         sync.RWMutex
	body       []byte
	statusCode int
	expiry     time.Time
}

// ProxyHandler holds dependencies for all HTTP handlers.
type ProxyHandler struct {
	auth           *auth.Authenticator
	client         *http.Client
	copilotURL     string
	log            *logger.Logger
	maxRetries     int
	retryBaseDelay time.Duration
	models         modelsCache
}

// NewProxyHandler creates a ProxyHandler with connection pooling and HTTP/2.
func NewProxyHandler(a *auth.Authenticator, log *logger.Logger) *ProxyHandler {
	return &ProxyHandler{
		auth: a,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		copilotURL: "https://api.githubcopilot.com",
		log:        log,
	}
}

func setCopilotHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("editor-version", "vscode/1.95.0")
	req.Header.Set("editor-plugin-version", "copilot-chat/0.26.7")
	req.Header.Set("user-agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("copilot-integration-id", "vscode-chat")
	req.Header.Set("x-github-api-version", "2025-04-01")
	req.Header.Set("x-request-id", uuid.New().String())
	req.Header.Set("openai-intent", "conversation-panel")
	req.Header.Set("Content-Type", "application/json")
}

// HandleAnthropicMessages handles POST /v1/messages by translating the Anthropic
// request to OpenAI format, forwarding to Copilot, and translating the response back.
func (h *ProxyHandler) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if len(body) == 0 {
			status = http.StatusRequestEntityTooLarge
		}
		writeAnthropicError(w, status, "invalid_request_error", err.Error())
		return
	}
	defer r.Body.Close()

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON in request body")
		return
	}

	h.log.Debug("anthropic request",
		logger.F("model", req.Model),
		logger.F("stream", req.Stream),
		logger.F("messages", len(req.Messages)),
		logger.F("tools", len(req.Tools)),
	)

	oaiReq, err := TranslateAnthropicToOpenAI(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}

	// Force streaming to upstream for non-streaming requests to ensure
	// reliable parallel tool call support. The upstream may not return
	// parallel tool calls in non-streaming mode.
	forceStream := !req.Stream
	if forceStream {
		b := true
		oaiReq.Stream = &b
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("failed to get token: %v", err))
		return
	}

	oaiBody, err := json.Marshal(oaiReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to marshal request")
		return
	}

	// Use background context with timeout to avoid cancellation from client
	// disconnects while still preventing goroutine leaks on upstream hangs.
	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, h.copilotURL+"/chat/completions", bytes.NewReader(oaiBody))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeAnthropicError(w, status, "api_error", fmt.Sprintf("upstream request failed: %v", err))
		return
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		h.log.Error("upstream error", logger.F("endpoint", "anthropic"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)), logger.F("request", string(oaiBody)))
		writeAnthropicError(w, resp.StatusCode, "api_error", fmt.Sprintf("upstream error (%d): %s", resp.StatusCode, string(errBody)))
		return
	}

	if req.Stream {
		StreamOpenAIToAnthropic(w, resp.Body, req.Model, "msg_"+uuid.New().String())
		return
	}

	// Non-streaming: aggregate the forced-streaming upstream response
	oaiResp, err := aggregateStreamToResponse(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to aggregate upstream response")
		return
	}

	anthropicResp := TranslateOpenAIToAnthropic(oaiResp, req.Model)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropicResp)
}

// HandleOpenAIChatCompletions handles POST /v1/chat/completions by forwarding the
// request to Copilot with only auth headers injected (near zero-copy passthrough).
func (h *ProxyHandler) HandleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if len(bodyBytes) == 0 {
			status = http.StatusRequestEntityTooLarge
		}
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer r.Body.Close()

	// Detect if the client requested streaming and if tools are present
	var partial struct {
		Stream *bool           `json:"stream,omitempty"`
		Tools  json.RawMessage `json:"tools,omitempty"`
	}
	json.Unmarshal(bodyBytes, &partial)
	isStreaming := partial.Stream != nil && *partial.Stream
	hasTools := len(partial.Tools) > 0 && string(partial.Tools) != "null"

	// Inject parallel_tool_calls: true when tools are present
	bodyBytes = injectParallelToolCalls(bodyBytes)

	// Force streaming to upstream for non-streaming requests with tools
	// to ensure reliable parallel tool call support.
	forceStream := !isStreaming && hasTools
	if forceStream {
		bodyBytes = injectForceStream(bodyBytes)
	}

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, h.copilotURL+"/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeOpenAIError(w, status, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	if isStreaming && resp.StatusCode == http.StatusOK {
		StreamOpenAIPassthrough(w, resp.Body)
		return
	}

	if forceStream && resp.StatusCode == http.StatusOK {
		// Aggregate forced-streaming response back to non-streaming
		oaiResp, err := aggregateStreamToResponse(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to aggregate upstream response", "server_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(oaiResp)
		return
	}

	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

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
		status := http.StatusBadRequest
		if len(bodyBytes) == 0 {
			status = http.StatusRequestEntityTooLarge
		}
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer r.Body.Close()

	// Detect if the client requested streaming
	var partial struct {
		Stream *bool `json:"stream,omitempty"`
	}
	json.Unmarshal(bodyBytes, &partial)
	isStreaming := partial.Stream != nil && *partial.Stream

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, h.copilotURL+"/responses", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeOpenAIError(w, status, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	if isStreaming && resp.StatusCode == http.StatusOK {
		StreamOpenAIPassthrough(w, resp.Body)
		return
	}

	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// compactPrompt is the system instruction used when the upstream does not
// support the /responses/compact endpoint natively.  The proxy converts
// the compact request into a regular /responses call with this prompt so
// the model produces a summarised conversation that Codex can resume from.
const compactPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`

// HandleCompact handles POST /v1/responses/compact by forwarding the request
// to the upstream /responses endpoint with a compaction system prompt injected.
// The upstream response is then transformed into the compact response format
// that Codex expects: {"output": [{"type": "compaction", "encrypted_content": "..."}]}.
func (h *ProxyHandler) HandleCompact(w http.ResponseWriter, r *http.Request) {
	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if len(bodyBytes) == 0 {
			status = http.StatusRequestEntityTooLarge
		}
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	// Append the compaction prompt to instructions so the model produces
	// a summary. Codex always sends its base instructions, so we append
	// rather than replace.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	var existing string
	if raw, ok := body["instructions"]; ok {
		_ = json.Unmarshal(raw, &existing)
	}
	combined, _ := json.Marshal(existing + "\n\n" + compactPrompt)
	body["instructions"] = combined
	bodyBytes, _ = json.Marshal(body)

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, h.copilotURL+"/responses", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeOpenAIError(w, status, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Parse the upstream /responses response and transform it into the
	// compact format Codex expects.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}

	var upstream struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(respBody, &upstream); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to parse upstream response", "server_error")
		return
	}

	// Extract text from the assistant's output to use as the compaction summary.
	var summaryText string
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
		if outputItem.Type == "message" {
			for _, c := range outputItem.Content {
				if (c.Type == "output_text" || c.Type == "text") && c.Text != "" {
					summaryText += c.Text
				}
			}
		}
	}

	// Build the compact response format Codex expects.
	type compactionItem struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}
	compactResp := struct {
		Output []compactionItem `json:"output"`
	}{
		Output: []compactionItem{
			{
				Type:             "compaction",
				EncryptedContent: summaryText,
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
		status := http.StatusBadRequest
		if len(bodyBytes) == 0 {
			status = http.StatusRequestEntityTooLarge
		}
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	// Parse the memory summarize request.
	var memReq struct {
		Model      string            `json:"model"`
		Traces     []json.RawMessage `json:"traces"`
		Reasoning  json.RawMessage   `json:"reasoning,omitempty"`
	}
	if err := json.Unmarshal(bodyBytes, &memReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}

	// Build a /responses request with the traces as user input.
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
	reqBody, _ := json.Marshal(responsesReq)

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, h.copilotURL+"/responses", bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeOpenAIError(w, status, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Extract text from upstream response.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}

	var upstream struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(respBody, &upstream); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to parse upstream response", "server_error")
		return
	}

	var summaryText string
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
		if outputItem.Type == "message" {
			for _, c := range outputItem.Content {
				if (c.Type == "output_text" || c.Type == "text") && c.Text != "" {
					summaryText += c.Text
				}
			}
		}
	}

	// Try to parse the model's response as a JSON array of summaries.
	// Strip markdown code fences if present.
	cleaned := strings.TrimSpace(summaryText)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	type memorySummary struct {
		TraceSummary   string `json:"trace_summary"`
		MemorySummary  string `json:"memory_summary"`
	}
	var summaries []memorySummary
	if err := json.Unmarshal([]byte(cleaned), &summaries); err != nil {
		// Fallback: if the model didn't return valid JSON, use the raw text
		// as both summary fields for each trace.
		summaries = make([]memorySummary, len(memReq.Traces))
		for i := range summaries {
			summaries[i] = memorySummary{
				TraceSummary:  cleaned,
				MemorySummary: cleaned,
			}
		}
	}

	// Pad or trim to match the number of input traces.
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

// HandleHealthz handles GET /healthz and returns {"status":"ok"}.
func (h *ProxyHandler) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// HandleModels handles GET /v1/models by proxying to the upstream Copilot API.
// Responses are cached for modelsCacheTTL to avoid repeated upstream calls.
// The response includes both the standard OpenAI "data" field and a Codex-compatible
// "models" field so that both OpenAI SDK clients and Codex CLI can parse it.
func (h *ProxyHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	// Check cache first
	h.models.mu.RLock()
	if h.models.body != nil && time.Now().Before(h.models.expiry) {
		body := h.models.body
		status := h.models.statusCode
		h.models.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	h.models.mu.RUnlock()

	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelsUpstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.copilotURL+"/models", nil)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to create request", "server_error")
		return
	}
	setCopilotHeaders(req, token)

	resp, err := h.client.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
		return
	}

	// Transform the response to include a Codex-compatible "models" field.
	if resp.StatusCode == http.StatusOK {
		body = transformModelsResponse(body)
	}

	// Cache successful responses
	if resp.StatusCode == http.StatusOK {
		h.models.mu.Lock()
		h.models.body = body
		h.models.statusCode = resp.StatusCode
		h.models.expiry = time.Now().Add(modelsCacheTTL)
		h.models.mu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// transformModelsResponse adds a Codex-compatible "models" field to the
// upstream Copilot /models response. The original "data" and "object" fields
// are preserved for standard OpenAI SDK compatibility.
func transformModelsResponse(body []byte) []byte {
	var upstream struct {
		Data   []json.RawMessage `json:"data"`
		Object string            `json:"object"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil || len(upstream.Data) == 0 {
		return body
	}

	type reasoningPreset struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	}
	type truncationPolicy struct {
		Mode  string `json:"mode"`
		Limit int64  `json:"limit"`
	}
	type codexModel struct {
		Slug                      string            `json:"slug"`
		DisplayName               string            `json:"display_name"`
		Description               string            `json:"description"`
		DefaultReasoningLevel     *string           `json:"default_reasoning_level,omitempty"`
		SupportedReasoningLevels  []reasoningPreset `json:"supported_reasoning_levels"`
		ShellType                 string            `json:"shell_type"`
		Visibility                string            `json:"visibility"`
		SupportedInAPI            bool              `json:"supported_in_api"`
		Priority                  int               `json:"priority"`
		BaseInstructions          string            `json:"base_instructions"`
		SupportsReasoningSummaries bool             `json:"supports_reasoning_summaries"`
		SupportVerbosity          bool              `json:"support_verbosity"`
		TruncationPolicy          truncationPolicy  `json:"truncation_policy"`
		SupportsParallelToolCalls bool              `json:"supports_parallel_tool_calls"`
		SupportsImageDetailOriginal bool            `json:"supports_image_detail_original"`
		ContextWindow             *int64            `json:"context_window,omitempty"`
		ExperimentalSupportedTools []string         `json:"experimental_supported_tools"`
		InputModalities           []string          `json:"input_modalities"`
	}

	var codexModels []codexModel
	for _, raw := range upstream.Data {
		var m struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Capabilities struct {
				Limits struct {
					MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
				} `json:"limits"`
				Supports struct {
					ParallelToolCalls bool     `json:"parallel_tool_calls"`
					ReasoningEffort   []string `json:"reasoning_effort"`
					Vision            bool     `json:"vision"`
					ToolCalls         bool     `json:"tool_calls"`
				} `json:"supports"`
			} `json:"capabilities"`
			ModelPickerEnabled  bool   `json:"model_picker_enabled"`
			ModelPickerCategory string `json:"model_picker_category"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}

		visibility := "hide"
		if m.ModelPickerEnabled {
			visibility = "list"
		}

		var reasoningLevels []reasoningPreset
		var defaultReasoning *string
		for _, level := range m.Capabilities.Supports.ReasoningEffort {
			reasoningLevels = append(reasoningLevels, reasoningPreset{
				Effort:      level,
				Description: level,
			})
		}
		if len(reasoningLevels) > 0 {
			mid := "medium"
			defaultReasoning = &mid
		}

		var ctxWindow *int64
		if m.Capabilities.Limits.MaxContextWindowTokens > 0 {
			v := m.Capabilities.Limits.MaxContextWindowTokens
			ctxWindow = &v
		}

		modalities := []string{"text"}
		if m.Capabilities.Supports.Vision {
			modalities = append(modalities, "image")
		}

		priority := 10
		switch m.ModelPickerCategory {
		case "powerful":
			priority = 0
		case "versatile":
			priority = 5
		case "lightweight":
			priority = 8
		}

		cm := codexModel{
			Slug:                       m.ID,
			DisplayName:                m.Name,
			Description:                m.Name,
			DefaultReasoningLevel:      defaultReasoning,
			SupportedReasoningLevels:   reasoningLevels,
			ShellType:                  "shell_command",
			Visibility:                 visibility,
			SupportedInAPI:             true,
			Priority:                   priority,
			BaseInstructions:           "",
			SupportsReasoningSummaries: len(reasoningLevels) > 0,
			SupportVerbosity:           false,
			TruncationPolicy:           truncationPolicy{Mode: "bytes", Limit: 10000},
			SupportsParallelToolCalls:  m.Capabilities.Supports.ParallelToolCalls,
			SupportsImageDetailOriginal: false,
			ContextWindow:              ctxWindow,
			ExperimentalSupportedTools: []string{},
			InputModalities:            modalities,
		}
		codexModels = append(codexModels, cm)
	}

	// Build combined response with both "data" (OpenAI) and "models" (Codex).
	result := struct {
		Data   []json.RawMessage `json:"data"`
		Object string            `json:"object"`
		Models []codexModel      `json:"models"`
	}{
		Data:   upstream.Data,
		Object: upstream.Object,
		Models: codexModels,
	}

	out, err := json.Marshal(result)
	if err != nil {
		return body
	}
	return out
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.AnthropicError{
		Type: "error",
		Error: struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{
			Type:    errType,
			Message: message,
		},
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    nil,
		},
	})
}

// readBody reads the request body up to maxRequestBodySize. If the body exceeds
// the limit, it returns an error so callers can return HTTP 413.
func readBody(r *http.Request) ([]byte, error) {
	var reader io.Reader = r.Body

	// Decompress request body if Content-Encoding is set.
	// Some clients (e.g., Codex CLI) send compressed request bodies.
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "gzip":
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress gzip body: %w", err)
		}
		defer func() { _ = gr.Close() }()
		reader = gr
	case "zstd":
		zr, err := zstd.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress zstd body: %w", err)
		}
		defer zr.Close()
		reader = zr
	}

	body, err := io.ReadAll(io.LimitReader(reader, maxRequestBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBodySize {
		return nil, fmt.Errorf("request body too large (max %d bytes)", maxRequestBodySize)
	}
	return body, nil
}

// injectParallelToolCalls adds parallel_tool_calls: true to an OpenAI request
// body when tools are present but the flag is not already set.
func injectParallelToolCalls(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, hasTools := m["tools"]; !hasTools {
		return body
	}
	if _, hasPTC := m["parallel_tool_calls"]; hasPTC {
		return body
	}
	m["parallel_tool_calls"] = json.RawMessage("true")
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

// injectForceStream adds stream: true and stream_options to a request body
// for forced streaming to the upstream.
func injectForceStream(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = json.RawMessage("true")
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}
