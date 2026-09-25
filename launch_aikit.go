package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sozercan/vekil/aikit"
	"github.com/sozercan/vekil/launch"
	"github.com/sozercan/vekil/proxy"
)

// launchAIKitProviderID names the provider a `--model aikit:<ref>` launch adds.
const launchAIKitProviderID = "aikit"

// launchAIKitOptions are the launcher flags that configure an aikit: model.
type launchAIKitOptions struct {
	contextSize int
	runtime     string
	backend     string
	keep        bool
	loadTimeout time.Duration
}

// aikitLaunchFlagNames are flags that only apply to `--model aikit:<ref>`.
var aikitLaunchFlagNames = []string{"context-size", "runtime", "backend", "keep", "load-timeout"}

// watchStartupSignals cancels startup work on the first managed signal. The
// returned stop function ends the watch and reports that signal, if any, so
// the same channel can then be handed to the agent supervisor.
func watchStartupSignals(signals <-chan os.Signal, cancel context.CancelFunc) func() os.Signal {
	done := make(chan struct{})
	finished := make(chan os.Signal, 1)
	go func() {
		select {
		case signalValue := <-signals:
			cancel()
			finished <- signalValue
		case <-done:
			finished <- nil
		}
	}()
	return func() os.Signal {
		close(done)
		return <-finished
	}
}

// validateLaunchAIKitOptions rejects option values a launch would refuse, so a
// dry run fails the same way.
func validateLaunchAIKitOptions(ref aikit.Reference, opts launchAIKitOptions) error {
	switch opts.runtime {
	case "", aikit.EngineAuto, aikit.EngineDocker, aikit.EnginePodman:
	default:
		return fmt.Errorf("unsupported --runtime %q: use auto, docker, or podman", opts.runtime)
	}
	_, err := aikit.ResolveBackend(ref, opts.backend)
	return err
}

// startLaunchAIKitModel starts the container for `--model aikit:<ref>`.
func startLaunchAIKitModel(ctx context.Context, ref aikit.Reference, opts launchAIKitOptions, stderr io.Writer) (*aikit.Session, error) {
	engine, err := aikit.DetectEngine(ctx, nil, aikit.DetectOptions{Preference: opts.runtime})
	if err != nil {
		return nil, err
	}
	if engine.Notice != "" {
		_, _ = fmt.Fprintf(stderr, "vekil: aikit: %s\n", engine.Notice)
	}
	return aikit.Start(ctx, aikit.Options{
		Reference:            ref,
		Backend:              opts.backend,
		ContextTokens:        opts.contextSize,
		MinimumContextTokens: aikit.AgentMinimumContextTokens,
		Keep:                 opts.keep,
		LoadTimeout:          opts.loadTimeout,
		Engine:               engine,
		Progress:             stderr,
		Environment:          os.Environ(),
	})
}

// mergeLaunchAIKitProvider adds the started session to cfg as a static
// openai-compatible provider. A launch without a providers config serves only
// the local model, so Copilot authentication is not needed.
func mergeLaunchAIKitProvider(cfg proxy.ProvidersConfig, session *aikit.Session) (proxy.ProvidersConfig, error) {
	out := cfg
	out.Providers = append([]proxy.ProviderConfig(nil), cfg.Providers...)
	id := launchAIKitProviderID
	for suffix := 2; providerIDTaken(out.Providers, id); suffix++ {
		id = fmt.Sprintf("%s-%d", launchAIKitProviderID, suffix)
	}
	provider := proxy.ProviderConfig{ID: id, Type: proxy.ProviderTypeAIKit}
	provider.Default = !providersHaveDefault(out.Providers)
	aikit.MaterializeProvider(&provider, session, false)
	out.Providers = append(out.Providers, provider)
	if err := proxy.ValidateProvidersConfig(out); err != nil {
		return proxy.ProvidersConfig{}, fmt.Errorf("add %s to the providers config: %w", session.ModelName, err)
	}
	return out, nil
}

func providerIDTaken(providers []proxy.ProviderConfig, id string) bool {
	for _, provider := range providers {
		if strings.TrimSpace(provider.ID) == id {
			return true
		}
	}
	return false
}

