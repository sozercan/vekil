package aikit

import (
	"bytes"
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sozercan/vekil/proxy"
)

func seedProviderFake(t *testing.T) *fakeEngine {
	t.Helper()
	fake := newFakeEngine(t)
	fake.outputs["podman machine list"] = `[{"Name":"m","Running":true,"VMType":"libkrun","Memory":"68719476736"}]`
	fake.outputs["docker info"] = "27.0.0\n"
	files := premadeImageFiles(premadeConfig, testGGUF("qwen35", 262144))
	fake.images["ghcr.io/kaito-project/aikit/qwen3.8:27b"] = files
	fake.images["ghcr.io/kaito-project/aikit/applesilicon/qwen3.8:27b"] = files
	fake.onRun = func(c *fakeContainer) {
		size, _ := strconv.Atoi(c.env["LOCALAI_CONTEXT_SIZE"])
		c.port = fake.serveLocalAI(nil, func() int { return size })
	}
	return fake
}

func TestStartProvidersMaterializesAIKitProviders(t *testing.T) {
	fake := seedProviderFake(t)
	cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{
		{ID: "local", Type: "aikit", Default: true, AIKit: &proxy.AIKitProviderConfig{Model: "qwen3.8:27b", ContextSize: 32768}},
		{ID: "named", Type: "aikit", AIKit: &proxy.AIKitProviderConfig{Model: "qwen3.8:27b"}, Models: []proxy.ProviderModelConfig{{PublicID: "local-qwen", Name: "Local Qwen"}}},
	}}
	var progress bytes.Buffer
	out, group, err := StartProviders(context.Background(), cfg, ProviderStartOptions{Progress: &progress, Environment: []string{}, Executor: fake})
	if err != nil {
		t.Fatalf("StartProviders: %v\n%s", err, progress.String())
	}
	defer func() { _ = group.Close(context.Background()) }()
	if len(group.Sessions()) != 2 || cfg.Providers[0].Type != "aikit" {
		t.Fatalf("sessions = %d, input mutated = %v", len(group.Sessions()), cfg.Providers[0].Type != "aikit")
	}
	first := out.Providers[0]
	if first.Type != "openai-compatible" || first.AuthType != "none" || first.UpstreamDialect != "localai" || first.AIKit != nil ||
		!strings.HasPrefix(first.BaseURL, "http://127.0.0.1:") || !strings.HasSuffix(first.BaseURL, "/v1") {
		t.Fatalf("first provider = %+v", first)
	}
	if len(first.Models) != 1 || first.Models[0].PublicID != "qwen-3.8-27b" || first.Models[0].Deployment != "qwen-3.8-27b" ||
		*first.Models[0].ContextWindow != 32768 || !reflect.DeepEqual(first.Models[0].Endpoints, proxy.AIKitModelEndpoints()) {
		t.Fatalf("first models = %+v", first.Models)
	}
	named := out.Providers[1].Models[0]
	if named.PublicID != "local-qwen" || named.Name != "Local Qwen" || named.Deployment != "qwen-3.8-27b" || *named.ContextWindow != 65536 {
		t.Fatalf("named model = %+v", named)
	}
	if err := group.Close(context.Background()); err != nil || len(fake.containers) != 0 {
		t.Fatalf("Close: %v, containers left %d", err, len(fake.containers))
	}
}

func TestStartProvidersLeavesRouteOnlyProvidersToRoutes(t *testing.T) {
	fake := seedProviderFake(t)
	cfg := proxy.ProvidersConfig{
		SchemaVersion: 2,
		StateBindings: &proxy.StateBindingsConfig{Mode: "memory"},
		Providers: []proxy.ProviderConfig{
			{ID: "local", Type: "aikit", Default: true, AIKit: &proxy.AIKitProviderConfig{Model: "qwen3.8:27b"}},
		},
		ModelRoutes: []proxy.ModelRouteConfig{{
			ID: "coder", PublicID: "coder", Endpoints: []string{"/chat/completions"},
			Targets: []proxy.ModelRouteTargetConfig{{ID: "local", Provider: "local", UpstreamModel: "qwen-3.8-27b"}},
		}},
	}
	out, group, err := StartProviders(context.Background(), cfg, ProviderStartOptions{Environment: []string{}, Executor: fake})
	if err != nil {
		t.Fatalf("StartProviders: %v", err)
	}
	defer func() { _ = group.Close(context.Background()) }()
	if len(out.Providers[0].Models) != 0 {
		t.Fatalf("route-only provider exposed models: %+v", out.Providers[0].Models)
	}
	if window := out.ModelRoutes[0].ContextWindow; window == nil || *window != 65536 {
		t.Fatalf("route context_window = %v, want the served 65536", window)
	}
	if cfg.ModelRoutes[0].ContextWindow != nil {
		t.Fatal("input config route was mutated")
	}
}

func TestStartProvidersCleansUpOnFailure(t *testing.T) {
	fake := seedProviderFake(t)
	cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{
		{ID: "ok", Type: "aikit", Default: true, AIKit: &proxy.AIKitProviderConfig{Model: "qwen3.8:27b"}},
		{ID: "bad", Type: "aikit", AIKit: &proxy.AIKitProviderConfig{Model: "missing:tag"}},
	}}
	_, _, err := StartProviders(context.Background(), cfg, ProviderStartOptions{Environment: []string{}, Executor: fake})
	if err == nil || !strings.Contains(err.Error(), "providers[1] (bad)") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.containers) != 0 {
		t.Fatalf("containers left after failure: %d", len(fake.containers))
	}
}

func TestStartProvidersPassesThroughConfigsWithoutAIKit(t *testing.T) {
	cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{ID: "copilot", Type: "copilot"}}}
	out, group, err := StartProviders(context.Background(), cfg, ProviderStartOptions{})
	if err != nil || group != nil || !reflect.DeepEqual(out, cfg) {
		t.Fatalf("StartProviders = %+v, %v, %v", out, group, err)
	}
	if err := group.Close(context.Background()); err != nil {
		t.Fatalf("nil group Close: %v", err)
	}
}

func TestValidateProviderReferences(t *testing.T) {
	ok := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{ID: "a", Type: "aikit", AIKit: &proxy.AIKitProviderConfig{Model: "hf.co/org/repo/file.gguf", Backend: "llama-cpp"}}}}
	if err := ValidateProviderReferences(ok); err != nil {
		t.Fatalf("valid reference: %v", err)
	}
	for _, block := range []proxy.AIKitProviderConfig{
		{Model: "qwen3.8"},
		{Model: "qwen3.8:27b", Backend: "llama-cpp"},
		{Model: "hf.co/org/repo@" + strings.Repeat("a", 40), Backend: "llama-cpp"},
	} {
		block := block
		cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{ID: "a", Type: "aikit", AIKit: &block}}}
		if err := ValidateProviderReferences(cfg); err == nil || !strings.Contains(err.Error(), "providers[0].aikit") {
			t.Fatalf("ValidateProviderReferences(%+v) = %v", block, err)
		}
	}
}
