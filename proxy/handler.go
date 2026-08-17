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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
	"golang.org/x/net/http2"
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
	// modelsCacheMaxEntries bounds caller-specific /models query variants while
	// retaining enough room for common client-version and feature queries.
	modelsCacheMaxEntries = 64
	// modelsCacheFailureBackoff prevents an expired canonical catalog from
	// stampeding an unavailable provider. Stale canonical data remains usable,
	// and one retry is allowed after this short fixed interval.
	modelsCacheFailureBackoff = time.Second
	// syntheticCompactionPrefix marks proxy-owned compaction payloads so they
	// can be expanded back into normal context on subsequent /responses calls.
	syntheticCompactionPrefix = "vekil.compaction.v1:"
	// legacySyntheticCompactionPrefix keeps older compacted histories readable
	// after the project rename.
	legacySyntheticCompactionPrefix   = "copilot-proxy.compaction.v1:"
	defaultCopilotEditorVersion       = "vscode/1.95.0"
	defaultCopilotEditorPluginVersion = "copilot-chat/0.26.7"
	defaultCopilotUserAgent           = "GitHubCopilotChat/0.26.7"
	defaultCopilotIntegrationID       = "vscode-chat"
	directGitHubAppIntegrationID      = "copilot-language-server"
	defaultCopilotGitHubAPIVersion    = "2025-05-01"
	defaultCopilotOpenAIIntent        = "conversation-panel"
	defaultResponsesWSCompactMaxItems = 8
	defaultResponsesWSCompactMaxBytes = 32 << 10
	defaultResponsesWSCompactKeepTail = 4
)

var errProxyLifecycleShutdown = fmt.Errorf("proxy lifecycle shutdown: %w", context.Canceled)

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
	mu                    sync.RWMutex
	entries               map[string]cachedModelsResponse
	canonicalFailureUntil time.Time
	canonicalFailureErr   error
	now                   func() time.Time
	flightMu              sync.Mutex
	flights               map[string]*modelsCacheFlight
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
	EditorVersion       string `json:"editor_version,omitempty" yaml:"editor_version,omitempty"`
	EditorPluginVersion string `json:"editor_plugin_version,omitempty" yaml:"editor_plugin_version,omitempty"`
	UserAgent           string `json:"user_agent,omitempty" yaml:"user_agent,omitempty"`
	IntegrationID       string `json:"copilot_integration_id,omitempty" yaml:"copilot_integration_id,omitempty"`
	GitHubAPIVersion    string `json:"github_api_version,omitempty" yaml:"github_api_version,omitempty"`
	OpenAIIntent        string `json:"openai_intent,omitempty" yaml:"openai_intent,omitempty"`
}

// CopilotHeaderProfilesConfig allows provider config to override Copilot header
// profiles globally for the provider or for a specific upstream endpoint. Empty
// fields inherit from the provider default profile and then the project defaults.
type CopilotHeaderProfilesConfig struct {
	Default         CopilotHeaderConfig `json:"default,omitempty" yaml:"default,omitempty"`
	ChatCompletions CopilotHeaderConfig `json:"chat_completions,omitempty" yaml:"chat_completions,omitempty"`
	Responses       CopilotHeaderConfig `json:"responses,omitempty" yaml:"responses,omitempty"`
}

