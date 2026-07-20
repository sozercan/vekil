package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

const targetRevisionPrefix = "target_"

type targetRevision string

var targetRevisionProcessKey = func() [sha256.Size]byte {
	var key [sha256.Size]byte
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		panic(fmt.Sprintf("initialize target revision key: %v", err))
	}
	return key
}()

type targetRevisionEncoder struct {
	hash hash.Hash
}

func newTargetRevisionEncoder(domain string) *targetRevisionEncoder {
	encoder := &targetRevisionEncoder{hash: hmac.New(sha256.New, targetRevisionProcessKey[:])}
	encoder.writeString("domain", domain)
	return encoder
}

func (e *targetRevisionEncoder) writeBytes(name string, value []byte) {
	if e == nil || e.hash == nil {
		return
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(name)))
	_, _ = e.hash.Write(length[:])
	_, _ = io.WriteString(e.hash, name)
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = e.hash.Write(length[:])
	_, _ = e.hash.Write(value)
}

func (e *targetRevisionEncoder) writeString(name, value string) {
	e.writeBytes(name, []byte(value))
}

func (e *targetRevisionEncoder) writeBool(name string, value bool) {
	if value {
		e.writeString(name, "true")
		return
	}
	e.writeString(name, "false")
}

func (e *targetRevisionEncoder) writeOptionalBool(name string, value *bool) {
	switch {
	case value == nil:
		e.writeString(name, "unset")
	case *value:
		e.writeString(name, "true")
	default:
		e.writeString(name, "false")
	}
}

func (e *targetRevisionEncoder) sum() []byte {
	if e == nil || e.hash == nil {
		return nil
	}
	return e.hash.Sum(nil)
}

func targetCredentialFingerprint(value string) string {
	if value == "" {
		return ""
	}
	encoder := newTargetRevisionEncoder("credential-fingerprint-v1")
	encoder.writeString("value", value)
	return base64.RawURLEncoding.EncodeToString(encoder.sum())
}

func deriveTargetRevision(contract publicModelContract, target targetBinding) targetRevision {
	encoder := newTargetRevisionEncoder("physical-target-revision-v1")
	provider := target.provider
	if provider == nil {
		encoder.writeString("provider.kind", "")
		encoder.writeString("provider.destination", "")
	} else {
		encoder.writeString("provider.kind", string(provider.kind))
		encoder.writeString("provider.destination", normalizeTargetRevisionDestination(provider.baseURL))
		writeTargetRevisionAuth(encoder, provider)
		encoder.writeString("provider.api_version", strings.TrimSpace(provider.apiVersion))
		encoder.writeString("provider.token_scope", strings.TrimSpace(provider.tokenScope))
		encoder.writeString("provider.path.chat_completions", normalizeTargetRevisionPath(provider.paths.chatCompletions))
		encoder.writeString("provider.path.responses", normalizeTargetRevisionPath(provider.paths.responses))
		encoder.writeString("provider.path.messages", normalizeTargetRevisionPath(provider.paths.messages))
		encoder.writeString("provider.path.models", normalizeTargetRevisionPath(provider.paths.models))
		encoder.writeString("provider.model_discovery", string(provider.modelDiscovery))
		encoder.writeString("provider.trust_domain", strings.TrimSpace(provider.trustDomain))
		encoder.writeOptionalBool("provider.classifier_no_store_supported", provider.classifierNoStoreSupported)
		writeTargetRevisionExtraHeaders(encoder, provider.extraHeaders)
		writeTargetRevisionCopilotHeaders(encoder, provider)
	}

	encoder.writeString("target.upstream_model", strings.TrimSpace(target.upstreamModel))
	writeTargetRevisionPolicy(encoder, "target.wire_policy", target.wirePolicy)
	writeTargetRevisionPolicy(encoder, "route.public_policy", contract.policy)
	writeTargetRevisionStringSet(encoder, "route.public_endpoints", contract.endpoints)

	return targetRevision(targetRevisionPrefix + base64.RawURLEncoding.EncodeToString(encoder.sum()))
}

