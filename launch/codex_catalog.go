package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const codexCatalogProbeTimeout = 2 * time.Second

type codexCatalog struct {
	Models []map[string]interface{} `json:"models"`
}

func fallbackCodexModelTemplate() map[string]interface{} {
	return map[string]interface{}{
		"slug":                             "vekil",
		"display_name":                     "Vekil",
		"description":                      "Vekil-routed model.",
		"default_reasoning_level":          "medium",
		"supported_reasoning_levels":       []map[string]string{{"effort": "low", "description": "Faster responses"}, {"effort": "medium", "description": "Balanced reasoning"}, {"effort": "high", "description": "Deeper reasoning"}, {"effort": "xhigh", "description": "Extra-deep reasoning"}},
		"shell_type":                       "shell_command",
		"visibility":                       "list",
		"supported_in_api":                 true,
		"priority":                         0,
		"additional_speed_tiers":           []interface{}{},
		"availability_nux":                 nil,
		"upgrade":                          nil,
		"base_instructions":                "You are Codex, a coding agent.",
		"supports_reasoning_summaries":     true,
		"default_reasoning_summary":        "none",
		"support_verbosity":                true,
		"default_verbosity":                "low",
		"apply_patch_tool_type":            "freeform",
		"web_search_tool_type":             "text",
		"truncation_policy":                map[string]interface{}{"mode": "tokens", "limit": 10000},
		"supports_parallel_tool_calls":     true,
		"supports_image_detail_original":   false,
		"context_window":                   128000,
		"max_context_window":               128000,
		"effective_context_window_percent": 95,
		"experimental_supported_tools":     []interface{}{},
		"input_modalities":                 []string{"text"},
		"supports_search_tool":             false,
	}
}

func loadCodexModelTemplate(executable resolvedExecutable, environment []string, modelID string) map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), codexCatalogProbeTimeout)
	defer cancel()
	args := append(append([]string(nil), executable.prefixArgs...), "debug", "models", "--bundled")
	output, err := runContainedCommand(ctx, executable.path, args, environment)
	if err != nil {
		return fallbackCodexModelTemplate()
	}
	var catalog codexCatalog
	if err := json.Unmarshal(output, &catalog); err != nil || len(catalog.Models) == 0 {
		return fallbackCodexModelTemplate()
	}
	for _, candidate := range []string{modelID, "gpt-5.5"} {
		for _, model := range catalog.Models {
			if slug, _ := model["slug"].(string); slug == candidate {
				return cloneStringMap(model)
			}
		}
	}
	return cloneStringMap(catalog.Models[0])
}

func cloneStringMap(input map[string]interface{}) map[string]interface{} {
	body, err := json.Marshal(input)
	if err != nil {
		return fallbackCodexModelTemplate()
	}
	var output map[string]interface{}
	if err := json.Unmarshal(body, &output); err != nil {
		return fallbackCodexModelTemplate()
	}
	return output
}

func buildCodexModelCatalog(executable resolvedExecutable, environment []string, model ModelInfo, dryRun bool) ([]byte, error) {
	template := fallbackCodexModelTemplate()
	if !dryRun {
		template = loadCodexModelTemplate(executable, environment, model.ID)
	}
	displayName := strings.TrimSpace(model.Name)
	if displayName == "" {
		displayName = model.ID
	}
	template["slug"] = model.ID
	template["display_name"] = displayName
	template["description"] = fmt.Sprintf("Vekil-routed model %s.", model.ID)
	template["priority"] = 0
	template["visibility"] = "list"
	template["supported_in_api"] = true
	template["availability_nux"] = nil
	template["upgrade"] = nil

	if contextWindow := modelContextWindow(model); contextWindow > 0 {
		template["context_window"] = contextWindow
	}
	if maxContextWindow := modelMaxContextWindow(model); maxContextWindow > 0 {
		template["max_context_window"] = maxContextWindow
	}
	if model.EffectiveContextWindowPercentage > 0 {
		template["effective_context_window_percent"] = model.EffectiveContextWindowPercentage
	}
	efforts := model.Capabilities.Supports.ReasoningEffort
	levels := make([]map[string]string, 0, len(efforts))
	validEfforts := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.TrimSpace(effort)
		if effort == "" {
			continue
		}
		validEfforts = append(validEfforts, effort)
		levels = append(levels, map[string]string{
			"effort":      effort,
			"description": "Use " + effort + " reasoning effort",
		})
	}
	template["supported_reasoning_levels"] = levels
	if len(validEfforts) > 0 {
		template["default_reasoning_level"] = defaultReasoningEffort(validEfforts)
	} else {
		template["default_reasoning_level"] = "none"
	}
	template["supports_reasoning_summaries"] = false
	template["support_verbosity"] = false
	template["supports_search_tool"] = false
	template["experimental_supported_tools"] = []interface{}{}
	template["supports_parallel_tool_calls"] = model.Capabilities.Supports.ParallelToolCalls
	template["supports_image_detail_original"] = false
	template["input_modalities"] = []string{"text"}
	if model.Capabilities.Supports.Vision {
		template["input_modalities"] = []string{"text", "image"}
	}
	return json.Marshal(codexCatalog{Models: []map[string]interface{}{template}})
}

func defaultReasoningEffort(efforts []string) string {
	for _, want := range []string{"medium", "low", "none"} {
		for _, effort := range efforts {
			if strings.TrimSpace(effort) == want {
				return want
			}
		}
	}
	for _, effort := range efforts {
		if effort = strings.TrimSpace(effort); effort != "" {
			return effort
		}
	}
	return "medium"
}