// ResponsesWebSocketConfig controls websocket-session state management for
// Codex-style GET /v1/responses clients.
type ResponsesWebSocketConfig struct {
	Enabled             bool
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
		Enabled:             false,
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

func mergeCopilotHeaderConfig(base, override CopilotHeaderConfig) CopilotHeaderConfig {
	if override.EditorVersion != "" {
		base.EditorVersion = override.EditorVersion
	}
	if override.EditorPluginVersion != "" {
		base.EditorPluginVersion = override.EditorPluginVersion
	}
	if override.UserAgent != "" {
		base.UserAgent = override.UserAgent
	}
	if override.IntegrationID != "" {
		base.IntegrationID = override.IntegrationID
	}
	if override.GitHubAPIVersion != "" {
		base.GitHubAPIVersion = override.GitHubAPIVersion
	}
	if override.OpenAIIntent != "" {
		base.OpenAIIntent = override.OpenAIIntent
	}
	return base
}

func (c CopilotHeaderProfilesConfig) profileForEndpointRaw(endpoint string, base CopilotHeaderConfig) CopilotHeaderConfig {
	profile := mergeCopilotHeaderConfig(base, c.Default)
	switch strings.TrimSpace(endpoint) {
	case "/chat/completions":
		profile = mergeCopilotHeaderConfig(profile, c.ChatCompletions)
	case "/responses":
		profile = mergeCopilotHeaderConfig(profile, c.Responses)
	}
	return profile
}

func (c CopilotHeaderProfilesConfig) profileForEndpoint(endpoint string, base CopilotHeaderConfig) CopilotHeaderConfig {
	return c.profileForEndpointRaw(endpoint, base).withDefaults()
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
	auth                             *auth.Authenticator
	client                           *http.Client
	copilotURL                       string
	copilotHeaders                   CopilotHeaderConfig
	providersConfig                  ProvidersConfig
	allowedModels                    map[string]struct{}
	providersState                   *providerSetup
	deferDynamicProviderModelRefresh bool
	policyRoutingMode                PolicyRoutingMode
	draining                         atomic.Bool
	lifecycleOnce                    sync.Once
	lifecycleCtx                     context.Context
	lifecycleCancel                  context.CancelCauseFunc
	shutdownSequenceOnce             sync.Once
	shutdownSequence                 atomic.Uint64
	lifecycleWorkersMu               sync.Mutex
	lifecycleWorkers                 sync.WaitGroup
	lifecycleWorkersActive           int
	startupAuthenticationPending     atomic.Bool
	dynamicProviderValidationPending atomic.Bool
	azureIdentityTokenSourceFactory  azureIdentityTokenSourceFactory
	toolOptimizers                   *ToolOptimizerManager
	toolContexts                     *ToolExecutionContextStore
	responsesWS                      ResponsesWebSocketConfig
	responsesWSSessionsMu            sync.Mutex
	responsesWSSessions              map[*responsesWebSocketSession]struct{}
	responsesWSDraining              bool
	streamingUpstreamTimeout         time.Duration
	compactChunkBodyBytes            int
	compactChunkConfigured           bool
	compactChunkConcurrency          int
	compactMaxAttempts               int
	compactLearnedTargetsMu          sync.Mutex
	compactInflightMu                sync.Mutex
	compactInflight                  map[string]*compactInflightCall
	compactLearnedTargets            map[compactLearnedTargetKey]compactLearnedTarget
	log                              *logger.Logger
	maxRetries                       int
	retryBaseDelay                   time.Duration
	models                           modelsCache
	chatRoutes                       chatRouteDiscoveryCache
	chatPolicyPlanner                chatPolicyPlanner
	policyRoutingController          policyRoutingController
	policyPreflightStateMu           sync.Mutex
	policyPreflightAttempts          int
	policyPreflightPermitOnce        sync.Once
	policyPreflightPermit            chan struct{}
	policyPreflightPending           atomic.Bool
	responsesChatReplayMu            sync.Mutex
	responsesChatReplay              *responsesChatReplayStore
	geminiCounts                     geminiCountTokensCache
	stats                            *statsCollector
	stateBindingsOnce                sync.Once
	stateBindings                    *stateBindingStore
	stateBindingsErr                 error
	insightGate                      *insightGate
	insightGateOnce                  sync.Once
}

// initializeLifecycle installs the proxy-owned cancellation root used by
// detached upstream work. sync.Once keeps zero-value handlers used by focused
// tests safe while NewProxyHandler initializes the lifecycle eagerly.
func (h *ProxyHandler) initializeLifecycle() {
	if h == nil {
		return
	}
	h.lifecycleOnce.Do(func() {
		h.lifecycleCtx, h.lifecycleCancel = context.WithCancelCause(context.Background())
	})
}

func (h *ProxyHandler) lifecycleContext() context.Context {
	if h == nil {
		return context.Background()
	}
	h.initializeLifecycle()
	return h.lifecycleCtx
}

func (h *ProxyHandler) responsesChatReplayStore() *responsesChatReplayStore {
	if h == nil {
		return nil
	}
	h.responsesChatReplayMu.Lock()
	defer h.responsesChatReplayMu.Unlock()
	if h.responsesChatReplay == nil && !h.ShuttingDown() {
		h.responsesChatReplay = newResponsesChatReplayStore()
	}
	return h.responsesChatReplay
}

// closeResponsesChatReplayStore clears process-local tool replay only after
// graceful HTTP shutdown has drained request handlers. Closing it in
// BeginShutdown would race in-flight Responses-backed streams publishing their
// terminal tool calls and misclassify local shutdown as a provider failure.
func (h *ProxyHandler) closeResponsesChatReplayStore() {
	if h == nil {
		return
	}
	h.responsesChatReplayMu.Lock()
	defer h.responsesChatReplayMu.Unlock()
	if h.responsesChatReplay != nil {
		_ = h.responsesChatReplay.Close()
	}
}

// BeginShutdown idempotently cancels proxy-owned upstream work. Existing and
// future lifecycle-rooted contexts observe cancellation immediately, while
// ordinary inference remains detached from inbound client disconnects before
// shutdown begins.
func (h *ProxyHandler) BeginShutdown() {
	if h == nil {
		return
	}
	h.shutdownSequenceOnce.Do(func() {
		publishResponsesLifecycleSequence(func(sequence uint64) {
			h.shutdownSequence.Store(sequence)
		})
	})
	h.draining.Store(true)
	h.initializeLifecycle()
	h.lifecycleCancel(errProxyLifecycleShutdown)
	if h.client != nil {
		h.client.CloseIdleConnections()
	}
}

// ShuttingDown reports whether admission has closed and lifecycle cancellation
// has begun. It becomes true before BeginShutdown cancels upstream contexts.
func (h *ProxyHandler) ShuttingDown() bool {
	return h != nil && h.draining.Load()
}

func (h *ProxyHandler) upstreamShutdownStarted() bool {
	return h.ShuttingDown()
}

// BindRequestBodyToLifecycle makes a request body read interruptible by proxy
// shutdown without changing the inbound request context. The separate cause
// context preserves whether client cancellation or lifecycle shutdown happened
// first, so a client disconnect racing shutdown is not rewritten as a local 503.
func (h *ProxyHandler) BindRequestBodyToLifecycle(r *http.Request, forceClose func()) func() {
	if h == nil || r == nil || r.Body == nil || r.Body == http.NoBody {
		return func() {}
	}

	causeCtx, cancelCause := context.WithCancelCause(r.Context())
	body := &lifecycleRequestBody{ReadCloser: r.Body, causeCtx: causeCtx}
	r.Body = body

	cancelForShutdown := func() {
		cancelCause(errProxyLifecycleShutdown)
		if !body.complete.Load() && forceClose != nil {
			forceClose()
			return
		}
		_ = body.Close()
	}
	stopLifecycle := context.AfterFunc(h.lifecycleContext(), cancelForShutdown)
	if h.ShuttingDown() {
		cancelForShutdown()
	}

	return func() {
		stopLifecycle()
		cancelCause(context.Canceled)
	}
}

type lifecycleRequestBody struct {
	io.ReadCloser
	causeCtx context.Context
	closeMu  sync.Mutex
	closed   bool
	closeErr error
	complete atomic.Bool
}

func (b *lifecycleRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.complete.Store(true)
	}
	if err != nil && b.causeCtx != nil && errors.Is(context.Cause(b.causeCtx), errProxyLifecycleShutdown) {
		return n, errProxyLifecycleShutdown
	}
	return n, err
}

func (b *lifecycleRequestBody) Close() error {
	if b == nil || b.ReadCloser == nil {
		return nil
	}
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if !b.closed {
		b.closed = true
		b.closeErr = b.ReadCloser.Close()
	}
	return b.closeErr
}

func (h *ProxyHandler) beginLifecycleWorker() bool {
	if h == nil {
		return false
	}
	h.lifecycleWorkersMu.Lock()
	defer h.lifecycleWorkersMu.Unlock()
	if h.ShuttingDown() {
		return false
	}
	h.lifecycleWorkers.Add(1)
	h.lifecycleWorkersActive++
	return true
}

func (h *ProxyHandler) endLifecycleWorker() {
	if h == nil {
		return
	}
	h.lifecycleWorkersMu.Lock()
	h.lifecycleWorkersActive--
	h.lifecycleWorkers.Done()
	h.lifecycleWorkersMu.Unlock()
}

// WaitLifecycleWorkers waits for proxy-owned detached workers after admission
// has closed. The mutex pairs worker registration with the transition to Wait,
// preventing a WaitGroup Add/Wait race while shutdown starts. Already-complete
// work wins over an expired shutdown context so repeated Stop calls do not add
// a duplicate deadline error.
func (h *ProxyHandler) WaitLifecycleWorkers(ctx context.Context) (err error) {
	if h == nil {
		return nil
	}
	// Server.stop calls this after http.Server.Shutdown. Clear replay state only
	// after a successful, non-expired drain; a forced-close timeout can still have
	// handler goroutines unwinding from lifecycle cancellation.
	defer func() {
		if err == nil && h.ShuttingDown() && (ctx == nil || ctx.Err() == nil) {
			h.closeResponsesChatReplayStore()
		}
	}()
	h.lifecycleWorkersMu.Lock()
	if h.lifecycleWorkersActive == 0 {
		h.lifecycleWorkersMu.Unlock()
		return nil
	}
	done := make(chan struct{})
	go func() {
		h.lifecycleWorkers.Wait()
		close(done)
	}()
	h.lifecycleWorkersMu.Unlock()

	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
		}
		h.lifecycleWorkersMu.Lock()
		completed := h.lifecycleWorkersActive == 0
		h.lifecycleWorkersMu.Unlock()
		if completed {
			return nil
		}
		return ctx.Err()
	}
}

