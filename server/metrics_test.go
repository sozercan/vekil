package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestNew_ExposesMetricsRouteByDefault(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", got, http.StatusOK)
	}
}

func TestNew_DisablesMetricsRouteWhenConfigured(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetricsEnabled(false),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d", got, http.StatusNotFound)
	}
}
