package proxy

import (
	"encoding/json"
	"sort"
	"strings"
)

// Hosted tools execute upstream. Conversation migration replays their visible
// call items only to targets whose provider declares the same hosted tool.
const hostedToolWebSearch = "web_search"

var providerHostedToolTypes = map[string]bool{hostedToolWebSearch: true}

func normalizeProviderHostedTools(values []string, path string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if !providerHostedToolTypes[name] {
			return nil, configPathError(path, "unsupported hosted tool %q; supported values are %s", value, strings.Join(providerHostedToolNames(), ", "))
		}
		if seen[name] {
			return nil, configPathError(path, "duplicates hosted tool %q", name)
		}
		seen[name] = true
		normalized = append(normalized, name)
	}
	return normalized, nil
}

func providerHostedToolNames() []string {
	names := make([]string, 0, len(providerHostedToolTypes))
	for name := range providerHostedToolTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func providerHostedToolSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if name := strings.TrimSpace(value); providerHostedToolTypes[name] {
			set[name] = true
		}
	}
	return set
}

func (p *providerRuntime) supportsHostedTools(required map[string]bool) bool {
	for name := range required {
		if p == nil || !p.hostedTools[name] {
			return false
		}
	}
	return true
}

// Tool definitions and replayed call items map to one hosted capability each.
func conversationHostedToolFromDefinition(kind string) string {
	switch kind {
	case "web_search", "web_search_preview":
		return hostedToolWebSearch
	}
	return ""
}

func conversationHostedToolFromItem(kind string) string {
	if kind == "web_search_call" {
		return hostedToolWebSearch
	}
	return ""
}

func conversationHostedToolsInDefinitions(raw json.RawMessage, into map[string]bool, depth int) {
	if len(raw) == 0 || depth > 1 {
		return
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return
	}
	for _, tool := range tools {
		kind := rawJSONString(tool["type"])
		if name := conversationHostedToolFromDefinition(kind); name != "" {
			into[name] = true
		}
		if kind == "namespace" {
			conversationHostedToolsInDefinitions(tool["tools"], into, depth+1)
		}
	}
}

// conversationHostedTools reports which hosted capabilities a turn's tool
// definitions, additional-tool catalogs, and visible history depend on.
func conversationHostedTools(tools json.RawMessage, additionalTools, items []json.RawMessage) map[string]bool {
	required := make(map[string]bool)
	conversationHostedToolsInDefinitions(tools, required, 0)
	for _, raw := range additionalTools {
		var catalog struct {
			Tools json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(raw, &catalog) == nil {
			conversationHostedToolsInDefinitions(catalog.Tools, required, 0)
		}
	}
	for _, raw := range items {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) == nil {
			if name := conversationHostedToolFromItem(item.Type); name != "" {
				required[name] = true
			}
		}
	}
	return required
}