func (h *ProxyHandler) handleResponseBodyWriteError(w http.ResponseWriter, r *http.Request, upstreamCtx context.Context, endpoint string, err error) bool {
	var bodyErr *responseBodyWriteError
	if !errors.As(err, &bodyErr) {
		return false
	}
	if !bodyErr.upstream {
		return true
	}
	if bodyErr.statusCode < http.StatusOK || bodyErr.statusCode >= http.StatusMultipleChoices {
		return true
	}
	if bodyErr.cancellationAtFailure && h.ShuttingDown() && upstreamCtx != nil && errors.Is(context.Cause(upstreamCtx), errProxyLifecycleShutdown) {
		if bodyErr.committed {
			if r != nil {
				suppressRequestStats(r.Context())
			}
		} else {
			h.WriteShutdownServiceUnavailable(w, r)
		}
		return true
	}
	if bodyErr.committed {
		if r != nil {
			observeResponseFailureStatus(r.Context(), http.StatusBadGateway)
		}
		if h.log != nil {
			h.log.Error("upstream response body failed", logger.F("endpoint", endpoint), logger.Err(err))
		}
		return true
	}
	return false
}

func (h *ProxyHandler) handleShutdownError(w http.ResponseWriter, r *http.Request, upstreamCtx context.Context, err error) bool {
	if err == nil || !h.ShuttingDown() {
		return false
	}
	lifecycleCanceled := errors.Is(err, errProxyLifecycleShutdown)
	if !lifecycleCanceled && upstreamCtx != nil && errors.Is(context.Cause(upstreamCtx), errProxyLifecycleShutdown) {
		lifecycleCanceled = contextTerminationMatches(upstreamCtx, err)
	}
	if !lifecycleCanceled {
		return false
	}
	h.WriteShutdownServiceUnavailable(w, r)
	return true
}

// WriteShutdownServiceUnavailable marks the request as locally suppressed from
// provider stats and writes a protocol-appropriate 503 response. Callers must
// use it only before committing a streaming response.
func (h *ProxyHandler) WriteShutdownServiceUnavailable(w http.ResponseWriter, r *http.Request) {
	if r != nil {
		suppressRequestStats(r.Context())
	}
	if w == nil {
		return
	}
	headers := w.Header()
	clear(headers)
	headers.Set("Cache-Control", "no-store")
	headers.Set("Retry-After", "1")
	path := ""
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	switch {
	case path == "/v1/messages" || path == "/v1/messages/count_tokens":
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", "server shutting down")
	case isGeminiModelsPath(path):
		writeGeminiError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "server shutting down")
	default:
		writeOpenAIError(w, http.StatusServiceUnavailable, "server shutting down", "service_unavailable")
	}
}

func (h *ProxyHandler) lifecycleStreamHooks(observeCtx context.Context, canceledAtFailure func() bool, precommit ...func()) streamLifecycleHooks {
	var writePrecommitShutdown func()
	if len(precommit) > 0 {
		writePrecommitShutdown = precommit[0]
	}
	return streamLifecycleHooks{
		transportCanceled: func() bool {
			return h.ShuttingDown() && canceledAtFailure != nil && canceledAtFailure()
		},
		suppressStats: func() {
			suppressRequestStats(observeCtx)
		},
		writePrecommitShutdown: writePrecommitShutdown,
	}
}

type compactLearnedTargetKey struct {
	ProviderID   string
	ProviderKind string
	BaseURL      string
	Model        string
	Endpoint     string
}

type compactLearnedTarget struct {
	TargetBytes int
	UpdatedAt   time.Time
}

const compactLearnedTargetTTL = 30 * time.Minute

func (h *ProxyHandler) learnedCompactTarget(key compactLearnedTargetKey, configuredTarget int) (int, bool) {
	if h == nil || configuredTarget <= 0 || key.ProviderID == "" || key.Endpoint == "" {
		return configuredTarget, false
	}

	now := time.Now()
	h.compactLearnedTargetsMu.Lock()
	defer h.compactLearnedTargetsMu.Unlock()

	entry, ok := h.compactLearnedTargets[key]
	if !ok {
		return configuredTarget, false
	}
	if now.Sub(entry.UpdatedAt) > compactLearnedTargetTTL {
		delete(h.compactLearnedTargets, key)
		return configuredTarget, false
	}
	if entry.TargetBytes <= 0 || entry.TargetBytes >= configuredTarget {
		return configuredTarget, false
	}
	return entry.TargetBytes, true
}

func (h *ProxyHandler) recordLearnedCompactTarget(key compactLearnedTargetKey, target int) bool {
	if h == nil || target <= 0 || key.ProviderID == "" || key.Endpoint == "" {
		return false
	}
	if target < compactUpstreamChunkBodyFloor {
		target = compactUpstreamChunkBodyFloor
	}

	h.compactLearnedTargetsMu.Lock()
	defer h.compactLearnedTargetsMu.Unlock()

	if h.compactLearnedTargets == nil {
		h.compactLearnedTargets = make(map[compactLearnedTargetKey]compactLearnedTarget, 4)
	}
	if existing, ok := h.compactLearnedTargets[key]; ok && existing.TargetBytes > 0 && existing.TargetBytes <= target && time.Since(existing.UpdatedAt) <= compactLearnedTargetTTL {
		return false
	}
	h.compactLearnedTargets[key] = compactLearnedTarget{TargetBytes: target, UpdatedAt: time.Now()}
	return true
}

// Option customizes ProxyHandler behavior.
type Option func(*ProxyHandler)

// WithCopilotHeaderConfig overrides the synthetic Copilot-identifying headers
// used for upstream requests. The raw override values are retained so endpoint-
// scoped header logic can distinguish an explicitly configured OpenAI intent
// from the built-in chat/responses default.
func WithCopilotHeaderConfig(cfg CopilotHeaderConfig) Option {
	return func(h *ProxyHandler) {
		h.copilotHeaders = cfg
	}
}

// WithAllowedModels restricts request routing to the listed public model IDs.
// Empty keeps the normal global model namespace available.
func WithAllowedModels(models ...string) Option {
	return func(h *ProxyHandler) {
		if h.allowedModels == nil {
			h.allowedModels = make(map[string]struct{})
		}
		for _, model := range models {
			if model = strings.TrimSpace(model); model != "" {
				h.allowedModels[model] = struct{}{}
			}
		}
	}
}

// WithProvidersConfig enables multi-provider model routing. When unset, the
// proxy keeps its legacy single-upstream Copilot behavior.
func WithProvidersConfig(cfg ProvidersConfig) Option {
	return func(h *ProxyHandler) {
		h.providersConfig = cfg
	}
}

