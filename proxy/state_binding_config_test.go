package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func stateBindingsConfigFixture() ProvidersConfig {
	return ProvidersConfig{
		SchemaVersion: 2,
		Providers:     []ProviderConfig{{ID: "upstream", Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", AuthType: "none"}},
		ModelRoutes:   []ModelRouteConfig{{ID: "route", PublicID: "public", Endpoints: []string{"/responses"}, Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "upstream", UpstreamModel: "physical"}}}},
	}
}

func TestLoadProvidersConfigFileStateBindings(t *testing.T) {
	for _, ext := range []string{".json", ".yaml"} {
		for _, tc := range []struct {
			name, state, wantMode, wantError string
			wantLimit                        int
		}{
			{name: "default", wantMode: "durable", wantLimit: 8388608},
			{name: "durable", state: `{"mode":"durable","max_entries":8388608}`, wantMode: "durable", wantLimit: 8388608},
			{name: "memory", state: `{"mode":"memory"}`, wantMode: "memory", wantLimit: defaultStateBindingMaxEntries},
			{name: "custom limit", state: `{"max_entries":7}`, wantMode: "durable", wantLimit: 7},
			{name: "bad mode", state: `{"mode":"disk"}`, wantError: "state_bindings.mode"},
			{name: "config only an override", state: `{"mode":"config"}`, wantError: "state_bindings.mode"},
			{name: "zero limit", state: `{"max_entries":0}`, wantError: "state_bindings.max_entries"},
			{name: "negative limit", state: `{"max_entries":-1}`, wantError: "state_bindings.max_entries"},
			{name: "relative file", state: `{"file":"bindings.db"}`, wantError: "state_bindings.file"},
			{name: "unclean file", state: `{"file":"/private/state/../bindings.db"}`, wantError: "state_bindings.file"},
			{name: "unknown field", state: `{"max_entry":7}`, wantError: "max_entry"},
			{name: "null block", state: `null`, wantError: "state_bindings"},
		} {
			t.Run(ext+"/"+tc.name, func(t *testing.T) {
				body, err := json.Marshal(stateBindingsConfigFixture())
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if err := json.Unmarshal(body, &document); err != nil {
					t.Fatal(err)
				}
				if tc.state != "" {
					var state any
					if err := json.Unmarshal([]byte(tc.state), &state); err != nil {
						t.Fatal(err)
					}
					document["state_bindings"] = state
				}
				if ext == ".yaml" {
					body, err = yaml.Marshal(document)
				} else {
					body, err = json.Marshal(document)
				}
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "providers"+ext)
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadProvidersConfigFile(path)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("load error = %v, want %s", err, tc.wantError)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := resolveStateBindingsConfig(cfg, StateBindingsConfig{})
				if err != nil || resolved.Mode != tc.wantMode || resolved.MaxEntries != tc.wantLimit {
					t.Fatalf("resolved = %+v, error = %v", resolved, err)
				}
			})
		}
	}
	legacy := ProvidersConfig{StateBindings: &StateBindingsConfig{Mode: "durable"}}
	if err := ValidateProvidersConfig(legacy); err == nil || !strings.Contains(err.Error(), "requires schema_version: 2") {
		t.Fatalf("legacy state block = %v", err)
	}
}

func TestStateBindingsConfigurationPrecedence(t *testing.T) {
	configFile, overrideFile := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "override.db")
	for _, tc := range []struct {
		name       string
		base       *StateBindingsConfig
		override   StateBindingsConfig
		mode, file string
		limit      int
	}{
		{name: "schema default", mode: "durable", limit: 8388608},
		{name: "provider memory", base: &StateBindingsConfig{Mode: "memory"}, mode: "memory", limit: defaultStateBindingMaxEntries},
		{name: "provider file", base: &StateBindingsConfig{File: configFile, MaxEntries: 17}, mode: "durable", file: configFile, limit: 17},
		{name: "config override follows file", base: &StateBindingsConfig{File: configFile, MaxEntries: 17}, override: StateBindingsConfig{Mode: "config"}, mode: "durable", file: configFile, limit: 17},
		{name: "memory override retains configured limit", base: &StateBindingsConfig{File: configFile, MaxEntries: 17}, override: StateBindingsConfig{Mode: "memory"}, mode: "memory", file: configFile, limit: 17},
		{name: "file override enables durability", base: &StateBindingsConfig{Mode: "memory"}, override: StateBindingsConfig{File: overrideFile, MaxEntries: 19}, mode: "durable", file: overrideFile, limit: 19},
		{name: "explicit memory overrides file", base: &StateBindingsConfig{File: configFile}, override: StateBindingsConfig{Mode: "memory", File: overrideFile}, mode: "memory", file: overrideFile, limit: defaultStateBindingMaxEntries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := stateBindingsConfigFixture()
			cfg.StateBindings = tc.base
			got, err := resolveStateBindingsConfig(cfg, tc.override)
			if err != nil || got.Mode != tc.mode || got.File != tc.file || got.MaxEntries != tc.limit {
				t.Fatalf("resolved = %+v, error = %v", got, err)
			}
		})
	}
	for _, cfg := range []ProvidersConfig{{}, {SchemaVersion: 1}, {SchemaVersion: 2}} {
		got, err := resolveStateBindingsConfig(cfg, StateBindingsConfig{})
		if err != nil || got.Mode != "memory" || got.MaxEntries != defaultStateBindingMaxEntries {
			t.Fatalf("legacy/no-routes = %+v, %v", got, err)
		}
	}
}

