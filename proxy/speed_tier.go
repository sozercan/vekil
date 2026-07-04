package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sozercan/vekil/logger"
)

const (
	speedTierSemanticsAll = "all"
	speedTierSemanticsAny = "any"

	speedTierDecisionDowngraded       = "downgraded"
	speedTierDecisionConsideredReject = "considered_rejected"
	speedTierDecisionOptedOut         = "opted_out"
	speedTierDecisionForcedAlias      = "forced_alias"
)

type SpeedTierConfig struct {
	DowngradeTo          string                   `json:"downgrade_to" yaml:"downgrade_to"`
	Semantics            string                   `json:"semantics,omitempty" yaml:"semantics,omitempty"`
	When                 SpeedTierWhenConfig      `json:"when,omitempty" yaml:"when,omitempty"`
	NeverWhen            SpeedTierNeverWhenConfig `json:"never_when,omitempty" yaml:"never_when,omitempty"`
	WSStickyAfterUpgrade *bool                    `json:"ws_sticky_after_upgrade,omitempty" yaml:"ws_sticky_after_upgrade,omitempty"`
}

type SpeedTierWhenConfig struct {
	MaxTokensLTE      *int     `json:"max_tokens_lte,omitempty" yaml:"max_tokens_lte,omitempty"`
	ToolsCountLTE     *int     `json:"tools_count_lte,omitempty" yaml:"tools_count_lte,omitempty"`
	InputCharsLTE     *int     `json:"input_chars_lte,omitempty" yaml:"input_chars_lte,omitempty"`
	SystemCharsLTE    *int     `json:"system_chars_lte,omitempty" yaml:"system_chars_lte,omitempty"`
	MessageCountLTE   *int     `json:"message_count_lte,omitempty" yaml:"message_count_lte,omitempty"`
	RequireEndpointIn []string `json:"require_endpoint_in,omitempty" yaml:"require_endpoint_in,omitempty"`
}

type SpeedTierNeverWhenConfig struct {
	ThinkingEnabled   bool     `json:"thinking_enabled,omitempty" yaml:"thinking_enabled,omitempty"`
	ReasoningEffortIn []string `json:"reasoning_effort_in,omitempty" yaml:"reasoning_effort_in,omitempty"`
	HasHeader         string   `json:"has_header,omitempty" yaml:"has_header,omitempty"`
}

type speedTierRule struct {
	downgradeTo          string
	semantics            string
	when                 SpeedTierWhenConfig
	neverWhen            SpeedTierNeverWhenConfig
	wsStickyAfterUpgrade bool
}

type speedTierDecision struct {
	from             string
	to               string
	decision         string
	triggeringSignal string
	inputChars       int
	systemChars      int
	toolsCount       int
	messageCount     int
	maxTokens        int
	endpoint         string
	clientOptOut     bool
	forcedAlias      bool
	reason           string
}

func normalizeSpeedTierRule(cfg *SpeedTierConfig) (*speedTierRule, error) {
	if cfg == nil {
		return nil, nil
	}
	rule := &speedTierRule{
		downgradeTo:          strings.TrimSpace(cfg.DowngradeTo),
		semantics:            strings.ToLower(strings.TrimSpace(cfg.Semantics)),
		when:                 normalizeSpeedTierWhen(cfg.When),
		neverWhen:            normalizeSpeedTierNeverWhen(cfg.NeverWhen),
		wsStickyAfterUpgrade: true,
	}
	if rule.semantics == "" {
		rule.semantics = speedTierSemanticsAll
	}
	if cfg.WSStickyAfterUpgrade != nil {
		rule.wsStickyAfterUpgrade = *cfg.WSStickyAfterUpgrade
	}
	return rule, nil
}

