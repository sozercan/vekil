package proxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	maxOptimizedCommandBytes                       = 32 << 10
	defaultToolOptimizerMaxProviderCallsPerTurn    = 8
	defaultToolOptimizerMaxConcurrentExternalCalls = 4
)

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

type externalToolOptimizer interface {
	optimizerUsesExternalProcess()
}

type stagedToolOptimizer struct {
	optimizer      ToolOptimizer
	commandRewrite bool
	outputReduce   bool
}

type ToolOptimizerManager struct {
	cfg           ToolOptimizersConfig
	providers     []stagedToolOptimizer
	shellNames    map[string]struct{}
	externalCalls chan struct{}
}

type toolOptimizerTurnBudgetContextKey struct{}

type toolOptimizerTurnBudget struct {
	manager *ToolOptimizerManager
	stage   string

	mu               sync.Mutex
	providerCalls    int
	stoppedProviders map[int]struct{}
}

func NewToolOptimizerManager(cfg ToolOptimizersConfig, providers []stagedToolOptimizer) *ToolOptimizerManager {
	cfg = cfg.withDefaults()
	m := &ToolOptimizerManager{
		cfg:           cfg,
		providers:     providers,
		shellNames:    make(map[string]struct{}),
		externalCalls: make(chan struct{}, defaultToolOptimizerMaxConcurrentExternalCalls),
	}
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
	if m == nil || !m.cfg.Enabled {
		return false
	}
	for _, provider := range m.providers {
		if provider.optimizer == nil {
			continue
		}
		if m.cfg.CommandRewrite.Enabled && provider.commandRewrite {
			return true
		}
		if m.cfg.OutputReduce.Enabled && provider.outputReduce {
			return true
		}
	}
	return false
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

func (h *ProxyHandler) withToolOptimizerStageContext(parent context.Context, manager *ToolOptimizerManager, stage string) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancelParent := context.WithCancel(parent)
	lifecycle := h.lifecycleContext()
	stopLifecycle := context.AfterFunc(lifecycle, cancelParent)
	if h.ShuttingDown() || lifecycle.Err() != nil {
		cancelParent()
	}
	stageCtx, cancelStage := manager.withTurnBudget(ctx, stage)
	return stageCtx, func() {
		cancelStage()
		stopLifecycle()
		cancelParent()
	}
}

// withTurnBudget installs one deadline and provider-call budget for all items in
// a request/response optimizer stage. Direct manager calls intentionally do not
// install this state, preserving their historical per-call timeout behavior.
func (m *ToolOptimizerManager) withTurnBudget(parent context.Context, stage string) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if existing := toolOptimizerTurnBudgetFromContext(parent, m, stage); existing != nil {
		return parent, func() {}
	}

	timeoutMS := 0
	switch stage {
	case toolOptimizerStageCommandRewrite:
		timeoutMS = m.cfg.CommandRewrite.TimeoutMS
	case toolOptimizerStageOutputReduce:
		timeoutMS = m.cfg.OutputReduce.TimeoutMS
	}
	budgetCtx, cancel := optimizerCallContext(parent, timeoutMS)
	budget := &toolOptimizerTurnBudget{
		manager:          m,
		stage:            stage,
		stoppedProviders: make(map[int]struct{}),
	}
	return context.WithValue(budgetCtx, toolOptimizerTurnBudgetContextKey{}, budget), cancel
}

func toolOptimizerTurnBudgetFromContext(ctx context.Context, manager *ToolOptimizerManager, stage string) *toolOptimizerTurnBudget {
	if ctx == nil {
		return nil
	}
	budget, _ := ctx.Value(toolOptimizerTurnBudgetContextKey{}).(*toolOptimizerTurnBudget)
	if budget == nil || budget.manager != manager || budget.stage != stage {
		return nil
	}
	return budget
}

