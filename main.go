package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/proxy"
)

func main() {
	port := flag.String("port", getEnv("PORT", "8080"), "Listen port")
	host := flag.String("host", getEnv("HOST", "0.0.0.0"), "Listen host")
	tokenDir := flag.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/copilot-proxy)")
	logLevel := flag.String("log-level", getEnv("LOG_LEVEL", "info"), "Log level")
	flag.Parse()

	if *logLevel == "debug" {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	authenticator := auth.NewAuthenticator(*tokenDir)

	// Authenticate on startup so the device code flow can run interactively
	log.Println("authenticating with GitHub Copilot...")
	ctx := context.Background()
	if _, err := authenticator.GetToken(ctx); err != nil {
		log.Fatalf("authentication failed: %v", err)
	}
	log.Println("authenticated successfully")

	handler := proxy.NewProxyHandler(authenticator)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", handler.HandleAnthropicMessages)
	mux.HandleFunc("POST /v1/chat/completions", handler.HandleOpenAIChatCompletions)
	mux.HandleFunc("POST /v1/responses", handler.HandleResponses)
	mux.HandleFunc("GET /healthz", handler.HandleHealthz)

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
		log.Printf("copilot-proxy listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("server stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