func normalizeSpeedTierWhen(in SpeedTierWhenConfig) SpeedTierWhenConfig {
	out := in
	if out.RequireEndpointIn != nil {
		endpoints := make([]string, 0, len(out.RequireEndpointIn))
		seen := map[string]struct{}{}
		for _, endpoint := range out.RequireEndpointIn {
			endpoint = strings.TrimSpace(endpoint)
			if endpoint == "" {
				continue
			}
			if _, ok := seen[endpoint]; ok {
				continue
			}
			seen[endpoint] = struct{}{}
			endpoints = append(endpoints, endpoint)
		}
		out.RequireEndpointIn = endpoints
	}
	return out
}

func normalizeSpeedTierNeverWhen(in SpeedTierNeverWhenConfig) SpeedTierNeverWhenConfig {
	out := in
	out.HasHeader = http.CanonicalHeaderKey(strings.TrimSpace(out.HasHeader))
	if out.ReasoningEffortIn != nil {
		values := make([]string, 0, len(out.ReasoningEffortIn))
		seen := map[string]struct{}{}
		for _, effort := range out.ReasoningEffortIn {
			effort = strings.ToLower(strings.TrimSpace(effort))
			if effort == "" {
				continue
			}
			if _, ok := seen[effort]; ok {
				continue
			}
			seen[effort] = struct{}{}
			values = append(values, effort)
		}
		out.ReasoningEffortIn = values
	}
	return out
}

func providerModelLookupName(model, endpoint string) string {
	model = strings.TrimSpace(model)
	if endpoint == providerEndpointMessages {
		return NormalizeModelName(model)
	}
	return model
}

func (h *ProxyHandler) resolveProviderModelWithFastAlias(model, endpoint string) (*providerRuntime, providerModel, bool, bool) {
	provider, owner, known := h.resolveProviderModel(providerModelLookupName(model, endpoint), endpoint)
	if known {
		return provider, owner, known, false
	}
	baseModel, alias := speedTierBaseModel(model)
	if !alias {
		return provider, owner, known, false
	}
	for _, candidate := range speedTierAliasLookupCandidates(baseModel, endpoint) {
		aliasProvider, aliasOwner, aliasKnown := h.resolveProviderModel(candidate, endpoint)
		if aliasProvider == nil {
			continue
		}
		if aliasKnown || aliasProvider.allowsUnknownModelEndpoint(endpoint) {
			return aliasProvider, aliasOwner, aliasKnown, true
		}
	}
	return provider, owner, known, false
}

func speedTierAliasLookupCandidates(baseModel, endpoint string) []string {
	primary := providerModelLookupName(baseModel, endpoint)
	normalized := NormalizeModelName(baseModel)
	if normalized == "" || normalized == primary {
		return []string{primary}
	}
	return []string{primary, normalized}
}

func speedTierBaseModel(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "fast/") {
		base := strings.TrimSpace(model[len("fast/"):])
		if base != "" {
			return base, true
		}
	}
	return model, false
}

func (h *ProxyHandler) speedTierDecisionForRequest(body []byte, endpoint string, headers http.Header) (providerModel, speedTierDecision, bool) {
	model := extractRequestModel(body)
	provider, owner, known, forcedAlias := h.resolveProviderModelWithFastAlias(model, endpoint)
	setup := h.providerSetup()
	if setup == nil || !setup.speedTierEnabled || provider == nil || !known || owner.speedTier == nil {
		return owner, speedTierDecision{}, false
	}
	decision := evaluateSpeedTier(body, endpoint, headers, owner, forcedAlias)
	decision.from = owner.publicID
	decision.endpoint = endpoint
	if decision.shouldDowngrade() {
		if target, ok := setup.lookupModel(owner.speedTier.downgradeTo); ok && target.providerID == owner.providerID && providerModelSupportsEndpoint(target, endpoint) {
			decision.to = target.publicID
		}
	}
	return owner, decision, true
}

