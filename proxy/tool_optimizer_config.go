package proxy

import "strings"

const (
	toolOptimizerStageCommandRewrite = "command_rewrite"
	toolOptimizerStageOutputReduce   = "output_reduce"

	defaultToolOptimizerCommandRewriteTimeoutMS = 200
	defaultToolOptimizerOutputReduceTimeoutMS   = 500
	defaultToolOptimizerOutputReduceMinBytes    = 20000
	defaultToolOptimizerOutputReduceMaxBytes    = 500000
	defaultToolOptimizerStreamingModeDisabled   = "disabled"
)

// ToolOptimizersConfig configures optional command/output optimization for
// tool calls flowing through /v1/responses. The feature is intentionally
// disabled by default; zero values preserve the legacy passthrough behavior.
type ToolOptimizersConfig struct {
	Enabled        bool                          `json:"enabled" yaml:"enabled"`
	Tools          ToolOptimizerToolsConfig      `json:"tools,omitempty" yaml:"tools,omitempty"`
	CommandRewrite ToolOptimizerRewriteConfig    `json:"command_rewrite,omitempty" yaml:"command_rewrite,omitempty"`
	OutputReduce   ToolOptimizerOutputConfig     `json:"output_reduce,omitempty" yaml:"output_reduce,omitempty"`
	Providers      []ToolOptimizerProviderConfig `json:"providers,omitempty" yaml:"providers,omitempty"`
}

type ToolOptimizerToolsConfig struct {
	ShellFunctionCalls ToolOptimizerShellFunctionCallsConfig `json:"shell_function_calls,omitempty" yaml:"shell_function_calls,omitempty"`
}

type ToolOptimizerShellFunctionCallsConfig struct {
	Enabled        *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Names          []string `json:"names,omitempty" yaml:"names,omitempty"`
	CommandArgPath string   `json:"command_arg_path,omitempty" yaml:"command_arg_path,omitempty"`
}

type ToolOptimizerRewriteConfig struct {
	Enabled       bool   `json:"enabled" yaml:"enabled"`
	StreamingMode string `json:"streaming_mode,omitempty" yaml:"streaming_mode,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
}

type ToolOptimizerOutputConfig struct {
	Enabled       bool `json:"enabled" yaml:"enabled"`
	TimeoutMS     int  `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
	MinInputBytes int  `json:"min_input_bytes,omitempty" yaml:"min_input_bytes,omitempty"`
	MaxInputBytes int  `json:"max_input_bytes,omitempty" yaml:"max_input_bytes,omitempty"`
}

type ToolOptimizerProviderConfig struct {
	ID             string   `json:"id" yaml:"id"`
	Type           string   `json:"type" yaml:"type"`
	Enabled        *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Path           string   `json:"path,omitempty" yaml:"path,omitempty"`
	Args           []string `json:"args,omitempty" yaml:"args,omitempty"`
	Stages         []string `json:"stages,omitempty" yaml:"stages,omitempty"`
	MaxStdoutBytes int      `json:"max_stdout_bytes,omitempty" yaml:"max_stdout_bytes,omitempty"`
	MaxStderrBytes int      `json:"max_stderr_bytes,omitempty" yaml:"max_stderr_bytes,omitempty"`
}

func defaultToolOptimizersConfig() ToolOptimizersConfig {
	return ToolOptimizersConfig{
		Enabled: false,
		Tools: ToolOptimizerToolsConfig{
			ShellFunctionCalls: ToolOptimizerShellFunctionCallsConfig{
				Names:          []string{"shell_command"},
				CommandArgPath: "/command",
			},
		},
		CommandRewrite: ToolOptimizerRewriteConfig{
			Enabled:       false,
			StreamingMode: defaultToolOptimizerStreamingModeDisabled,
			TimeoutMS:     defaultToolOptimizerCommandRewriteTimeoutMS,
		},
		OutputReduce: ToolOptimizerOutputConfig{
			Enabled:       false,
			TimeoutMS:     defaultToolOptimizerOutputReduceTimeoutMS,
			MinInputBytes: defaultToolOptimizerOutputReduceMinBytes,
			MaxInputBytes: defaultToolOptimizerOutputReduceMaxBytes,
		},
		Providers: nil,
	}
}

func (c ToolOptimizersConfig) withDefaults() ToolOptimizersConfig {
	defaults := defaultToolOptimizersConfig()
	defaults.Enabled = c.Enabled

	defaults.Tools = c.Tools.withDefaults()

	defaults.CommandRewrite.Enabled = c.CommandRewrite.Enabled
	if strings.TrimSpace(c.CommandRewrite.StreamingMode) != "" {
		defaults.CommandRewrite.StreamingMode = strings.TrimSpace(c.CommandRewrite.StreamingMode)
	}
	if c.CommandRewrite.TimeoutMS > 0 {
		defaults.CommandRewrite.TimeoutMS = c.CommandRewrite.TimeoutMS
	}

	defaults.OutputReduce.Enabled = c.OutputReduce.Enabled
	if c.OutputReduce.TimeoutMS > 0 {
		defaults.OutputReduce.TimeoutMS = c.OutputReduce.TimeoutMS
	}
	if c.OutputReduce.MinInputBytes > 0 {
		defaults.OutputReduce.MinInputBytes = c.OutputReduce.MinInputBytes
	}
	if c.OutputReduce.MaxInputBytes > 0 {
		defaults.OutputReduce.MaxInputBytes = c.OutputReduce.MaxInputBytes
	}

	if len(c.Providers) > 0 {
		defaults.Providers = c.Providers
	}
	return defaults
}

func (c ToolOptimizerToolsConfig) withDefaults() ToolOptimizerToolsConfig {
	defaults := defaultToolOptimizersConfig().Tools
	if c.ShellFunctionCalls.Enabled != nil {
		defaults.ShellFunctionCalls.Enabled = c.ShellFunctionCalls.Enabled
	}
	if len(c.ShellFunctionCalls.Names) > 0 {
		defaults.ShellFunctionCalls.Names = cleanedStringList(c.ShellFunctionCalls.Names)
	}
	if strings.TrimSpace(c.ShellFunctionCalls.CommandArgPath) != "" {
		defaults.ShellFunctionCalls.CommandArgPath = strings.TrimSpace(c.ShellFunctionCalls.CommandArgPath)
	}
	return defaults
}

func (c ToolOptimizerShellFunctionCallsConfig) enabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

func (c ToolOptimizerProviderConfig) enabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

func (c ToolOptimizerProviderConfig) supportsStage(stage string) bool {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return false
	}
	if len(c.Stages) == 0 {
		return true
	}
	for _, configured := range c.Stages {
		if strings.EqualFold(strings.TrimSpace(configured), stage) {
			return true
		}
	}
	return false
}

func cleanedStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
