package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

func TestNewMenubarServerWiresDashboardConfigSourceAndMode(t *testing.T) {
	configDir := t.TempDir()
	providers, err := resolveProvidersConfigForStartup(
		"",
		proxy.ProvidersConfigUseManaged,
		func() (string, error) { return configDir, nil },
		func(string) error { return nil },
	)
	if err != nil {
		t.Fatalf("resolveProvidersConfigForStartup() error = %v", err)
	}

	srv, err := newMenubarServer(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		proxy.PolicyRoutingModeOff,
		providers,
	)
	if err != nil {
		t.Fatalf("newMenubarServer() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})

	resp, err := http.Get("http://" + srv.Addr() + "/dashboard/api/v1/config")
	if err != nil {
		t.Fatalf("GET dashboard config: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close dashboard config response body: %v", err)
		}
	}()
	var body struct {
		Capability struct {
			Available bool   `json:"available"`
			Writable  bool   `json:"writable"`
			Mode      string `json:"mode"`
		} `json:"capability"`
		Source struct {
			ID string `json:"id"`
		} `json:"source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode dashboard config: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !body.Capability.Available || !body.Capability.Writable {
		t.Fatalf("dashboard capability status=%d body=%+v", resp.StatusCode, body)
	}
	if body.Capability.Mode != server.DashboardConfigModeMenubar {
		t.Fatalf("dashboard mode = %q, want %q", body.Capability.Mode, server.DashboardConfigModeMenubar)
	}
	if body.Source.ID != string(proxy.ProvidersConfigSourceImplicitCopilot) {
		t.Fatalf("dashboard source = %q, want implicit Copilot", body.Source.ID)
	}
}
