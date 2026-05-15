package proxy

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

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
// tool calls flowing through supported API surfaces. The feature is
// intentionally disabled by default; zero values preserve the legacy passthrough
// behavior.
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

	timeoutMSSet     bool
	minInputBytesSet bool
	maxInputBytesSet bool
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
type toolOptimizerOutputConfigFields struct {
	Enabled       bool `json:"enabled" yaml:"enabled"`
	TimeoutMS     *int `json:"timeout_ms" yaml:"timeout_ms"`
	MinInputBytes *int `json:"min_input_bytes" yaml:"min_input_bytes"`
	MaxInputBytes *int `json:"max_input_bytes" yaml:"max_input_bytes"`
}

func (c *ToolOptimizerOutputConfig) UnmarshalJSON(data []byte) error {
	var fields toolOptimizerOutputConfigFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	c.applyDecodedFields(fields)
	return nil
}

func (c *ToolOptimizerOutputConfig) UnmarshalYAML(value *yaml.Node) error {
	var fields toolOptimizerOutputConfigFields
	if err := value.Decode(&fields); err != nil {
		return err
	}
	c.applyDecodedFields(fields)
	return nil
}

func (c *ToolOptimizerOutputConfig) applyDecodedFields(fields toolOptimizerOutputConfigFields) {
	c.Enabled = fields.Enabled
	c.TimeoutMS = 0
	c.MinInputBytes = 0
	c.MaxInputBytes = 0
	c.timeoutMSSet = false
	c.minInputBytesSet = false
	c.maxInputBytesSet = false
	if fields.TimeoutMS != nil {
		c.TimeoutMS = *fields.TimeoutMS
		c.timeoutMSSet = true
	}
	if fields.MinInputBytes != nil {
		c.MinInputBytes = *fields.MinInputBytes
		c.minInputBytesSet = true
	}
	if fields.MaxInputBytes != nil {
		c.MaxInputBytes = *fields.MaxInputBytes
		c.maxInputBytesSet = true
	}
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
	if c.OutputReduce.timeoutMSSet {
		defaults.OutputReduce.TimeoutMS = c.OutputReduce.TimeoutMS
		defaults.OutputReduce.timeoutMSSet = true
	} else if c.OutputReduce.TimeoutMS > 0 {
		defaults.OutputReduce.TimeoutMS = c.OutputReduce.TimeoutMS
	}
	if c.OutputReduce.minInputBytesSet {
		defaults.OutputReduce.MinInputBytes = c.OutputReduce.MinInputBytes
		defaults.OutputReduce.minInputBytesSet = true
	} else if c.OutputReduce.MinInputBytes > 0 {
		defaults.OutputReduce.MinInputBytes = c.OutputReduce.MinInputBytes
	}
	if c.OutputReduce.maxInputBytesSet {
		defaults.OutputReduce.MaxInputBytes = c.OutputReduce.MaxInputBytes
		defaults.OutputReduce.maxInputBytesSet = true
	} else if c.OutputReduce.MaxInputBytes > 0 {
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
