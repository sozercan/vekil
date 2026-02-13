package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/proxy"
)

func main() {
	port := flag.String("port", getEnv("PORT", "1337"), "Listen port")
	host := flag.String("host", getEnv("HOST", "0.0.0.0"), "Listen host")
	tokenDir := flag.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/copilot-proxy)")
	logLevel := flag.String("log-level", getEnv("LOG_LEVEL", "info"), "Log level")
	flag.Parse()

	log := logger.New(logger.ParseLevel(*logLevel))

	authenticator := auth.NewAuthenticator(*tokenDir)

	// Authenticate on startup so the device code flow can run interactively
	log.Info("authenticating with GitHub Copilot...")
	ctx := context.Background()
	if _, err := authenticator.GetToken(ctx); err != nil {
		log.Fatal("authentication failed", logger.Err(err))
	}
	log.Info("authenticated successfully")

	handler := proxy.NewProxyHandler(authenticator, log)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", handler.HandleAnthropicMessages)
	mux.HandleFunc("POST /v1/chat/completions", handler.HandleOpenAIChatCompletions)
	mux.HandleFunc("POST /v1/responses", handler.HandleResponses)
	mux.HandleFunc("GET /healthz", handler.HandleHealthz)
	mux.HandleFunc("GET /v1/models", handler.HandleModels)

	addr := fmt.Sprintf("%s:%s", *host, *port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Info("copilot-proxy listening", logger.F("addr", addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", logger.Err(err))
		}
	}()

	<-stop
	log.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatal("shutdown error", logger.Err(err))
	}
	log.Info("server stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
