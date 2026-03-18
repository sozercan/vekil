// Package proxy implements HTTP handlers that forward requests to GitHub
// Copilot's backend. It provides Anthropic-to-OpenAI translation for the
// /v1/messages endpoint and near zero-copy passthrough for OpenAI endpoints.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	// readyzUpstreamTimeout bounds readiness probes that validate upstream reachability.
	readyzUpstreamTimeout = 10 * time.Second
	// modelsUpstreamTimeout is the timeout for the /models metadata request.
	modelsUpstreamTimeout = 30 * time.Second
	// modelsCacheTTL is how long the /models response is cached.
	modelsCacheTTL = 5 * time.Minute
	// syntheticCompactionPrefix marks proxy-owned compaction payloads so they
	// can be expanded back into normal context on subsequent /responses calls.
	syntheticCompactionPrefix         = "copilot-proxy.compaction.v1:"
	defaultCopilotEditorVersion       = "vscode/1.95.0"
	defaultCopilotEditorPluginVersion = "copilot-chat/0.26.7"
	defaultCopilotUserAgent           = "GitHubCopilotChat/0.26.7"
	defaultCopilotIntegrationID       = "vscode-chat"
	defaultCopilotGitHubAPIVersion    = "2025-04-01"
	defaultCopilotOpenAIIntent        = "conversation-panel"
)

var preferredResponsesFallbackModels = []string{
	"gpt-5.4",
	"gpt-5.3-codex",
	"gpt-5.2",
	"gpt-5.2-codex",
	"gpt-5.1",
	"gpt-5.1-codex",
	"gpt-5.1-codex-max",
	"gpt-5.1-codex-mini",
	"gpt-5-mini",
}

// modelsCache holds a cached /models response to avoid repeated upstream calls.
type modelsCache struct {
	mu      sync.RWMutex
	entries map[string]cachedModelsResponse
}

type cachedModelsResponse struct {
	body       []byte
	statusCode int
	expiry     time.Time
	etag       string
}

// CopilotHeaderConfig controls the synthetic editor-identifying headers sent to
// the upstream Copilot backend. Empty fields fall back to project defaults.
type CopilotHeaderConfig struct {
	EditorVersion       string
	EditorPluginVersion string
	UserAgent           string
	IntegrationID       string
	GitHubAPIVersion    string
	OpenAIIntent        string
}

func DefaultCopilotHeaderConfig() CopilotHeaderConfig {
	return CopilotHeaderConfig{
		EditorVersion:       defaultCopilotEditorVersion,
		EditorPluginVersion: defaultCopilotEditorPluginVersion,
		UserAgent:           defaultCopilotUserAgent,
		IntegrationID:       defaultCopilotIntegrationID,
		GitHubAPIVersion:    defaultCopilotGitHubAPIVersion,
		OpenAIIntent:        defaultCopilotOpenAIIntent,
	}
}

func (c CopilotHeaderConfig) withDefaults() CopilotHeaderConfig {
	defaults := DefaultCopilotHeaderConfig()
	if c.EditorVersion == "" {
		c.EditorVersion = defaults.EditorVersion
	}
	if c.EditorPluginVersion == "" {
		c.EditorPluginVersion = defaults.EditorPluginVersion
	}
	if c.UserAgent == "" {
		c.UserAgent = defaults.UserAgent
	}
	if c.IntegrationID == "" {
		c.IntegrationID = defaults.IntegrationID
	}
	if c.GitHubAPIVersion == "" {
		c.GitHubAPIVersion = defaults.GitHubAPIVersion
	}
	if c.OpenAIIntent == "" {
		c.OpenAIIntent = defaults.OpenAIIntent
	}
	return c
}

// ProxyHandler holds dependencies for all HTTP handlers.
type ProxyHandler struct {
	auth           *auth.Authenticator
	client         *http.Client
	copilotURL     string
	copilotHeaders CopilotHeaderConfig
	log            *logger.Logger
	maxRetries     int
	retryBaseDelay time.Duration
	models         modelsCache
	geminiCounts   geminiCountTokensCache
}

// Option customizes ProxyHandler behavior.
type Option func(*ProxyHandler)

// WithCopilotHeaderConfig overrides the synthetic Copilot-identifying headers
// used for upstream requests.
func WithCopilotHeaderConfig(cfg CopilotHeaderConfig) Option {
	return func(h *ProxyHandler) {
		h.copilotHeaders = cfg.withDefaults()
	}
}

