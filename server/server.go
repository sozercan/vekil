package server

import (
	"bufio"
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
	httpServer   *http.Server
	proxyHandler *proxy.ProxyHandler
	log          *logger.Logger
	running      atomic.Bool
}

type options struct {
	proxyOptions []proxy.Option
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

// WithCompactUpstreamChunkBytes overrides the initial body size targeted when
// chunking /v1/responses/compact retries after an upstream 413.
func WithCompactUpstreamChunkBytes(bytes int) Option {
	return WithProxyOptions(proxy.WithCompactUpstreamChunkBytes(bytes))
}

// WithCompactUpstreamChunkConcurrency overrides the maximum number of sibling
// compact chunks sent concurrently after the first chunk succeeds.
func WithCompactUpstreamChunkConcurrency(concurrency int) Option {
	return WithProxyOptions(proxy.WithCompactUpstreamChunkConcurrency(concurrency))
}

// WithCompactUpstreamMaxAttempts caps the total logical compaction calls the
// /v1/responses/compact 413 fallback may perform per inbound request. Logical
// calls can produce extra real upstream POSTs through model fallback or the
// shared transport-retry policy.
func WithCompactUpstreamMaxAttempts(max int) Option {
	return WithProxyOptions(proxy.WithCompactUpstreamMaxAttempts(max))
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if r.status == 0 {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, rw, nil
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func withRequestLog(next http.Handler, log *logger.Logger, handler *proxy.ProxyHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		ctx, summary := proxy.WithRequestSummary(r.Context())

		tracked := handler != nil && handler.TracksRequest(r.URL.Path)
		if tracked {
			handler.IncInflight()
			defer handler.DecInflight()
		}

		next.ServeHTTP(recorder, r.WithContext(ctx))
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		elapsed := time.Since(start)
		if tracked {
			handler.RecordRequest(summary, status, r.Header.Get("User-Agent"), elapsed)
		}
		if log != nil {
			fields := []logger.Field{
				logger.F("method", r.Method),
				logger.F("path", r.URL.Path),
				logger.F("status", status),
				logger.F("bytes", recorder.bytes),
				logger.F("duration_ms", elapsed.Milliseconds()),
			}
			if requestID := proxy.UpstreamRequestID(recorder.Header()); requestID != "" {
				summaryFields := summary.LoggerFields()
				alreadyCaptured := false
				for _, field := range summaryFields {
					if field.Key == "upstream_request_id" {
						alreadyCaptured = true
						break
					}
				}
				if !alreadyCaptured {
					fields = append(fields, logger.F("upstream_request_id", requestID))
				}
			}
			fields = append(fields, summary.LoggerFields()...)
			log.Info("request completed", fields...)
		}
	})
}

// New creates a Server with routes and timeouts configured.
func New(authenticator *auth.Authenticator, log *logger.Logger, host, port string, opts ...Option) (*Server, error) {
	cfg := options{}
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
	mux.HandleFunc("POST /v1/messages/count_tokens", handler.HandleAnthropicMessagesCountTokens)
	mux.HandleFunc("POST /v1/messages", handler.HandleAnthropicMessages)
	mux.HandleFunc("POST /v1/chat/completions", handler.HandleOpenAIChatCompletions)
	mux.HandleFunc("POST /v1beta/models/", handler.HandleGeminiModels)
	mux.HandleFunc("POST /v1/models/", handler.HandleGeminiModels)
	mux.HandleFunc("POST /models/", handler.HandleGeminiModels)
	mux.HandleFunc("POST /v1/responses/compact", handler.HandleCompact)
	mux.HandleFunc("POST /v1/responses", handler.HandleResponses)
	mux.HandleFunc("GET /v1/responses", handler.HandleResponsesWebSocket)
	mux.HandleFunc("POST /v1/memories/trace_summarize", handler.HandleMemorySummarize)
	mux.HandleFunc("GET /healthz", handler.HandleHealthz)
	mux.HandleFunc("GET /readyz", handler.HandleReadyz)
	mux.HandleFunc("GET /v1/models", handler.HandleModels)
	mux.HandleFunc("GET /dashboard", handler.HandleDashboard)
	mux.HandleFunc("GET /dashboard/{asset}", handler.HandleDashboardAsset)
	mux.HandleFunc("POST /dashboard/insight", handler.HandleDashboardInsight)
	mux.HandleFunc("GET /stats.json", handler.HandleStatsJSON)
	mux.HandleFunc("GET /favicon.ico", handler.HandleFavicon)

	addr := fmt.Sprintf("%s:%s", host, port)
	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      withRequestLog(mux, log, handler),
			ReadTimeout:  30 * time.Second,
			WriteTimeout: handler.ServerWriteTimeout(),
			IdleTimeout:  120 * time.Second,
		},
		proxyHandler: handler,
		log:          log,
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
	if s.proxyHandler != nil {
		s.proxyHandler.ShutdownWebSocketSessions(ctx)
	}
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
