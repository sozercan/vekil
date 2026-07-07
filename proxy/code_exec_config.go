package proxy

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Default policy and loop values for proxy-mediated code execution. They are
// deliberately conservative: the feature is disabled unless explicitly enabled,
// and once enabled the defaults bound runaway commands and internal loops.
const (
	defaultCodeExecBackend        = "local"
	defaultCodeExecTimeoutMS      = 30000
	defaultCodeExecMaxOutputBytes = 1 << 20 // 1 MiB per stream
	defaultCodeExecMaxLoopDepth   = 8
	defaultCodeExecOwnedTool      = "Bash"
)

// Environment variable overrides. These let an operator toggle and tune the
// feature without a full config file; they mirror the JSON/YAML fields.
const (
	envCodeExecEnabled        = "VEKIL_CODE_EXEC_ENABLED"
	envCodeExecOwnedTools     = "VEKIL_CODE_EXEC_OWNED_TOOLS"
	envCodeExecBackend        = "VEKIL_CODE_EXEC_BACKEND"
	envCodeExecTimeoutMS      = "VEKIL_CODE_EXEC_TIMEOUT_MS"
	envCodeExecMaxOutputBytes = "VEKIL_CODE_EXEC_MAX_OUTPUT_BYTES"
	envCodeExecWorkDir        = "VEKIL_CODE_EXEC_WORKDIR"
	envCodeExecEnvAllowlist   = "VEKIL_CODE_EXEC_ENV_ALLOWLIST"
	envCodeExecMaxLoopDepth   = "VEKIL_CODE_EXEC_MAX_LOOP_DEPTH"
)

