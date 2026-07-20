package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const dashboardConfigBodyLimit = 1 << 20

type dashboardConfigReadResponse struct {
	Capability        DashboardConfigCapability   `json:"capability"`
	Revision          string                      `json:"revision,omitempty"`
	Generation        uint64                      `json:"generation,omitempty"`
	SchemaVersion     int                         `json:"schema_version,omitempty"`
	Source            *dashboardConfigSourceView  `json:"source,omitempty"`
	Policy            *dashboardConfigPolicyView  `json:"policy,omitempty"`
	Config            json.RawMessage             `json:"config,omitempty"`
	SecretStates      []dashboardSecretState      `json:"secret_states,omitempty"`
	PreservedPaths    []string                    `json:"preserved_paths,omitempty"`
	ProviderTypes     []dashboardProviderType     `json:"provider_capabilities,omitempty"`
	PolicyEligibility *dashboardPolicyEligibility `json:"policy_eligibility,omitempty"`
	CSRFToken         string                      `json:"csrf_token,omitempty"`
}

type dashboardConfigSourceView struct {
	Kind            ProvidersConfigSourceKind `json:"kind"`
	ID              string                    `json:"id"`
	BootstrapPath   string                    `json:"bootstrap_path,omitempty"`
	BootstrapDigest string                    `json:"bootstrap_digest"`
	ManagedPath     string                    `json:"managed_path,omitempty"`
	ManagedActive   bool                      `json:"managed_active"`
}

type dashboardConfigPolicyView struct {
	ProcessCeiling string                         `json:"process_ceiling"`
	Profiles       []dashboardConfigPolicyProfile `json:"profiles,omitempty"`
}

type dashboardConfigPolicyProfile struct {
	ID             string `json:"id"`
	PublicID       string `json:"public_id"`
	ConfiguredMode string `json:"configured_mode"`
	EffectiveMode  string `json:"effective_mode"`
}

type dashboardSecretState struct {
	Path   string `json:"path"`
	State  string `json:"state"`
	Source string `json:"source"`
}

type dashboardPolicyEligibility struct {
	TerminalRoutes   []dashboardEligibleRoute `json:"terminal_routes"`
	ClassifierRoutes []dashboardEligibleRoute `json:"classifier_routes"`
}

type dashboardEligibleRoute struct {
	ID       string `json:"id"`
	PublicID string `json:"public_id,omitempty"`
	Exposure string `json:"exposure"`
}

type dashboardProviderType struct {
	Type              string   `json:"type"`
	Fields            []string `json:"fields"`
	SecretFields      []string `json:"secret_fields"`
	SupportedAuth     []string `json:"supported_auth,omitempty"`
	SupportsDiscovery bool     `json:"supports_discovery"`
}

type dashboardConfigMutationEnvelope struct {
	BaseRevision     string            `json:"base_revision"`
	Config           json.RawMessage   `json:"config"`
	SecretOperations []SecretOperation `json:"secret_operations,omitempty"`
}

func (h *ProxyHandler) HandleDashboardConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	serveDashboardFile(w, "dashboard/config.html", "text/html; charset=utf-8", false)
}