// WithPolicyRoutingMode sets the process-wide policy safety ceiling. New
// handlers follow configured profile modes by default; an explicit off (or
// zero) value preserves the rollback ceiling.
func WithPolicyRoutingMode(mode PolicyRoutingMode) Option {
	return func(h *ProxyHandler) {
		h.policyRoutingMode = mode
	}
}

// WithChatPolicyPlanner installs a planner seam for focused tests. Production
// policy configuration replaces this with the compiled planner during handler
// initialization.
func WithChatPolicyPlanner(planner chatPolicyPlanner) Option {
	return func(h *ProxyHandler) {
		h.chatPolicyPlanner = planner
	}
}

// WithDeferredDynamicProviderModelValidation skips startup-time dynamic model
// discovery. Call ValidateDynamicProviderModels after any required interactive
// auth has completed to preserve collision checks without blocking liveness.
func WithDeferredDynamicProviderModelValidation(deferValidation bool) Option {
	return func(h *ProxyHandler) {
		h.deferDynamicProviderModelRefresh = deferValidation
	}
}

// WithCopilotBaseURL overrides the legacy single-upstream Copilot base URL.
// It is primarily useful for local contract tests and diagnostic harnesses.
func WithCopilotBaseURL(baseURL string) Option {
	return func(h *ProxyHandler) {
		h.copilotURL = strings.TrimRight(baseURL, "/")
	}
}

func withCopilotBaseURLForTest(baseURL string) Option {
	return WithCopilotBaseURL(baseURL)
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

// WithCompactUpstreamChunkBytes overrides the initial target body size used
// when retrying /v1/responses/compact requests after the upstream returns
// 413. Non-positive values fall back to the default. The chunker still
// halves the target down to compactUpstreamChunkBodyFloor on recursive 413s.
func WithCompactUpstreamChunkBytes(bytes int) Option {
	return func(h *ProxyHandler) {
		if bytes > 0 {
			h.compactChunkBodyBytes = bytes
			h.compactChunkConfigured = true
		}
	}
}

// WithCompactUpstreamChunkConcurrency overrides the maximum number of sibling
// compact chunks sent concurrently after the first chunk succeeds at the current
// target. Non-positive values fall back to the default.
func WithCompactUpstreamChunkConcurrency(concurrency int) Option {
	return func(h *ProxyHandler) {
		if concurrency > 0 {
			h.compactChunkConcurrency = concurrency
		}
	}
}

// WithCompactUpstreamMaxAttempts caps the total number of logical compaction
// calls the /v1/responses/compact 413 fallback may make for a single inbound
// request. Each logical call may produce extra real upstream POSTs through model
// fallback or the shared transport-retry policy. Non-positive values fall back
// to the default.
func WithCompactUpstreamMaxAttempts(max int) Option {
	return func(h *ProxyHandler) {
		if max > 0 {
			h.compactMaxAttempts = max
		}
	}
}

func newInferenceTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 100
	transport.IdleConnTimeout = 55 * time.Second
	transport.ForceAttemptHTTP2 = true

	tlsConfig := transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	tlsConfig.MinVersion = tls.VersionTLS12
	if tlsConfig.ClientSessionCache == nil {
		tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(0)
	}
	transport.TLSClientConfig = tlsConfig

	if h2Transport, err := http2.ConfigureTransports(transport); err == nil {
		h2Transport.ReadIdleTimeout = 30 * time.Second
		h2Transport.PingTimeout = 15 * time.Second
	}

	return transport
}

// NewProxyHandler creates a ProxyHandler with connection pooling and HTTP/2.
func NewProxyHandler(a *auth.Authenticator, log *logger.Logger, opts ...Option) (*ProxyHandler, error) {
	h := &ProxyHandler{
		auth: a,
		client: &http.Client{
			Transport: newInferenceTransport(),
		},
		copilotURL: "https://api.githubcopilot.com",
		// Keep global Copilot header overrides empty by default. Header application
		// fills built-in defaults per endpoint, and /models must not inherit the
		// built-in openai-intent value.
		copilotHeaders:                  CopilotHeaderConfig{},
		azureIdentityTokenSourceFactory: newDefaultAzureIdentityTokenSource,
		responsesWS:                     DefaultResponsesWebSocketConfig(),
		streamingUpstreamTimeout:        streamingUpstreamTimeout,
		chatRoutes:                      newChatRouteDiscoveryCache(),
		policyRoutingMode:               PolicyRoutingModeConfig,
		responsesChatReplay:             newResponsesChatReplayStore(),
		log:                             log,
		stats:                           newStatsCollector(),
	}
	h.initializeLifecycle()
	for _, opt := range opts {
		if opt != nil {
			opt(h)
		}
	}
	if !h.policyRoutingMode.valid() {
		h.BeginShutdown()
		return nil, fmt.Errorf("invalid policy routing mode %q", h.policyRoutingMode)
	}
	if _, err := h.ensureStateBindingStore(); err != nil {
		h.BeginShutdown()
		return nil, err
	}
	h.initializeToolOptimizers()
	if err := h.initializeProviders(); err != nil {
		h.BeginShutdown()
		return nil, err
	}
	controller, err := newChatPolicyRoutingController(h, h.providersConfig, h.policyRoutingMode)
	if err != nil {
		h.BeginShutdown()
		return nil, err
	}
	if controller != nil {
		h.policyRoutingController = controller
		h.chatPolicyPlanner = controller
		h.policyPreflightPending.Store(controller.Active())
	}
	h.validateInsightModel()
	return h, nil
}

