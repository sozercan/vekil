package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultConfigApplyTimeout   = 2 * time.Minute
	defaultConfigStatusTTL      = 15 * time.Minute
	defaultConfigStatusMaxItems = 64
)

type ApplyState string

const (
	ApplyStateAccepted          ApplyState = "accepted"
	ApplyStateBuilding          ApplyState = "building"
	ApplyStateDiscovering       ApplyState = "discovering"
	ApplyStatePreflighting      ApplyState = "preflighting"
	ApplyStateEncoding          ApplyState = "encoding"
	ApplyStatePersisting        ApplyState = "persisting"
	ApplyStatePublishing        ApplyState = "publishing"
	ApplyStateSucceeded         ApplyState = "succeeded"
	ApplyStateFailedDecode      ApplyState = "failed_decode"
	ApplyStateFailedRevision    ApplyState = "failed_revision"
	ApplyStateFailedValidation  ApplyState = "failed_validation"
	ApplyStateFailedDiscovery   ApplyState = "failed_discovery"
	ApplyStateFailedPreflight   ApplyState = "failed_preflight"
	ApplyStateFailedEncoding    ApplyState = "failed_encoding"
	ApplyStateFailedPersistence ApplyState = "failed_persistence"
	ApplyStateTimedOut          ApplyState = "timed_out"
	ApplyStateCanceled          ApplyState = "canceled"
	ApplyStateCanceledShutdown  ApplyState = "canceled_shutdown"
)

type SecretOperation struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Value     string `json:"value,omitempty"`
}

type ApplyRequest struct {
	BaseRevision     string
	Config           ProvidersConfig
	SecretOperations []SecretOperation
}

type ResetRequest struct {
	BaseRevision string
}

type ApplyReceipt struct {
	ID       string     `json:"id"`
	State    ApplyState `json:"state"`
	Location string     `json:"location,omitempty"`
}

