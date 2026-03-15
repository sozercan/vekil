package server

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/proxy"
)

// Server encapsulates the HTTP server lifecycle.
type Server struct {
	httpServer *http.Server
	log        *logger.Logger
	running    atomic.Bool
}

// New creates a Server with routes and timeouts configured.
func New(authenticator *auth.Authenticator, log *logger.Logger, host, port string) *Server {
	handler := proxy.NewProxyHandler(authenticator, log)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", handler.HandleAnthropicMessages)
	mux.HandleFunc("POST /v1/chat/completions", handler.HandleOpenAIChatCompletions)
	mux.HandleFunc("POST /v1beta/models/", handler.HandleGeminiModels)
	mux.HandleFunc("POST /v1/models/", handler.HandleGeminiModels)
	mux.HandleFunc("POST /models/", handler.HandleGeminiModels)
	mux.HandleFunc("POST /v1/responses/compact", handler.HandleCompact)
	mux.HandleFunc("POST /v1/responses", handler.HandleResponses)
	mux.HandleFunc("POST /v1/memories/trace_summarize", handler.HandleMemorySummarize)
	mux.HandleFunc("GET /healthz", handler.HandleHealthz)
	mux.HandleFunc("GET /v1/models", handler.HandleModels)

	addr := fmt.Sprintf("%s:%s", host, port)
	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 5 * time.Minute,
			IdleTimeout:  120 * time.Second,
		},
		log: log,
	}
}

// Start begins listening in a goroutine. It returns an error if the listener
// cannot be established.
func (s *Server) Start() error {
	s.running.Store(true)
	s.log.Info("copilot-proxy listening", logger.F("addr", s.httpServer.Addr))

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Fatal("server error", logger.Err(err))
		}
		s.running.Store(false)
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