func (h *ProxyHandler) maybeApplySpeedTierRouting(body []byte, endpoint string, headers http.Header, provider *providerRuntime, owner providerModel, known bool, forcedAlias bool) (providerModel, []byte) {
	setup := h.providerSetup()
	if setup == nil || !setup.speedTierEnabled || provider == nil || !known || owner.speedTier == nil {
		return owner, body
	}

	decision := evaluateSpeedTier(body, endpoint, headers, owner, forcedAlias)
	decision.from = owner.publicID
	decision.endpoint = endpoint
	if !decision.shouldDowngrade() {
		h.logSpeedTierDecision(decision)
		return owner, body
	}

	target, ok := setup.lookupModel(owner.speedTier.downgradeTo)
	if !ok || target.providerID != owner.providerID || !providerModelSupportsEndpoint(target, endpoint) {
		// Validation should prevent this. Fail open to the original model at request time.
		decision.decision = speedTierDecisionConsideredReject
		decision.reason = "target_unavailable"
		h.logSpeedTierDecision(decision)
		return owner, body
	}

	decision.to = target.publicID
	if forcedAlias {
		decision.decision = speedTierDecisionForcedAlias
	} else {
		decision.decision = speedTierDecisionDowngraded
	}
	h.logSpeedTierDecision(decision)

	rewritten, _, err := rewriteRequestModelForProvider(body, target.publicID)
	if err != nil {
		return target, body
	}
	return target, rewritten
}

func speedTierDecisionPinsWebSocket(decision speedTierDecision) bool {
	return decision.decision == speedTierDecisionConsideredReject && decision.reason == "signals_not_matched"
}

func (d speedTierDecision) shouldDowngrade() bool {
	return d.decision == speedTierDecisionDowngraded || d.decision == speedTierDecisionForcedAlias
}

func evaluateSpeedTier(body []byte, endpoint string, headers http.Header, owner providerModel, forcedAlias bool) speedTierDecision {
	features := collectSpeedTierFeatures(body)
	decision := speedTierDecision{
		inputChars:   features.inputChars,
		systemChars:  features.systemChars,
		toolsCount:   features.toolsCount,
		messageCount: features.messageCount,
		maxTokens:    features.maxTokens,
		endpoint:     endpoint,
		forcedAlias:  forcedAlias,
	}
	if owner.speedTier == nil {
		decision.decision = speedTierDecisionConsideredReject
		decision.reason = "not_configured"
		return decision
	}

	if speedTierClientOptOut(headers, owner.speedTier.neverWhen.HasHeader) || features.deniedBy(owner.speedTier.neverWhen, headers) {
		decision.decision = speedTierDecisionOptedOut
		decision.clientOptOut = speedTierClientOptOut(headers, owner.speedTier.neverWhen.HasHeader)
		decision.reason = features.denyReason(owner.speedTier.neverWhen, headers)
		if decision.reason == "" {
			decision.reason = "client_opt_out"
		}
		return decision
	}

	if forcedAlias || speedTierClientOptIn(headers) {
		decision.decision = speedTierDecisionDowngraded
		if forcedAlias {
			decision.triggeringSignal = "fast_alias"
		} else {
			decision.triggeringSignal = "client_header"
		}
		return decision
	}

	matches, first, configured := features.matchWhen(owner.speedTier.when, endpoint)
	if configured == 0 {
		decision.decision = speedTierDecisionConsideredReject
		decision.reason = "no_when_signals"
		return decision
	}
	if owner.speedTier.semantics == speedTierSemanticsAll {
		if matches == configured {
			decision.decision = speedTierDecisionDowngraded
			decision.triggeringSignal = speedTierSemanticsAll
			return decision
		}
	} else if matches > 0 {
		decision.decision = speedTierDecisionDowngraded
		decision.triggeringSignal = first
		return decision
	}

	decision.decision = speedTierDecisionConsideredReject
	decision.triggeringSignal = first
	decision.reason = "signals_not_matched"
	return decision
}