// CodeExecPolicyConfig is the serializable form of the execution policy. It is
// converted to a CodeExecPolicy (with a typed timeout) via toPolicy.
type CodeExecPolicyConfig struct {
	TimeoutMS      int      `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
	MaxOutputBytes int      `json:"max_output_bytes,omitempty" yaml:"max_output_bytes,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty" yaml:"working_dir,omitempty"`
	EnvAllowlist   []string `json:"env_allowlist,omitempty" yaml:"env_allowlist,omitempty"`
}

// CodeExecConfig configures optional proxy-mediated code execution. The feature
// is intentionally disabled by default; the zero value preserves the default
// transparent proxy behavior. When enabled, Vekil intercepts owned tool calls
// in buffered (non-streaming) chat responses, executes them through the
// configured backend, and loops results back to the model internally before
// returning only the final assistant response to the client.
type CodeExecConfig struct {
	Enabled      bool                 `json:"enabled" yaml:"enabled"`
	OwnedTools   []string             `json:"owned_tools,omitempty" yaml:"owned_tools,omitempty"`
	Backend      string               `json:"backend,omitempty" yaml:"backend,omitempty"`
	MaxLoopDepth int                  `json:"max_loop_depth,omitempty" yaml:"max_loop_depth,omitempty"`
	Policy       CodeExecPolicyConfig `json:"policy,omitempty" yaml:"policy,omitempty"`
}

func defaultCodeExecConfig() CodeExecConfig {
	return CodeExecConfig{
		Enabled:      false,
		OwnedTools:   []string{defaultCodeExecOwnedTool},
		Backend:      defaultCodeExecBackend,
		MaxLoopDepth: defaultCodeExecMaxLoopDepth,
		Policy: CodeExecPolicyConfig{
			TimeoutMS:      defaultCodeExecTimeoutMS,
			MaxOutputBytes: defaultCodeExecMaxOutputBytes,
			// WorkingDir intentionally empty: the local backend requires an
			// operator-provided directory before it will execute anything.
			WorkingDir:   "",
			EnvAllowlist: nil,
		},
	}
}

// withDefaults returns a fully-populated config, filling unset fields with
// defaults while preserving any operator-provided values.
func (c CodeExecConfig) withDefaults() CodeExecConfig {
	defaults := defaultCodeExecConfig()
	defaults.Enabled = c.Enabled

	if tools := cleanedStringList(c.OwnedTools); len(tools) > 0 {
		defaults.OwnedTools = tools
	}
	if backend := strings.TrimSpace(c.Backend); backend != "" {
		defaults.Backend = strings.ToLower(backend)
	}
	if c.MaxLoopDepth > 0 {
		defaults.MaxLoopDepth = c.MaxLoopDepth
	}
	if c.Policy.TimeoutMS > 0 {
		defaults.Policy.TimeoutMS = c.Policy.TimeoutMS
	}
	if c.Policy.MaxOutputBytes > 0 {
		defaults.Policy.MaxOutputBytes = c.Policy.MaxOutputBytes
	}
	if dir := strings.TrimSpace(c.Policy.WorkingDir); dir != "" {
		defaults.Policy.WorkingDir = dir
	}
	if allow := cleanedStringList(c.Policy.EnvAllowlist); len(allow) > 0 {
		defaults.Policy.EnvAllowlist = allow
	}
	return defaults
}

// withEnvOverrides applies VEKIL_CODE_EXEC_* environment variables on top of the
// existing config. Environment values take precedence over file config so an
// operator can enable or tune the feature at deploy time. Invalid numeric
// values are ignored (the prior value is kept) rather than failing startup.
func (c CodeExecConfig) withEnvOverrides() CodeExecConfig {
	if v, ok := os.LookupEnv(envCodeExecEnabled); ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			c.Enabled = parsed
		}
	}
	if v, ok := os.LookupEnv(envCodeExecOwnedTools); ok {
		if tools := splitAndClean(v); len(tools) > 0 {
			c.OwnedTools = tools
		}
	}
	if v, ok := os.LookupEnv(envCodeExecBackend); ok {
		if backend := strings.TrimSpace(v); backend != "" {
			c.Backend = strings.ToLower(backend)
		}
	}
	if v, ok := os.LookupEnv(envCodeExecMaxLoopDepth); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			c.MaxLoopDepth = parsed
		}
	}
	if v, ok := os.LookupEnv(envCodeExecTimeoutMS); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			c.Policy.TimeoutMS = parsed
		}
	}
	if v, ok := os.LookupEnv(envCodeExecMaxOutputBytes); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			c.Policy.MaxOutputBytes = parsed
		}
	}
	if v, ok := os.LookupEnv(envCodeExecWorkDir); ok {
		if dir := strings.TrimSpace(v); dir != "" {
			c.Policy.WorkingDir = dir
		}
	}
	if v, ok := os.LookupEnv(envCodeExecEnvAllowlist); ok {
		c.Policy.EnvAllowlist = splitAndClean(v)
	}
	return c
}

// active reports whether the feature is enabled and has at least one owned tool
// to intercept.
func (c CodeExecConfig) active() bool {
	return c.Enabled && len(cleanedStringList(c.OwnedTools)) > 0
}

// ownsTool reports whether the given tool name is configured for proxy-owned
// execution. Matching is case-insensitive to tolerate provider name casing.
func (c CodeExecConfig) ownsTool(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, owned := range c.OwnedTools {
		if strings.EqualFold(strings.TrimSpace(owned), name) {
			return true
		}
	}
	return false
}

// toPolicy converts the serializable policy config into the runtime policy
// applied to each execution.
func (c CodeExecPolicyConfig) toPolicy() CodeExecPolicy {
	return CodeExecPolicy{
		Timeout:        time.Duration(c.TimeoutMS) * time.Millisecond,
		MaxOutputBytes: c.MaxOutputBytes,
		WorkingDir:     strings.TrimSpace(c.WorkingDir),
		EnvAllowlist:   cleanedStringList(c.EnvAllowlist),
	}
}

// splitAndClean splits a comma-separated environment value into a de-duplicated,
// whitespace-trimmed list.
func splitAndClean(value string) []string {
	return cleanedStringList(strings.Split(value, ","))
}
