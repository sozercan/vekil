package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

// Server encapsulates the HTTP server lifecycle.
type Server struct {
	httpServer *http.Server
	log        *logger.Logger
	running    atomic.Bool
}

type options struct {
	proxyOptions   []proxy.Option
	metricsEnabled bool
	buildVersion   string
}

// Option customizes server creation.
type Option func(*options)

// WithProxyOptions forwards proxy-level options to the underlying handler.
func WithProxyOptions(opts ...proxy.Option) Option {
	return func(o *options) {
		o.proxyOptions = append(o.proxyOptions, opts...)
	}
}

// WithCopilotHeaderConfig overrides the synthetic Copilot-identifying headers
// sent on upstream requests.
func WithCopilotHeaderConfig(cfg proxy.CopilotHeaderConfig) Option {
	return WithProxyOptions(proxy.WithCopilotHeaderConfig(cfg))
}

// WithResponsesWebSocketConfig overrides websocket-session handling for
// GET /v1/responses Codex clients.
func WithResponsesWebSocketConfig(cfg proxy.ResponsesWebSocketConfig) Option {
	return WithProxyOptions(proxy.WithResponsesWebSocketConfig(cfg))
}

// WithStreamingUpstreamTimeout overrides the timeout used for streaming
// upstream inference requests and derives the server write timeout from it.
func WithStreamingUpstreamTimeout(timeout time.Duration) Option {
	return WithProxyOptions(proxy.WithStreamingUpstreamTimeout(timeout))
}

// WithMetricsEnabled controls whether the Prometheus /metrics endpoint is mounted.
func WithMetricsEnabled(enabled bool) Option {
	return func(o *options) {
		o.metricsEnabled = enabled
	}
}

// WithBuildVersion sets the version exported via vekil_build_info.
func WithBuildVersion(version string) Option {
	return func(o *options) {
		o.buildVersion = version
	}
}

// New creates a Server with routes and timeouts configured.
func New(authenticator *auth.Authenticator, log *logger.Logger, host, port string, opts ...Option) (*Server, error) {
	cfg := options{metricsEnabled: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	handler, err := proxy.NewProxyHandler(authenticator, log, cfg.proxyOptions...)
	if err != nil {
		return nil, err
	}

	metrics := newMetrics(cfg.buildVersion)
	m := http.NewServeMux()
	handle := func(pattern, route, method string, next http.HandlerFunc) {
		m.Handle(pattern, metrics.instrument(route, method, next))
	}

	handle("POST /v1/messages", "/v1/messages", http.MethodPost, handler.HandleAnthropicMessages)
	handle("POST /v1/chat/completions", "/v1/chat/completions", http.MethodPost, handler.HandleOpenAIChatCompletions)
	handle("POST /v1beta/models/", "/v1beta/models/*", http.MethodPost, handler.HandleGeminiModels)
	handle("POST /v1/models/", "/v1/models/*", http.MethodPost, handler.HandleGeminiModels)
	handle("POST /models/", "/models/*", http.MethodPost, handler.HandleGeminiModels)
	handle("POST /v1/responses/compact", "/v1/responses/compact", http.MethodPost, handler.HandleCompact)
	handle("POST /v1/responses", "/v1/responses", http.MethodPost, handler.HandleResponses)
	handle("GET /v1/responses", "/v1/responses", http.MethodGet, handler.HandleResponsesWebSocket)
	handle("POST /v1/memories/trace_summarize", "/v1/memories/trace_summarize", http.MethodPost, handler.HandleMemorySummarize)
	handle("GET /healthz", "/healthz", http.MethodGet, handler.HandleHealthz)
	handle("GET /readyz", "/readyz", http.MethodGet, handler.HandleReadyz)
	handle("GET /v1/models", "/v1/models", http.MethodGet, handler.HandleModels)
	if cfg.metricsEnabled {
		m.Handle("GET /metrics", metrics.handler())
	}

	addr := fmt.Sprintf("%s:%s", host, port)
	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      m,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: handler.ServerWriteTimeout(),
			IdleTimeout:  120 * time.Second,
		},
		log: log,
	}, nil
}

// Start begins listening in a goroutine. It returns an error if the listener
// cannot be established.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.httpServer.Addr, err)
	}

	s.running.Store(true)
	s.log.Info("vekil listening", logger.F("addr", s.httpServer.Addr))

	go func() {
		defer s.running.Store(false)
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("server error", logger.Err(err))
		}
	}()

	return nil
}

// Stop performs a graceful shutdown of the server.
func (s *Server) Stop(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	s.running.Store(false)
	return err
}

// IsRunning returns whether the server is currently running.
func (s *Server) IsRunning() bool {
	return s.running.Load()
}

// Addr returns the listen address.
func (s *Server) Addr() string {
	return s.httpServer.Addr
}