func (h *ProxyHandler) HandleDashboardConfigRead(w http.ResponseWriter, r *http.Request) {
	capability := h.dashboardConfigCapability()
	response := dashboardConfigReadResponse{Capability: capability}
	if !capability.Available {
		writeDashboardConfigJSON(w, http.StatusOK, response)
		return
	}
	snapshot := h.runtimeForContext(r.Context())
	if snapshot == nil {
		writeDashboardConfigAPIError(w, http.StatusServiceUnavailable, NewConfigError(ConfigErrorCode("runtime_unavailable"), "", "active runtime is unavailable", nil))
		return
	}
	redacted, err := redactedProvidersConfigJSON(snapshot.config)
	if err != nil {
		writeDashboardConfigAPIError(w, http.StatusInternalServerError, NewConfigError(ConfigErrorCode("encoding_failed"), "", "active config could not be encoded", nil))
		return
	}
	response.Revision = snapshot.revision
	response.Generation = snapshot.generation
	response.SchemaVersion = snapshot.config.EffectiveSchemaVersion()
	response.Config = redacted
	response.SecretStates = dashboardConfigSecretStates(snapshot.config)
	response.PreservedPaths = []string{"/tool_optimizers", "/insight_model"}
	response.ProviderTypes = dashboardProviderCapabilities()
	response.Policy = h.dashboardPolicyView(snapshot)
	response.PolicyEligibility = dashboardPolicyEligibilityForSnapshot(snapshot)
	response.CSRFToken = h.dashboardConfigCSRFToken()
	if h.dashboardConfigSource != nil {
		source := h.dashboardConfigSource.resolved.Bootstrap.Source
		response.Source = &dashboardConfigSourceView{
			Kind:            source.Kind,
			ID:              source.ID,
			BootstrapPath:   source.BootstrapPath,
			BootstrapDigest: source.BootstrapDigest,
			ManagedPath:     source.ManagedPath,
			ManagedActive:   h.dashboardConfigSource.resolved.Managed || snapshot.revision != h.dashboardConfigSource.resolved.Bootstrap.Revision,
		}
	}
	w.Header().Set("ETag", strconv.Quote(snapshot.revision))
	writeDashboardConfigJSON(w, http.StatusOK, response)
}

