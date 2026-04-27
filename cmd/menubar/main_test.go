package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

func TestNewMenubarServerDisablesMetricsEndpoint(t *testing.T) {
	t.Parallel()

	srv, err := newMenubarServer(nil, logger.New(logger.ParseLevel("error")), proxy.ProvidersConfig{})
	if err != nil {
		t.Fatalf("newMenubarServer() error = %v", err)
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d", metricsRec.Code, http.StatusNotFound)
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want %d", healthRec.Code, http.StatusOK)
	}
}
