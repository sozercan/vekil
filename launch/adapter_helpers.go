package launch

import (
	"regexp"
	"sort"
	"strings"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var commonAgentCredentialEnvironment = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_CUSTOM_HEADERS",
	"AWS_ACCESS_KEY_ID",
	"AWS_BEARER_TOKEN_BEDROCK",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AZURE_CLIENT_CERTIFICATE_PASSWORD",
	"AZURE_CLIENT_CERTIFICATE_PATH",
	"AZURE_CLIENT_SECRET",
	"AZURE_FEDERATED_TOKEN_FILE",
	"AZURE_OPENAI_API_KEY",
	"AZURE_OPENAI_ENDPOINT",
	"AZURE_PASSWORD",
	"AZURE_USERNAME",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
	"CLAUDE_CODE_OAUTH_SCOPES",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CODEX_API_KEY",
	"COPILOT_GITHUB_TOKEN",
	"COPILOT_PROVIDER_API_KEY",
	"COPILOT_PROVIDER_BEARER_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GH_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"GITHUB_TOKEN",
	"IDENTITY_HEADER",
	"MSI_SECRET",
	"OPENAI_ACCOUNT_ID",
	"OPENAI_API_KEY",
	"OPENAI_BASE_URL",
	"OPENAI_ORGANIZATION",
	"OPENAI_ORG_ID",
	"OPENAI_PROJECT",
	"OPENAI_PROJECT_ID",
}

func mergeEnvironmentKeys(groups ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, group := range groups {
		for _, raw := range group {
			key := strings.TrimSpace(raw)
			if key == "" {
				continue
			}
			canonical := canonicalEnvironmentKey(key)
			if _, ok := seen[canonical]; ok {
				continue
			}
			seen[canonical] = struct{}{}
			out = append(out, key)
		}
	}
	return out
}

func sortedEnvironmentNames(groups ...[]string) []string {
	keys := mergeEnvironmentKeys(groups...)
	out := keys[:0]
	for _, key := range keys {
		if environmentNamePattern.MatchString(key) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func openAIBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/v1"
}

func modelContextWindow(model ModelInfo) int64 {
	if model.ContextWindow != nil && *model.ContextWindow > 0 {
		return *model.ContextWindow
	}
	if model.Capabilities.Limits.MaxContextWindowTokens > 0 {
		return model.Capabilities.Limits.MaxContextWindowTokens
	}
	if model.MaxContextWindow != nil && *model.MaxContextWindow > 0 {
		return *model.MaxContextWindow
	}
	return 0
}

func modelMaxContextWindow(model ModelInfo) int64 {
	if model.MaxContextWindow != nil && *model.MaxContextWindow > 0 {
		return *model.MaxContextWindow
	}
	if model.Capabilities.Limits.MaxContextWindowTokens > 0 {
		return model.Capabilities.Limits.MaxContextWindowTokens
	}
	return modelContextWindow(model)
}
