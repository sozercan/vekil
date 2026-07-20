package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ConfigErrorCode is a stable machine-readable provider-config error category.
type ConfigErrorCode string

const (
	ConfigErrorEmpty                    ConfigErrorCode = "config_empty"
	ConfigErrorInvalidJSON              ConfigErrorCode = "invalid_json"
	ConfigErrorDuplicateField           ConfigErrorCode = "duplicate_field"
	ConfigErrorUnknownField             ConfigErrorCode = "unknown_field"
	ConfigErrorTrailingValue            ConfigErrorCode = "trailing_value"
	ConfigErrorInvalidConfig            ConfigErrorCode = "invalid_config"
	ConfigErrorInvalidSource            ConfigErrorCode = "invalid_source"
	ConfigErrorManagedSourceConflict    ConfigErrorCode = "managed_source_conflict"
	ConfigErrorRevisionMismatch         ConfigErrorCode = "revision_mismatch"
	ConfigErrorUnsupportedManagedSchema ConfigErrorCode = "unsupported_managed_schema"
	ConfigErrorManagedEnvelope          ConfigErrorCode = "invalid_managed_envelope"
	ConfigErrorManagedStore             ConfigErrorCode = "managed_store"
)

// ConfigError preserves a legacy error message while adding a stable code and
// an optional RFC 6901 JSON Pointer for dashboard and control-plane callers.
type ConfigError struct {
	Code    ConfigErrorCode
	Pointer string
	Message string
	Err     error
}

func (e *ConfigError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Code)
}

func (e *ConfigError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewConfigError constructs a typed config error. pointer must be empty or an
// RFC 6901 JSON Pointer.
func NewConfigError(code ConfigErrorCode, pointer, message string, cause error) *ConfigError {
	return &ConfigError{
		Code:    code,
		Pointer: normalizeJSONPointer(pointer),
		Message: message,
		Err:     cause,
	}
}

// WrapConfigError adds stable metadata to an existing error without replacing
// metadata already supplied by a ConfigError.
func WrapConfigError(code ConfigErrorCode, err error) error {
	if err == nil {
		return nil
	}
	var typed *ConfigError
	if errors.As(err, &typed) {
		return err
	}
	return NewConfigError(code, legacyConfigErrorPointer(err), err.Error(), err)
}

// JoinJSONPointer appends raw path segments to an RFC 6901 JSON Pointer.
func JoinJSONPointer(pointer string, segments ...string) string {
	pointer = normalizeJSONPointer(pointer)
	for _, segment := range segments {
		pointer += "/" + escapeJSONPointerToken(segment)
	}
	return pointer
}

// ConfigPathToJSONPointer converts the repository's legacy dotted/indexed
// config paths (for example policy_profiles[0].classifier.recent_turns) into
// RFC 6901 JSON Pointers. Invalid paths return an empty string.
func ConfigPathToJSONPointer(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") {
		return normalizeJSONPointer(path)
	}

	var segments []string
	for index := 0; index < len(path); {
		switch path[index] {
		case '.':
			index++
			if index == len(path) {
				return ""
			}
		case '[':
			end, value, ok := parseConfigPathBracket(path, index)
			if !ok {
				return ""
			}
			segments = append(segments, value)
			index = end
		default:
			start := index
			for index < len(path) && path[index] != '.' && path[index] != '[' {
				index++
			}
			if start == index {
				return ""
			}
			segment := strings.TrimSpace(path[start:index])
			if segment == "" || strings.ContainsAny(segment, " \t\r\n") {
				return ""
			}
			segments = append(segments, segment)
		}
	}

	pointer := ""
	for _, segment := range segments {
		pointer = JoinJSONPointer(pointer, segment)
	}
	return pointer
}