// validateInsightModel logs a non-fatal warning when the configured dashboard
// insight model cannot be served through Vekil's Chat compatibility route. The
// route may use native /chat/completions or the Chat-over-Responses adapter; only
// models that support neither native endpoint are startup misconfigurations.
func (h *ProxyHandler) validateInsightModel() {
	model := strings.TrimSpace(h.providersConfig.InsightModel)
	if model == "" || h.log == nil || h.DynamicProviderValidationPending() {
		return
	}
	if entry, known := h.providerSetup().lookupPublicModelEntry(model); known && entry != nil && entry.kind == publicEntryPolicy {
		h.log.Info("dashboard insight_model cannot use a policy profile in policy-routing v1; insights will not work", logger.F("insight_model", model))
		return
	}
	provider, owner, known := h.resolveProviderModel(model, providerEndpointChatCompletions)
	if _, err := chooseChatRoute(provider, owner, known, model); err != nil {
		providerID := ""
		if provider != nil {
			providerID = provider.id
		}
		h.log.Info("dashboard insight_model cannot be served through Chat compatibility; insights will not work",
			logger.F("insight_model", model), logger.F("provider", providerID), logger.Err(err))
	}
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

// DefaultCompactUpstreamChunkBytes is the default target body size for chunked
// /v1/responses/compact retries. Exposed so callers can show the default value
// in flag/env help text.
func DefaultCompactUpstreamChunkBytes() int {
	return compactUpstreamChunkBodySize
}

func (h *ProxyHandler) effectiveCompactChunkBodyBytes() int {
	if h == nil || h.compactChunkBodyBytes <= 0 {
		return compactUpstreamChunkBodySize
	}
	return h.compactChunkBodyBytes
}

func (h *ProxyHandler) compactProactiveChunkingEnabled() bool {
	return h != nil && h.compactChunkConfigured && h.effectiveCompactChunkBodyBytes() < compactUpstreamChunkBodySize
}

// DefaultCompactUpstreamChunkConcurrency returns the default max parallelism for
// sibling chunk compaction calls after the first chunk succeeds.
func DefaultCompactUpstreamChunkConcurrency() int {
	return compactUpstreamChunkConcurrency
}

func (h *ProxyHandler) effectiveCompactChunkConcurrency() int {
	if h == nil || h.compactChunkConcurrency <= 0 {
		return compactUpstreamChunkConcurrency
	}
	return h.compactChunkConcurrency
}

// DefaultCompactUpstreamMaxAttempts returns the default upstream attempt cap
// for chunked /v1/responses/compact 413 fallback retries.
func DefaultCompactUpstreamMaxAttempts() int {
	return compactUpstreamMaxAttempts
}

func (h *ProxyHandler) effectiveCompactMaxAttempts() int {
	if h == nil || h.compactMaxAttempts <= 0 {
		return compactUpstreamMaxAttempts
	}
	return h.compactMaxAttempts
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
	cfg = cfg.withCredentialDefaults(token)
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

func (c CopilotHeaderConfig) withCredentialDefaults(token string) CopilotHeaderConfig {
	if c.IntegrationID == "" && strings.HasPrefix(strings.TrimSpace(token), "ghu_") {
		c.IntegrationID = directGitHubAppIntegrationID
	}
	return c.withDefaults()
}

func clearCopilotHeaders(headers http.Header) {
	for _, header := range []string{
		"Authorization",
		"editor-version",
		"editor-plugin-version",
		"user-agent",
		"copilot-integration-id",
		"x-github-api-version",
		"x-request-id",
		"openai-intent",
	} {
		headers.Del(header)
	}
}

func copilotEndpointUsesDefaultOpenAIIntent(endpoint string) bool {
	switch strings.TrimSpace(endpoint) {
	case "/chat/completions", "/responses":
		return true
	default:
		return false
	}
}

func setCopilotHeadersForEndpoint(req *http.Request, token string, cfg CopilotHeaderConfig, endpoint string) {
	explicitOpenAIIntent := cfg.OpenAIIntent != ""
	cfg = cfg.withCredentialDefaults(token)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("editor-version", cfg.EditorVersion)
	req.Header.Set("editor-plugin-version", cfg.EditorPluginVersion)
	req.Header.Set("user-agent", cfg.UserAgent)
	req.Header.Set("copilot-integration-id", cfg.IntegrationID)
	req.Header.Set("x-github-api-version", cfg.GitHubAPIVersion)
	req.Header.Set("x-request-id", uuid.New().String())
	if explicitOpenAIIntent || copilotEndpointUsesDefaultOpenAIIntent(endpoint) {
		req.Header.Set("openai-intent", cfg.OpenAIIntent)
	} else {
		req.Header.Del("openai-intent")
	}
	if req.Method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
}

func (h *ProxyHandler) setCopilotHeadersForProvider(req *http.Request, token string, provider *providerRuntime, endpoint string) {
	cfg := h.copilotHeaders
	if provider != nil && provider.kind == providerTypeCopilot {
		cfg = provider.headerProfiles.profileForEndpointRaw(endpoint, cfg)
	}
	setCopilotHeadersForEndpoint(req, token, cfg, endpoint)
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

func (h *ProxyHandler) storeModelsCacheEntry(cacheKey string, entry cachedModelsResponse) {
	if h == nil {
		return
	}

	now := h.models.nowTime()
	h.models.mu.Lock()
	defer h.models.mu.Unlock()

	if h.models.entries == nil {
		h.models.entries = make(map[string]cachedModelsResponse)
	}
	for key, cached := range h.models.entries {
		if key == "" {
			continue
		}
		if !cached.expiry.IsZero() && !now.Before(cached.expiry) {
			delete(h.models.entries, key)
		}
	}

	delete(h.models.entries, cacheKey)
	for len(h.models.entries) >= modelsCacheMaxEntries {
		evictionKey, ok := oldestModelsCacheEntry(h.models.entries, true)
		if !ok {
			evictionKey, ok = oldestModelsCacheEntry(h.models.entries, false)
		}
		if !ok {
			break
		}
		delete(h.models.entries, evictionKey)
	}
	h.models.entries[cacheKey] = entry
}

func oldestModelsCacheEntry(entries map[string]cachedModelsResponse, skipCanonical bool) (string, bool) {
	var oldestKey string
	var oldestExpiry time.Time
	found := false
	for key, entry := range entries {
		if skipCanonical && key == "" {
			continue
		}
		if !found || entry.expiry.Before(oldestExpiry) {
			oldestKey = key
			oldestExpiry = entry.expiry
			found = true
		}
	}
	return oldestKey, found
}

func isCanonicalModelsQuery(rawQuery string) bool {
	return strings.TrimSpace(rawQuery) == ""
}

func (h *ProxyHandler) replaceModelsCacheWithCanonical(entry cachedModelsResponse) {
	if h == nil {
		return
	}
	h.models.mu.Lock()
	h.models.entries = map[string]cachedModelsResponse{"": entry}
	h.models.canonicalFailureUntil = time.Time{}
	h.models.canonicalFailureErr = nil
	h.models.mu.Unlock()
}

func (h *ProxyHandler) ensureCanonicalModelsCacheEntry(ctx, waitCtx context.Context) (cachedModelsResponse, bool, error) {
	if h == nil {
		return cachedModelsResponse{}, false, fmt.Errorf("proxy handler is required")
	}

	now := h.models.nowTime()
	entry, ok, failureErr := h.canonicalModelsCacheState(now)
	if failureErr != nil || (ok && now.Before(entry.expiry)) {
		return entry, ok, failureErr
	}

	result := h.models.doFlight(waitCtx, "", func() modelsCacheFlightResult {
		now := h.models.nowTime()
		entry, ok, failureErr := h.canonicalModelsCacheState(now)
		if failureErr != nil || (ok && now.Before(entry.expiry)) {
			return modelsCacheFlightResult{entry: entry, hasEntry: ok, err: failureErr}
		}

		ifNoneMatch := ""
		if ok {
			ifNoneMatch = entry.etag
		}
		refreshed, notModified, err := h.buildMergedModelsEntry(ctx, "", ifNoneMatch)
		if err != nil {
			h.recordCanonicalModelsRefreshFailure(h.models.nowTime(), err)
			return modelsCacheFlightResult{entry: entry, hasEntry: ok, err: err}
		}
		if notModified {
			if !ok {
				err := fmt.Errorf("canonical models request unexpectedly returned not modified without a cached entry")
				h.recordCanonicalModelsRefreshFailure(h.models.nowTime(), err)
				return modelsCacheFlightResult{err: err}
			}
			entry.expiry = h.models.nowTime().Add(modelsCacheTTL)
			h.storeModelsCacheEntry("", entry)
			h.clearCanonicalModelsRefreshFailure()
			return modelsCacheFlightResult{entry: entry, hasEntry: true}
		}
		if refreshed.statusCode != http.StatusOK {
			err := fmt.Errorf("canonical models request returned status %d", refreshed.statusCode)
			h.recordCanonicalModelsRefreshFailure(h.models.nowTime(), err)
			return modelsCacheFlightResult{entry: entry, hasEntry: ok, err: err}
		}
		h.replaceModelsCacheWithCanonical(refreshed)
		return modelsCacheFlightResult{entry: refreshed, hasEntry: true}
	})

	return result.entry, result.hasEntry, result.err
}

func (h *ProxyHandler) canonicalModelsCacheState(now time.Time) (cachedModelsResponse, bool, error) {
	h.models.mu.RLock()
	defer h.models.mu.RUnlock()

	entry, ok := h.models.entries[""]
	if h.models.canonicalFailureErr != nil && now.Before(h.models.canonicalFailureUntil) {
		return entry, ok, h.models.canonicalFailureErr
	}
	return entry, ok, nil
}

func (h *ProxyHandler) recordCanonicalModelsRefreshFailure(now time.Time, err error) {
	h.models.mu.Lock()
	h.models.canonicalFailureUntil = now.Add(modelsCacheFailureBackoff)
	h.models.canonicalFailureErr = err
	h.models.mu.Unlock()
}

func (h *ProxyHandler) clearCanonicalModelsRefreshFailure() {
	h.models.mu.Lock()
	h.models.canonicalFailureUntil = time.Time{}
	h.models.canonicalFailureErr = nil
	h.models.mu.Unlock()
}

// HandleHealthz handles GET /healthz and returns {"status":"ok"}.
func (h *ProxyHandler) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// HandleReadyz validates that the proxy can obtain an auth token and reach the
// configured upstream providers.
func (h *ProxyHandler) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzUpstreamTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return
	}
	if h.startupAuthenticationPending.Load() {
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", "startup authentication pending")
		return
	}
	if h.dynamicProviderValidationPending.Load() {
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", "provider model validation pending")
		return
	}
	if h.PolicyRoutingPreflightPending() {
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", "policy routing preflight pending")
		return
	}
	if diagnostic := h.PolicyRoutingReadinessDiagnostic(); diagnostic != "" {
		writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", diagnostic)
		return
	}

	setup := h.providerSetup()
	for _, providerID := range setup.providerOrder {
		provider := setup.providerByID(providerID)
		if !h.providerWithinAllowedModelScope(provider) {
			continue
		}
		if err := h.checkProviderReady(ctx, provider); err != nil {
			if shouldSuppressReadyzResponse(r.Context(), err) {
				return
			}
			writeReadyzStatus(w, http.StatusServiceUnavailable, "not_ready", err.Error())
			return
		}
	}

	writeReadyzStatus(w, http.StatusOK, "ready", "")
}

func (h *ProxyHandler) checkProviderReady(ctx context.Context, provider *providerRuntime) error {
	if providerSkipsReadyzProbe(provider) {
		return nil
	}

	req, err := h.newProviderProbeRequest(ctx, provider)
	if err != nil {
		return err
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("provider %q upstream probe failed: %w", provider.id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := fmt.Sprintf("provider %q upstream probe returned %d", provider.id, resp.StatusCode)
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			message += ": " + trimmed
		}
		return fmt.Errorf("%s", message)
	}

	return nil
}

func providerSkipsReadyzProbe(provider *providerRuntime) bool {
	if provider == nil {
		return false
	}
	switch provider.kind {
	case providerTypeAzureOpenAI:
		return true
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		return provider.modelDiscovery == providerModelDiscoveryStatic
	default:
		return false
	}
}

func (h *ProxyHandler) newProviderProbeRequest(ctx context.Context, provider *providerRuntime) (*http.Request, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	switch provider.kind {
	case providerTypeCopilot:
		token, err := h.auth.GetTokenNonInteractive(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get token for provider %q: %w", provider.id, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.baseURL+"/models", nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create upstream probe request: %w", err)
		}
		h.setCopilotHeadersForProvider(req, token, provider, "/models")
		return req, nil
	case providerTypeAzureOpenAI:
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodGet, "/models", nil, nil, "")
		if err != nil {
			return nil, fmt.Errorf("failed to create provider %q probe request: %w", provider.id, err)
		}
		return req, nil
	case providerTypeOpenAICodex:
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodGet, "/models", nil, nil, openAICodexModelsRawQuery(""))
		if err != nil {
			return nil, fmt.Errorf("failed to create provider %q probe request: %w", provider.id, err)
		}
		return req, nil
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		req, err := h.newProviderJSONRequest(ctx, provider, http.MethodGet, providerEndpointModels, nil, nil, "")
		if err != nil {
			return nil, fmt.Errorf("failed to create provider %q probe request: %w", provider.id, err)
		}
		return req, nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q", provider.kind)
	}
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

