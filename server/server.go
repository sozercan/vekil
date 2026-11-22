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

type Server struct {
	httpServer              *http.Server
	proxyHandler            *proxy.ProxyHandler
	log                     *logger.Logger
	running                 atomic.Bool
	boundAddr               atomic.Pointer[string]
	dynamicAddr             bool
	stopMu                  sync.Mutex
	stopDone                chan struct{}
	stopErr                 error
	listenerDone            chan struct{}
	cancelListen            context.CancelFunc
	serveDone               chan error
	listen                  func(context.Context, string, string) (net.Listener, error)
	requestHandlersMu       sync.Mutex
	requestHandlers         sync.WaitGroup
	requestHandlersDraining bool
}

type options struct {
	proxyOptions                         []proxy.Option
	inboundAuthToken                     string
	policyRoutingAllowRemoteSingleTenant bool
}

type serverConnContextKey struct{}

type Option func(*options)

func WithInboundAuthToken(token string) Option {
	return func(o *options) {
		o.inboundAuthToken = strings.TrimSpace(token)
	}
}

func WithProxyOptions(opts ...proxy.Option) Option {
	return func(o *options) {
		o.proxyOptions = append(o.proxyOptions, opts...)
	}
}

func WithPolicyRoutingAllowRemoteSingleTenant(allow bool) Option {
	return func(o *options) {
		o.policyRoutingAllowRemoteSingleTenant = allow
	}
}

func WithCopilotHeaderConfig(cfg proxy.CopilotHeaderConfig) Option {
	return WithProxyOptions(proxy.WithCopilotHeaderConfig(cfg))
}

func WithResponsesWebSocketConfig(cfg proxy.ResponsesWebSocketConfig) Option {
	return WithProxyOptions(proxy.WithResponsesWebSocketConfig(cfg))
}

func WithStreamingUpstreamTimeout(timeout time.Duration) Option {
	return WithProxyOptions(proxy.WithStreamingUpstreamTimeout(timeout))
}

func WithCompactUpstreamChunkBytes(bytes int) Option {
	return WithProxyOptions(proxy.WithCompactUpstreamChunkBytes(bytes))
}

func WithCompactUpstreamChunkConcurrency(concurrency int) Option {
	return WithProxyOptions(proxy.WithCompactUpstreamChunkConcurrency(concurrency))
}

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
		handler.BeginShutdown()
		return nil, errors.Join(err, handler.WaitLifecycleWorkers(context.Background()))
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
	mux.HandleFunc("POST /v1/decisions", handler.HandleDecisions)
	mux.HandleFunc("POST /mcp/web-search", handler.HandleWebSearchMCP)
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
	srv := &Server{
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
	}
	srv.httpServer.Handler = srv.withRequestHandlerDrain(httpHandler)
	return srv, nil
}

func (s *Server) withRequestHandlerDrain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestHandlersMu.Lock()
		tracked := !s.requestHandlersDraining
		if tracked {
			s.requestHandlers.Add(1)
		}
		s.requestHandlersMu.Unlock()
		if tracked {
			defer s.requestHandlers.Done()
		}
		next.ServeHTTP(w, r)
	})
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