func targetRevisionForBinding(contract publicModelContract, target targetBinding) targetRevision {
	if target.revision != "" {
		return target.revision
	}
	return deriveTargetRevision(contract, target)
}

func ensureModelRouteTargetRevisions(route *modelRoute) {
	if route == nil {
		return
	}
	for index := range route.targets {
		if route.targets[index].revision == "" {
			route.targets[index].revision = deriveTargetRevision(route.public, route.targets[index])
		}
	}
}

func stateBindingOwnerMatchesTarget(owner stateBindingOwner, route *modelRoute, target targetBinding) bool {
	if route == nil || owner.routeID != route.public.routeID || owner.targetID != target.id {
		return false
	}
	current := targetRevisionForBinding(route.public, target)
	if owner.targetRevision == current {
		return true
	}
	// Hand-built unit-test routes historically omitted revisions entirely. A
	// zero owner remains compatible only with another unmaterialized fixture
	// target; production-compiled routes always carry a concrete revision.
	return owner.targetRevision == "" && target.revision == ""
}

func (h *ProxyHandler) explicitRouteTargetRevision(routeID, targetID string) (targetRevision, bool) {
	if h == nil {
		return "", false
	}
	setup := h.providerSetup()
	if setup == nil {
		return "", false
	}
	route, ok := setup.lookupTerminalRoute(strings.TrimSpace(routeID))
	if !ok || route == nil || route.legacy {
		return "", false
	}
	target, ok := route.targetByID(strings.TrimSpace(targetID))
	if !ok {
		return "", false
	}
	return targetRevisionForBinding(route.public, target), true
}

func targetRevisionForResolvedChatRoute(route resolvedChatRoute) targetRevision {
	policy := providerRequestPolicy{
		parallelToolCalls:      cloneBoolPtr(route.owner.parallelToolCalls),
		dropSamplingParams:     route.owner.dropSamplingParams,
		useMaxCompletionTokens: route.owner.useMaxCompletionTokens,
	}
	return deriveTargetRevision(
		publicModelContract{
			id:        route.publicModel,
			endpoints: append([]string(nil), route.owner.supportedEndpoints...),
			policy:    policy,
		},
		targetBinding{
			provider:      route.provider,
			upstreamModel: route.upstreamModel,
			wirePolicy:    policy,
		},
	)
}

func writeTargetRevisionAuth(encoder *targetRevisionEncoder, provider *providerRuntime) {
	if encoder == nil || provider == nil {
		return
	}

	authMode := strings.TrimSpace(string(provider.authMode))
	authType := strings.TrimSpace(string(provider.authType))
	authHeader := strings.TrimSpace(provider.authHeader)
	authPrefix := strings.TrimSpace(provider.authPrefix)
	authSource := ""
	authSourceDetail := ""

	switch provider.kind {
	case providerTypeCopilot:
		authMode = "copilot"
		authType = string(providerAuthTypeBearer)
		authHeader = "Authorization"
		authPrefix = "Bearer"
		authSource = "copilot-authenticator"
	case providerTypeAzureOpenAI:
		authMode = string(provider.azureAuthMode())
		switch provider.azureAuthMode() {
		case providerAuthModeAzureIdentity:
			authType = string(providerAuthTypeBearer)
			authHeader = "Authorization"
			authPrefix = "Bearer"
			authSource = "azure-identity"
			if provider.azureToken != nil {
				authSourceDetail = fmt.Sprintf("%T", provider.azureToken)
			}
		default:
			authType = string(providerAuthTypeAPIKeyHeader)
			authHeader = "api-key"
			authPrefix = ""
			authSource = "configured-api-key"
		}
	case providerTypeOpenAICodex:
		authMode = openAICodexAuthMode
		authType = string(providerAuthTypeBearer)
		authHeader = "Authorization"
		authPrefix = "Bearer"
		authSource = "chatgpt-auth-file"
		if provider.codexAuth != nil {
			authSourceDetail = normalizeTargetRevisionFileSource(provider.codexAuth.path)
		}
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		if authType == "" {
			authType = string(providerAuthTypeNone)
		}
		if provider.authType == providerAuthTypeNone || provider.authType == "" {
			authSource = "none"
		} else {
			authSource = "configured-api-key"
		}
	default:
		authSource = "unknown"
	}

	encoder.writeString("provider.auth.mode", authMode)
	encoder.writeString("provider.auth.type", authType)
	encoder.writeString("provider.auth.source", authSource)
	encoder.writeString("provider.auth.source_detail", authSourceDetail)
	encoder.writeString("provider.auth.header", strings.ToLower(http.CanonicalHeaderKey(authHeader)))
	encoder.writeString("provider.auth.prefix", authPrefix)
	encoder.writeString("provider.auth.credential_fingerprint", targetCredentialFingerprint(provider.apiKey))
}

