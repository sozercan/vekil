package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const defaultDurableStateBindingMaxEntries = 8 * 1024 * 1024

// StateBindingsConfig controls explicit-route ownership. Schema-v2 routes use
// durable storage by default; memory explicitly opts out of restart recovery.
// MaxEntries limits logical records and never reserves their storage in advance.
type StateBindingsConfig struct {
	Mode       string `json:"mode,omitempty" yaml:"mode,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty" yaml:"max_entries,omitempty"`
	File       string `json:"file,omitempty" yaml:"file,omitempty"`

	maxEntriesSet bool
}

// WithStateBindingsConfig supplies optional process overrides. Empty fields
// follow the providers config. Mode "config" also follows the providers config;
// mode "memory" overrides a configured durable file without opening it.
func WithStateBindingsConfig(config StateBindingsConfig) Option {
	return func(h *ProxyHandler) { h.stateBindingsOverride = config }
}

// StateBindingsEnvironmentOverrides is shared by the CLI and menubar. Provider
// config needs no environment settings; these are optional process overrides.
func StateBindingsEnvironmentOverrides() (StateBindingsConfig, error) {
	return ParseStateBindingsOverrides(os.Getenv("STATE_BINDINGS_MODE"), os.Getenv("STATE_BINDINGS_FILE"), os.Getenv("STATE_BINDINGS_MAX_ENTRIES"))
}

// ParseStateBindingsOverrides parses the effective optional flags or environment
// values. Parse after flag selection so an explicit flag can replace a bad env value.
func ParseStateBindingsOverrides(mode, file, maxEntries string) (StateBindingsConfig, error) {
	config := StateBindingsConfig{
		Mode: strings.TrimSpace(mode),
		File: file,
	}
	if value := strings.TrimSpace(maxEntries); value != "" {
		var err error
		config.MaxEntries, err = strconv.Atoi(value)
		if err != nil || config.MaxEntries < 0 {
			return StateBindingsConfig{}, configPathError("state_bindings.max_entries", "override must be a nonnegative integer; zero follows configuration")
		}
	}
	if err := validateStateBindingsValues(config, true); err != nil {
		return StateBindingsConfig{}, err
	}
	return config, nil
}

func validateProviderStateBindings(cfg ProvidersConfig) error {
	if cfg.StateBindings == nil {
		if cfg.stateBindingsSet {
			return configPathError("state_bindings", "must be an object")
		}
		return nil
	}
	if cfg.EffectiveSchemaVersion() != ProvidersConfigSchemaVersion2 {
		return configPathError("state_bindings", "requires schema_version: 2")
	}
	return validateStateBindingsValues(*cfg.StateBindings, false)
}

func validateStateBindingsValues(config StateBindingsConfig, override bool) error {
	switch strings.TrimSpace(config.Mode) {
	case "", "durable", "memory":
	case "config":
		if !override {
			return configPathError("state_bindings.mode", "must be durable or memory")
		}
	default:
		return configPathError("state_bindings.mode", "must be durable or memory")
	}
	if config.MaxEntries < 0 || (config.maxEntriesSet && config.MaxEntries == 0) {
		return configPathError("state_bindings.max_entries", "must be a positive integer")
	}
	if config.File != "" && (!filepath.IsAbs(config.File) || filepath.Clean(config.File) != config.File || strings.ContainsRune(config.File, '\x00')) {
		return configPathError("state_bindings.file", "must be an absolute, clean path")
	}
	return nil
}

func resolveStateBindingsConfig(providers ProvidersConfig, override StateBindingsConfig) (StateBindingsConfig, error) {
	if err := validateProviderStateBindings(providers); err != nil {
		return StateBindingsConfig{}, err
	}
	if err := validateStateBindingsValues(override, true); err != nil {
		return StateBindingsConfig{}, err
	}
	config := StateBindingsConfig{Mode: "memory"}
	if providers.EffectiveSchemaVersion() == ProvidersConfigSchemaVersion2 && len(providers.ModelRoutes) > 0 {
		config.Mode = "durable"
	}
	if source := providers.StateBindings; source != nil {
		if mode := strings.TrimSpace(source.Mode); mode != "" {
			config.Mode = mode
		}
		config.File, config.MaxEntries = source.File, source.MaxEntries
	}
	if override.File != "" {
		config.File, config.Mode = override.File, "durable"
	}
	if override.MaxEntries != 0 {
		config.MaxEntries = override.MaxEntries
	}
	if mode := strings.TrimSpace(override.Mode); mode != "" && mode != "config" {
		config.Mode = mode
	}
	if config.MaxEntries == 0 {
		config.MaxEntries = defaultStateBindingMaxEntries
		if config.Mode == "durable" {
			config.MaxEntries = defaultDurableStateBindingMaxEntries
		}
	}
	return config, nil
}

func defaultStateBindingsPath() (string, error) {
	return stateBindingsPathForOS(runtime.GOOS, os.Getenv("XDG_DATA_HOME"), os.UserHomeDir)
}

func stateBindingsPathForOS(goos, xdgDataHome string, homeDir func() (string, error)) (string, error) {
	var base string
	switch goos {
	case "linux":
		base = xdgDataHome
		if base == "" {
			home, err := homeDir()
			if err != nil {
				return "", fmt.Errorf("locate state application-data directory: %w", err)
			}
			base = filepath.Join(home, ".local", "share")
		}
	case "darwin":
		home, err := homeDir()
		if err != nil {
			return "", fmt.Errorf("locate state application-data directory: %w", err)
		}
		base = filepath.Join(home, "Library", "Application Support")
	default:
		return "", errDurableStatePlatform
	}
	if !filepath.IsAbs(base) || filepath.Clean(base) != base {
		return "", fmt.Errorf("state application-data directory must be an absolute, clean path")
	}
	return filepath.Join(base, "vekil", "state", "bindings.db"), nil
}
