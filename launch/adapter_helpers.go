package launch

import (
	"regexp"
	"sort"
	"strings"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
