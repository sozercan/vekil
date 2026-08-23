package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/vekil/proxy"
)

const menubarConfigFilename = "menubar.json"

var (
	userConfigDir          = os.UserConfigDir
	errMenubarConfigLoad   = errors.New("menubar config load failed")
	errProvidersConfigLoad = errors.New("providers config load failed")
)

type menubarConfig struct {
	ProvidersConfigPath string `json:"providers_config_path,omitempty"`
	versionedState      map[string]json.RawMessage
}

func menubarConfigPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vekil", menubarConfigFilename), nil
}

func loadMenubarConfig() (menubarConfig, error) {
	path, err := menubarConfigPath()
	if err != nil {
		return menubarConfig{}, err
	}

	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return menubarConfig{}, nil
		}
		return menubarConfig{}, fmt.Errorf("read menubar config %q: %w", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return menubarConfig{}, fmt.Errorf("decode menubar config %q: %w", path, err)
	}
	var probe struct {
		Version             int    `json:"version"`
		ConfigMode          string `json:"config_mode"`
		SelectedPath        string `json:"selected_path"`
		ProvidersConfigPath string `json:"providers_config_path"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return menubarConfig{}, fmt.Errorf("decode menubar config %q: %w", path, err)
	}
	cfg := menubarConfig{ProvidersConfigPath: strings.TrimSpace(probe.ProvidersConfigPath)}
	if probe.Version > 0 {
		cfg.versionedState = raw
		if strings.TrimSpace(probe.ConfigMode) == "external" {
			cfg.ProvidersConfigPath = strings.TrimSpace(probe.SelectedPath)
			if cfg.ProvidersConfigPath == "" {
				cfg.ProvidersConfigPath = strings.TrimSpace(probe.ProvidersConfigPath)
			}
		} else {
			cfg.ProvidersConfigPath = ""
		}
	}
	return cfg, nil
}

func saveMenubarConfig(cfg menubarConfig) error {
	path, err := menubarConfigPath()
	if err != nil {
		return err
	}

	cfg.ProvidersConfigPath = strings.TrimSpace(cfg.ProvidersConfigPath)
	if cfg.versionedState == nil {
		existing, err := loadMenubarConfig()
		if err != nil {
			return fmt.Errorf("preserve existing menubar config: %w", err)
		}
		cfg.versionedState = existing.versionedState
	}
	if cfg.versionedState == nil && cfg.ProvidersConfigPath == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove menubar config %q: %w", path, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create menubar config dir: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("protect menubar config dir: %w", err)
	}

	var body []byte
	if cfg.versionedState != nil {
		raw := make(map[string]json.RawMessage, len(cfg.versionedState)+3)
		for key, value := range cfg.versionedState {
			raw[key] = append(json.RawMessage(nil), value...)
		}
		set := func(key string, value any) error {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			raw[key] = encoded
			return nil
		}
		if cfg.ProvidersConfigPath == "" {
			_ = set("config_mode", "legacy")
			delete(raw, "selected_path")
			delete(raw, "providers_config_path")
			_ = set("selected_config_revision", "cfg_legacy_copilot")
		} else {
			_ = set("config_mode", "external")
			_ = set("selected_path", cfg.ProvidersConfigPath)
			_ = set("providers_config_path", cfg.ProvidersConfigPath)
			delete(raw, "selected_config_revision")
		}
		body, err = json.MarshalIndent(raw, "", "  ")
	} else {
		body, err = json.MarshalIndent(cfg, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("encode menubar config: %w", err)
	}
	body = append(body, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("write menubar config %q: %w", path, err)
	}
	// Existing installations may have created this file with broader permissions.
	// Secure the open inode before replacing contents that can include a signed URL.
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure menubar config %q: %w", path, err)
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return fmt.Errorf("truncate menubar config %q: %w", path, err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write menubar config %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close menubar config %q: %w", path, err)
	}

	return nil
}

func loadProvidersConfigForMenubar() (menubarConfig, proxy.ProvidersConfig, error) {
	cfg, err := loadMenubarConfig()
	if err != nil {
		return menubarConfig{}, proxy.ProvidersConfig{}, fmt.Errorf("%w: %w", errMenubarConfigLoad, err)
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(cfg.ProvidersConfigPath)
	if err != nil {
		return cfg, proxy.ProvidersConfig{}, fmt.Errorf("%w: %w", errProvidersConfigLoad, err)
	}

	return cfg, providersCfg, nil
}

func isMenubarConfigLoadError(err error) bool {
	return errors.Is(err, errMenubarConfigLoad)
}
