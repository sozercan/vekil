package aikit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/sozercan/vekil/proxy"
)

// ProviderStartOptions configures StartProviders.
type ProviderStartOptions struct {
	Progress    io.Writer
	Environment []string
	// MinimumContextTokens applies to every provider; zero disables the floor.
	MinimumContextTokens int
	// Executor runs engine commands; nil uses the host.
	Executor Executor
}

// Group owns the sessions started for a providers config.
type Group struct {
	mu       sync.Mutex
	sessions []*Session
}

// Sessions returns the started sessions in provider order.
func (g *Group) Sessions() []*Session {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*Session(nil), g.sessions...)
}

func (g *Group) add(session *Session) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions = append(g.sessions, session)
}

// Close removes every container that is not kept.
func (g *Group) Close(ctx context.Context) error {
	if g == nil {
		return nil
	}
	var errs []error
	for _, session := range g.Sessions() {
		if err := session.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", session.ContainerName, err))
		}
	}
	return errors.Join(errs...)
}

// KeepHints describes kept containers.
func (g *Group) KeepHints() []string {
	var hints []string
	for _, session := range g.Sessions() {
		if session.Keep {
			hints = append(hints, session.KeepHint())
		}
	}
	return hints
}

// StartProviders starts a container for every type: aikit provider and returns
// a copy of cfg in which each such provider is the static openai-compatible
// provider that routes to it. Configs without aikit providers are returned
// unchanged with a nil group. On error, containers started so far are removed.
func StartProviders(ctx context.Context, cfg proxy.ProvidersConfig, opts ProviderStartOptions) (proxy.ProvidersConfig, *Group, error) {
	if !cfg.HasAIKitProviders() {
		return cfg, nil, nil
	}
	out := cfg
	out.Providers = append([]proxy.ProviderConfig(nil), cfg.Providers...)
	group := &Group{}
	engines := map[string]*Engine{}
	routeReferenced := routeReferencedProviders(cfg)
	for index := range out.Providers {
		provider := &out.Providers[index]
		if !provider.IsAIKit() {
			continue
		}
		session, err := startProviderSession(ctx, *provider, engines, opts)
		if err != nil {
			closeGroup(group)
			return proxy.ProvidersConfig{}, nil, fmt.Errorf("providers[%d] (%s): %w", index, provider.ID, err)
		}
		group.add(session)
		MaterializeProvider(provider, session, routeReferenced[strings.TrimSpace(provider.ID)])
	}
	if err := proxy.ValidateProvidersConfig(out); err != nil {
		closeGroup(group)
		return proxy.ProvidersConfig{}, nil, fmt.Errorf("validate started aikit providers: %w", err)
	}
	return out, group, nil
}

func closeGroup(group *Group) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = group.Close(ctx)
}

func startProviderSession(ctx context.Context, provider proxy.ProviderConfig, engines map[string]*Engine, opts ProviderStartOptions) (*Session, error) {
	block := provider.AIKit
	if block == nil {
		return nil, fmt.Errorf("aikit block is required")
	}
	ref, err := ParseReference(block.Model)
	if err != nil {
		return nil, err
	}
	loadTimeout, err := block.LoadTimeoutDuration()
	if err != nil {
		return nil, fmt.Errorf("load_timeout: %w", err)
	}
	preference := strings.TrimSpace(block.Runtime)
	if preference == "" {
		preference = EngineAuto
	}
	engine := engines[preference]
	if engine == nil {
		engine, err = DetectEngine(ctx, opts.Executor, DetectOptions{Preference: preference})
		if err != nil {
			return nil, err
		}
		if engine.Notice != "" && opts.Progress != nil {
			_, _ = fmt.Fprintf(opts.Progress, "vekil: aikit: %s\n", engine.Notice)
		}
		engines[preference] = engine
	}
	return Start(ctx, Options{
		Reference:            ref,
		Backend:              block.Backend,
		ContextTokens:        block.ContextSize,
		MinimumContextTokens: opts.MinimumContextTokens,
		Keep:                 block.Keep,
		LoadTimeout:          loadTimeout,
		Engine:               engine,
		Progress:             opts.Progress,
		Environment:          opts.Environment,
	})
}

// MaterializeProvider rewrites an aikit provider into the static
// openai-compatible provider for session. Declared models keep their public
// IDs and gain the served model, endpoints, and context window where unset.
// A provider without models exposes the served model unless routes reference
// it, in which case routes own its public contract.
func MaterializeProvider(provider *proxy.ProviderConfig, session *Session, routeReferenced bool) {
	contextWindow := int64(session.ContextTokens)
	provider.Type = "openai-compatible"
	provider.BaseURL = session.OpenAIBaseURL()
	provider.AuthType = "none"
	provider.UpstreamDialect = "localai"
	provider.AIKit = nil
	if len(provider.Models) == 0 {
		if routeReferenced {
			return
		}
		provider.Models = []proxy.ProviderModelConfig{{PublicID: session.ModelName}}
	}
	models := append([]proxy.ProviderModelConfig(nil), provider.Models...)
	for index := range models {
		model := &models[index]
		if strings.TrimSpace(model.Deployment) == "" {
			model.Deployment = session.ModelName
		}
		if len(model.Endpoints) == 0 {
			model.Endpoints = proxy.AIKitModelEndpoints()
		}
		if model.ContextWindow == nil {
			value := contextWindow
			model.ContextWindow = &value
		}
	}
	provider.Models = models
}

func routeReferencedProviders(cfg proxy.ProvidersConfig) map[string]bool {
	referenced := map[string]bool{}
	for _, route := range cfg.ModelRoutes {
		for _, target := range route.Targets {
			referenced[strings.TrimSpace(target.Provider)] = true
		}
	}
	return referenced
}

// ValidateProviderReferences checks every aikit provider's model reference
// offline, without contacting a registry or starting a container.
func ValidateProviderReferences(cfg proxy.ProvidersConfig) error {
	for index, provider := range cfg.Providers {
		if !provider.IsAIKit() || provider.AIKit == nil {
			continue
		}
		ref, err := ParseReference(provider.AIKit.Model)
		if err != nil {
			return fmt.Errorf("providers[%d].aikit.model: %w", index, err)
		}
		if backend := strings.TrimSpace(provider.AIKit.Backend); backend != "" {
			if !ref.IsRunner() {
				return fmt.Errorf("providers[%d].aikit.backend: applies only to runner references (hf.co/... or https .gguf URLs)", index)
			}
			if ref.Kind == RefRunnerRepo && backend != BackendVLLMCPP {
				return fmt.Errorf("providers[%d].aikit.backend: repository references are served by vllm-cpp", index)
			}
		}
	}
	return nil
}
