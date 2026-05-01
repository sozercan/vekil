package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
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

// WithMetricsEnabled enables or disables the Prometheus-compatible /metrics
// endpoint. Metrics are enabled by default.
func WithMetricsEnabled(enabled bool) Option {
	return func(o *options) {
		o.metricsEnabled = enabled
	}
}

// WithBuildVersion sets the build version exposed by vekil_build_info.
func WithBuildVersion(version string) Option {
	return func(o *options) {
		o.buildVersion = version
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

// New creates a Server with routes and timeouts configured.
func New(authenticator *auth.Authenticator, log *logger.Logger, host, port string, opts ...Option) (*Server, error) {
	cfg := options{
		metricsEnabled: true,
		buildVersion:   "dev",
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	handler, err := proxy.NewProxyHandler(authenticator, log, cfg.proxyOptions...)
	if err != nil {
		return nil, err
	}

	var metrics *serverMetrics
	if cfg.metricsEnabled {
		metrics, err = newServerMetrics(cfg.buildVersion)
		if err != nil {
			return nil, err
		}
	}

	mux := http.NewServeMux()
	register := func(pattern string, next http.HandlerFunc) {
		if metrics != nil {
			next = metrics.instrument(routeLabelFromPattern(pattern), next)
		}
		mux.HandleFunc(pattern, next)
	}

	register("POST /v1/messages", handler.HandleAnthropicMessages)
	register("POST /v1/chat/completions", handler.HandleOpenAIChatCompletions)
	register("POST /v1beta/models/", handler.HandleGeminiModels)
	register("POST /v1/models/", handler.HandleGeminiModels)
	register("POST /models/", handler.HandleGeminiModels)
	register("POST /v1/responses/compact", handler.HandleCompact)
	register("POST /v1/responses", handler.HandleResponses)
	// Defer websocket-session metrics until follow-up instrumentation work.
	mux.HandleFunc("GET /v1/responses", handler.HandleResponsesWebSocket)
	register("POST /v1/memories/trace_summarize", handler.HandleMemorySummarize)
	register("GET /healthz", handler.HandleHealthz)
	register("GET /readyz", handler.HandleReadyz)
	register("GET /v1/models", handler.HandleModels)
	if metrics != nil {
		mux.Handle("GET /metrics", metrics.handler())
	}

	addr := fmt.Sprintf("%s:%s", host, port)
	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      mux,
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

	s.httpServer.Addr = ln.Addr().String()
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

func routeLabelFromPattern(pattern string) string {
	method, route, ok := strings.Cut(pattern, " ")
	if ok && method != "" && route != "" {
		return route
	}
	return pattern
}