func (s *Server) Start() error {
	s.stopMu.Lock()
	if s.stopDone != nil || (s.proxyHandler != nil && s.proxyHandler.ShuttingDown()) {
		s.stopMu.Unlock()
		return errors.New("server is shut down")
	}
	if s.listenerDone != nil {
		s.stopMu.Unlock()
		return errors.New("server has already started")
	}
	listenCtx, cancelListen := context.WithCancel(context.Background())
	listenerDone := make(chan struct{})
	s.listenerDone = listenerDone
	s.cancelListen = cancelListen
	s.stopMu.Unlock()
	defer cancelListen()

	listen := s.listen
	if listen == nil {
		listen = (&net.ListenConfig{}).Listen
	}
	ln, err := listen(listenCtx, "tcp", s.httpServer.Addr)
	s.stopMu.Lock()
	stopping := s.stopDone != nil
	if err != nil || stopping {
		s.stopMu.Unlock()
		if ln != nil {
			err = errors.Join(err, ln.Close())
		}
		close(listenerDone)
		if err != nil {
			err = fmt.Errorf("listen on %s: %w", s.httpServer.Addr, err)
		} else {
			err = errors.New("server shut down during startup")
		}
		if stopping {
			return err
		}
		return errors.Join(err, s.Stop(context.Background()))
	}

	boundAddr := ln.Addr().String()
	s.boundAddr.Store(&boundAddr)
	s.running.Store(true)
	go func() {
		defer close(listenerDone)
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
	s.stopMu.Unlock()
	s.log.Info("vekil listening", logger.F("addr", s.listenerLogAddr(boundAddr)))

	return nil
}

func (s *Server) Done() <-chan error {
	if s == nil {
		return nil
	}
	return s.serveDone
}

func (s *Server) ModelUsesCopilot(model string) bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.ModelUsesCopilot(model)
}

func (s *Server) UsesCopilot() bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.UsesCopilot()
}

func (s *Server) MessagesAliasResolvesTo(ctx context.Context, alias, model string) bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.MessagesAliasResolvesTo(ctx, alias, model)
}

func (s *Server) SetStartupAuthenticationPending(pending bool) {
	if s.proxyHandler != nil {
		s.proxyHandler.SetStartupAuthenticationPending(pending)
	}
}

func (s *Server) ValidateDynamicProviderModels(ctx context.Context) error {
	if s.proxyHandler == nil {
		return nil
	}
	return s.proxyHandler.ValidateDynamicProviderModels(ctx)
}

func (s *Server) DynamicProviderValidationPending() bool {
	return s != nil && s.proxyHandler != nil && s.proxyHandler.DynamicProviderValidationPending()
}

func (s *Server) InitializePolicyRouting(ctx context.Context) error {
	if s == nil || s.proxyHandler == nil {
		return nil
	}
	return s.proxyHandler.InitializePolicyRouting(ctx)
}

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
	cancelListen := s.cancelListen
	listenerDone := s.listenerDone
	s.stopMu.Unlock()
	if cancelListen != nil {
		cancelListen()
	}

	err := s.stop(ctx, listenerDone)
	s.stopMu.Lock()
	s.stopErr = err
	close(done)
	s.stopMu.Unlock()
	return err
}

func (s *Server) stop(ctx context.Context, listenerDone <-chan struct{}) error {
	var websocketErr error
	var workerErr error
	var forceCloseErr error
	if s.proxyHandler != nil {
		s.proxyHandler.BeginShutdown()
		s.proxyHandler.SetStartupAuthenticationPending(false)
	}
	s.requestHandlersMu.Lock()
	s.requestHandlersDraining = true
	s.requestHandlersMu.Unlock()
	if s.proxyHandler != nil {
		websocketErr = s.proxyHandler.ShutdownWebSocketSessions(ctx)
	}
	shutdownErr := s.httpServer.Shutdown(ctx)
	if shutdownErr != nil {
		forceCloseErr = s.httpServer.Close()
		if errors.Is(forceCloseErr, http.ErrServerClosed) {
			forceCloseErr = nil
		}
	}
	finalized := make(chan error, 1)
	go func() {
		if listenerDone != nil {
			<-listenerDone
		}
		s.requestHandlers.Wait()
		var err error
		if s.proxyHandler != nil {
			err = s.proxyHandler.WaitLifecycleWorkers(context.Background())
		}
		if err != nil && s.log != nil {
			s.log.Error("server resource finalization failed", logger.Err(err))
		}
		finalized <- err
	}()
	select {
	case workerErr = <-finalized:
	case <-ctx.Done():
		workerErr = ctx.Err()
	}
	s.running.Store(false)
	return errors.Join(websocketErr, shutdownErr, forceCloseErr, workerErr)
}

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
