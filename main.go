package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/server"
)

func main() {
	port := flag.String("port", getEnv("PORT", "1337"), "Listen port")
	host := flag.String("host", getEnv("HOST", "0.0.0.0"), "Listen host")
	tokenDir := flag.String("token-dir", getEnv("TOKEN_DIR", ""), "Token storage directory (default: ~/.config/copilot-proxy)")
	logLevel := flag.String("log-level", getEnv("LOG_LEVEL", "info"), "Log level")
	flag.Parse()

	log := logger.New(logger.ParseLevel(*logLevel))

	authenticator := auth.NewAuthenticator(*tokenDir)

	log.Info("authenticating with GitHub Copilot...")
	ctx := context.Background()
	if _, err := authenticator.GetToken(ctx); err != nil {
		log.Fatal("authentication failed", logger.Err(err))
	}
	log.Info("authenticated successfully")

	srv := server.New(authenticator, log, *host, *port)

	if err := srv.Start(); err != nil {
		log.Fatal("server start error", logger.Err(err))
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
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
