package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sozercan/vekil/proxy"
)

func TestLoadMenubarConfigMissingFile(t *testing.T) {
	configDir := stubUserConfigDir(t)

	cfg, err := loadMenubarConfig()
	if err != nil {
		t.Fatalf("loadMenubarConfig() error = %v", err)
	}
	if cfg.ProvidersConfigPath != "" {
		t.Fatalf("loadMenubarConfig() ProvidersConfigPath = %q, want empty", cfg.ProvidersConfigPath)
	}

	if _, err := os.Stat(filepath.Join(configDir, "vekil", menubarConfigFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected config file to be absent, stat error = %v", err)
	}
}

func TestSaveMenubarConfigRoundTrip(t *testing.T) {
	configDir := stubUserConfigDir(t)
	wantPath := "/tmp/providers.json"

	if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: "  " + wantPath + "  "}); err != nil {
		t.Fatalf("saveMenubarConfig() error = %v", err)
	}

	cfg, err := loadMenubarConfig()
	if err != nil {
		t.Fatalf("loadMenubarConfig() error = %v", err)
	}
	if cfg.ProvidersConfigPath != wantPath {
		t.Fatalf("loadMenubarConfig() ProvidersConfigPath = %q, want %q", cfg.ProvidersConfigPath, wantPath)
	}

	body, err := os.ReadFile(filepath.Join(configDir, "vekil", menubarConfigFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatalf("saved config should end with newline, got %q", string(body))
	}
}

func TestSaveMenubarConfigClearsFile(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)

	if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: "/tmp/providers.json"}); err != nil {
		t.Fatalf("saveMenubarConfig(set) error = %v", err)
	}
	if err := saveMenubarConfig(menubarConfig{}); err != nil {
		t.Fatalf("saveMenubarConfig(clear) error = %v", err)
	}

	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected config file to be removed, stat error = %v", err)
	}
}

func TestProvidersRequireGitHubAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  proxy.ProvidersConfig
		err  error
		want bool
	}{
		{
			name: "default config uses copilot",
			want: true,
		},
		{
			name: "provider only config skips auth",
			cfg: proxy.ProvidersConfig{
				Providers: []proxy.ProviderConfig{
					{ID: "azure", Type: "azure-openai"},
				},
			},
			want: false,
		},
		{
			name: "invalid config does not trigger auth refresh",
			cfg: proxy.ProvidersConfig{
				Providers: []proxy.ProviderConfig{
					{ID: "azure", Type: "azure-openai"},
				},
			},
			err:  errors.New("boom"),
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := providersRequireGitHubAuth(tt.cfg, tt.err); got != tt.want {
				t.Fatalf("providersRequireGitHubAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func stubUserConfigDir(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	prev := userConfigDir
	userConfigDir = func() (string, error) {
		return tmpDir, nil
	}
	t.Cleanup(func() {
		userConfigDir = prev
	})

	return tmpDir
}