// NewProxyHandler creates a ProxyHandler with connection pooling and HTTP/2.
func NewProxyHandler(a *auth.Authenticator, log *logger.Logger, opts ...Option) *ProxyHandler {
	h := &ProxyHandler{
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
		copilotURL:     "https://api.githubcopilot.com",
		copilotHeaders: DefaultCopilotHeaderConfig(),
		log:            log,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(h)
		}
	}
	return h
}

func setCopilotHeaders(req *http.Request, token string) {
	setCopilotHeadersWithConfig(req, token, DefaultCopilotHeaderConfig())
}

func setCopilotHeadersWithConfig(req *http.Request, token string, cfg CopilotHeaderConfig) {
	cfg = cfg.withDefaults()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("editor-version", cfg.EditorVersion)
	req.Header.Set("editor-plugin-version", cfg.EditorPluginVersion)
	req.Header.Set("user-agent", cfg.UserAgent)
	req.Header.Set("copilot-integration-id", cfg.IntegrationID)
	req.Header.Set("x-github-api-version", cfg.GitHubAPIVersion)
	req.Header.Set("x-request-id", uuid.New().String())
	req.Header.Set("openai-intent", cfg.OpenAIIntent)
	req.Header.Set("Content-Type", "application/json")
}

func (h *ProxyHandler) setCopilotHeaders(req *http.Request, token string) {
	setCopilotHeadersWithConfig(req, token, h.copilotHeaders)
}

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// Strip inline render markers that some upstream clients inject into plain text,
// such as citation tokens like "citeturn5view1". These are useful only to
// richer UIs, so proxy-owned summary surfaces should store clean text instead.
var inlineRenderMarkerRegexp = regexp.MustCompile(`[^]*`)

func sanitizeProxySummaryText(text string) string {
	if text == "" {
		return ""
	}
	return strings.TrimSpace(inlineRenderMarkerRegexp.ReplaceAllString(text, ""))
}

func copyPassthroughHeaders(dst, src http.Header) {
	connectionTokens := make(map[string]struct{})
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = http.CanonicalHeaderKey(strings.TrimSpace(token))
			if token != "" {
				connectionTokens[token] = struct{}{}
			}
		}
	}

	for key, values := range src {
		canonicalKey := http.CanonicalHeaderKey(key)
		if _, skip := hopByHopHeaders[canonicalKey]; skip {
			continue
		}
		if _, skip := connectionTokens[canonicalKey]; skip {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func writeCachedModelsResponse(w http.ResponseWriter, entry cachedModelsResponse) {
	w.Header().Set("Content-Type", "application/json")
	if entry.etag != "" {
		w.Header().Set("ETag", entry.etag)
	}
	w.WriteHeader(entry.statusCode)
	_, _ = w.Write(entry.body)
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
		h.setCopilotHeaders(req, token)
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
		h.setCopilotHeaders(req, token)
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
		copyPassthroughHeaders(w.Header(), resp.Header)
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
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
		h.setCopilotHeaders(req, token)
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
		copyPassthroughHeaders(w.Header(), resp.Header)
		StreamOpenAIPassthrough(w, resp.Body)
		return
	}

	defer resp.Body.Close()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
		status := http.StatusBadRequest
		if len(bodyBytes) == 0 {
			status = http.StatusRequestEntityTooLarge
		}
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

	// Replace instructions with the compaction prompt. The conversation
	// history in the input array already contains all context — the model
	// just needs to know its job is to produce a handoff summary, not
	// continue acting as a coding assistant.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body", "invalid_request_error")
		return
	}
	prompt, _ := json.Marshal(compactPrompt)
	body["instructions"] = prompt
	bodyBytes, _ = json.Marshal(body)

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.postResponsesWithFallback(upstreamCtx, token, bodyBytes)
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
	summaryText = sanitizeProxySummaryText(summaryText)

	// Build the compact response format Codex expects: an assistant message
	// with the summary text followed by a compaction item.
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
		Model     string            `json:"model"`
		Traces    []json.RawMessage `json:"traces"`
		Reasoning json.RawMessage   `json:"reasoning,omitempty"`
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
	if len(memReq.Reasoning) > 0 && string(memReq.Reasoning) != "null" {
		responsesReq["reasoning"] = json.RawMessage(memReq.Reasoning)
	}
	reqBody, _ := json.Marshal(responsesReq)

	upstreamCtx, upstreamCancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer upstreamCancel()

	resp, err := h.postResponsesWithFallback(upstreamCtx, token, reqBody)
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
	summaryText = sanitizeProxySummaryText(summaryText)

	// Try to parse the model's response as a JSON array of summaries.
	// Strip markdown code fences if present.
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

// HandleReadyz validates that the proxy can obtain an auth token and reach the
// upstream Copilot API.
func (h *ProxyHandler) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzUpstreamTimeout)
	defer cancel()

	token, err := h.auth.GetTokenNonInteractive(ctx)
	if err != nil {
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", fmt.Sprintf("failed to get token: %v", err))
		return
	}

	if err := h.checkUpstreamReady(ctx, token); err != nil {
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", err.Error())
		return
	}

	writeReadyzStatus(w, http.StatusOK, "ready", "")
}