type ApplyStatus struct {
	ID         string                `json:"id"`
	State      ApplyState            `json:"state"`
	StartedAt  time.Time             `json:"started_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
	Generation uint64                `json:"generation,omitempty"`
	Revision   string                `json:"revision,omitempty"`
	Message    string                `json:"message,omitempty"`
	Warning    string                `json:"warning,omitempty"`
	Error      *DashboardConfigError `json:"error,omitempty"`
}

type DashboardConfigError struct {
	Code    ConfigErrorCode `json:"code"`
	Path    string          `json:"path,omitempty"`
	Message string          `json:"message"`
}

type RuntimeControl interface {
	Current() *runtimeSnapshot
	Submit(ApplyRequest) (ApplyReceipt, error)
	Status(string) (ApplyStatus, bool)
	Reset(ResetRequest) (ApplyReceipt, error)
	Shutdown(context.Context) error
}

type dashboardConfigSourceState struct {
	resolved       ResolvedProvidersConfig
	store          *ManagedProvidersConfigStore
	readOnlyReason string
}

type runtimeControl struct {
	h      *ProxyHandler
	source *dashboardConfigSourceState

	mu           sync.Mutex
	commitMu     sync.Mutex
	activeID     string
	activeCancel context.CancelCauseFunc
	statuses     map[string]ApplyStatus
	order        []string
	applyTimeout time.Duration
	statusTTL    time.Duration
	maxStatuses  int
	now          func() time.Time
}

var (
	errConfigApplyInProgress    = errors.New("dashboard config apply already in progress")
	errConfigRevisionMismatch   = errors.New("dashboard config revision mismatch")
	errConfigControlUnavailable = errors.New("dashboard config control is unavailable")
)

// WithDashboardConfigReadOnlyReason preserves the actionable startup reason
// when a long-lived server can expose the config document but cannot persist
// edits safely.
func WithDashboardConfigReadOnlyReason(reason error) Option {
	return func(h *ProxyHandler) {
		if h == nil || reason == nil {
			return
		}
		if h.dashboardConfigSource == nil {
			h.dashboardConfigSource = &dashboardConfigSourceState{}
		}
		h.dashboardConfigSource.readOnlyReason = strings.TrimSpace(reason.Error())
	}
}

// WithDashboardConfigSource attaches one resolved bootstrap/managed source to a
// long-lived handler. Server listener policy still decides whether HTTP access
// is available.
func WithDashboardConfigSource(resolved ResolvedProvidersConfig, store *ManagedProvidersConfigStore) Option {
	return func(h *ProxyHandler) {
		if h == nil {
			return
		}
		reason := ""
		if h.dashboardConfigSource != nil {
			reason = h.dashboardConfigSource.readOnlyReason
		}
		h.dashboardConfigSource = &dashboardConfigSourceState{resolved: resolved, store: store, readOnlyReason: reason}
		h.initialRuntimeRevision = strings.TrimSpace(resolved.Revision)
	}
}

func newRuntimeControl(h *ProxyHandler, source *dashboardConfigSourceState) *runtimeControl {
	return &runtimeControl{
		h:            h,
		source:       source,
		statuses:     make(map[string]ApplyStatus),
		applyTimeout: defaultConfigApplyTimeout,
		statusTTL:    defaultConfigStatusTTL,
		maxStatuses:  defaultConfigStatusMaxItems,
		now:          time.Now,
	}
}

func (c *runtimeControl) Current() *runtimeSnapshot {
	if c == nil || c.h == nil {
		return nil
	}
	return c.h.currentRuntime()
}

func (c *runtimeControl) Submit(request ApplyRequest) (ApplyReceipt, error) {
	if c == nil || c.h == nil || c.source == nil || c.source.store == nil {
		return ApplyReceipt{}, errConfigControlUnavailable
	}
	current := c.Current()
	if current == nil {
		return ApplyReceipt{}, errConfigControlUnavailable
	}
	baseRevision := strings.TrimSpace(request.BaseRevision)
	if baseRevision == "" || baseRevision != current.revision {
		return ApplyReceipt{}, errConfigRevisionMismatch
	}
	candidate, err := mergeDashboardConfigCandidate(current.config, request.Config, request.SecretOperations)
	if err != nil {
		return ApplyReceipt{}, err
	}
	if err := ValidateProvidersConfigTyped(candidate); err != nil {
		return ApplyReceipt{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneStatusesLocked()
	if c.activeID != "" {
		return ApplyReceipt{}, errConfigApplyInProgress
	}
	if latest := c.Current(); latest == nil || latest.revision != baseRevision {
		return ApplyReceipt{}, errConfigRevisionMismatch
	}
	if c.h.ShuttingDown() || !c.h.beginLifecycleWorker() {
		return ApplyReceipt{}, errProxyLifecycleShutdown
	}
	id := "apply_" + uuid.NewString()
	now := c.now()
	status := ApplyStatus{ID: id, State: ApplyStateAccepted, StartedAt: now, UpdatedAt: now}
	c.statuses[id] = status
	c.order = append(c.order, id)
	c.activeID = id
	go c.runApply(id, baseRevision, candidate, false)
	return ApplyReceipt{ID: id, State: ApplyStateAccepted, Location: "/dashboard/api/v1/config/applies/" + id}, nil
}

func (c *runtimeControl) Reset(request ResetRequest) (ApplyReceipt, error) {
	if c == nil || c.h == nil || c.source == nil || c.source.store == nil {
		return ApplyReceipt{}, errConfigControlUnavailable
	}
	current := c.Current()
	baseRevision := strings.TrimSpace(request.BaseRevision)
	if current == nil || baseRevision == "" || baseRevision != current.revision {
		return ApplyReceipt{}, errConfigRevisionMismatch
	}
	candidate := cloneProvidersConfigForValidation(c.source.resolved.Bootstrap.Config)
	if err := ValidateProvidersConfigTyped(candidate); err != nil {
		return ApplyReceipt{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneStatusesLocked()
	if c.activeID != "" {
		return ApplyReceipt{}, errConfigApplyInProgress
	}
	if latest := c.Current(); latest == nil || latest.revision != baseRevision {
		return ApplyReceipt{}, errConfigRevisionMismatch
	}
	if c.h.ShuttingDown() || !c.h.beginLifecycleWorker() {
		return ApplyReceipt{}, errProxyLifecycleShutdown
	}
	id := "apply_" + uuid.NewString()
	now := c.now()
	c.statuses[id] = ApplyStatus{ID: id, State: ApplyStateAccepted, StartedAt: now, UpdatedAt: now}
	c.order = append(c.order, id)
	c.activeID = id
	go c.runApply(id, baseRevision, candidate, true)
	return ApplyReceipt{ID: id, State: ApplyStateAccepted, Location: "/dashboard/api/v1/config/applies/" + id}, nil
}

func (c *runtimeControl) Status(id string) (ApplyStatus, bool) {
	if c == nil {
		return ApplyStatus{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneStatusesLocked()
	status, ok := c.statuses[strings.TrimSpace(id)]
	return status, ok
}

func (c *runtimeControl) Shutdown(ctx context.Context) error {
	if c == nil || c.h == nil {
		return nil
	}
	return c.h.WaitLifecycleWorkers(ctx)
}

func (c *runtimeControl) runApply(id, baseRevision string, cfg ProvidersConfig, reset bool) {
	defer c.h.endLifecycleWorker()
	causeCtx, cancelCause := context.WithCancelCause(c.h.lifecycleContext())
	ctx, cancelTimeout := context.WithTimeout(causeCtx, c.applyTimeout)
	c.setActiveCancel(id, cancelCause)
	defer func() {
		c.clearActiveCancel(id)
		cancelTimeout()
		cancelCause(context.Canceled)
	}()

	c.setState(id, ApplyStateBuilding, "")
	current := c.Current()
	generation := uint64(1)
	if current != nil {
		generation = current.generation + 1
	}
	c.setState(id, ApplyStateDiscovering, "")
	candidate, err := c.h.buildRuntimeSnapshot(ctx, cfg, generation, "", true)
	if err != nil {
		c.fail(id, ctx, ApplyStateFailedDiscovery, "candidate provider discovery failed", err)
		return
	}

	c.setState(id, ApplyStatePreflighting, "")
	if err := candidate.preflight(ctx); err != nil {
		c.fail(id, ctx, ApplyStateFailedPreflight, "candidate policy preflight failed", err)
		return
	}

	c.setState(id, ApplyStateEncoding, "")
	if _, err := EncodeProvidersConfigJSON(candidate.config); err != nil {
		c.fail(id, ctx, ApplyStateFailedEncoding, "candidate config encoding failed", err)
		return
	}

	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	c.h.runtimeCommitMu.Lock()
	defer c.h.runtimeCommitMu.Unlock()
	if err := ctx.Err(); err != nil {
		c.fail(id, ctx, ApplyStateCanceled, "candidate apply canceled", err)
		return
	}
	if c.h.ShuttingDown() {
		c.fail(id, ctx, ApplyStateCanceledShutdown, "server shutdown canceled candidate apply", errProxyLifecycleShutdown)
		return
	}
	if current := c.Current(); current == nil || current.revision != baseRevision || !c.ownsActiveJob(id) {
		c.fail(id, ctx, ApplyStateFailedRevision, "active config changed before commit", errConfigRevisionMismatch)
		return
	}

	c.setState(id, ApplyStatePersisting, "")
	var revision string
	var warning error
	if reset {
		result, err := c.source.store.Reset(ctx, baseRevision)
		if err != nil {
			c.fail(id, ctx, ApplyStateFailedPersistence, "managed config reset failed", err)
			return
		}
		revision = result.Revision
		warning = result.DurabilityWarning
		candidate.managedActive = false
	} else {
		result, err := c.source.store.Commit(ctx, baseRevision, candidate.config)
		if err != nil {
			c.fail(id, ctx, ApplyStateFailedPersistence, "managed config persistence failed", err)
			return
		}
		revision = result.Envelope.Revision
		warning = result.DurabilityWarning
		candidate.managedActive = true
	}

	candidate.revision = revision
	c.setState(id, ApplyStatePublishing, "")
	c.h.publishRuntime(candidate)
	c.succeed(id, candidate, warning)
}

func (c *runtimeControl) setActiveCancel(id string, cancel context.CancelCauseFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeID == id {
		c.activeCancel = cancel
	}
}

func (c *runtimeControl) clearActiveCancel(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeID == id {
		c.activeCancel = nil
	}
}

func (c *runtimeControl) cancelForShutdown() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.activeCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel(errProxyLifecycleShutdown)
	}
}

func (c *runtimeControl) setState(id string, state ApplyState, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.statuses[id]
	if !ok {
		return
	}
	status.State = state
	status.UpdatedAt = c.now()
	status.Message = message
	c.statuses[id] = status
}

func (c *runtimeControl) fail(id string, ctx context.Context, fallback ApplyState, message string, cause error) {
	state := fallback
	switch {
	case c.h != nil && c.h.ShuttingDown(), errors.Is(context.Cause(ctx), errProxyLifecycleShutdown):
		state = ApplyStateCanceledShutdown
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		state = ApplyStateTimedOut
	case errors.Is(cause, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		state = ApplyStateCanceled
	case errors.Is(cause, errConfigRevisionMismatch):
		state = ApplyStateFailedRevision
	default:
		var configErr *ConfigError
		if errors.As(cause, &configErr) && configErr.Code == ConfigErrorRevisionMismatch {
			state = ApplyStateFailedRevision
		}
	}
	configErr := configErrorForStatus(cause, state, message)
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.statuses[id]
	if ok {
		status.State = state
		status.UpdatedAt = c.now()
		status.Message = message
		status.Error = configErr
		c.statuses[id] = status
	}
	if c.activeID == id {
		c.activeID = ""
		c.activeCancel = nil
	}
}

func (c *runtimeControl) succeed(id string, snapshot *runtimeSnapshot, warning error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.statuses[id]
	if ok {
		status.State = ApplyStateSucceeded
		status.UpdatedAt = c.now()
		status.Generation = snapshot.generation
		status.Revision = snapshot.revision
		status.Message = "configuration applied"
		if warning != nil {
			status.Warning = "configuration committed; directory durability could not be confirmed"
		}
		c.statuses[id] = status
	}
	if c.activeID == id {
		c.activeID = ""
		c.activeCancel = nil
	}
}

func (c *runtimeControl) ownsActiveJob(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeID == id
}

func (c *runtimeControl) pruneStatusesLocked() {
	now := c.now()
	kept := c.order[:0]
	for _, id := range c.order {
		status, ok := c.statuses[id]
		if !ok {
			continue
		}
		terminal := applyStateTerminal(status.State)
		if terminal && c.statusTTL > 0 && now.Sub(status.UpdatedAt) > c.statusTTL {
			delete(c.statuses, id)
			continue
		}
		kept = append(kept, id)
	}
	c.order = kept
	for len(c.order) > c.maxStatuses {
		id := c.order[0]
		if id == c.activeID {
			break
		}
		delete(c.statuses, id)
		c.order = c.order[1:]
	}
}

func applyStateTerminal(state ApplyState) bool {
	return state == ApplyStateSucceeded || strings.HasPrefix(string(state), "failed_") || state == ApplyStateTimedOut || state == ApplyStateCanceled || state == ApplyStateCanceledShutdown
}

func configErrorForStatus(err error, state ApplyState, message string) *DashboardConfigError {
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return &DashboardConfigError{Code: configErr.Code, Path: configErr.Pointer, Message: message}
	}
	code := ConfigErrorCode("apply_failed")
	if state == ApplyStateFailedRevision {
		code = ConfigErrorRevisionMismatch
	}
	return &DashboardConfigError{Code: code, Message: message}
}

func mergeDashboardConfigCandidate(base, submitted ProvidersConfig, operations []SecretOperation) (ProvidersConfig, error) {
	candidate := cloneProvidersConfigForValidation(submitted)
	if strings.TrimSpace(candidate.InsightModel) != "" && candidate.InsightModel != base.InsightModel {
		return ProvidersConfig{}, NewConfigError(ConfigErrorCode("preserved_field"), "/insight_model", "insight_model is server-preserved", nil)
	}
	if !reflect.DeepEqual(candidate.ToolOptimizers, ToolOptimizersConfig{}) && !reflect.DeepEqual(candidate.ToolOptimizers, base.ToolOptimizers) {
		return ProvidersConfig{}, NewConfigError(ConfigErrorCode("preserved_field"), "/tool_optimizers", "tool_optimizers is server-preserved", nil)
	}
	candidate.InsightModel = base.InsightModel
	candidate.ToolOptimizers = base.ToolOptimizers
	for index, provider := range candidate.Providers {
		if rawURL := strings.TrimSpace(provider.BaseURL); rawURL != "" {
			parsed, err := url.Parse(rawURL)
			if err != nil {
				return ProvidersConfig{}, NewConfigError(ConfigErrorInvalidConfig, fmt.Sprintf("/providers/%d/base_url", index), "base_url is invalid", err)
			}
			if parsed.User != nil {
				return ProvidersConfig{}, NewConfigError(ConfigErrorInvalidConfig, fmt.Sprintf("/providers/%d/base_url", index), "base_url must not contain userinfo", nil)
			}
		}
	}

	opByPath := make(map[string]SecretOperation, len(operations))
	for _, operation := range operations {
		operation.Path = strings.TrimSpace(operation.Path)
		operation.Operation = strings.ToLower(strings.TrimSpace(operation.Operation))
		if operation.Path == "" {
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("invalid_secret_operation"), "/secret_operations", "secret operation path is required", nil)
		}
		if _, exists := opByPath[operation.Path]; exists {
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("invalid_secret_operation"), operation.Path, "secret operation path is duplicated", nil)
		}
		switch operation.Operation {
		case "keep", "set", "clear":
		default:
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("invalid_secret_operation"), operation.Path, "secret operation must be keep, set, or clear", nil)
		}
		if operation.Operation == "set" && (operation.Value == "" || operation.Value == "***") {
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("invalid_secret_operation"), operation.Path, "set requires a non-placeholder secret value", nil)
		}
		opByPath[operation.Path] = operation
	}

	baseProviders := make(map[string]ProviderConfig, len(base.Providers))
	for _, provider := range base.Providers {
		baseProviders[strings.TrimSpace(provider.ID)] = provider
	}
	consumed := make(map[string]struct{}, len(opByPath))
	for index := range candidate.Providers {
		provider := &candidate.Providers[index]
		providerID := strings.TrimSpace(provider.ID)
		baseProvider, hadBase := baseProviders[providerID]
		compatible := hadBase && dashboardSecretIdentityCompatible(baseProvider, *provider)

		apiPath := dashboardProviderSecretPath(providerID, "api_key")
		if provider.APIKey != "" && provider.APIKey != "***" {
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("secret_in_config"), apiPath, "raw secrets must use secret_operations", nil)
		}
		provider.APIKey = ""
		if operation, ok := opByPath[apiPath]; ok {
			consumed[apiPath] = struct{}{}
			if operation.Operation == "keep" && compatible && baseProvider.APIKey == "" && strings.TrimSpace(baseProvider.APIKeyEnv) != "" {
				provider.APIKey = ""
			} else {
				value, err := applyDashboardSecretOperation(operation, compatible, baseProvider.APIKey)
				if err != nil {
					return ProvidersConfig{}, err
				}
				provider.APIKey = value
			}
		} else if baseProvider.APIKey != "" {
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("missing_secret_operation"), apiPath, "configured secret requires an explicit keep, set, or clear operation", nil)
		}

		candidateHeaders := provider.ExtraHeaders
		if candidateHeaders == nil {
			candidateHeaders = map[string]string{}
		}
		headerNames := make(map[string]struct{})
		for name := range candidateHeaders {
			headerNames[name] = struct{}{}
		}
		for name := range baseProvider.ExtraHeaders {
			headerNames[name] = struct{}{}
		}
		orderedNames := make([]string, 0, len(headerNames))
		for name := range headerNames {
			orderedNames = append(orderedNames, name)
		}
		sort.Strings(orderedNames)
		mergedHeaders := make(map[string]string, len(orderedNames))
		for _, name := range orderedNames {
			path := dashboardProviderSecretPath(providerID, "extra_headers", name)
			if value := candidateHeaders[name]; value != "" && value != "***" {
				return ProvidersConfig{}, NewConfigError(ConfigErrorCode("secret_in_config"), path, "raw header secrets must use secret_operations", nil)
			}
			operation, ok := opByPath[path]
			if !ok {
				if _, configured := baseProvider.ExtraHeaders[name]; configured {
					return ProvidersConfig{}, NewConfigError(ConfigErrorCode("missing_secret_operation"), path, "configured header requires an explicit keep, set, or clear operation", nil)
				}
				continue
			}
			consumed[path] = struct{}{}
			value, err := applyDashboardSecretOperation(operation, compatible, baseProvider.ExtraHeaders[name])
			if err != nil {
				return ProvidersConfig{}, err
			}
			if operation.Operation != "clear" {
				mergedHeaders[name] = value
			}
		}
		if len(mergedHeaders) == 0 {
			provider.ExtraHeaders = nil
		} else {
			provider.ExtraHeaders = mergedHeaders
		}
	}
	for path := range opByPath {
		if _, ok := consumed[path]; !ok {
			return ProvidersConfig{}, NewConfigError(ConfigErrorCode("invalid_secret_operation"), path, "secret operation does not match the submitted provider config", nil)
		}
	}
	return candidate, nil
}

func applyDashboardSecretOperation(operation SecretOperation, compatible bool, prior string) (string, error) {
	switch operation.Operation {
	case "keep":
		if !compatible || prior == "" {
			return "", NewConfigError(ConfigErrorCode("incompatible_secret_keep"), operation.Path, "keep requires a matching provider/auth identity with a configured secret", nil)
		}
		return prior, nil
	case "set":
		return operation.Value, nil
	case "clear":
		return "", nil
	default:
		return "", NewConfigError(ConfigErrorCode("invalid_secret_operation"), operation.Path, "unsupported secret operation", nil)
	}
}

func dashboardSecretIdentityCompatible(base, candidate ProviderConfig) bool {
	return strings.TrimSpace(base.ID) == strings.TrimSpace(candidate.ID) &&
		strings.TrimSpace(base.Type) == strings.TrimSpace(candidate.Type) &&
		strings.TrimRight(strings.TrimSpace(base.BaseURL), "/") == strings.TrimRight(strings.TrimSpace(candidate.BaseURL), "/") &&
		strings.TrimSpace(base.AuthMode) == strings.TrimSpace(candidate.AuthMode) &&
		strings.TrimSpace(base.APIKeyEnv) == strings.TrimSpace(candidate.APIKeyEnv) &&
		strings.TrimSpace(base.AuthType) == strings.TrimSpace(candidate.AuthType) &&
		strings.TrimSpace(base.AuthHeader) == strings.TrimSpace(candidate.AuthHeader) &&
		strings.TrimSpace(base.AuthPrefix) == strings.TrimSpace(candidate.AuthPrefix) &&
		strings.TrimSpace(base.APIVersion) == strings.TrimSpace(candidate.APIVersion) &&
		strings.TrimSpace(base.TokenScope) == strings.TrimSpace(candidate.TokenScope)
}

func dashboardProviderSecretPath(providerID string, segments ...string) string {
	parts := []string{"providers", providerID}
	parts = append(parts, segments...)
	return JoinJSONPointer("", parts...)
}

func runtimeControlHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errConfigApplyInProgress):
		return http.StatusConflict
	case errors.Is(err, errConfigRevisionMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, errConfigControlUnavailable), errors.Is(err, errProxyLifecycleShutdown):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}
