// Package proxy implements HTTP handlers that forward requests to GitHub
// Copilot's backend. It provides Anthropic-to-OpenAI translation for the
// /v1/messages endpoint and near zero-copy passthrough for OpenAI endpoints.
package proxy

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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
	// maxLargeRequestBodySize gives proxy-owned summarization endpoints a higher
	// ceiling because they can legitimately contain full session histories or
	// trace bundles that need to be summarized.
	maxLargeRequestBodySize = 64 << 20
	// upstreamTimeout is the timeout for non-streaming LLM inference requests.
	upstreamTimeout = 5 * time.Minute
	// streamingUpstreamTimeout gives streaming inference enough time to finish.
	streamingUpstreamTimeout = 60 * time.Minute
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
	defaultResponsesWSCompactMaxItems = 48
	defaultResponsesWSCompactMaxBytes = 256 << 10
	defaultResponsesWSCompactKeepTail = 12
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

type requestBodyError struct {
	statusCode int
	err        error
}

func (e *requestBodyError) Error() string {
	return e.err.Error()
}

func (e *requestBodyError) Unwrap() error {
	return e.err
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

// ResponsesWebSocketConfig controls websocket-session state management for
// Codex-style GET /v1/responses clients.
type ResponsesWebSocketConfig struct {
	TurnStateDelta      bool
	DisableAutoCompact  bool
	AutoCompactMaxItems int
	AutoCompactMaxBytes int
	AutoCompactKeepTail int
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

func DefaultResponsesWebSocketConfig() ResponsesWebSocketConfig {
	return ResponsesWebSocketConfig{
		AutoCompactMaxItems: defaultResponsesWSCompactMaxItems,
		AutoCompactMaxBytes: defaultResponsesWSCompactMaxBytes,
		AutoCompactKeepTail: defaultResponsesWSCompactKeepTail,
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

func (c ResponsesWebSocketConfig) withDefaults() ResponsesWebSocketConfig {
	defaults := DefaultResponsesWebSocketConfig()
	if c.AutoCompactMaxItems <= 0 {
		c.AutoCompactMaxItems = defaults.AutoCompactMaxItems
	}
	if c.AutoCompactMaxBytes <= 0 {
		c.AutoCompactMaxBytes = defaults.AutoCompactMaxBytes
	}
	if c.AutoCompactKeepTail <= 0 {
		c.AutoCompactKeepTail = defaults.AutoCompactKeepTail
	}
	return c
}

func (c ResponsesWebSocketConfig) autoCompactEnabled() bool {
	return !c.DisableAutoCompact &&
		c.AutoCompactKeepTail > 0 &&
		(c.AutoCompactMaxItems > 0 || c.AutoCompactMaxBytes > 0)
}

// ProxyHandler holds dependencies for all HTTP handlers.
type ProxyHandler struct {
	auth                     *auth.Authenticator
	client                   *http.Client
	copilotURL               string
	copilotHeaders           CopilotHeaderConfig
	responsesWS              ResponsesWebSocketConfig
	streamingUpstreamTimeout time.Duration
	log                      *logger.Logger
	maxRetries               int
	retryBaseDelay           time.Duration
	models                   modelsCache
	geminiCounts             geminiCountTokensCache
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

// WithResponsesWebSocketConfig overrides websocket-session state behavior for
// GET /v1/responses Codex clients.
func WithResponsesWebSocketConfig(cfg ResponsesWebSocketConfig) Option {
	return func(h *ProxyHandler) {
		h.responsesWS = cfg.withDefaults()
	}
}

func normalizeStreamingUpstreamTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return streamingUpstreamTimeout
	}
	return timeout
}

// DefaultStreamingUpstreamTimeout returns the default timeout used for
// streaming upstream inference requests.
func DefaultStreamingUpstreamTimeout() time.Duration {
	return streamingUpstreamTimeout
}

// WithStreamingUpstreamTimeout overrides the timeout used for streaming
// upstream inference requests. Non-positive values fall back to the default.
func WithStreamingUpstreamTimeout(timeout time.Duration) Option {
	return func(h *ProxyHandler) {
		h.streamingUpstreamTimeout = normalizeStreamingUpstreamTimeout(timeout)
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
		copilotURL:               "https://api.githubcopilot.com",
		copilotHeaders:           DefaultCopilotHeaderConfig(),
		responsesWS:              DefaultResponsesWebSocketConfig(),
		streamingUpstreamTimeout: streamingUpstreamTimeout,
		log:                      log,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(h)
		}
	}
	return h
}

func (h *ProxyHandler) responsesWebSocketConfig() ResponsesWebSocketConfig {
	return h.responsesWS.withDefaults()
}

func (h *ProxyHandler) effectiveStreamingUpstreamTimeout() time.Duration {
	if h == nil {
		return DefaultStreamingUpstreamTimeout()
	}
	return normalizeStreamingUpstreamTimeout(h.streamingUpstreamTimeout)
}

// ServerWriteTimeout returns the HTTP server write timeout derived from the
// configured streaming upstream timeout plus the non-streaming request budget.
func (h *ProxyHandler) ServerWriteTimeout() time.Duration {
	return h.effectiveStreamingUpstreamTimeout() + upstreamTimeout
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

	if err := ctx.Err(); err != nil {
		return
	}

	token, err := h.auth.GetTokenNonInteractive(ctx)
	if err != nil {
		if shouldSuppressReadyzResponse(r.Context(), err) {
			return
		}
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", fmt.Sprintf("failed to get token: %v", err))
		return
	}

	if err := h.checkUpstreamReady(ctx, token); err != nil {
		if shouldSuppressReadyzResponse(r.Context(), err) {
			return
		}
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

func shouldSuppressReadyzResponse(parent context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Only suppress deadline errors when the caller's context already timed out.
	// The proxy's own readiness timeout should still surface as not_ready.
	return parent.Err() != nil
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
	if err := json.Unmarshal(body, &upstream); err != nil {
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

	codexModels := make([]codexModel, 0, len(upstream.Data))
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
			defaultLevel := reasoningLevels[0].Effort
			for _, level := range reasoningLevels {
				if level.Effort == "medium" {
					defaultLevel = level.Effort
					break
				}
			}
			defaultReasoning = &defaultLevel
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
	return readBodyWithLimit(r, maxRequestBodySize)
}

// readBodyWithLimit reads and transparently decompresses the request body up to
// the provided limit. If the body exceeds the limit, it returns an error so
// callers can return HTTP 413.
func readBodyWithLimit(r *http.Request, limit int64) ([]byte, error) {
	var reader io.Reader = r.Body

	// Decompress request body if Content-Encoding is set.
	// Some clients (e.g., Codex CLI) send compressed request bodies.
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "gzip":
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, &requestBodyError{
				statusCode: http.StatusBadRequest,
				err:        fmt.Errorf("failed to decompress gzip body: %w", err),
			}
		}
		defer func() { _ = gr.Close() }()
		reader = gr
	case "zstd":
		zr, err := zstd.NewReader(r.Body)
		if err != nil {
			return nil, &requestBodyError{
				statusCode: http.StatusBadRequest,
				err:        fmt.Errorf("failed to decompress zstd body: %w", err),
			}
		}
		defer zr.Close()
		reader = zr
	}

	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, &requestBodyError{
			statusCode: http.StatusBadRequest,
			err:        err,
		}
	}
	if int64(len(body)) > limit {
		return nil, &requestBodyError{
			statusCode: http.StatusRequestEntityTooLarge,
			err:        fmt.Errorf("request body too large (max %d bytes)", limit),
		}
	}
	return body, nil
}

func readBodyStatusCode(err error) int {
	var bodyErr *requestBodyError
	if errors.As(err, &bodyErr) {
		return bodyErr.statusCode
	}
	return http.StatusBadRequest
}
