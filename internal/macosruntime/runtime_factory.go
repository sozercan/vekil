package macosruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

const (
	defaultRuntimeStopTimeout = 10 * time.Second

	DefaultHost = "127.0.0.1"
	DefaultPort = "1337"
)

// RuntimeFactoryOptions configures concrete server construction.
type RuntimeFactoryOptions struct {
	Authenticator *auth.Authenticator
	Logger        *logger.Logger
	Host          string
	Port          string
	Secrets       *SecretProjectionStore
	ProxyOptions  []proxy.Option
	ServerOptions []server.Option
}

// RuntimeFactory constructs app-controlled server.Server instances.
type RuntimeFactory struct {
	authenticator *auth.Authenticator
	log           *logger.Logger
	host          string
	port          string
	secrets       *SecretProjectionStore
	proxyOptions  []proxy.Option
	serverOptions []server.Option
}

// NewRuntimeFactory applies production loopback defaults while allowing port 0
// and test hosts to be injected.
func NewRuntimeFactory(opts RuntimeFactoryOptions) (*RuntimeFactory, error) {
	if opts.Authenticator == nil {
		return nil, errors.New("authenticator is required")
	}
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		host = DefaultHost
	}
	port := strings.TrimSpace(opts.Port)
	if port == "" {
		port = DefaultPort
	}
	if opts.Logger == nil {
		opts.Logger = logger.New(logger.LevelInfo)
	}
	return &RuntimeFactory{
		authenticator: opts.Authenticator,
		log:           opts.Logger,
		host:          host,
		port:          port,
		secrets:       opts.Secrets,
		proxyOptions:  append([]proxy.Option(nil), opts.ProxyOptions...),
		serverOptions: append([]server.Option(nil), opts.ServerOptions...),
	}, nil
}

// NewRuntime implements appcontrol.RuntimeFactory.
func (f *RuntimeFactory) NewRuntime(ctx context.Context, configuration appcontrol.Configuration) (appcontrol.Runtime, error) {
	cfg, ok := configuration.Value.(proxy.ProvidersConfig)
	if !ok {
		return nil, fmt.Errorf("unsupported runtime configuration type %T", configuration.Value)
	}
	proxyOptions := append([]proxy.Option(nil), f.proxyOptions...)
	proxyOptions = append(proxyOptions,
		proxy.WithProvidersConfig(cfg),
		proxy.WithPolicyRoutingMode(proxy.PolicyRoutingModeConfig),
		// App-controlled construction is always offline. Dynamic discovery is an
		// explicit cancellable startup phase after listener/auth initialization.
		proxy.WithDeferredDynamicProviderModelValidation(true),
	)
	if configuration.SecretGeneration > 0 {
		if f.secrets == nil {
			return nil, errors.New("secret projection store is required for managed secret generation")
		}
		proxyOptions = append(proxyOptions, proxy.WithProviderSecretResolver(
			f.secrets.Resolver(configuration.Revision, configuration.SecretGeneration),
		))
	}
	serverOptions := append([]server.Option(nil), f.serverOptions...)
	serverOptions = append(serverOptions, server.WithProxyOptions(proxyOptions...))
	srv, err := server.NewContext(ctx, f.authenticator, f.log, f.host, f.port, serverOptions...)
	if err != nil {
		return nil, err
	}
	return &serverRuntime{server: srv}, nil
}

// ValidateManagedCandidate constructs the exact candidate with its immutable
// secret resolver and runs explicit dynamic discovery without binding a
// listener. Interactive authentication remains disabled by the helper's
// authenticator configuration.
func (f *RuntimeFactory) ValidateManagedCandidate(ctx context.Context, candidate ManagedCandidate) error {
	runtime, err := f.NewRuntime(ctx, appcontrol.Configuration{
		Revision: candidate.Revision, SecretGeneration: candidate.SecretGeneration, Value: candidate.Config,
	})
	if err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRuntimeStopTimeout)
		_ = runtime.Stop(stopCtx)
		cancel()
	}()
	if runtime.UsesCopilot() {
		if _, err := f.authenticator.GetToken(ctx); err != nil {
			return err
		}
	}
	return runtime.ValidateDynamicProviderModels(ctx)
}

type serverRuntime struct{ server *server.Server }

func (r *serverRuntime) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.server.Start(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRuntimeStopTimeout)
		defer cancel()
		return errors.Join(err, r.server.Stop(stopCtx))
	}
	return nil
}

func (r *serverRuntime) Stop(ctx context.Context) error { return r.server.Stop(ctx) }
func (r *serverRuntime) Done() <-chan error             { return r.server.Done() }
func (r *serverRuntime) Addr() string                   { return r.server.Addr() }
func (r *serverRuntime) UsesCopilot() bool              { return r.server.UsesCopilot() }
func (r *serverRuntime) SetStartupAuthenticationPending(pending bool) {
	r.server.SetStartupAuthenticationPending(pending)
}
func (r *serverRuntime) ValidateDynamicProviderModels(ctx context.Context) error {
	return r.server.ValidateDynamicProviderModels(ctx)
}
func (r *serverRuntime) InitializePolicyRouting(ctx context.Context) error {
	return r.server.InitializePolicyRouting(ctx)
}