func parseConfigPathBracket(path string, start int) (int, string, bool) {
	if start >= len(path) || path[start] != '[' {
		return start, "", false
	}
	index := start + 1
	if index >= len(path) {
		return start, "", false
	}
	if path[index] == '"' {
		stringStart := index
		index++
		escaped := false
		for index < len(path) {
			char := path[index]
			if escaped {
				escaped = false
				index++
				continue
			}
			if char == '\\' {
				escaped = true
				index++
				continue
			}
			if char == '"' {
				index++
				break
			}
			index++
		}
		if index >= len(path) || path[index] != ']' {
			return start, "", false
		}
		var value string
		if err := json.Unmarshal([]byte(path[stringStart:index]), &value); err != nil {
			return start, "", false
		}
		return index + 1, value, true
	}

	numberStart := index
	for index < len(path) && path[index] >= '0' && path[index] <= '9' {
		index++
	}
	if numberStart == index || index >= len(path) || path[index] != ']' {
		return start, "", false
	}
	return index + 1, path[numberStart:index], true
}

func normalizeJSONPointer(pointer string) string {
	pointer = strings.TrimSpace(pointer)
	if pointer == "" {
		return ""
	}
	if strings.HasPrefix(pointer, "/") {
		return pointer
	}
	return ConfigPathToJSONPointer(pointer)
}

func escapeJSONPointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

func legacyConfigErrorPointer(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	colon := strings.Index(message, ":")
	if colon <= 0 {
		return ""
	}
	candidate := strings.TrimSpace(message[:colon])
	if candidate == "" || candidate == "json" || strings.ContainsAny(candidate, " \t\r\n") {
		return ""
	}
	return ConfigPathToJSONPointer(candidate)
}

func classifyJSONDecodeError(err error, fallback ConfigErrorCode) ConfigErrorCode {
	if err == nil {
		return fallback
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "duplicate mapping key"):
		return ConfigErrorDuplicateField
	case strings.Contains(message, "unknown field"):
		return ConfigErrorUnknownField
	default:
		return fallback
	}
}

// DecodeProvidersConfigJSON strictly decodes one provider-config JSON object.
// It preserves the same duplicate-key, nested unknown-field, trailing-value,
// and field-presence behavior as the JSON branch of LoadProvidersConfigFile.
// Semantic validation is intentionally separate.
func DecodeProvidersConfigJSON(body []byte) (ProvidersConfig, error) {
	var cfg ProvidersConfig
	if len(bytes.TrimSpace(body)) == 0 {
		err := fmt.Errorf("providers config JSON is empty")
		return cfg, NewConfigError(ConfigErrorEmpty, "", err.Error(), err)
	}
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return cfg, NewConfigError(
			classifyJSONDecodeError(err, ConfigErrorDuplicateField),
			legacyConfigErrorPointer(err),
			err.Error(),
			err,
		)
	}
	if err := validateJSONConfigFieldPaths(body); err != nil {
		return cfg, NewConfigError(
			classifyJSONDecodeError(err, ConfigErrorInvalidJSON),
			legacyConfigErrorPointer(err),
			err.Error(),
			err,
		)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return ProvidersConfig{}, NewConfigError(
			classifyJSONDecodeError(err, ConfigErrorInvalidJSON),
			legacyConfigErrorPointer(err),
			err.Error(),
			err,
		)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("more than one JSON value")
		} else {
			err = fmt.Errorf("trailing JSON value: %w", err)
		}
		return ProvidersConfig{}, NewConfigError(ConfigErrorTrailingValue, "", err.Error(), err)
	}

	present, err := jsonTopLevelConfigFields(body)
	if err != nil {
		return ProvidersConfig{}, NewConfigError(ConfigErrorInvalidJSON, "", err.Error(), err)
	}
	cfg.schemaVersionSet = present["schema_version"]
	cfg.modelRoutesSet = present["model_routes"]
	cfg.policyProfilesSet = present["policy_profiles"]
	markJSONProvidersConfigFieldPresence(body, &cfg)
	return cfg, nil
}

// ValidateProvidersConfigTyped applies the existing network-free validation and
// returns typed path metadata without changing legacy validation behavior.
func ValidateProvidersConfigTyped(cfg ProvidersConfig) error {
	return WrapConfigError(ConfigErrorInvalidConfig, ValidateProvidersConfig(cfg))
}

