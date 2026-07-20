package proxy

import (
	"context"
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

// targetOpenAICodexCredentialsFingerprint binds the exact account/principal
// identity used to authenticate one request behind the process-keyed HMAC used
// by target revisions. Access-token rotation stays stable when a durable
// account or subject is available; otherwise the exact bearer token is the
// fail-closed identity fallback.
func targetOpenAICodexCredentialsFingerprint(credentials openAICodexCredentials) string {
	encoder := newTargetRevisionEncoder("openai-codex-auth-fingerprint-v1")
	encoder.writeString("status", "loaded")
	encoder.writeString("account_id", credentials.accountID)
	encoder.writeString("subject", credentials.subject)
	encoder.writeBool("fedramp", credentials.fedRAMP)
	if credentials.accountID == "" && credentials.subject == "" {
		encoder.writeString("credential_fallback", targetCredentialFingerprint(credentials.accessToken))
	}
	return base64.RawURLEncoding.EncodeToString(encoder.sum())
}

// targetOpenAICodexAuthFingerprint resolves the current file-backed identity for
// pre-dispatch continuation validation. The final request revision is derived
// again from the exact credentials acquired for that request.
func targetOpenAICodexAuthFingerprint(auth *openAICodexAuth) string {
	if auth == nil {
		return ""
	}

	state, err := auth.readIdentityState()
	if err != nil {
		encoder := newTargetRevisionEncoder("openai-codex-auth-fingerprint-v1")
		encoder.writeString("status", "unavailable")
		return base64.RawURLEncoding.EncodeToString(encoder.sum())
	}
	return targetOpenAICodexCredentialsFingerprint(openAICodexCredentialsFromTokens(state.tokens))
}

type providerRequestAuthIdentity struct {
	credentialFingerprint string
}

type targetRevisionRequestBinding struct {
	contract publicModelContract
	target   targetBinding
	expected targetRevision
}

type targetRevisionRequestBindingContextKey struct{}
type targetRevisionRequestValueContextKey struct{}

func withTargetRevisionRequest(ctx context.Context, contract publicModelContract, target targetBinding, expected targetRevision) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, targetRevisionRequestBindingContextKey{}, targetRevisionRequestBinding{
		contract: contract,
		target:   target,
		expected: expected,
	})
}

func finalizeTargetRevisionRequest(req *http.Request, identity providerRequestAuthIdentity) (*http.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("provider request is required")
	}
	binding, ok := req.Context().Value(targetRevisionRequestBindingContextKey{}).(targetRevisionRequestBinding)
	if !ok {
		return req, nil
	}

	revision := targetRevisionForBinding(binding.contract, binding.target)
	if binding.target.provider != nil && binding.target.provider.kind == providerTypeOpenAICodex {
		if identity.credentialFingerprint == "" {
			return nil, &providerRequestError{
				statusCode: http.StatusInternalServerError,
				err:        fmt.Errorf("OpenAI Codex request auth identity is unavailable"),
			}
		}
		revision = deriveTargetRevisionWithCredentialFingerprint(binding.contract, binding.target, identity.credentialFingerprint)
	}
	if binding.expected != "" && revision != binding.expected {
		return nil, &providerRequestError{
			statusCode: http.StatusServiceUnavailable,
			err:        fmt.Errorf("route target auth identity changed before upstream dispatch"),
		}
	}
	return req.WithContext(context.WithValue(req.Context(), targetRevisionRequestValueContextKey{}, revision)), nil
}

func targetRevisionFromRequest(req *http.Request) (targetRevision, bool) {
	if req == nil {
		return "", false
	}
	revision, ok := req.Context().Value(targetRevisionRequestValueContextKey{}).(targetRevision)
	return revision, ok && revision != ""
}

func targetRevisionFromResponse(resp *http.Response) (targetRevision, bool) {
	if resp == nil {
		return "", false
	}
	return targetRevisionFromRequest(resp.Request)
}

func deriveTargetRevision(contract publicModelContract, target targetBinding) targetRevision {
	return deriveTargetRevisionWithAuthOverride(contract, target, nil)
}

func deriveTargetRevisionWithCredentialFingerprint(contract publicModelContract, target targetBinding, fingerprint string) targetRevision {
	return deriveTargetRevisionWithAuthOverride(contract, target, &fingerprint)
}

func deriveTargetRevisionWithAuthOverride(contract publicModelContract, target targetBinding, credentialFingerprint *string) targetRevision {
	encoder := newTargetRevisionEncoder("physical-target-revision-v1")
	provider := target.provider
	if provider == nil {
		encoder.writeString("provider.kind", "")
		encoder.writeString("provider.destination", "")
	} else {
		encoder.writeString("provider.kind", string(provider.kind))
		encoder.writeString("provider.destination", normalizeTargetRevisionDestination(provider.baseURL))
		writeTargetRevisionAuth(encoder, provider, credentialFingerprint)
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
	// Codex credentials are file-backed and may be replaced independently of a
	// runtime config generation. Recompute their identity fence so continuations
	// cannot cross an account/principal change at the same auth-file path.
	if target.provider != nil && target.provider.kind == providerTypeOpenAICodex {
		return deriveTargetRevision(contract, target)
	}
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

func targetRevisionBindingForResolvedChatRoute(route resolvedChatRoute) (publicModelContract, targetBinding) {
	policy := providerRequestPolicy{
		parallelToolCalls:      cloneBoolPtr(route.owner.parallelToolCalls),
		dropSamplingParams:     route.owner.dropSamplingParams,
		useMaxCompletionTokens: route.owner.useMaxCompletionTokens,
	}
	return publicModelContract{
			id:        route.publicModel,
			endpoints: append([]string(nil), route.owner.supportedEndpoints...),
			policy:    policy,
		}, targetBinding{
			provider:      route.provider,
			upstreamModel: route.upstreamModel,
			wirePolicy:    policy,
		}
}

func writeTargetRevisionAuth(encoder *targetRevisionEncoder, provider *providerRuntime, credentialFingerprintOverride *string) {
	if encoder == nil || provider == nil {
		return
	}

	authMode := strings.TrimSpace(string(provider.authMode))
	authType := strings.TrimSpace(string(provider.authType))
	authHeader := strings.TrimSpace(provider.authHeader)
	authPrefix := strings.TrimSpace(provider.authPrefix)
	authSource := ""
	authSourceDetail := ""
	credentialFingerprint := targetCredentialFingerprint(provider.apiKey)

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
		if credentialFingerprintOverride != nil {
			credentialFingerprint = *credentialFingerprintOverride
		} else if provider.codexAuth != nil {
			credentialFingerprint = targetOpenAICodexAuthFingerprint(provider.codexAuth)
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
	encoder.writeString("provider.auth.credential_fingerprint", credentialFingerprint)
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