func writeTargetRevisionExtraHeaders(encoder *targetRevisionEncoder, headers http.Header) {
	if encoder == nil {
		return
	}
	canonical := make(map[string][]string, len(headers))
	for name, values := range headers {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		canonical[name] = append(canonical[name], values...)
	}
	names := make([]string, 0, len(canonical))
	for name := range canonical {
		names = append(names, name)
	}
	sort.Strings(names)
	encoder.writeString("provider.extra_headers.count", fmt.Sprintf("%d", len(names)))
	for headerIndex, name := range names {
		values := append([]string(nil), canonical[name]...)
		sort.Strings(values)
		prefix := fmt.Sprintf("provider.extra_headers.%d", headerIndex)
		encoder.writeString(prefix+".name", name)
		encoder.writeString(prefix+".count", fmt.Sprintf("%d", len(values)))
		for valueIndex, value := range values {
			encoder.writeString(
				fmt.Sprintf("%s.value.%d", prefix, valueIndex),
				targetCredentialFingerprint(strings.TrimSpace(value)),
			)
		}
	}
}

func writeTargetRevisionCopilotHeaders(encoder *targetRevisionEncoder, provider *providerRuntime) {
	if encoder == nil || provider == nil || provider.kind != providerTypeCopilot {
		return
	}
	for _, endpoint := range []string{
		providerEndpointChatCompletions,
		providerEndpointResponses,
		providerEndpointMessages,
		providerEndpointModels,
	} {
		profile := provider.headerProfiles.profileForEndpoint(endpoint, CopilotHeaderConfig{})
		prefix := "provider.copilot_headers." + endpoint
		encoder.writeString(prefix+".editor_version", profile.EditorVersion)
		encoder.writeString(prefix+".editor_plugin_version", profile.EditorPluginVersion)
		encoder.writeString(prefix+".user_agent", profile.UserAgent)
		encoder.writeString(prefix+".integration_id", profile.IntegrationID)
		encoder.writeString(prefix+".github_api_version", profile.GitHubAPIVersion)
		encoder.writeString(prefix+".openai_intent", profile.OpenAIIntent)
	}
}

func writeTargetRevisionPolicy(encoder *targetRevisionEncoder, prefix string, policy providerRequestPolicy) {
	if encoder == nil {
		return
	}
	encoder.writeOptionalBool(prefix+".parallel_tool_calls", policy.parallelToolCalls)
	encoder.writeBool(prefix+".drop_sampling_params", policy.dropSamplingParams)
	encoder.writeBool(prefix+".use_max_completion_tokens", policy.useMaxCompletionTokens)
}

func writeTargetRevisionStringSet(encoder *targetRevisionEncoder, prefix string, values []string) {
	if encoder == nil {
		return
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(set))
	for value := range set {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	encoder.writeString(prefix+".count", fmt.Sprintf("%d", len(ordered)))
	for index, value := range ordered {
		encoder.writeString(fmt.Sprintf("%s.%d", prefix, index), value)
	}
}

func normalizeTargetRevisionDestination(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimRight(raw, "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		parsed.Host = net.JoinHostPort(hostname, port)
	case strings.Contains(hostname, ":"):
		parsed.Host = "[" + hostname + "]"
	default:
		parsed.Host = hostname
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = parsed.Query().Encode()
	}
	return parsed.String()
}

func normalizeTargetRevisionPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "/" {
		return path
	}
	return strings.TrimRight(path, "/")
}

func normalizeTargetRevisionFileSource(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}