func (h *ProxyHandler) checkUpstreamReady(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.copilotURL+"/models", nil)
	if err != nil {
		return fmt.Errorf("failed to create upstream probe request: %w", err)
	}
	h.setCopilotHeaders(req, token)

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("upstream probe failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := fmt.Sprintf("upstream probe returned %d", resp.StatusCode)
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			message += ": " + trimmed
		}
		return fmt.Errorf("%s", message)
	}

	return nil
}

func writeReadyzStatus(w http.ResponseWriter, statusCode int, status string, errMessage string) {
	response := map[string]string{"status": status}
	if errMessage != "" {
		response["error"] = errMessage
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(response)
}

// HandleModels handles GET /v1/models by proxying to the upstream Copilot API.
// Responses are cached for modelsCacheTTL to avoid repeated upstream calls.
// The response includes both the standard OpenAI "data" field and a Codex-compatible
// "models" field so that both OpenAI SDK clients and Codex CLI can parse it.
func (h *ProxyHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	cacheKey := r.URL.RawQuery
	now := time.Now()

	var cachedEntry cachedModelsResponse
	var hasCachedEntry bool
	h.models.mu.RLock()
	if h.models.entries != nil {
		cachedEntry, hasCachedEntry = h.models.entries[cacheKey]
	}
	h.models.mu.RUnlock()

	// Without an ETag we cannot safely revalidate, so honor the TTL-based cache.
	if hasCachedEntry && cachedEntry.etag == "" && now.Before(cachedEntry.expiry) {
		writeCachedModelsResponse(w, cachedEntry)
		return
	}

	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelsUpstreamTimeout)
	defer cancel()

	upstreamURL := h.copilotURL + "/models"
	if cacheKey != "" {
		upstreamURL += "?" + cacheKey
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to create request", "server_error")
		return
	}
	h.setCopilotHeaders(req, token)
	if hasCachedEntry && cachedEntry.etag != "" {
		req.Header.Set("If-None-Match", cachedEntry.etag)
	}

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

	if resp.StatusCode == http.StatusNotModified && hasCachedEntry {
		cachedEntry.expiry = time.Now().Add(modelsCacheTTL)
		h.models.mu.Lock()
		if h.models.entries == nil {
			h.models.entries = make(map[string]cachedModelsResponse)
		}
		h.models.entries[cacheKey] = cachedEntry
		h.models.mu.Unlock()

		writeCachedModelsResponse(w, cachedEntry)
		return
	}

	// Transform the response to include a Codex-compatible "models" field.
	if resp.StatusCode == http.StatusOK {
		body = transformModelsResponse(body)
	}

	// Cache successful responses
	if resp.StatusCode == http.StatusOK {
		entry := cachedModelsResponse{
			body:       body,
			statusCode: resp.StatusCode,
			expiry:     time.Now().Add(modelsCacheTTL),
			etag:       resp.Header.Get("ETag"),
		}
		h.models.mu.Lock()
		if h.models.entries == nil {
			h.models.entries = make(map[string]cachedModelsResponse)
		}
		h.models.entries[cacheKey] = entry
		h.models.mu.Unlock()
	}

	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
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
		Slug                        string            `json:"slug"`
		DisplayName                 string            `json:"display_name"`
		Description                 string            `json:"description"`
		DefaultReasoningLevel       *string           `json:"default_reasoning_level,omitempty"`
		SupportedReasoningLevels    []reasoningPreset `json:"supported_reasoning_levels"`
		ShellType                   string            `json:"shell_type"`
		Visibility                  string            `json:"visibility"`
		SupportedInAPI              bool              `json:"supported_in_api"`
		Priority                    int               `json:"priority"`
		BaseInstructions            string            `json:"base_instructions"`
		SupportsReasoningSummaries  bool              `json:"supports_reasoning_summaries"`
		SupportVerbosity            bool              `json:"support_verbosity"`
		TruncationPolicy            truncationPolicy  `json:"truncation_policy"`
		SupportsParallelToolCalls   bool              `json:"supports_parallel_tool_calls"`
		SupportsImageDetailOriginal bool              `json:"supports_image_detail_original"`
		ContextWindow               *int64            `json:"context_window,omitempty"`
		ExperimentalSupportedTools  []string          `json:"experimental_supported_tools"`
		InputModalities             []string          `json:"input_modalities"`
	}

	var codexModels []codexModel
	for _, raw := range upstream.Data {
		var m struct {
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			Capabilities       struct {
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

		supportsResponses := supportsEndpoint(m.SupportedEndpoints, "/responses")

		visibility := "hide"
		if m.ModelPickerEnabled && supportsResponses {
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
			Slug:                        m.ID,
			DisplayName:                 m.Name,
			Description:                 m.Name,
			DefaultReasoningLevel:       defaultReasoning,
			SupportedReasoningLevels:    reasoningLevels,
			ShellType:                   "shell_command",
			Visibility:                  visibility,
			SupportedInAPI:              supportsResponses,
			Priority:                    priority,
			BaseInstructions:            "",
			SupportsReasoningSummaries:  len(reasoningLevels) > 0,
			SupportVerbosity:            false,
			TruncationPolicy:            truncationPolicy{Mode: "bytes", Limit: 10000},
			SupportsParallelToolCalls:   m.Capabilities.Supports.ParallelToolCalls,
			SupportsImageDetailOriginal: false,
			ContextWindow:               ctxWindow,
			ExperimentalSupportedTools:  []string{},
			InputModalities:             modalities,
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

func supportsEndpoint(supportedEndpoints []string, endpoint string) bool {
	for _, candidate := range supportedEndpoints {
		if candidate == endpoint {
			return true
		}
	}
	return false
}

func (h *ProxyHandler) postResponses(ctx context.Context, token string, bodyBytes []byte) (*http.Response, error) {
	return h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.copilotURL+"/responses", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		h.setCopilotHeaders(req, token)
		return req, nil
	})
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

type syntheticCompactionPayload struct {
	Summary string `json:"summary"`
}

const proxyCompactionContextIntro = "You are resuming an interrupted assistant turn from a context checkpoint."

func encodeSyntheticCompaction(summary string) string {
	payload, err := json.Marshal(syntheticCompactionPayload{Summary: summary})
	if err != nil {
		return syntheticCompactionPrefix
	}
	return syntheticCompactionPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func rewriteSyntheticCompactionRequest(body []byte) ([]byte, int) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, 0
	}

	rawInput, ok := req["input"]
	if !ok {
		return body, 0
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return body, 0
	}

	rewrittenInput, rewriteCount := rewriteSyntheticCompactionValue(input)
	if rewriteCount == 0 {
		return body, 0
	}

	encodedInput, err := json.Marshal(rewrittenInput)
	if err != nil {
		return body, 0
	}
	req["input"] = encodedInput

	rewrittenBody, err := json.Marshal(req)
	if err != nil {
		return body, 0
	}
	return rewrittenBody, rewriteCount
}

// When a compacted checkpoint is restored without a remaining user turn, add a
// small synthetic user prompt so the upstream model resumes the interrupted
// task instead of replying with a generic "what should I work on next?".
func injectSyntheticCompactionResumePrompt(body []byte) ([]byte, bool) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}

	rawInput, ok := req["input"]
	if !ok {
		return body, false
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return body, false
	}

	inputItems, ok := input.([]interface{})
	if !ok {
		return body, false
	}
	if !shouldInjectSyntheticCompactionResumePrompt(inputItems) {
		return body, false
	}

	inputItems = append(inputItems, proxyCompactionResumeMessage())
	encodedInput, err := json.Marshal(inputItems)
	if err != nil {
		return body, false
	}
	req["input"] = encodedInput

	rewrittenBody, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return rewrittenBody, true
}

