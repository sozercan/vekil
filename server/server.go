package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
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
	boundAddr    atomic.Pointer[string]
	dynamicAddr  bool
	stopMu       sync.Mutex
	stopDone     chan struct{}
	stopErr      error
	serveDone    chan error
}

type options struct {
	proxyOptions                         []proxy.Option
	inboundAuthToken                     string
	policyRoutingAllowRemoteSingleTenant bool
}

type serverConnContextKey struct{}

// Option customizes server creation.
type Option func(*options)

// WithInboundAuthToken requires this bearer or x-api-key token on every route
// except health and readiness probes. Empty keeps the public server behavior.
func WithInboundAuthToken(token string) Option {
	return func(o *options) {
		o.inboundAuthToken = strings.TrimSpace(token)
	}
}

// WithProxyOptions forwards proxy-level options to the underlying handler.
func WithProxyOptions(opts ...proxy.Option) Option {
	return func(o *options) {
		o.proxyOptions = append(o.proxyOptions, opts...)
	}
}

// WithPolicyRoutingAllowRemoteSingleTenant acknowledges that a policy-routing
// server bound beyond loopback is operated as a trusted single-tenant service.
func WithPolicyRoutingAllowRemoteSingleTenant(allow bool) Option {
	return func(o *options) {
		o.policyRoutingAllowRemoteSingleTenant = allow
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
	status                int
	bytes                 int64
	trustedBrowserContent bool
}

var responseRecorderPool = sync.Pool{
	New: func() any { return new(responseRecorder) },
}

func acquireResponseRecorder(w http.ResponseWriter, trustedBrowserContent bool) *responseRecorder {
	recorder := responseRecorderPool.Get().(*responseRecorder)
	recorder.ResponseWriter = w
	recorder.trustedBrowserContent = trustedBrowserContent
	return recorder
}

func releaseResponseRecorder(recorder *responseRecorder) {
	if recorder == nil {
		return
	}
	recorder.ResponseWriter = nil
	recorder.status = 0
	recorder.bytes = 0
	recorder.trustedBrowserContent = false
	responseRecorderPool.Put(recorder)
}

func (r *responseRecorder) prepareHeaders() {
	header := r.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	if r.trustedBrowserContent {
		return
	}
	header.Set("Content-Type", safeAPIResponseContentType(header.Get("Content-Type")))
}

func safeAPIResponseContentType(contentType string) string {
	contentType = strings.TrimSpace(contentType)
	mediaType := contentType
	if separator := strings.IndexByte(mediaType, ';'); separator >= 0 {
		mediaType = strings.TrimSpace(mediaType[:separator])
	}
	if strings.EqualFold(mediaType, "text/event-stream") {
		return "text/event-stream"
	}
	if strings.EqualFold(mediaType, "text/plain") {
		return contentType
	}
	return "application/json"
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.prepareHeaders()
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	writer := r.ResponseWriter
	if r.status == 0 {
		header := writer.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		if !r.trustedBrowserContent {
			header.Set("Content-Type", safeAPIResponseContentType(header.Get("Content-Type")))
		}
		r.status = http.StatusOK
	}
	n, err := writer.Write(p)
	r.bytes += int64(n)
	return n, err
}

func statusIsSuccess(status int) bool {
	return status == http.StatusSwitchingProtocols || (status >= 200 && status < 300)
}

func (r *responseRecorder) Flush() {
	if r.status == 0 {
		r.prepareHeaders()
		r.status = http.StatusOK
	}
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

type startupReadinessGate interface {
	StartupAuthenticationPending() bool
	DynamicProviderValidationPending() bool
	PolicyRoutingPreflightPending() bool
	PolicyRoutingReadinessDiagnostic() string
}

func withInboundAuth(next http.Handler, token string) http.Handler {
	token = strings.TrimSpace(token)
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestPassesInboundAuth(r, token) {
			// Auth sits inside withRequestLog so the rejection is logged, but no inference
			// handler ran: counting it would let an unauthenticated caller move request-rate,
			// latency and error dashboards.
			proxy.RequestSummaryFromContext(r.Context()).SuppressStats()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestPassesInboundAuth(r *http.Request, token string) bool {
	if token == "" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("x-api-key"))
	if provided == "" {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(authorization) > len("Bearer ") && strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
			provided = strings.TrimSpace(authorization[len("Bearer "):])
		}
	}
	return len(provided) == len(token) && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func withProviderValidationGate(next http.Handler, handler startupReadinessGate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler != nil && (handler.StartupAuthenticationPending() || handler.DynamicProviderValidationPending() || handler.PolicyRoutingPreflightPending()) && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			message := "provider model validation pending"
			switch {
			case handler.StartupAuthenticationPending():
				message = "startup authentication pending"
			case handler.DynamicProviderValidationPending():
				// Preserve provider-validation precedence when both startup
				// validations are pending.
			case handler.PolicyRoutingPreflightPending():
				message = strings.TrimSpace(handler.PolicyRoutingReadinessDiagnostic())
				if message == "" {
					message = "policy routing preflight pending"
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"service_unavailable"}}`, message)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withRequestLog(next http.Handler, log *logger.Logger, handler *proxy.ProxyHandler, inboundAuthToken string) http.Handler {
	inboundAuthToken = strings.TrimSpace(inboundAuthToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := acquireResponseRecorder(w, servesTrustedBrowserContent(r))
		defer releaseResponseRecorder(recorder)
		ctx, summary, ownsSummary := proxy.AcquireRequestSummary(r.Context())
		if ownsSummary {
			defer proxy.ReleaseRequestSummary(summary)
		}

		admitted := handler == nil || !handler.ShuttingDown() || r.URL.Path == "/healthz"
		authenticated := requestPassesInboundAuth(r, inboundAuthToken)
		tracked := admitted && authenticated && handler != nil && handler.TracksRequest(r.Method, r.URL.Path)
		if tracked {
			handler.IncInflight()
			defer handler.DecInflight()
		}

		// Mark the request context so upstream retries made on behalf of a tracked
		// inference request are counted in retry stats. The mark must be set here
		// (not derived inside the upstream call) because newInferenceUpstreamContext
		// rebuilds the upstream context from the detached proxy lifecycle root;
		// only an explicitly propagated positive marker survives.
		if handler != nil && admitted && authenticated {
			ctx = handler.MarkRetryStatsTrackedIfInference(ctx, r.Method, r.URL.Path)
		}

		r = r.WithContext(ctx)
		var requestBodyBinding proxy.RequestBodyLifecycleBinding
		if admitted && handler != nil {
			var forceClose io.Closer
			if conn, ok := r.Context().Value(serverConnContextKey{}).(net.Conn); ok && conn != nil {
				forceClose = conn
			}
			requestBodyBinding = handler.BindRequestBodyToLifecycle(r, forceClose)
			defer requestBodyBinding.ReleaseAndRecycle(r)
		}
		if admitted {
			next.ServeHTTP(recorder, r)
		} else {
			handler.WriteShutdownServiceUnavailable(recorder, r)
		}
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		// A streamed response can fail after its 200 header was committed (e.g. a
		// /responses turn that emits response.failed/incomplete). The handler
		// records that out-of-band on the summary; prefer it for stats so the
		// dashboard does not count a post-commit failure as a success.
		statsStatus := status
		if failure := summary.FailureStatus(); failure != 0 {
			statsStatus = failure
		}
		elapsed := time.Since(start)
		if tracked && !summary.StatsSuppressed() {
			handler.RecordRequest(summary, statsStatus, r.Header.Get("User-Agent"), elapsed)
		}
		if log != nil && log.Enabled(logger.LevelWarn) {
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
			if statsStatus != status {
				fields = append(fields, logger.F("stats_status", statsStatus))
			}
			switch {
			case statusIsSuccess(status) && statsStatus == status:
				log.Info("request completed", fields...)
			case statsStatus != status:
				log.Warn("request failed after response headers were committed", fields...)
			default:
				log.Warn("request completed", fields...)
			}
		}
	})
}

func servesTrustedBrowserContent(r *http.Request) bool {
	if r == nil || r.URL == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	switch r.URL.Path {
	case "/dashboard", "/dashboard/uPlot.min.js", "/dashboard/uPlot.min.css":
		return true
	default:
		return false
	}
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
	if err := validatePolicyRoutingListenHost(host, handler.PolicyRoutingActive(), cfg.policyRoutingAllowRemoteSingleTenant); err != nil {
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
	httpHandler := withRequestLog(withInboundAuth(withProviderValidationGate(mux, handler), cfg.inboundAuthToken), log, handler, cfg.inboundAuthToken)
	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      httpHandler,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: handler.ServerWriteTimeout(),
			IdleTimeout:  120 * time.Second,
			ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
				return context.WithValue(ctx, serverConnContextKey{}, conn)
			},
		},
		proxyHandler: handler,
		log:          log,
		dynamicAddr:  strings.TrimSpace(port) == "0",
		serveDone:    make(chan error, 1),
	}, nil
}

func validatePolicyRoutingListenHost(host string, policyActive, allowRemoteSingleTenant bool) error {
	if !policyActive || allowRemoteSingleTenant || isLoopbackListenHost(host) {
		return nil
	}
	return fmt.Errorf("policy routing on non-loopback host %q requires --policy-routing-allow-remote-single-tenant", host)
}

func isLoopbackListenHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Start begins listening in a goroutine. It returns an error if the listener
// cannot be established.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.httpServer.Addr, err)
	}

	boundAddr := ln.Addr().String()
	s.boundAddr.Store(&boundAddr)
	s.running.Store(true)
	s.log.Info("vekil listening", logger.F("addr", s.listenerLogAddr(boundAddr)))

	go func() {
		defer s.running.Store(false)
		err := s.httpServer.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		if err != nil {
			s.log.Error("server error", logger.Err(err))
		}
		s.serveDone <- err
		close(s.serveDone)
	}()

	return nil
}

// Done reports the terminal Serve result. It yields nil after a normal
// shutdown and a non-nil error when the listener stops unexpectedly.
func (s *Server) Done() <-chan error {
	if s == nil {
		return nil
	}
	return s.serveDone
}

// ModelUsesCopilot reports whether launcher startup needs Copilot authentication
// to resolve or collision-check model.
func (s *Server) ModelUsesCopilot(model string) bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.ModelUsesCopilot(model)
}

// UsesCopilot reports whether the active serve scope needs Copilot auth.
func (s *Server) UsesCopilot() bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.UsesCopilot()
}

// SetStartupAuthenticationPending gates non-health routes while startup auth is in progress.
func (s *Server) SetStartupAuthenticationPending(pending bool) {
	if s.proxyHandler != nil {
		s.proxyHandler.SetStartupAuthenticationPending(pending)
	}
}

// ValidateDynamicProviderModels loads deferred dynamic provider catalogs.
func (s *Server) ValidateDynamicProviderModels(ctx context.Context) error {
	if s.proxyHandler == nil {
		return nil
	}
	return s.proxyHandler.ValidateDynamicProviderModels(ctx)
}

// DynamicProviderValidationPending reports whether deferred discovery remains.
func (s *Server) DynamicProviderValidationPending() bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.DynamicProviderValidationPending()
}

// InitializePolicyRouting performs any configured policy-routing preflight.
func (s *Server) InitializePolicyRouting(ctx context.Context) error {
	if s == nil || s.proxyHandler == nil {
		return nil
	}
	return s.proxyHandler.InitializePolicyRouting(ctx)
}

// Stop performs a graceful shutdown of the server.
func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.stopMu.Lock()
	if done := s.stopDone; done != nil {
		s.stopMu.Unlock()
		select {
		case <-done:
			s.stopMu.Lock()
			err := s.stopErr
			s.stopMu.Unlock()
			return err
		default:
		}
		select {
		case <-done:
			s.stopMu.Lock()
			err := s.stopErr
			s.stopMu.Unlock()
			return err
		case <-ctx.Done():
			select {
			case <-done:
				s.stopMu.Lock()
				err := s.stopErr
				s.stopMu.Unlock()
				return err
			default:
			}
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	s.stopDone = done
	s.stopMu.Unlock()

	err := s.stop(ctx)
	s.stopMu.Lock()
	s.stopErr = err
	close(done)
	s.stopMu.Unlock()
	return err
}

func (s *Server) stop(ctx context.Context) error {
	var websocketErr error
	var workerErr error
	var forceCloseErr error
	if s.proxyHandler != nil {
		s.proxyHandler.BeginShutdown()
		s.proxyHandler.SetStartupAuthenticationPending(false)
		websocketErr = s.proxyHandler.ShutdownWebSocketSessions(ctx)
	}
	shutdownErr := s.httpServer.Shutdown(ctx)
	if shutdownErr != nil {
		// Shutdown leaves active connections alone after its deadline. Force-close
		// them so a stalled upload or non-reading downstream cannot survive into a
		// later server generation (notably in the menubar stop/start flow).
		forceCloseErr = s.httpServer.Close()
		if errors.Is(forceCloseErr, http.ErrServerClosed) {
			forceCloseErr = nil
		}
	}
	if s.proxyHandler != nil {
		workerErr = s.proxyHandler.WaitLifecycleWorkers(ctx)
	}
	s.running.Store(false)
	return errors.Join(websocketErr, shutdownErr, forceCloseErr, workerErr)
}

// IsRunning returns whether the server is currently running.
func (s *Server) IsRunning() bool {
	return s.running.Load()
}

func (s *Server) listenerLogAddr(boundAddr string) string {
	if s != nil && s.dynamicAddr {
		return boundAddr
	}
	if s == nil || s.httpServer == nil {
		return boundAddr
	}
	return s.httpServer.Addr
}

// Addr returns the configured listen address, except that a server configured
// with port 0 reports the actual bound address after Start succeeds.
func (s *Server) Addr() string {
	if s != nil && s.dynamicAddr {
		if boundAddr := s.boundAddr.Load(); boundAddr != nil {
			return *boundAddr
		}
	}
	if s == nil || s.httpServer == nil {
		return ""
	}
	return s.httpServer.Addr
}