func (h *ProxyHandler) SetStartupAuthenticationPending(pending bool) {
	if h != nil {
		h.startupAuthenticationPending.Store(pending)
	}
}

func (h *ProxyHandler) StartupAuthenticationPending() bool {
	return h != nil && h.startupAuthenticationPending.Load()
}

func (h *ProxyHandler) DynamicProviderValidationPending() bool {
	return h != nil && h.dynamicProviderValidationPending.Load()
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

// HandleModels handles GET /v1/models by building a merged model catalog across
// the configured providers. Responses are cached for modelsCacheTTL to avoid
// repeated upstream calls.
func (h *ProxyHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	cacheKey := r.URL.RawQuery
	canonicalQuery := isCanonicalModelsQuery(cacheKey)

	ctx, cancel := h.newLifecycleUpstreamContext(modelsUpstreamTimeout)
	defer cancel()

	canonicalEntry, hasCanonicalEntry, err := h.ensureCanonicalModelsCacheEntry(ctx, r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if h.handleShutdownError(w, r, ctx, err) {
			return
		}
		h.log.Error("upstream request failed", logger.F("endpoint", "models"), logger.Err(err))
		if canonicalQuery && hasCanonicalEntry {
			writeCachedModelsResponse(w, canonicalEntry)
			return
		}
		if !canonicalQuery && hasCanonicalEntry {
			h.models.mu.RLock()
			cachedEntry, hasCachedEntry := h.models.entries[cacheKey]
			h.models.mu.RUnlock()
			if hasCachedEntry {
				writeCachedModelsResponse(w, cachedEntry)
				return
			}
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		writeOpenAIUpstreamRequestFailure(w, statusCode, err)
		return
	}
	if canonicalQuery {
		writeCachedModelsResponse(w, canonicalEntry)
		return
	}

	var cachedEntry cachedModelsResponse
	var hasCachedEntry bool
	h.models.mu.RLock()
	if h.models.entries != nil {
		cachedEntry, hasCachedEntry = h.models.entries[cacheKey]
	}
	h.models.mu.RUnlock()

	// Without an ETag we cannot safely revalidate, so honor the TTL-based cache.
	if hasCachedEntry && cachedEntry.etag == "" && h.models.nowTime().Before(cachedEntry.expiry) {
		writeCachedModelsResponse(w, cachedEntry)
		return
	}

	result := h.refreshModelsCacheVariant(r.Context(), cacheKey)
	entry, hasCachedEntry, err := result.entry, result.hasEntry, result.err
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if h.handleShutdownError(w, r, ctx, err) {
			return
		}
		h.log.Error("upstream request failed", logger.F("endpoint", "models"), logger.Err(err))
		if hasCachedEntry {
			writeCachedModelsResponse(w, entry)
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		writeOpenAIUpstreamRequestFailure(w, statusCode, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if etag := entry.etag; etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(entry.statusCode)
	_, _ = w.Write(entry.body)
}

func (h *ProxyHandler) refreshModelsCacheVariant(waitCtx context.Context, cacheKey string) modelsCacheFlightResult {
	return h.models.doFlight(waitCtx, cacheKey, func() modelsCacheFlightResult {
		ctx, cancel := h.newLifecycleUpstreamContext(modelsUpstreamTimeout)
		defer cancel()

		now := h.models.nowTime()
		h.models.mu.RLock()
		cachedEntry, hasCachedEntry := h.models.entries[cacheKey]
		h.models.mu.RUnlock()

		// Query variants intentionally do not send conditional ETags because a
		// provider-local 304 cannot safely reconstruct a merged multi-provider
		// variant. Entries without ETags remain ordinary TTL caches.
		if hasCachedEntry && cachedEntry.etag == "" && now.Before(cachedEntry.expiry) {
			return modelsCacheFlightResult{entry: cachedEntry, hasEntry: true}
		}

		entry, notModified, err := h.buildMergedModelsEntry(ctx, cacheKey, "")
		if err != nil {
			if errors.Is(context.Cause(ctx), errProxyLifecycleShutdown) && contextTerminationMatches(ctx, err) {
				err = errProxyLifecycleShutdown
			}
			return modelsCacheFlightResult{entry: cachedEntry, hasEntry: hasCachedEntry, err: err}
		}
		if notModified {
			if !hasCachedEntry {
				return modelsCacheFlightResult{err: fmt.Errorf("models query variant %q unexpectedly returned not modified without a cached entry", cacheKey)}
			}
			cachedEntry.expiry = h.models.nowTime().Add(modelsCacheTTL)
			h.storeModelsCacheEntry(cacheKey, cachedEntry)
			return modelsCacheFlightResult{entry: cachedEntry, hasEntry: true}
		}
		if entry.statusCode == http.StatusOK {
			h.storeModelsCacheEntry(cacheKey, entry)
		}
		return modelsCacheFlightResult{entry: entry, hasEntry: entry.statusCode == http.StatusOK}
	})
}

func (h *ProxyHandler) buildMergedModelsEntry(ctx context.Context, rawQuery, ifNoneMatch string) (cachedModelsResponse, bool, error) {
	setup := h.providerSetup()
	canonicalRefresh := isCanonicalModelsQuery(rawQuery)
	rawEntries := make([]json.RawMessage, 0)
	owners := make(map[string]mergedModelReservation)
	if registry := setup.routeRegistry(); registry != nil {
		for _, route := range registry.explicitRoutes() {
			if route == nil {
				continue
			}
			publicID := strings.TrimSpace(route.public.id)
			if len(h.allowedModels) > 0 {
				if _, allowed := h.allowedModels[publicID]; !allowed {
					continue
				}
			}
			reservation := mergedModelReservation{
				ownerID:     route.public.routeID,
				rawPublicID: publicID,
				ownerType:   mergedModelOwnerExplicitRoute,
			}
			for _, alias := range configuredPublicModelAliases(reservation.rawPublicID) {
				owners[alias] = reservation
			}
			rawEntries = append(rawEntries, append(json.RawMessage(nil), route.public.raw...))
		}
	}
	refreshedDynamicModels := make(map[string][]providerModel)
	mergedETag := ""
	sawDynamicProvider := false
	allDynamicProvidersUnchanged := true

	for _, providerID := range setup.providerOrder {
		provider := setup.providerByID(providerID)
		if !h.providerWithinAllowedModelScope(provider) {
			continue
		}

		result, err := h.fetchProviderModels(ctx, provider, rawQuery, ifNoneMatch)
		if err != nil {
			return cachedModelsResponse{}, false, err
		}

		models := h.filterAllowedModels(filterHiddenProviderModels(provider, filterProviderModels(provider, result.models)))

		if result.notModified {
			models = h.filterAllowedModels(filterHiddenProviderModels(provider, filterProviderModels(provider, setup.modelsForProvider(provider.id))))
			for _, model := range models {
				appendModel, err := reserveMergedModelOwner(owners, model)
				if err != nil {
					return cachedModelsResponse{}, false, err
				}
				if appendModel {
					rawEntries = append(rawEntries, model.raw)
				}
			}
		}

		if providerUsesDynamicModels(provider) {
			sawDynamicProvider = true
			if result.notModified {
				continue
			}
			allDynamicProvidersUnchanged = false
			if canonicalRefresh {
				refreshedDynamicModels[provider.id] = models
			}
			if result.etag != "" {
				mergedETag = result.etag
			}
		}

		for _, model := range models {
			appendModel, err := reserveMergedModelOwner(owners, model)
			if err != nil {
				return cachedModelsResponse{}, false, err
			}
			if appendModel {
				rawEntries = append(rawEntries, model.raw)
			}
		}
	}

	if sawDynamicProvider && allDynamicProvidersUnchanged {
		return cachedModelsResponse{etag: ifNoneMatch}, true, nil
	}

	body, err := json.Marshal(struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}{
		Object: "list",
		Data:   rawEntries,
	})
	if err != nil {
		return cachedModelsResponse{}, false, err
	}

	if err := setup.replaceProviderModelsBatch(refreshedDynamicModels); err != nil {
		return cachedModelsResponse{}, false, err
	}

	return cachedModelsResponse{
		body:       transformModelsResponse(body),
		statusCode: http.StatusOK,
		expiry:     h.models.nowTime().Add(modelsCacheTTL),
		etag:       mergedETag,
	}, false, nil
}

type mergedModelOwnerType uint8

const (
	mergedModelOwnerProvider mergedModelOwnerType = iota
	mergedModelOwnerExplicitRoute
)

type mergedModelReservation struct {
	ownerID     string
	rawPublicID string
	ownerType   mergedModelOwnerType
}

// reserveMergedModelOwner reserves both the raw public ID and every normalized
// request alias before a provider model is exposed in any /models response.
// Exact duplicates from the same provider retain the historical first-entry
// behavior; distinct IDs that share an alias are collisions even within one
// provider, matching the strict version-2 route registry.
func reserveMergedModelOwner(owners map[string]mergedModelReservation, model providerModel) (bool, error) {
	publicID := strings.TrimSpace(model.publicID)
	reservation := mergedModelReservation{
		ownerID:     model.providerID,
		rawPublicID: publicID,
		ownerType:   mergedModelOwnerProvider,
	}
	aliases := configuredPublicModelAliases(publicID)
	duplicate := false
	for _, alias := range aliases {
		existing, exists := owners[alias]
		if !exists {
			continue
		}
		if existing.ownerType == reservation.ownerType &&
			existing.ownerID == reservation.ownerID &&
			existing.rawPublicID == reservation.rawPublicID {
			duplicate = true
			continue
		}
		return false, providerModelCollisionError(alias, existing.ownerID, model.providerID)
	}
	if duplicate {
		return false, nil
	}
	for _, alias := range aliases {
		owners[alias] = reservation
	}
	return true, nil
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
		MaxContextWindow            *int64            `json:"max_context_window,omitempty"`
		AutoCompactTokenLimit       *int64            `json:"auto_compact_token_limit,omitempty"`
		EffectiveContextWindowPct   int64             `json:"effective_context_window_percent"`
		ExperimentalSupportedTools  []string          `json:"experimental_supported_tools"`
		InputModalities             []string          `json:"input_modalities"`
	}

	positivePtr := func(value int64) *int64 {
		if value <= 0 {
			return nil
		}
		v := value
		return &v
	}
	firstPositivePtr := func(values ...int64) *int64 {
		for _, value := range values {
			if ptr := positivePtr(value); ptr != nil {
				return ptr
			}
		}
		return nil
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
					ContextWindow          int64 `json:"context_window"`
					ContextWindowTokens    int64 `json:"context_window_tokens"`
					MaxPromptTokens        int64 `json:"max_prompt_tokens"`
					MaxPrompt              int64 `json:"max_prompt"`
					MaxInputTokens         int64 `json:"max_input_tokens"`
				} `json:"limits"`
				Supports struct {
					ParallelToolCalls bool     `json:"parallel_tool_calls"`
					ReasoningEffort   []string `json:"reasoning_effort"`
					Vision            bool     `json:"vision"`
					ToolCalls         bool     `json:"tool_calls"`
				} `json:"supports"`
			} `json:"capabilities"`
			ModelPickerEnabled        bool   `json:"model_picker_enabled"`
			ModelPickerCategory       string `json:"model_picker_category"`
			ContextWindow             *int64 `json:"context_window,omitempty"`
			MaxContextWindow          *int64 `json:"max_context_window,omitempty"`
			AutoCompactTokenLimit     *int64 `json:"auto_compact_token_limit,omitempty"`
			EffectiveContextWindowPct *int64 `json:"effective_context_window_percent,omitempty"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}

		supportsResponses := supportsEndpoint(m.SupportedEndpoints, "/responses")

		visibility := "hide"
		if m.ModelPickerEnabled && supportsResponses {
			visibility = "list"
		}

		reasoningLevels := make([]reasoningPreset, 0, len(m.Capabilities.Supports.ReasoningEffort))
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

		promptWindow := firstPositivePtr(
			m.Capabilities.Limits.MaxPromptTokens,
			m.Capabilities.Limits.MaxPrompt,
			m.Capabilities.Limits.MaxInputTokens,
		)
		totalWindow := firstPositivePtr(
			m.Capabilities.Limits.MaxContextWindowTokens,
			m.Capabilities.Limits.ContextWindow,
			m.Capabilities.Limits.ContextWindowTokens,
		)

		ctxWindow := m.ContextWindow
		if ctxWindow == nil {
			ctxWindow = promptWindow
		}
		if ctxWindow == nil {
			ctxWindow = totalWindow
		}
		maxContextWindow := m.MaxContextWindow
		if maxContextWindow == nil {
			maxContextWindow = totalWindow
		}
		if maxContextWindow == nil {
			maxContextWindow = ctxWindow
		}
		effectiveContextWindowPct := int64(95)
		if m.EffectiveContextWindowPct != nil && *m.EffectiveContextWindowPct > 0 {
			effectiveContextWindowPct = *m.EffectiveContextWindowPct
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
			MaxContextWindow:            maxContextWindow,
			AutoCompactTokenLimit:       m.AutoCompactTokenLimit,
			EffectiveContextWindowPct:   effectiveContextWindowPct,
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
	_ = json.NewEncoder(w).Encode(models.AnthropicError{
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

var upstreamRequestIDHeaderNames = []string{"X-Request-Id", "X-Azure-Request-Id", "Openai-Request-Id"}

// UpstreamRequestID returns the first recognized upstream request id from headers.
func UpstreamRequestID(headers http.Header) string {
	for _, name := range upstreamRequestIDHeaderNames {
		if value := headers.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func writeOpenAIErrorWithRetryAfter(w http.ResponseWriter, status int, message, errType, retryAfter string, upstreamHeaders http.Header) {
	writeOpenAIErrorWithRetryAfterDetails(w, status, message, errType, retryAfter, upstreamHeaders, "", "")
}

func writeOpenAIErrorWithRetryAfterDetails(w http.ResponseWriter, status int, message, errType, retryAfter string, upstreamHeaders http.Header, param, code string) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	for _, name := range upstreamRequestIDHeaderNames {
		for _, value := range headerValuesCI(upstreamHeaders, name) {
			w.Header().Add(name, value)
		}
	}
	var paramValue interface{}
	if strings.TrimSpace(param) != "" {
		paramValue = strings.TrimSpace(param)
	}
	var codeValue interface{}
	if strings.TrimSpace(code) != "" {
		codeValue = strings.TrimSpace(code)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"param":   paramValue,
			"code":    codeValue,
		},
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	writeOpenAIErrorWithRetryAfter(w, status, message, errType, "", nil)
}

func writeOpenAIRequestBodyError(w http.ResponseWriter, status int, err error) {
	message := err.Error()
	code := ""
	if status == http.StatusRequestEntityTooLarge {
		code = "request_too_large"
	}
	writeOpenAIErrorWithDetails(w, status, message, "invalid_request_error", "", code)
}

func jsonDecodeErrorDetails(err error, fallback string) (string, string) {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && strings.TrimSpace(typeErr.Field) != "" {
		field := strings.TrimSpace(typeErr.Field)
		return fmt.Sprintf("invalid value for field %q: expected %s", field, typeErr.Type), field
	}
	return fallback, ""
}

func writeOpenAIErrorWithDetails(w http.ResponseWriter, status int, message, errType, param, code string) {
	writeOpenAIErrorWithRetryAfterDetails(w, status, message, errType, "", nil, param, code)
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
