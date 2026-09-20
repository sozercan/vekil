package proxy

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultConversationMigrationMaxHistoryBytes = 8 * 1024 * 1024
	defaultConversationMigrationMaxTotalBytes   = 256 * 1024 * 1024
	defaultConversationMigrationMaxSnapshots    = 4096

	maxConversationMigrationHistoryBytes       = 64 * 1024 * 1024
	maxConversationMigrationTotalBytes   int64 = 16 * 1024 * 1024 * 1024
	maxConversationMigrationSnapshots          = 1_000_000
)

// ConversationMigrationConfig opts selected operational routes into durable
// Responses conversation migration between Azure and Copilot targets. Limits
// bound complete snapshots, total stored bytes, and count without reserving storage.
type ConversationMigrationConfig struct {
	Routes          []string `json:"routes" yaml:"routes"`
	MaxHistoryBytes int      `json:"max_history_bytes,omitempty" yaml:"max_history_bytes,omitempty"`
	MaxTotalBytes   int64    `json:"max_total_bytes,omitempty" yaml:"max_total_bytes,omitempty"`
	MaxSnapshots    int      `json:"max_snapshots,omitempty" yaml:"max_snapshots,omitempty"`

	maxHistoryBytesSet bool
	maxTotalBytesSet   bool
	maxSnapshotsSet    bool
}

func (config ConversationMigrationConfig) withDefaults() ConversationMigrationConfig {
	if config.MaxHistoryBytes == 0 {
		config.MaxHistoryBytes = defaultConversationMigrationMaxHistoryBytes
	}
	if config.MaxTotalBytes == 0 {
		config.MaxTotalBytes = defaultConversationMigrationMaxTotalBytes
	}
	if config.MaxSnapshots == 0 {
		config.MaxSnapshots = defaultConversationMigrationMaxSnapshots
	}
	return config
}

func validateYAMLConversationMigrationFieldTypes(node *yaml.Node) error {
	// yaml.v3 converts floating-point scalars to integers during struct decoding.
	// Require integer limits before that conversion loses the fractional part.
	for _, field := range []string{"max_history_bytes", "max_total_bytes", "max_snapshots"} {
		value := yamlDereferenceAlias(yamlMappingValue(node, field))
		if value != nil && value.Tag != "!!int" && value.Tag != "!!null" {
			return configPathError("conversation_migration."+field, "must be a positive integer")
		}
	}
	if routes := yamlDereferenceAlias(yamlMappingValue(node, "routes")); routes != nil && routes.Kind == yaml.SequenceNode {
		for index, rawRoute := range routes.Content {
			route := yamlDereferenceAlias(rawRoute)
			if route == nil || route.Kind != yaml.ScalarNode || route.Tag != "!!str" {
				return configPathError(fmt.Sprintf("conversation_migration.routes[%d]", index), "must be an operational route ID string")
			}
		}
	}
	return nil
}

func normalizeAndValidateConversationMigrationConfig(cfg *ProvidersConfig, routes map[string]*ModelRouteConfig, providers map[string]providerConfigDescriptor) error {
	config := cfg.ConversationMigration
	if config == nil {
		if cfg.conversationMigrationSet {
			return configPathError("conversation_migration", "must be an object")
		}
		return nil
	}
	if cfg.StateBindings != nil && strings.TrimSpace(cfg.StateBindings.Mode) == "memory" {
		return configPathError("conversation_migration", "requires durable state_bindings")
	}
	for _, limit := range []struct {
		field string
		value int64
		max   int64
		set   bool
	}{
		{"max_history_bytes", int64(config.MaxHistoryBytes), maxConversationMigrationHistoryBytes, config.maxHistoryBytesSet},
		{"max_total_bytes", config.MaxTotalBytes, maxConversationMigrationTotalBytes, config.maxTotalBytesSet},
		{"max_snapshots", int64(config.MaxSnapshots), maxConversationMigrationSnapshots, config.maxSnapshotsSet},
	} {
		if limit.value < 0 || limit.value > limit.max || limit.set && limit.value == 0 {
			return configPathError("conversation_migration."+limit.field, "must be between 1 and %d", limit.max)
		}
	}
	*config = config.withDefaults()
	if int64(config.MaxHistoryBytes) > config.MaxTotalBytes {
		return configPathError("conversation_migration.max_total_bytes", "must be at least max_history_bytes (%d)", config.MaxHistoryBytes)
	}
	if len(config.Routes) == 0 {
		return configPathError("conversation_migration.routes", "must contain at least one operational route ID")
	}
	if len(config.Routes) > maxExplicitModelRoutes {
		return configPathError("conversation_migration.routes", "contains %d routes; maximum is %d", len(config.Routes), maxExplicitModelRoutes)
	}
	seen := make(map[string]int, len(config.Routes))
	for index, rawRouteID := range config.Routes {
		path := fmt.Sprintf("conversation_migration.routes[%d]", index)
		routeID, err := normalizeOperationalID(rawRouteID, path)
		if err != nil {
			return err
		}
		if prior, exists := seen[routeID]; exists {
			return configPathError(path, "duplicates conversation_migration.routes[%d]", prior)
		}
		seen[routeID] = index
		config.Routes[index] = routeID
		route, exists := routes[routeID]
		if !exists {
			return configPathError(path, "references unknown operational route %q", routeID)
		}
		if !modelRouteConfigIsPublic(route, cfg.EffectiveSchemaVersion()) {
			return configPathError(path, "route %q must have public exposure", routeID)
		}
		if !supportsEndpoint(route.Endpoints, providerEndpointResponses) {
			return configPathError(path, "route %q must support %q", routeID, providerEndpointResponses)
		}
		if route.Routing.Mode != string(routeModePriorityFailover) {
			return configPathError(path, "route %q must use routing.mode %q", routeID, routeModePriorityFailover)
		}
		if len(route.Targets) < 2 {
			return configPathError(path, "route %q must have at least two targets", routeID)
		}
		if route.Routing.MaxTargetAttempts < 2 || route.Routing.MaxUpstreamSends < 2 {
			return configPathError(path, "route %q must allow at least two target attempts and upstream sends", routeID)
		}
		for _, target := range route.Targets {
			if !conversationMigrationProviderSupported(providers[target.Provider].kind) {
				return configPathError(path, "route %q target %q must use an azure-openai or copilot provider", routeID, target.ID)
			}
		}
	}
	return nil
}

func conversationMigrationProviderSupported(kind providerType) bool {
	return kind == providerTypeAzureOpenAI || kind == providerTypeCopilot
}
