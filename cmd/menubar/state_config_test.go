package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

func TestDurableStateMenubarSharesProviderConfigAndStore(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires supported local storage")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	for _, name := range []string{"STATE_BINDINGS_MODE", "STATE_BINDINGS_FILE", "STATE_BINDINGS_MAX_ENTRIES", "POLICY_ROUTING_MODE"} {
		t.Setenv(name, "")
	}
	stubUserConfigDir(t)
	previousLog := log
	log = logger.NewWithWriter(logger.LevelError, io.Discard)
	t.Cleanup(func() { log = previousLog })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_shared","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	config := proxy.ProvidersConfig{
		SchemaVersion: 2,
		StateBindings: &proxy.StateBindingsConfig{MaxEntries: 16},
		Providers:     []proxy.ProviderConfig{{ID: "upstream", Type: "openai-compatible", BaseURL: upstream.URL, AuthType: "none"}},
		ModelRoutes:   []proxy.ModelRouteConfig{{ID: "route", PublicID: "public", Endpoints: []string{"/responses"}, Targets: []proxy.ModelRouteTargetConfig{{ID: "target", Provider: "upstream", UpstreamModel: "physical"}}}},
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "providers.json")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: file}); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := loadProvidersConfigForMenubar()
	if err != nil {
		t.Fatal(err)
	}
	result := runProxyStartupAt(t.Context(), auth.NewTestAuthenticator("synthetic-token"), loaded, nil, "127.0.0.1", "0")
	if result.err != nil {
		t.Fatal(result.err)
	}
	tray := result.server.(*server.Server)
	stop := func(srv *server.Server) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { stop(tray) })
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(srv *server.Server, payload string) {
		resp, err := client.Post("http://"+srv.Addr()+"/v1/responses", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("response = %d: %s", resp.StatusCode, body)
		}
	}
	stats := func(srv *server.Server, mode string, entries int) {
		resp, err := client.Get("http://" + srv.Addr() + "/stats.json")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var payload struct {
			State struct {
				Mode     string `json:"mode"`
				Entries  int    `json:"entries"`
				Capacity int    `json:"max_entries"`
				Bytes    int64  `json:"database_bytes"`
			} `json:"state_bindings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.State.Mode != mode || payload.State.Entries != entries || payload.State.Capacity != 16 {
			t.Fatalf("state stats = %+v", payload.State)
		}
		if mode == "durable" && payload.State.Bytes == 0 {
			t.Fatal("durable database size missing")
		}
	}
	post(tray, `{"model":"public","input":"first"}`)
	stats(tray, "durable", 1)
	// A second proxy can opt out explicitly without touching the locked store.
	t.Setenv("STATE_BINDINGS_MODE", "memory")
	memoryResult := runProxyStartupAt(t.Context(), auth.NewTestAuthenticator("synthetic-token"), loaded, nil, "127.0.0.1", "0")
	if memoryResult.err != nil {
		t.Fatal(memoryResult.err)
	}
	memory := memoryResult.server.(*server.Server)
	t.Cleanup(func() { stop(memory) })
	stats(memory, "memory", 0)
	stop(memory)
	t.Setenv("STATE_BINDINGS_MODE", "")
	stop(tray)
	// The CLI uses this same constructor and provider config. It resumes proof
	// issued by the tray without a state flag, environment variable or menu setting.
	cli, err := server.New(auth.NewTestAuthenticator("synthetic-token"), log, "127.0.0.1", "0", server.WithProxyOptions(proxy.WithProvidersConfig(loaded)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(cli) })
	if err := cli.Start(); err != nil {
		t.Fatal(err)
	}
	stats(cli, "durable", 1)
	post(cli, `{"model":"public","previous_response_id":"resp_shared","input":"next"}`)
}