func shouldInjectSyntheticCompactionResumePrompt(inputItems []interface{}) bool {
	lastCheckpointIdx := -1
	for i, item := range inputItems {
		if isProxyCompactionContextMessage(item) {
			lastCheckpointIdx = i
		}
	}

	if lastCheckpointIdx == -1 {
		return !inputHasMessageRole(inputItems, "user")
	}

	for _, item := range inputItems[lastCheckpointIdx+1:] {
		if messageHasRole(item, "user") {
			return false
		}
	}
	return true
}

func rewriteSyntheticCompactionValue(v interface{}) (interface{}, int) {
	switch typed := v.(type) {
	case []interface{}:
		rewritten := make([]interface{}, 0, len(typed))
		total := 0
		for _, item := range typed {
			next, count := rewriteSyntheticCompactionValue(item)
			total += count
			rewritten = append(rewritten, next)
		}
		return rewritten, total

	case map[string]interface{}:
		if itemType, _ := typed["type"].(string); itemType == "compaction" {
			if encryptedContent, _ := typed["encrypted_content"].(string); encryptedContent != "" {
				if summary, ok := extractSyntheticOrLegacyCompactionSummary(encryptedContent); ok {
					return proxyCompactionContextMessage(summary), 1
				}
			}
		}

		rewritten := make(map[string]interface{}, len(typed))
		total := 0
		for key, value := range typed {
			next, count := rewriteSyntheticCompactionValue(value)
			total += count
			rewritten[key] = next
		}
		return rewritten, total
	default:
		return v, 0
	}
}