func (b *toolOptimizerTurnBudget) providerAvailable(providerIndex int) (available, exhausted bool) {
	if b == nil {
		return true, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.providerCalls >= defaultToolOptimizerMaxProviderCallsPerTurn {
		return false, true
	}
	_, stopped := b.stoppedProviders[providerIndex]
	return !stopped, false
}

func (b *toolOptimizerTurnBudget) startProviderCall(providerIndex int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.providerCalls >= defaultToolOptimizerMaxProviderCallsPerTurn {
		return false
	}
	if _, stopped := b.stoppedProviders[providerIndex]; stopped {
		return false
	}
	b.providerCalls++
	return true
}

func (b *toolOptimizerTurnBudget) stopProvider(providerIndex int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.stoppedProviders[providerIndex] = struct{}{}
	b.mu.Unlock()
}

func (m *ToolOptimizerManager) RewriteCommand(ctx context.Context, req ToolCommandRewriteRequest) ToolCommandRewriteResult {
	if !m.CommandRewriteEnabled() || strings.TrimSpace(req.Command) == "" {
		return ToolCommandRewriteResult{}
	}
	budget := toolOptimizerTurnBudgetFromContext(ctx, m, toolOptimizerStageCommandRewrite)
	for providerIndex, provider := range m.providers {
		if provider.optimizer == nil || !provider.commandRewrite {
			continue
		}
		if budget != nil {
			available, exhausted := budget.providerAvailable(providerIndex)
			if exhausted {
				return ToolCommandRewriteResult{}
			}
			if !available {
				continue
			}
		}

		callCtx, cancel := optimizerProviderCallContext(ctx, m.cfg.CommandRewrite.TimeoutMS, budget)
		if optimizerContextErr(callCtx) != nil {
			cancel()
			return ToolCommandRewriteResult{}
		}
		release, acquired := m.acquireExternalCall(callCtx, provider.optimizer)
		if !acquired {
			if budget != nil && optimizerContextErr(callCtx) != nil {
				budget.stopProvider(providerIndex)
			}
			cancel()
			return ToolCommandRewriteResult{}
		}
		if budget != nil && !budget.startProviderCall(providerIndex) {
			release()
			cancel()
			return ToolCommandRewriteResult{}
		}

		result, err := provider.optimizer.RewriteCommand(callCtx, req)
		release()
		if callCtxErr := optimizerContextErr(callCtx); callCtxErr != nil {
			cancel()
			if budget != nil {
				budget.stopProvider(providerIndex)
			}
			return ToolCommandRewriteResult{}
		}
		if err != nil {
			cancel()
			if budget != nil && optimizerProviderTimedOut(err) {
				budget.stopProvider(providerIndex)
			}
			continue
		}
		if !result.Changed {
			cancel()
			continue
		}
		result.Provider = firstNonEmpty(result.Provider, provider.optimizer.ID())
		if !validCommandReplacement(req.Command, result.Command) {
			cancel()
			continue
		}
		if optimizerContextErr(callCtx) != nil {
			cancel()
			if budget != nil {
				budget.stopProvider(providerIndex)
			}
			return ToolCommandRewriteResult{}
		}
		cancel()
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
	budget := toolOptimizerTurnBudgetFromContext(ctx, m, toolOptimizerStageOutputReduce)
	for providerIndex, provider := range m.providers {
		if provider.optimizer == nil || !provider.outputReduce {
			continue
		}
		if budget != nil {
			available, exhausted := budget.providerAvailable(providerIndex)
			if exhausted {
				return ToolOutputReduceResult{}
			}
			if !available {
				continue
			}
		}

		callCtx, cancel := optimizerProviderCallContext(ctx, m.cfg.OutputReduce.TimeoutMS, budget)
		if optimizerContextErr(callCtx) != nil {
			cancel()
			return ToolOutputReduceResult{}
		}
		release, acquired := m.acquireExternalCall(callCtx, provider.optimizer)
		if !acquired {
			if budget != nil && optimizerContextErr(callCtx) != nil {
				budget.stopProvider(providerIndex)
			}
			cancel()
			return ToolOutputReduceResult{}
		}
		if budget != nil && !budget.startProviderCall(providerIndex) {
			release()
			cancel()
			return ToolOutputReduceResult{}
		}

		result, err := provider.optimizer.ReduceOutput(callCtx, req)
		release()
		if callCtxErr := optimizerContextErr(callCtx); callCtxErr != nil {
			cancel()
			if budget != nil {
				budget.stopProvider(providerIndex)
			}
			return ToolOutputReduceResult{}
		}
		if err != nil {
			cancel()
			if budget != nil && optimizerProviderTimedOut(err) {
				budget.stopProvider(providerIndex)
			}
			continue
		}
		if !result.Changed {
			cancel()
			continue
		}
		result.Provider = firstNonEmpty(result.Provider, provider.optimizer.ID())
		if !validOutputReplacement(req.Output, result.Output) {
			cancel()
			continue
		}
		if optimizerContextErr(callCtx) != nil {
			cancel()
			if budget != nil {
				budget.stopProvider(providerIndex)
			}
			return ToolOutputReduceResult{}
		}
		cancel()
		return result
	}
	return ToolOutputReduceResult{}
}

func optimizerProviderCallContext(parent context.Context, timeoutMS int, budget *toolOptimizerTurnBudget) (context.Context, context.CancelFunc) {
	if budget != nil {
		if parent == nil {
			parent = context.Background()
		}
		return parent, func() {}
	}
	return optimizerCallContext(parent, timeoutMS)
}

func optimizerProviderTimedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (m *ToolOptimizerManager) acquireExternalCall(ctx context.Context, optimizer ToolOptimizer) (func(), bool) {
	if _, external := optimizer.(externalToolOptimizer); !external || m == nil || m.externalCalls == nil {
		return func() {}, true
	}
	if optimizerContextErr(ctx) != nil {
		return nil, false
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && ctx.Done() == nil {
		select {
		case m.externalCalls <- struct{}{}:
			return func() { <-m.externalCalls }, true
		default:
			return nil, false
		}
	}
	select {
	case m.externalCalls <- struct{}{}:
		if optimizerContextErr(ctx) != nil {
			<-m.externalCalls
			return nil, false
		}
		return func() { <-m.externalCalls }, true
	case <-ctx.Done():
		return nil, false
	}
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

func optimizerCallContext(parent context.Context, timeoutMS int) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeoutMS <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
}

func validCommandReplacement(original, replacement string) bool {
	replacement = strings.TrimSpace(replacement)
	if replacement == "" || replacement == strings.TrimSpace(original) {
		return false
	}
	if len(replacement) > maxOptimizedCommandBytes {
		return false
	}
	if strings.Contains(replacement, "\x00") {
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