func TestStateBindingsDefaultPaths(t *testing.T) {
	home := t.TempDir()
	getHome := func() (string, error) { return home, nil }
	for _, tc := range []struct{ goos, xdg, want string }{
		{"darwin", filepath.Join(home, "ignored"), filepath.Join(home, "Library", "Application Support", "vekil", "state", "bindings.db")},
		{"linux", "", filepath.Join(home, ".local", "share", "vekil", "state", "bindings.db")},
		{"linux", filepath.Join(home, "data"), filepath.Join(home, "data", "vekil", "state", "bindings.db")},
	} {
		got, err := stateBindingsPathForOS(tc.goos, tc.xdg, getHome)
		if err != nil || got != tc.want {
			t.Fatalf("path(%s, %s) = %q, %v", tc.goos, tc.xdg, got, err)
		}
	}
	if _, err := stateBindingsPathForOS("linux", "relative", getHome); err == nil {
		t.Fatal("relative XDG_DATA_HOME accepted")
	}
	if _, err := stateBindingsPathForOS("windows", "", getHome); !errors.Is(err, errDurableStatePlatform) {
		t.Fatalf("unsupported platform = %v", err)
	}
	if _, err := stateBindingsPathForOS("darwin", "", func() (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Fatal("missing home accepted")
	}
}

func TestDurableStateBindingsConfigStartup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires supported local storage")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	path, err := defaultStateBindingsPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg := stateBindingsConfigFixture()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(home, "providers.json")
	if err := os.WriteFile(configFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProvidersConfigFile(configFile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline validation created state directory: %v", err)
	}
	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.BeginShutdown(); _ = h.WaitLifecycleWorkers(context.Background()) })
	if h.stateBindings.durable == nil || h.stateBindings.maxEntries != 8388608 {
		t.Fatal("schema-v2 default is not durable with configured capacity")
	}
	for _, name := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("permissions for %s = %o", name, info.Mode().Perm())
		}
	}
	if other, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg)); !errors.Is(err, errDurableStateLocked) {
		if other != nil {
			other.BeginShutdown()
			_ = other.WaitLifecycleWorkers(context.Background())
		}
		t.Fatalf("second default writer = %v", err)
	}
	memory, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithStateBindingsConfig(StateBindingsConfig{Mode: "memory"}))
	if err != nil || memory.stateBindings.durable != nil {
		t.Fatalf("memory override = %v", err)
	}
	memory.BeginShutdown()
	if err := memory.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	distinct := filepath.Join(t.TempDir(), "other.db")
	if err := os.Chmod(filepath.Dir(distinct), 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithStateBindingsConfig(StateBindingsConfig{File: distinct, MaxEntries: 3}))
	if err != nil {
		t.Fatal(err)
	}
	other.BeginShutdown()
	if err := other.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.BeginShutdown()
	if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg))
	if err != nil {
		t.Fatalf("shutdown retained default file lock: %v", err)
	}
	reopened.BeginShutdown()
	if err := reopened.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDurableStateDefaultDirectoryRejectsUnsafePaths(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires supported local storage")
	}
	for _, kind := range []string{"public directory", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "state")
			switch kind {
			case "public directory":
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := createPrivateStateDirectory(filepath.Join(dir, "bindings.db")); !errors.Is(err, errDurableStatePath) {
				t.Fatalf("unsafe directory = %v", err)
			}
		})
	}
}