func (h *ProxyHandler) logSpeedTierDecision(d speedTierDecision) {
	fields := []logger.Field{
		logger.F("from", d.from),
		logger.F("decision", d.decision),
		logger.F("endpoint", d.endpoint),
		logger.F("input_chars", d.inputChars),
		logger.F("tools_count", d.toolsCount),
		logger.F("message_count", d.messageCount),
		logger.F("system_chars", d.systemChars),
		logger.F("max_tokens", d.maxTokens),
		logger.F("client_opt_out", d.clientOptOut),
	}
	if d.to != "" {
		fields = append(fields, logger.F("to", d.to), logger.F("routed_to", d.to))
	}
	if d.triggeringSignal != "" {
		fields = append(fields, logger.F("triggering_signal", d.triggeringSignal))
	}
	if d.reason != "" {
		fields = append(fields, logger.F("reason", d.reason))
	}
	h.log.Info("speed tier routing decision", fields...)
}

type speedTierFeatures struct {
	inputChars       int
	systemChars      int
	systemCharsSet   bool
	toolsCount       int
	messageCount     int
	maxTokens        int
	thinkingEnabled  bool
	reasoningEffort  string
	validJSONPayload bool
}

func collectSpeedTierFeatures(body []byte) speedTierFeatures {
	features := speedTierFeatures{inputChars: utf8.RuneCount(body), maxTokens: -1, toolsCount: -1, messageCount: -1}
	var payload map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return features
	}
	features.validJSONPayload = true
	features.maxTokens = firstJSONInt(payload, "max_tokens", "max_completion_tokens", "max_output_tokens")
	features.toolsCount = rawArrayLen(payload["tools"])
	features.messageCount = requestMessageCount(payload)
	features.systemChars, features.systemCharsSet = requestSystemChars(payload)
	features.thinkingEnabled = requestThinkingEnabled(payload)
	features.reasoningEffort = requestReasoningEffort(payload)
	return features
}

func (f speedTierFeatures) matchWhen(when SpeedTierWhenConfig, endpoint string) (matches int, first string, configured int) {
	check := func(signal string, ok bool) {
		configured++
		if ok {
			matches++
			if first == "" {
				first = signal
			}
		}
	}
	if when.MaxTokensLTE != nil {
		check("max_tokens_lte", f.maxTokens >= 0 && f.maxTokens <= *when.MaxTokensLTE)
	}
	if when.ToolsCountLTE != nil {
		check("tools_count_lte", f.toolsCount >= 0 && f.toolsCount <= *when.ToolsCountLTE)
	}
	if when.InputCharsLTE != nil {
		check("input_chars_lte", f.inputChars <= *when.InputCharsLTE)
	}
	if when.SystemCharsLTE != nil {
		check("system_chars_lte", f.systemCharsSet && f.systemChars <= *when.SystemCharsLTE)
	}
	if when.MessageCountLTE != nil {
		check("message_count_lte", f.messageCount >= 0 && f.messageCount <= *when.MessageCountLTE)
	}
	if len(when.RequireEndpointIn) > 0 && !stringInSlice(endpoint, when.RequireEndpointIn) {
		return 0, "", configured
	}
	return matches, first, configured
}

func (f speedTierFeatures) deniedBy(never SpeedTierNeverWhenConfig, headers http.Header) bool {
	return f.denyReason(never, headers) != ""
}

func (f speedTierFeatures) denyReason(never SpeedTierNeverWhenConfig, headers http.Header) string {
	if never.HasHeader != "" && headerPresent(headers, never.HasHeader) {
		return "has_header"
	}
	if never.ThinkingEnabled && f.thinkingEnabled {
		return "thinking_enabled"
	}
	if f.reasoningEffort != "" && stringInSlice(strings.ToLower(f.reasoningEffort), never.ReasoningEffortIn) {
		return "reasoning_effort"
	}
	return ""
}

func noSpeedTierRoutingHeaders() http.Header {
	return http.Header{"X-Vekil-Routing": []string{"default"}}
}

func speedTierClientOptIn(headers http.Header) bool {
	routing := strings.ToLower(strings.TrimSpace(headers.Get("X-Vekil-Routing")))
	legacy := strings.ToLower(strings.TrimSpace(headers.Get("X-Vekil-Tier")))
	return routing == "speed" || legacy == "speed"
}