func (h *ProxyHandler) HandleDashboardConfigValidate(w http.ResponseWriter, r *http.Request) {
	if !h.requireDashboardConfigWritable(w) {
		return
	}
	envelope, cfg, err := decodeDashboardConfigMutation(r)
	if err != nil {
		writeDashboardConfigAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := requireDashboardBaseRevision(r, envelope.BaseRevision, h.runtimeRevision()); err != nil {
		writeDashboardConfigAPIError(w, http.StatusPreconditionFailed, err)
		return
	}
	current := h.runtimeForContext(r.Context())
	if current == nil {
		writeDashboardConfigAPIError(w, http.StatusServiceUnavailable, errConfigControlUnavailable)
		return
	}
	candidate, err := mergeDashboardConfigCandidate(current.config, cfg, envelope.SecretOperations)
	if err == nil {
		err = ValidateProvidersConfigTyped(candidate)
	}
	if err != nil {
		writeDashboardConfigAPIError(w, http.StatusBadRequest, err)
		return
	}
	if h.runtimeRevision() != envelope.BaseRevision {
		writeDashboardConfigAPIError(w, http.StatusPreconditionFailed, errConfigRevisionMismatch)
		return
	}
	writeDashboardConfigJSON(w, http.StatusOK, map[string]any{
		"valid":          true,
		"base_revision":  current.revision,
		"schema_version": candidate.EffectiveSchemaVersion(),
	})
}

func (h *ProxyHandler) HandleDashboardConfigApply(w http.ResponseWriter, r *http.Request) {
	if !h.requireDashboardConfigWritable(w) {
		return
	}
	envelope, cfg, err := decodeDashboardConfigMutation(r)
	if err != nil {
		writeDashboardConfigAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := requireDashboardBaseRevision(r, envelope.BaseRevision, h.runtimeRevision()); err != nil {
		writeDashboardConfigAPIError(w, http.StatusPreconditionFailed, err)
		return
	}
	if h.runtimeControl == nil {
		writeDashboardConfigAPIError(w, http.StatusServiceUnavailable, errConfigControlUnavailable)
		return
	}
	receipt, err := h.runtimeControl.Submit(ApplyRequest{BaseRevision: envelope.BaseRevision, Config: cfg, SecretOperations: envelope.SecretOperations})
	if err != nil {
		writeDashboardConfigAPIError(w, runtimeControlHTTPStatus(err), err)
		return
	}
	w.Header().Set("Location", receipt.Location)
	writeDashboardConfigJSON(w, http.StatusAccepted, receipt)
}

func (h *ProxyHandler) HandleDashboardConfigApplyStatus(w http.ResponseWriter, r *http.Request) {
	if !h.dashboardConfigCapability().Available || h.runtimeControl == nil {
		writeDashboardConfigAPIError(w, http.StatusServiceUnavailable, errConfigControlUnavailable)
		return
	}
	status, ok := h.runtimeControl.Status(r.PathValue("id"))
	if !ok {
		writeDashboardConfigAPIError(w, http.StatusNotFound, NewConfigError(ConfigErrorCode("apply_not_found"), "", "apply status was not found or has expired", nil))
		return
	}
	writeDashboardConfigJSON(w, http.StatusOK, status)
}

func (h *ProxyHandler) HandleDashboardConfigReset(w http.ResponseWriter, r *http.Request) {
	if !h.requireDashboardConfigWritable(w) {
		return
	}
	baseRevision := unquoteETag(strings.TrimSpace(r.Header.Get("If-Match")))
	if baseRevision == "" || baseRevision != h.runtimeRevision() {
		writeDashboardConfigAPIError(w, http.StatusPreconditionFailed, errConfigRevisionMismatch)
		return
	}
	if h.runtimeControl == nil {
		writeDashboardConfigAPIError(w, http.StatusServiceUnavailable, errConfigControlUnavailable)
		return
	}
	receipt, err := h.runtimeControl.Reset(ResetRequest{BaseRevision: baseRevision})
	if err != nil {
		writeDashboardConfigAPIError(w, runtimeControlHTTPStatus(err), err)
		return
	}
	w.Header().Set("Location", receipt.Location)
	writeDashboardConfigJSON(w, http.StatusAccepted, receipt)
}

func (h *ProxyHandler) requireDashboardConfigWritable(w http.ResponseWriter) bool {
	capability := h.dashboardConfigCapability()
	if !capability.Available || !capability.Writable {
		message := capability.Reason
		if message == "" {
			message = "configuration control is read-only"
		}
		writeDashboardConfigAPIError(w, http.StatusServiceUnavailable, NewConfigError(ConfigErrorCode("config_read_only"), "", message, nil))
		return false
	}
	return true
}

func decodeDashboardConfigMutation(r *http.Request) (dashboardConfigMutationEnvelope, ProvidersConfig, error) {
	var envelope dashboardConfigMutationEnvelope
	if r == nil {
		return envelope, ProvidersConfig{}, NewConfigError(ConfigErrorInvalidJSON, "", "request is required", nil)
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return envelope, ProvidersConfig{}, NewConfigError(ConfigErrorInvalidJSON, "", "Content-Type must be application/json", err)
	}
	limited := &io.LimitedReader{R: r.Body, N: dashboardConfigBodyLimit + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return envelope, ProvidersConfig{}, NewConfigError(ConfigErrorInvalidJSON, "", "config request body is unreadable", err)
	}
	if len(body) > dashboardConfigBodyLimit {
		return envelope, ProvidersConfig{}, NewConfigError(ConfigErrorInvalidJSON, "", "config request body exceeds the size limit", nil)
	}
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return envelope, ProvidersConfig{}, WrapConfigError(ConfigErrorDuplicateField, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return envelope, ProvidersConfig{}, WrapConfigError(ConfigErrorInvalidJSON, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return envelope, ProvidersConfig{}, NewConfigError(ConfigErrorTrailingValue, "", "request contains a trailing JSON value", err)
	}
	if len(bytes.TrimSpace(envelope.Config)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Config), []byte("null")) {
		return envelope, ProvidersConfig{}, NewConfigError(ConfigErrorInvalidConfig, "/config", "config is required", nil)
	}
	cfg, err := DecodeProvidersConfigJSON(envelope.Config)
	if err != nil {
		var typed *ConfigError
		if errors.As(err, &typed) {
			pointer := "/config"
			if typed.Pointer != "" {
				pointer += typed.Pointer
			}
			return envelope, ProvidersConfig{}, NewConfigError(typed.Code, pointer, typed.Message, typed.Err)
		}
		return envelope, ProvidersConfig{}, err
	}
	return envelope, cfg, nil
}

func requireDashboardBaseRevision(r *http.Request, bodyRevision, activeRevision string) error {
	bodyRevision = strings.TrimSpace(bodyRevision)
	headerRevision := unquoteETag(strings.TrimSpace(r.Header.Get("If-Match")))
	if bodyRevision == "" || headerRevision == "" || bodyRevision != headerRevision || bodyRevision != activeRevision {
		return NewConfigError(ConfigErrorRevisionMismatch, "/base_revision", "base revision does not match the active config", errConfigRevisionMismatch)
	}
	return nil
}

func unquoteETag(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return value
}

func writeDashboardConfigJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDashboardConfigAPIError(w http.ResponseWriter, status int, err error) {
	apiError := DashboardConfigError{Code: ConfigErrorCode("request_failed"), Message: "request failed"}
	var typed *ConfigError
	switch {
	case errors.As(err, &typed):
		apiError.Code = typed.Code
		apiError.Path = typed.Pointer
		apiError.Message = typed.Message
	case errors.Is(err, errConfigApplyInProgress):
		apiError.Code = ConfigErrorCode("apply_in_progress")
		apiError.Message = "another configuration apply is already in progress"
	case errors.Is(err, errConfigRevisionMismatch):
		apiError.Code = ConfigErrorRevisionMismatch
		apiError.Message = "base revision does not match the active config"
	case errors.Is(err, errConfigControlUnavailable):
		apiError.Code = ConfigErrorCode("config_unavailable")
		apiError.Message = "configuration control is unavailable"
	case errors.Is(err, errProxyLifecycleShutdown):
		apiError.Code = ConfigErrorCode("shutting_down")
		apiError.Message = "server is shutting down"
	}
	writeDashboardConfigJSON(w, status, map[string]any{"error": apiError})
}

func redactedProvidersConfigJSON(cfg ProvidersConfig) (json.RawMessage, error) {
	body, err := EncodeProvidersConfigJSON(cfg)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	providers, _ := document["providers"].([]any)
	for _, rawProvider := range providers {
		provider, _ := rawProvider.(map[string]any)
		delete(provider, "api_key")
		if headers, ok := provider["extra_headers"].(map[string]any); ok {
			for key := range headers {
				headers[key] = ""
			}
		}
	}
	redacted, err := json.Marshal(document)
	return json.RawMessage(redacted), err
}

func dashboardConfigSecretStates(cfg ProvidersConfig) []dashboardSecretState {
	states := make([]dashboardSecretState, 0)
	for _, provider := range cfg.Providers {
		id := strings.TrimSpace(provider.ID)
		switch {
		case provider.APIKey != "":
			states = append(states, dashboardSecretState{Path: dashboardProviderSecretPath(id, "api_key"), State: "configured", Source: "inline"})
		case strings.TrimSpace(provider.APIKeyEnv) != "":
			states = append(states, dashboardSecretState{Path: dashboardProviderSecretPath(id, "api_key"), State: "configured", Source: "env"})
		default:
			states = append(states, dashboardSecretState{Path: dashboardProviderSecretPath(id, "api_key"), State: "clear", Source: "none"})
		}
		headerNames := make([]string, 0, len(provider.ExtraHeaders))
		for name := range provider.ExtraHeaders {
			headerNames = append(headerNames, name)
		}
		sort.Strings(headerNames)
		for _, name := range headerNames {
			states = append(states, dashboardSecretState{Path: dashboardProviderSecretPath(id, "extra_headers", name), State: "configured", Source: "inline"})
		}
	}
	return states
}

func dashboardProviderCapabilities() []dashboardProviderType {
	common := []string{"id", "type", "default", "include_models", "exclude_models", "base_url", "auth_mode", "api_key_env", "api_version", "token_scope", "auth_type", "auth_header", "auth_prefix", "extra_headers", "chat_completions_path", "responses_path", "messages_path", "models_path", "model_discovery", "legacy_catalog", "trust_domain", "classifier_no_store_supported", "copilot_headers", "models"}
	return []dashboardProviderType{
		{Type: string(providerTypeCopilot), Fields: common, SecretFields: []string{"api_key", "extra_headers"}, SupportedAuth: []string{"copilot"}, SupportsDiscovery: true},
		{Type: string(providerTypeAzureOpenAI), Fields: common, SecretFields: []string{"api_key", "extra_headers"}, SupportedAuth: []string{string(providerAuthModeAPIKey), string(providerAuthModeAzureIdentity)}, SupportsDiscovery: true},
		{Type: string(providerTypeOpenAICodex), Fields: common, SecretFields: []string{"extra_headers"}, SupportedAuth: []string{"chatgpt_auth_file"}, SupportsDiscovery: true},
		{Type: string(providerTypeOpenAICompatible), Fields: common, SecretFields: []string{"api_key", "extra_headers"}, SupportedAuth: []string{string(providerAuthTypeBearer), string(providerAuthTypeAPIKeyHeader), string(providerAuthTypeNone)}, SupportsDiscovery: true},
		{Type: string(providerTypeAnthropicCompatible), Fields: common, SecretFields: []string{"api_key", "extra_headers"}, SupportedAuth: []string{string(providerAuthTypeBearer), string(providerAuthTypeAPIKeyHeader), string(providerAuthTypeNone)}, SupportsDiscovery: false},
	}
}

func (h *ProxyHandler) dashboardPolicyView(snapshot *runtimeSnapshot) *dashboardConfigPolicyView {
	view := &dashboardConfigPolicyView{ProcessCeiling: string(h.policyRoutingMode)}
	controller, _ := snapshot.policy.controller.(*chatPolicyRoutingController)
	for _, profile := range snapshot.config.PolicyProfiles {
		effective := policyModeOff.String()
		if controller != nil {
			if compiled := controller.profiles[profile.ID]; compiled != nil {
				effective = compiled.effectiveMode().String()
			}
		}
		view.Profiles = append(view.Profiles, dashboardConfigPolicyProfile{ID: profile.ID, PublicID: profile.PublicID, ConfiguredMode: profile.Mode, EffectiveMode: effective})
	}
	return view
}

func (h *ProxyHandler) dashboardConfigDebugString() string {
	return fmt.Sprintf("generation=%d revision=%s", h.runtimeGeneration(), h.runtimeRevision())
}

func dashboardPolicyEligibilityForSnapshot(snapshot *runtimeSnapshot) *dashboardPolicyEligibility {
	view := &dashboardPolicyEligibility{}
	if snapshot == nil || snapshot.providers == nil {
		return view
	}
	for _, configured := range snapshot.config.ModelRoutes {
		route, ok := snapshot.providers.lookupTerminalRoute(configured.ID)
		if !ok || route == nil {
			continue
		}
		entry := dashboardEligibleRoute{ID: configured.ID, PublicID: configured.PublicID, Exposure: route.exposure}
		if route.internalPurpose == modelRouteInternalPurposePolicyClassifier && route.exposure == modelRouteExposureInternal && route.supportsEndpoint(providerEndpointChatCompletions) {
			view.ClassifierRoutes = append(view.ClassifierRoutes, entry)
			continue
		}
		if route.isPublic() && route.supportsEndpoint(providerEndpointChatCompletions) {
			view.TerminalRoutes = append(view.TerminalRoutes, entry)
		}
	}
	return view
}
