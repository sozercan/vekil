package proxy

import (
	"context"
	"strings"
	"time"
)

const maxOptimizedCommandBytes = 32 << 10

type ToolCommandRewriteRequest struct {
	ToolName string
	CallID   string
	Command  string
	Metadata map[string]string
}

type ToolCommandRewriteResult struct {
	Changed  bool
	Command  string
	Provider string
	Reason   string
}

type ToolOutputReduceRequest struct {
	ToolName   string
	CallID     string
	Command    string
	FilterHint string
	Output     string
	Metadata   map[string]string
}

type ToolOutputReduceResult struct {
	Changed  bool
	Output   string
	Provider string
	Reason   string
}

type ToolOptimizer interface {
	ID() string
	RewriteCommand(context.Context, ToolCommandRewriteRequest) (ToolCommandRewriteResult, error)
	ReduceOutput(context.Context, ToolOutputReduceRequest) (ToolOutputReduceResult, error)
}

type stagedToolOptimizer struct {
	optimizer      ToolOptimizer
	commandRewrite bool
	outputReduce   bool
}

type ToolOptimizerManager struct {
	cfg        ToolOptimizersConfig
	providers  []stagedToolOptimizer
	shellNames map[string]struct{}
}

func NewToolOptimizerManager(cfg ToolOptimizersConfig, providers []stagedToolOptimizer) *ToolOptimizerManager {
	cfg = cfg.withDefaults()
	m := &ToolOptimizerManager{cfg: cfg, providers: providers, shellNames: make(map[string]struct{})}
	for _, name := range cfg.Tools.ShellFunctionCalls.Names {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			m.shellNames[strings.ToLower(trimmed)] = struct{}{}
		}
	}
	if len(m.shellNames) == 0 {
		m.shellNames["shell_command"] = struct{}{}
	}
	return m
}

func (m *ToolOptimizerManager) Enabled() bool {
	return m != nil && m.cfg.Enabled
}

func (m *ToolOptimizerManager) CommandRewriteEnabled() bool {
	return m != nil && m.cfg.Enabled && m.cfg.CommandRewrite.Enabled
}

func (m *ToolOptimizerManager) OutputReduceEnabled() bool {
	return m != nil && m.cfg.Enabled && m.cfg.OutputReduce.Enabled
}

func (m *ToolOptimizerManager) ShouldInspectNonStreamingResponses() bool {
	return m != nil && m.cfg.Enabled && (m.cfg.CommandRewrite.Enabled || m.cfg.OutputReduce.Enabled)
}

func (m *ToolOptimizerManager) StreamingCommandRewriteEnabled() bool {
	if !m.CommandRewriteEnabled() {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(m.cfg.CommandRewrite.StreamingMode), defaultToolOptimizerStreamingModeDisabled)
}

func (m *ToolOptimizerManager) ShellFunctionCallsEnabled() bool {
	return m != nil && m.cfg.Enabled && m.cfg.Tools.ShellFunctionCalls.enabled()
}

func (m *ToolOptimizerManager) MatchShellToolName(name string) bool {
	if !m.ShellFunctionCallsEnabled() {
		return false
	}
	_, ok := m.shellNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func (m *ToolOptimizerManager) ShellCommandArgPath() string {
	if m == nil {
		return "/command"
	}
	path := strings.TrimSpace(m.cfg.Tools.ShellFunctionCalls.CommandArgPath)
	if path == "" {
		return "/command"
	}
	return path
}

func (m *ToolOptimizerManager) RewriteCommand(ctx context.Context, req ToolCommandRewriteRequest) ToolCommandRewriteResult {
	if !m.CommandRewriteEnabled() || strings.TrimSpace(req.Command) == "" {
		return ToolCommandRewriteResult{}
	}
	for _, provider := range m.providers {
		if provider.optimizer == nil || !provider.commandRewrite {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.CommandRewrite.TimeoutMS)*time.Millisecond)
		result, err := provider.optimizer.RewriteCommand(callCtx, req)
		cancel()
		if err != nil || !result.Changed {
			continue
		}
		result.Provider = firstNonEmpty(result.Provider, provider.optimizer.ID())
		if !validCommandReplacement(req.Command, result.Command) {
			continue
		}
		return result
	}
	return ToolCommandRewriteResult{}
}

func (m *ToolOptimizerManager) ReduceOutput(ctx context.Context, req ToolOutputReduceRequest) ToolOutputReduceResult {
	if !m.OutputReduceEnabled() {
		return ToolOutputReduceResult{}
	}
	if !m.outputWithinConfiguredThresholds(req.Output) {
		return ToolOutputReduceResult{}
	}
	for _, provider := range m.providers {
		if provider.optimizer == nil || !provider.outputReduce {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.OutputReduce.TimeoutMS)*time.Millisecond)
		result, err := provider.optimizer.ReduceOutput(callCtx, req)
		cancel()
		if err != nil || !result.Changed {
			continue
		}
		result.Provider = firstNonEmpty(result.Provider, provider.optimizer.ID())
		if !validOutputReplacement(req.Output, result.Output) {
			continue
		}
		return result
	}
	return ToolOutputReduceResult{}
}

func (m *ToolOptimizerManager) outputWithinConfiguredThresholds(output string) bool {
	if m == nil {
		return false
	}
	size := len(output)
	if m.cfg.OutputReduce.MinInputBytes > 0 && size < m.cfg.OutputReduce.MinInputBytes {
		return false
	}
	if m.cfg.OutputReduce.MaxInputBytes > 0 && size > m.cfg.OutputReduce.MaxInputBytes {
		return false
	}
	return size > 0
}

func validCommandReplacement(original, replacement string) bool {
	replacement = strings.TrimSpace(replacement)
	if replacement == "" || replacement == strings.TrimSpace(original) {
		return false
	}
	if len(replacement) > maxOptimizedCommandBytes {
		return false
	}
	if strings.ContainsAny(replacement, "\x00\r\n") {
		return false
	}
	return true
}

func validOutputReplacement(original, replacement string) bool {
	if strings.TrimSpace(replacement) == "" {
		return false
	}
	return replacement != original
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (h *ProxyHandler) initializeToolOptimizers() {
	if h == nil {
		return
	}
	cfg := h.providersConfig.ToolOptimizers.withDefaults()
	providers := buildConfiguredToolOptimizers(cfg)
	h.toolOptimizers = NewToolOptimizerManager(cfg, providers)
	h.toolContexts = NewToolExecutionContextStore()
}

func buildConfiguredToolOptimizers(cfg ToolOptimizersConfig) []stagedToolOptimizer {
	cfg = cfg.withDefaults()
	if !cfg.Enabled {
		return nil
	}
	providers := make([]stagedToolOptimizer, 0, len(cfg.Providers))
	for _, providerCfg := range cfg.Providers {
		if !providerCfg.enabled() {
			continue
		}
		optimizer := newToolOptimizerFromConfig(providerCfg)
		if optimizer == nil {
			continue
		}
		providers = append(providers, stagedToolOptimizer{
			optimizer:      optimizer,
			commandRewrite: providerCfg.supportsStage(toolOptimizerStageCommandRewrite),
			outputReduce:   providerCfg.supportsStage(toolOptimizerStageOutputReduce),
		})
	}
	return providers
}

func newToolOptimizerFromConfig(cfg ToolOptimizerProviderConfig) ToolOptimizer {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "noop":
		return noopToolOptimizer{id: firstNonEmpty(cfg.ID, "noop")}
	case "rtk_cli":
		return newRTKToolOptimizer(cfg)
	case "exec_json":
		return newExecJSONToolOptimizer(cfg)
	default:
		return nil
	}
}