// providersHaveDefault reports whether cfg already routes unknown models:
// an explicit default or a Copilot provider.
func providersHaveDefault(providers []proxy.ProviderConfig) bool {
	for _, provider := range providers {
		if provider.Default || strings.TrimSpace(provider.Type) == "copilot" {
			return true
		}
	}
	return false
}

// launchLocalModelProfile describes the pinned model when an aikit or LocalAI
// provider serves it, directly or as a target of its model route.
func launchLocalModelProfile(cfg proxy.ProvidersConfig, modelID string) *launch.LocalModel {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	localProviders := map[string]bool{}
	for _, provider := range cfg.Providers {
		if provider.IsAIKit() || strings.TrimSpace(provider.UpstreamDialect) == "localai" {
			localProviders[strings.TrimSpace(provider.ID)] = true
		}
	}
	for _, route := range cfg.ModelRoutes {
		if strings.TrimSpace(route.PublicID) != modelID {
			continue
		}
		// Any local target means requests must fit the function-tools-only
		// contract; the route's own context window, when set, is authoritative.
		for _, target := range route.Targets {
			if localProviders[strings.TrimSpace(target.Provider)] {
				profile := &launch.LocalModel{FunctionToolsOnly: true}
				if route.ContextWindow != nil {
					profile.ContextTokens = *route.ContextWindow
				}
				return profile
			}
		}
		return nil
	}
	for _, provider := range cfg.Providers {
		if !localProviders[strings.TrimSpace(provider.ID)] {
			continue
		}
		for _, model := range provider.Models {
			if strings.TrimSpace(model.PublicID) != modelID {
				continue
			}
			profile := &launch.LocalModel{FunctionToolsOnly: true}
			if model.ContextWindow != nil {
				profile.ContextTokens = *model.ContextWindow
			} else if provider.AIKit != nil && provider.AIKit.ContextSize > 0 {
				profile.ContextTokens = int64(provider.AIKit.ContextSize)
			}
			return profile
		}
	}
	return nil
}

// printAIKitDryRun describes the container a real launch would start.
func printAIKitDryRun(w io.Writer, ref aikit.Reference, opts launchAIKitOptions) {
	_, _ = fmt.Fprintf(w, "aikit model: %s\n", ref.Redacted())
	switch {
	case ref.Premade:
		_, _ = fmt.Fprintf(w, "  image: %s (an applesilicon/ variant on a podman libkrun machine)\n", ref.Image)
	case ref.Kind == aikit.RefImage:
		_, _ = fmt.Fprintf(w, "  image: %s\n", ref.Image)
	default:
		backend, _ := aikit.ResolveBackend(ref, opts.backend)
		_, _ = fmt.Fprintf(w, "  runner: %s/runners/%s-<cpu|cuda>:latest\n", aikit.PremadeRegistry, backend)
	}
	runtime := opts.runtime
	if runtime == "" {
		runtime = aikit.EngineAuto
	}
	_, _ = fmt.Fprintf(w, "  runtime: %s\n", runtime)
	if opts.contextSize > 0 {
		_, _ = fmt.Fprintf(w, "  context: %d\n", opts.contextSize)
	} else {
		_, _ = fmt.Fprintf(w, "  context: %d capped at the model's trained context (resolved at launch)\n", aikit.DefaultContextTokens)
	}
	_, _ = fmt.Fprintf(w, "  served model ID: resolved from the image at launch\n")
	if opts.keep {
		_, _ = fmt.Fprintln(w, "  container: kept running after exit")
	}
}

// closeLaunchAIKit stops started containers and prints kept-container hints.
func closeLaunchAIKit(stderr io.Writer, session *aikit.Session, group *aikit.Group) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if session != nil {
		if session.Keep {
			_, _ = fmt.Fprintf(stderr, "vekil: aikit: %s\n", session.KeepHint())
		} else if err := session.Close(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "vekil: warning: aikit: remove %s: %v\n", session.ContainerName, err)
		}
	}
	if group != nil {
		for _, hint := range group.KeepHints() {
			_, _ = fmt.Fprintf(stderr, "vekil: aikit: %s\n", hint)
		}
		if err := group.Close(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "vekil: warning: aikit: %v\n", err)
		}
	}
}
