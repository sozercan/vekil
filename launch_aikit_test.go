package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sozercan/vekil/aikit"
	"github.com/sozercan/vekil/proxy"
)

func TestParseLaunchAgentOptionsAIKitFlags(t *testing.T) {
	target, _ := launchTarget("claude")
	opts, err := parseLaunchAgentOptions(target, []string{
		"--model", "aikit:qwen3.8:27b", "--context-size", "98304", "--runtime", "podman", "--keep", "--load-timeout", "20m",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseLaunchAgentOptions: %v", err)
	}
	if opts.model != "aikit:qwen3.8:27b" || opts.aikit.contextSize != 98304 || opts.aikit.runtime != "podman" || !opts.aikit.keep || opts.aikit.loadTimeout != 20*time.Minute {
		t.Fatalf("opts = %+v", opts)
	}

	defaults, err := parseLaunchAgentOptions(target, []string{"--model", "gpt-5.4-mini"}, io.Discard)
	if err != nil || defaults.aikit.runtime != aikit.EngineAuto || defaults.aikit.loadTimeout != aikit.DefaultLoadTimeout {
		t.Fatalf("defaults = %+v, %v", defaults.aikit, err)
	}

	for _, args := range [][]string{
		{"--model", "gpt-5.4-mini", "--context-size", "65536"},
		{"--keep"},
		{"--model", "gpt-5.4-mini", "--runtime", "docker"},
	} {
		if _, err := parseLaunchAgentOptions(target, args, io.Discard); err == nil || !strings.Contains(err.Error(), "requires --model aikit:<ref>") {
			t.Fatalf("parseLaunchAgentOptions(%v) error = %v", args, err)
		}
	}
	if _, err := parseLaunchAgentOptions(target, []string{"--model", "aikit:x:y", "--context-size", "-1"}, io.Discard); err == nil {
		t.Fatal("negative context size accepted")
	}
}

func testLaunchSession() *aikit.Session {
	return &aikit.Session{BaseURL: "http://127.0.0.1:41234", ModelName: "qwen-3.8-27b", ContextTokens: 65536}
}

func TestMergeLaunchAIKitProviderWithoutConfig(t *testing.T) {
	cfg, err := mergeLaunchAIKitProvider(proxy.ProvidersConfig{}, testLaunchSession())
	if err != nil {
		t.Fatalf("mergeLaunchAIKitProvider: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.UsesCopilot() {
		t.Fatalf("config = %+v", cfg)
	}
	provider := cfg.Providers[0]
	if provider.ID != "aikit" || !provider.Default || provider.Type != "openai-compatible" || provider.BaseURL != "http://127.0.0.1:41234/v1" || provider.UpstreamDialect != "localai" {
		t.Fatalf("provider = %+v", provider)
	}
	profile := launchLocalModelProfile(cfg, "qwen-3.8-27b")
	if profile == nil || profile.ContextTokens != 65536 || !profile.FunctionToolsOnly {
		t.Fatalf("profile = %+v", profile)
	}
	if launchLocalModelProfile(cfg, "gpt-5.4-mini") != nil {
		t.Fatal("remote model got a local profile")
	}
}

func TestMergeLaunchAIKitProviderIntoExistingConfig(t *testing.T) {
	cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{
		{ID: "copilot", Type: "copilot"},
		{ID: "aikit", Type: "openai-compatible", BaseURL: "http://other/v1", Models: []proxy.ProviderModelConfig{{PublicID: "other"}}},
	}}
	merged, err := mergeLaunchAIKitProvider(cfg, testLaunchSession())
	if err != nil {
		t.Fatalf("mergeLaunchAIKitProvider: %v", err)
	}
	added := merged.Providers[2]
	if added.ID != "aikit-2" || added.Default {
		t.Fatalf("added = %+v", added)
	}
	if len(cfg.Providers) != 2 {
		t.Fatal("input config was mutated")
	}

	collision := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{
		ID: "local", Type: "openai-compatible", Default: true, BaseURL: "http://other/v1", Models: []proxy.ProviderModelConfig{{PublicID: "qwen-3.8-27b"}},
	}}}
	if _, err := mergeLaunchAIKitProvider(collision, testLaunchSession()); err == nil || !strings.Contains(err.Error(), "qwen-3.8-27b") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestLaunchLocalModelProfileForUnstartedAIKitProvider(t *testing.T) {
	cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{
		ID: "local", Type: "aikit", AIKit: &proxy.AIKitProviderConfig{Model: "qwen3.8:27b", ContextSize: 98304},
		Models: []proxy.ProviderModelConfig{{PublicID: "local-qwen"}},
	}}}
	profile := launchLocalModelProfile(cfg, "local-qwen")
	if profile == nil || profile.ContextTokens != 98304 || !profile.FunctionToolsOnly {
		t.Fatalf("profile = %+v", profile)
	}
}

func TestWatchStartupSignals(t *testing.T) {
	signals := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	stop := watchStartupSignals(signals, cancel)
	signals <- syscall.SIGINT
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("startup context was not canceled")
	}
	if got := stop(); got != syscall.SIGINT {
		t.Fatalf("stop() = %v", got)
	}

	quiet := make(chan os.Signal, 1)
	_, cancelQuiet := context.WithCancel(context.Background())
	defer cancelQuiet()
	stopQuiet := watchStartupSignals(quiet, cancelQuiet)
	if got := stopQuiet(); got != nil {
		t.Fatalf("stop() without a signal = %v", got)
	}
	quiet <- syscall.SIGINT
	if len(quiet) != 1 {
		t.Fatal("a stopped watcher consumed a later signal")
	}
}

func TestPrintAIKitDryRun(t *testing.T) {
	ref, _ := aikit.ParseReference("hf.co/org/repo/model-Q4.gguf")
	var out bytes.Buffer
	printAIKitDryRun(&out, ref, launchAIKitOptions{contextSize: 65536, keep: true})
	text := out.String()
	for _, want := range []string{"runners/llama-cpp-<cpu|cuda>", "resolve/main/model-Q4.gguf", "context: 65536", "kept running"} {
		if !strings.Contains(text, want) {
			t.Fatalf("dry-run output %q missing %q", text, want)
		}
	}
}

func TestConfigValidateAcceptsAIKitProviders(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	if err := os.WriteFile(valid, []byte("providers:\n  - id: local\n    type: aikit\n    aikit:\n      model: qwen3.8:27b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateProvidersConfigFileWithAIKit(valid); err != nil {
		t.Fatalf("valid aikit config: %v", err)
	}
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("providers:\n  - id: local\n    type: aikit\n    aikit:\n      model: qwen3.8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateProvidersConfigFileWithAIKit(invalid); err == nil || !strings.Contains(err.Error(), "needs a tag") {
		t.Fatalf("invalid reference error = %v", err)
	}
}