func inputHasMessageRole(v interface{}, role string) bool {
	switch typed := v.(type) {
	case []interface{}:
		for _, item := range typed {
			if inputHasMessageRole(item, role) {
				return true
			}
		}
	case map[string]interface{}:
		if itemType, _ := typed["type"].(string); itemType == "message" {
			if messageRole, _ := typed["role"].(string); messageRole == role {
				return true
			}
		}
		for _, value := range typed {
			if inputHasMessageRole(value, role) {
				return true
			}
		}
	}
	return false
}

func messageHasRole(v interface{}, role string) bool {
	typed, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	if itemType, _ := typed["type"].(string); itemType != "message" {
		return false
	}
	messageRole, _ := typed["role"].(string)
	return messageRole == role
}

func isProxyCompactionContextMessage(v interface{}) bool {
	typed, ok := v.(map[string]interface{})
	if !ok || !messageHasRole(v, "developer") {
		return false
	}

	content, ok := typed["content"].([]interface{})
	if !ok || len(content) == 0 {
		return false
	}

	firstPart, ok := content[0].(map[string]interface{})
	if !ok {
		return false
	}
	if partType, _ := firstPart["type"].(string); partType != "input_text" {
		return false
	}
	text, _ := firstPart["text"].(string)
	return strings.HasPrefix(text, proxyCompactionContextIntro)
}

func extractSyntheticOrLegacyCompactionSummary(encryptedContent string) (string, bool) {
	if strings.HasPrefix(encryptedContent, syntheticCompactionPrefix) {
		raw := strings.TrimPrefix(encryptedContent, syntheticCompactionPrefix)
		payloadBytes, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return "", false
		}

		var payload syntheticCompactionPayload
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return "", false
		}
		return payload.Summary, true
	}

	// Legacy fallback: older proxy versions wrote plaintext summaries directly
	// into encrypted_content. Real upstream tokens are opaque, space-free blobs.
	if encryptedContent != "" && (!looksOpaqueCompactionToken(encryptedContent) || strings.ContainsAny(encryptedContent, " \t\r\n")) {
		return encryptedContent, true
	}
	return "", false
}

func looksOpaqueCompactionToken(token string) bool {
	if len(token) < 32 {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '=', r == '.':
		default:
			return false
		}
	}
	return true
}

func proxyCompactionContextMessage(summary string) map[string]interface{} {
	summary = sanitizeProxySummaryText(summary)
	text := proxyCompactionContextIntro + " This checkpoint is the active working state for the same conversation, not passive background history.\n\nResume behavior:\n- Continue the same task immediately from this checkpoint.\n- Treat the checkpoint as authoritative for prior progress, constraints, and next steps.\n- Do not ask the user what to work on next unless the checkpoint explicitly says the assistant was blocked waiting for user input or that the task is complete.\n\nCheckpoint summary:\n" + summary
	return map[string]interface{}{
		"type": "message",
		"role": "developer",
		"content": []interface{}{
			map[string]interface{}{
				"type": "input_text",
				"text": text,
			},
		},
	}
}

func proxyCompactionResumeMessage() map[string]interface{} {
	return map[string]interface{}{
		"type": "message",
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type": "input_text",
				"text": "Continue from the checkpoint above and resume the interrupted task from the next unfinished step. Do not ask for a new assignment unless the checkpoint says you were blocked waiting for user input or the work is already complete.",
			},
		},
	}
}
