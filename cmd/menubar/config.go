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

var userConfigDir = os.UserConfigDir

type menubarConfig struct {
	ProvidersConfigPath string `json:"providers_config_path,omitempty"`
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

	var cfg menubarConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return menubarConfig{}, fmt.Errorf("decode menubar config %q: %w", path, err)
	}

	cfg.ProvidersConfigPath = strings.TrimSpace(cfg.ProvidersConfigPath)
	return cfg, nil
}

func saveMenubarConfig(cfg menubarConfig) error {
	path, err := menubarConfigPath()
	if err != nil {
		return err
	}

	cfg.ProvidersConfigPath = strings.TrimSpace(cfg.ProvidersConfigPath)
	if cfg.ProvidersConfigPath == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove menubar config %q: %w", path, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create menubar config dir: %w", err)
	}

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode menubar config: %w", err)
	}

	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write menubar config %q: %w", path, err)
	}

	return nil
}

func loadProvidersConfigForMenubar() (menubarConfig, proxy.ProvidersConfig, error) {
	cfg, err := loadMenubarConfig()
	if err != nil {
		return menubarConfig{}, proxy.ProvidersConfig{}, err
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(cfg.ProvidersConfigPath)
	if err != nil {
		return cfg, proxy.ProvidersConfig{}, err
	}

	return cfg, providersCfg, nil
}
