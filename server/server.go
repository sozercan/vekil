package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

// WithMetricsEnabled enables or disables the Prometheus /metrics endpoint.
func WithMetricsEnabled(enabled bool) Option {
	return func(o *options) {
		o.metricsEnabled = enabled
	}
}

// WithBuildVersion sets the version exported by vekil_build_info.
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

	mux := http.NewServeMux()
	metrics := newMetrics(cfg.buildVersion)
	register := func(pattern string, next http.HandlerFunc) {
		mux.Handle(pattern, metrics.Wrap(pattern, http.HandlerFunc(next)))
	}

	register("POST /v1/messages", handler.HandleAnthropicMessages)
	register("POST /v1/chat/completions", handler.HandleOpenAIChatCompletions)
	register("POST /v1beta/models/", handler.HandleGeminiModels)
	register("POST /v1/models/", handler.HandleGeminiModels)
	register("POST /models/", handler.HandleGeminiModels)
	register("POST /v1/responses/compact", handler.HandleCompact)
	register("POST /v1/responses", handler.HandleResponses)
	register("GET /v1/responses", handler.HandleResponsesWebSocket)
	register("POST /v1/memories/trace_summarize", handler.HandleMemorySummarize)
	register("GET /healthz", handler.HandleHealthz)
	register("GET /readyz", handler.HandleReadyz)
	register("GET /v1/models", handler.HandleModels)
	if cfg.metricsEnabled {
		register("GET /metrics", promhttp.HandlerFor(metrics.registry, promhttp.HandlerOpts{}).ServeHTTP)
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

type serverMetrics struct {
	registry        *prometheus.Registry
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

func newMetrics(buildVersion string) serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)

	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total HTTP requests handled by Vekil.",
		},
		[]string{"handler", "method", "code_class"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vekil_http_request_duration_seconds",
			Help:    "HTTP request duration for Vekil handlers.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"handler", "method"},
	)

	registry.MustRegister(buildInfo, requestsTotal, requestDuration)

	return serverMetrics{
		registry:        registry,
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}
}

func (m serverMetrics) Wrap(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		handlerLabel := normalizeMetricsHandlerLabel(pattern)
		m.requestsTotal.WithLabelValues(handlerLabel, r.Method, statusCodeClass(recorder.status)).Inc()
		m.requestDuration.WithLabelValues(handlerLabel, r.Method).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.status = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	pusher, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if readerFrom, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		if r.status == 0 {
			r.status = http.StatusOK
		}
		return readerFrom.ReadFrom(src)
	}
	return io.Copy(r.ResponseWriter, src)
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func normalizeMetricsHandlerLabel(pattern string) string {
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) != 2 {
		return pattern
	}
	return parts[1]
}

func statusCodeClass(code int) string {
	if code < 100 || code > 999 {
		return "unknown"
	}
	return fmt.Sprintf("%dxx", code/100)
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