func speedTierClientOptOut(headers http.Header, configuredHeader string) bool {
	routing := strings.ToLower(strings.TrimSpace(headers.Get("X-Vekil-Routing")))
	if routing == "default" || routing == "no-downgrade" {
		return true
	}
	if headerPresent(headers, "X-Vekil-No-Downgrade") {
		return true
	}
	return configuredHeader != "" && headerPresent(headers, configuredHeader)
}

func headerPresent(headers http.Header, name string) bool {
	name = http.CanonicalHeaderKey(strings.TrimSpace(name))
	if name == "" || headers == nil {
		return false
	}
	values, ok := headers[name]
	if !ok {
		return false
	}
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func firstJSONInt(payload map[string]json.RawMessage, keys ...string) int {
	for _, key := range keys {
		if raw, ok := payload[key]; ok {
			if value, ok := jsonInt(raw); ok {
				return value
			}
		}
	}
	return -1
}

func jsonInt(raw json.RawMessage) (int, bool) {
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		if i, err := num.Int64(); err == nil {
			return int(i), true
		}
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(f), true
	}
	return 0, false
}

func rawArrayLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return -1
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return -1
	}
	return len(arr)
}

func requestMessageCount(payload map[string]json.RawMessage) int {
	if n := rawArrayLen(payload["messages"]); n >= 0 {
		return n
	}
	if n := rawArrayLen(payload["input"]); n >= 0 {
		return n
	}
	return -1
}

func requestSystemChars(payload map[string]json.RawMessage) (int, bool) {
	total := 0
	present := false
	if raw, ok := payload["system"]; ok {
		present = true
		total += rawTextChars(raw)
	}
	if raw, ok := payload["instructions"]; ok {
		present = true
		total += rawTextChars(raw)
	}
	if raw, ok := payload["messages"]; ok {
		chars, ok := requestInstructionMessageChars(raw)
		if ok {
			present = true
			total += chars
		}
	}
	if raw, ok := payload["input"]; ok {
		chars, ok := requestInstructionMessageChars(raw)
		if ok {
			present = true
			total += chars
		}
	}
	return total, present
}

func requestInstructionMessageChars(raw json.RawMessage) (int, bool) {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return 0, false
	}
	total := 0
	found := false
	for _, msg := range messages {
		role := strings.ToLower(speedTierRawJSONString(msg["role"]))
		if role == "system" || role == "developer" {
			found = true
			total += rawTextChars(msg["content"])
		}
	}
	return total, found
}

func rawTextChars(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return utf8.RuneCountInString(s)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		total := 0
		for _, item := range arr {
			total += rawTextChars(item)
		}
		return total
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		if text, ok := obj["text"]; ok {
			return rawTextChars(text)
		}
		if content, ok := obj["content"]; ok {
			return rawTextChars(content)
		}
	}
	return 0
}

func requestThinkingEnabled(payload map[string]json.RawMessage) bool {
	var thinking map[string]json.RawMessage
	if raw, ok := payload["thinking"]; ok && json.Unmarshal(raw, &thinking) == nil {
		if strings.EqualFold(speedTierRawJSONString(thinking["type"]), "enabled") {
			return true
		}
		var enabled bool
		if rawEnabled, ok := thinking["enabled"]; ok && json.Unmarshal(rawEnabled, &enabled) == nil && enabled {
			return true
		}
	}
	return false
}

func requestReasoningEffort(payload map[string]json.RawMessage) string {
	if raw, ok := payload["reasoning_effort"]; ok {
		if effort := strings.TrimSpace(speedTierRawJSONString(raw)); effort != "" {
			return effort
		}
	}
	var reasoning map[string]json.RawMessage
	if raw, ok := payload["reasoning"]; ok && json.Unmarshal(raw, &reasoning) == nil {
		return strings.TrimSpace(speedTierRawJSONString(reasoning["effort"]))
	}
	return ""
}

func speedTierRawJSONString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return strings.TrimSpace(s)
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), value) {
			return true
		}
	}
	return false
}