func normalizeProvidersConfigForPersistence(cfg ProvidersConfig) (ProvidersConfig, error) {
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		return ProvidersConfig{}, WrapConfigError(ConfigErrorInvalidConfig, err)
	}
	if err := ValidateProvidersConfig(validated.config); err != nil {
		return ProvidersConfig{}, WrapConfigError(ConfigErrorInvalidConfig, err)
	}
	return validated.config, nil
}

// EncodeProvidersConfigJSON returns deterministic canonical JSON without ever
// marshaling ProvidersConfig directly. The custom codec preserves private
// presence state when it changes behavior, including explicit route budgets,
// policy-classifier zero values, nullable schema-gated fields, and tool output
// optimizer zero values.
func EncodeProvidersConfigJSON(cfg ProvidersConfig) ([]byte, error) {
	encoded, err := json.Marshal(canonicalProvidersConfigValue(cfg))
	if err != nil {
		return nil, NewConfigError(ConfigErrorInvalidConfig, "", fmt.Sprintf("encode providers config: %v", err), err)
	}
	return encoded, nil
}

// ProvidersConfigDigest returns the canonical secretful semantic source digest.
func ProvidersConfigDigest(cfg ProvidersConfig) (string, error) {
	body, err := EncodeProvidersConfigJSON(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ProvidersConfigRevision returns a deterministic optimistic-concurrency token
// for the complete secretful configuration.
func ProvidersConfigRevision(cfg ProvidersConfig) (string, error) {
	body, err := EncodeProvidersConfigJSON(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "cfg_" + hex.EncodeToString(sum[:]), nil
}

func canonicalProvidersConfigValue(cfg ProvidersConfig) map[string]interface{} {
	root := map[string]interface{}{
		"providers":       canonicalProviderConfigs(cfg.Providers),
		"tool_optimizers": canonicalToolOptimizersConfig(cfg.ToolOptimizers),
	}
	if cfg.schemaVersionSet || cfg.SchemaVersion != 0 {
		root["schema_version"] = cfg.SchemaVersion
	}
	if cfg.modelRoutesSet || cfg.ModelRoutes != nil {
		root["model_routes"] = canonicalModelRouteConfigs(cfg.ModelRoutes)
	}
	if cfg.policyProfilesSet || cfg.PolicyProfiles != nil {
		root["policy_profiles"] = canonicalPolicyProfileConfigs(cfg.PolicyProfiles)
	}
	if cfg.InsightModel != "" {
		root["insight_model"] = cfg.InsightModel
	}
	return root
}

func canonicalProviderConfigs(configs []ProviderConfig) []interface{} {
	out := make([]interface{}, len(configs))
	for index, cfg := range configs {
		out[index] = canonicalProviderConfig(cfg)
	}
	return out
}

func canonicalProviderConfig(cfg ProviderConfig) map[string]interface{} {
	value := map[string]interface{}{
		"id":      cfg.ID,
		"type":    cfg.Type,
		"default": cfg.Default,
	}
	putString(value, "base_url", cfg.BaseURL)
	putString(value, "auth_mode", cfg.AuthMode)
	putString(value, "api_key", cfg.APIKey)
	putString(value, "api_key_env", cfg.APIKeyEnv)
	putString(value, "api_version", cfg.APIVersion)
	putString(value, "token_scope", cfg.TokenScope)
	putString(value, "auth_type", cfg.AuthType)
	putString(value, "auth_header", cfg.AuthHeader)
	putString(value, "auth_prefix", cfg.AuthPrefix)
	putString(value, "chat_completions_path", cfg.ChatCompletionsPath)
	putString(value, "responses_path", cfg.ResponsesPath)
	putString(value, "messages_path", cfg.MessagesPath)
	putString(value, "models_path", cfg.ModelsPath)
	putString(value, "model_discovery", cfg.ModelDiscovery)
	if cfg.trustDomainSet || cfg.TrustDomain != "" {
		value["trust_domain"] = cfg.TrustDomain
	}
	putOptionalBool(value, "classifier_no_store_supported", cfg.ClassifierNoStoreSupported, cfg.classifierNoStoreSupportedSet)
	putStringSlice(value, "include_models", cfg.IncludeModels)
	putStringSlice(value, "exclude_models", cfg.ExcludeModels)
	if cfg.ExtraHeaders != nil {
		headers := make(map[string]string, len(cfg.ExtraHeaders))
		for key, headerValue := range cfg.ExtraHeaders {
			headers[key] = headerValue
		}
		value["extra_headers"] = headers
	}
	if headers := canonicalCopilotHeaderProfiles(cfg.Headers); headers != nil {
		value["headers"] = headers
	}
	if cfg.Models != nil {
		models := make([]interface{}, len(cfg.Models))
		for index, model := range cfg.Models {
			models[index] = canonicalProviderModelConfig(model)
		}
		value["models"] = models
	}
	return value
}

func canonicalProviderModelConfig(cfg ProviderModelConfig) map[string]interface{} {
	value := map[string]interface{}{"public_id": cfg.PublicID}
	putString(value, "deployment", cfg.Deployment)
	putString(value, "name", cfg.Name)
	putStringSlice(value, "endpoints", cfg.Endpoints)
	putBoolPtr(value, "model_picker_enabled", cfg.ModelPickerEnabled)
	putString(value, "model_picker_category", cfg.ModelPickerCategory)
	putStringSlice(value, "reasoning_effort", cfg.ReasoningEffort)
	putBoolPtr(value, "vision", cfg.Vision)
	putBoolPtr(value, "parallel_tool_calls", cfg.ParallelToolCalls)
	putBoolPtr(value, "drop_sampling_params", cfg.DropSamplingParams)
	putBoolPtr(value, "use_max_completion_tokens", cfg.UseMaxCompletionTokens)
	if cfg.ContextWindow != nil {
		value["context_window"] = *cfg.ContextWindow
	}
	return value
}

func canonicalCopilotHeaderProfiles(cfg CopilotHeaderProfilesConfig) map[string]interface{} {
	value := make(map[string]interface{})
	if profile := canonicalCopilotHeaderConfig(cfg.Default); profile != nil {
		value["default"] = profile
	}
	if profile := canonicalCopilotHeaderConfig(cfg.ChatCompletions); profile != nil {
		value["chat_completions"] = profile
	}
	if profile := canonicalCopilotHeaderConfig(cfg.Responses); profile != nil {
		value["responses"] = profile
	}
	if len(value) == 0 {
		return nil
	}
	return value
}

func canonicalCopilotHeaderConfig(cfg CopilotHeaderConfig) map[string]interface{} {
	value := make(map[string]interface{})
	putString(value, "editor_version", cfg.EditorVersion)
	putString(value, "editor_plugin_version", cfg.EditorPluginVersion)
	putString(value, "user_agent", cfg.UserAgent)
	putString(value, "copilot_integration_id", cfg.IntegrationID)
	putString(value, "github_api_version", cfg.GitHubAPIVersion)
	putString(value, "openai_intent", cfg.OpenAIIntent)
	if len(value) == 0 {
		return nil
	}
	return value
}

func canonicalModelRouteConfigs(configs []ModelRouteConfig) []interface{} {
	out := make([]interface{}, len(configs))
	for index, cfg := range configs {
		value := map[string]interface{}{
			"id":      cfg.ID,
			"targets": canonicalModelRouteTargets(cfg.Targets),
		}
		if cfg.Endpoints == nil {
			value["endpoints"] = nil
		} else {
			value["endpoints"] = append([]string{}, cfg.Endpoints...)
		}
		if cfg.exposureSet || cfg.Exposure != "" {
			value["exposure"] = cfg.Exposure
		}
		if cfg.internalPurposeSet || cfg.InternalPurpose != "" {
			value["internal_purpose"] = cfg.InternalPurpose
		}
		if cfg.publicIDSet || cfg.PublicID != "" {
			value["public_id"] = cfg.PublicID
		}
		putString(value, "name", cfg.Name)
		putStringSlice(value, "reasoning_effort", cfg.ReasoningEffort)
		putBoolPtr(value, "parallel_tool_calls", cfg.ParallelToolCalls)
		putBoolPtr(value, "vision", cfg.Vision)
		if cfg.ContextWindow != nil {
			value["context_window"] = *cfg.ContextWindow
		}
		putOptionalBool(value, "model_picker_enabled", cfg.ModelPickerEnabled, cfg.modelPickerEnabledSet)
		if cfg.modelPickerCategorySet || cfg.ModelPickerCategory != "" {
			value["model_picker_category"] = cfg.ModelPickerCategory
		}
		putBoolPtr(value, "drop_sampling_params", cfg.DropSamplingParams)
		if routing := canonicalModelRouteRouting(cfg.Routing); routing != nil {
			value["routing"] = routing
		}
		out[index] = value
	}
	return out
}

func canonicalModelRouteTargets(targets []ModelRouteTargetConfig) []interface{} {
	out := make([]interface{}, len(targets))
	for index, target := range targets {
		value := map[string]interface{}{
			"id":             target.ID,
			"provider":       target.Provider,
			"upstream_model": target.UpstreamModel,
		}
		putBoolPtr(value, "use_max_completion_tokens", target.UseMaxCompletionTokens)
		out[index] = value
	}
	return out
}

func canonicalModelRouteRouting(cfg ModelRouteRoutingConfig) map[string]interface{} {
	value := make(map[string]interface{})
	if cfg.modeSet || cfg.Mode != "" {
		value["mode"] = cfg.Mode
	}
	if cfg.maxTargetAttemptsSet || cfg.MaxTargetAttempts != 0 {
		value["max_target_attempts"] = cfg.MaxTargetAttempts
	}
	if cfg.maxUpstreamSendsSet || cfg.MaxUpstreamSends != 0 {
		value["max_upstream_sends"] = cfg.MaxUpstreamSends
	}
	if len(value) == 0 {
		return nil
	}
	return value
}

func canonicalPolicyProfileConfigs(configs []PolicyProfileConfig) []interface{} {
	out := make([]interface{}, len(configs))
	for index, cfg := range configs {
		value := map[string]interface{}{
			"id":                cfg.ID,
			"public_id":         cfg.PublicID,
			"lightweight_route": cfg.LightweightRoute,
			"powerful_route":    cfg.PowerfulRoute,
			"classifier":        canonicalPolicyClassifierConfig(cfg.Classifier),
			"data_policy":       canonicalPolicyDataPolicyConfig(cfg.DataPolicy),
		}
		putString(value, "name", cfg.Name)
		putString(value, "mode", cfg.Mode)
		putBoolPtr(value, "model_picker_enabled", cfg.ModelPickerEnabled)
		putString(value, "model_picker_category", cfg.ModelPickerCategory)
		putString(value, "baseline_tier", cfg.BaselineTier)
		putString(value, "classifier_unavailable_tier", cfg.ClassifierUnavailableTier)
		putString(value, "classifier_uncertain_tier", cfg.ClassifierUncertainTier)
		out[index] = value
	}
	return out
}

func canonicalPolicyClassifierConfig(cfg PolicyClassifierConfig) map[string]interface{} {
	value := map[string]interface{}{"route": cfg.Route}
	putString(value, "profile", cfg.Profile)
	putOptionalInt(value, "timeout_ms", cfg.TimeoutMS, cfg.timeoutMSSet, cfg.timeoutMSNull)
	putOptionalInt(value, "max_completion_tokens", cfg.MaxCompletionTokens, cfg.maxCompletionTokensSet, cfg.maxCompletionTokensNull)
	putOptionalInt(value, "max_request_bytes", cfg.MaxRequestBytes, cfg.maxRequestBytesSet, cfg.maxRequestBytesNull)
	putOptionalInt(value, "recent_turns", cfg.RecentTurns, cfg.recentTurnsSet, cfg.recentTurnsNull)
	putOptionalInt(value, "max_concurrency", cfg.MaxConcurrency, cfg.maxConcurrencySet, cfg.maxConcurrencyNull)
	if cfg.observeSampleRateNull {
		value["observe_sample_rate"] = nil
	} else if cfg.observeSampleRateSet || cfg.ObserveSampleRate != 0 {
		value["observe_sample_rate"] = cfg.ObserveSampleRate
	}
	return value
}

func canonicalPolicyDataPolicyConfig(cfg PolicyDataPolicyConfig) map[string]interface{} {
	return map[string]interface{}{
		"content_forwarding_acknowledged": cfg.ContentForwardingAcknowledged,
		"allow_cross_trust_domain":        cfg.AllowCrossTrustDomain,
		"allow_provider_retention":        cfg.AllowProviderRetention,
	}
}

func canonicalToolOptimizersConfig(cfg ToolOptimizersConfig) map[string]interface{} {
	value := map[string]interface{}{
		"enabled": cfg.Enabled,
		"command_rewrite": map[string]interface{}{
			"enabled": cfg.CommandRewrite.Enabled,
		},
		"output_reduce": map[string]interface{}{
			"enabled": cfg.OutputReduce.Enabled,
		},
	}
	command := value["command_rewrite"].(map[string]interface{})
	putString(command, "streaming_mode", cfg.CommandRewrite.StreamingMode)
	if cfg.CommandRewrite.TimeoutMS != 0 {
		command["timeout_ms"] = cfg.CommandRewrite.TimeoutMS
	}
	output := value["output_reduce"].(map[string]interface{})
	if cfg.OutputReduce.timeoutMSSet || cfg.OutputReduce.TimeoutMS != 0 {
		output["timeout_ms"] = cfg.OutputReduce.TimeoutMS
	}
	if cfg.OutputReduce.minInputBytesSet || cfg.OutputReduce.MinInputBytes != 0 {
		output["min_input_bytes"] = cfg.OutputReduce.MinInputBytes
	}
	if cfg.OutputReduce.maxInputBytesSet || cfg.OutputReduce.MaxInputBytes != 0 {
		output["max_input_bytes"] = cfg.OutputReduce.MaxInputBytes
	}

	if shell := canonicalToolOptimizerShellConfig(cfg.Tools.ShellFunctionCalls); shell != nil {
		value["tools"] = map[string]interface{}{"shell_function_calls": shell}
	}
	if cfg.Providers != nil {
		providers := make([]interface{}, len(cfg.Providers))
		for index, provider := range cfg.Providers {
			providers[index] = canonicalToolOptimizerProviderConfig(provider)
		}
		value["providers"] = providers
	}
	return value
}

func canonicalToolOptimizerShellConfig(cfg ToolOptimizerShellFunctionCallsConfig) map[string]interface{} {
	value := make(map[string]interface{})
	putBoolPtr(value, "enabled", cfg.Enabled)
	putStringSlice(value, "names", cfg.Names)
	putString(value, "command_arg_path", cfg.CommandArgPath)
	if len(value) == 0 {
		return nil
	}
	return value
}

func canonicalToolOptimizerProviderConfig(cfg ToolOptimizerProviderConfig) map[string]interface{} {
	value := map[string]interface{}{
		"id":   cfg.ID,
		"type": cfg.Type,
	}
	putBoolPtr(value, "enabled", cfg.Enabled)
	putString(value, "path", cfg.Path)
	putStringSlice(value, "args", cfg.Args)
	putStringSlice(value, "stages", cfg.Stages)
	if cfg.MaxStdoutBytes != 0 {
		value["max_stdout_bytes"] = cfg.MaxStdoutBytes
	}
	if cfg.MaxStderrBytes != 0 {
		value["max_stderr_bytes"] = cfg.MaxStderrBytes
	}
	return value
}

func putString(object map[string]interface{}, key, value string) {
	if value != "" {
		object[key] = value
	}
}

func putStringSlice(object map[string]interface{}, key string, values []string) {
	if values != nil {
		object[key] = append([]string{}, values...)
	}
}

func putBoolPtr(object map[string]interface{}, key string, value *bool) {
	if value != nil {
		object[key] = *value
	}
}

func putOptionalBool(object map[string]interface{}, key string, value *bool, present bool) {
	if value != nil {
		object[key] = *value
	} else if present {
		object[key] = nil
	}
}

func putOptionalInt(object map[string]interface{}, key string, value int, present, isNull bool) {
	if isNull {
		object[key] = nil
	} else if present || value != 0 {
		object[key] = value
	}
}

func validSHA256Value(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	digest := strings.TrimPrefix(value, prefix)
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
