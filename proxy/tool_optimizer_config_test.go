package proxy

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestToolOptimizerOutputConfigJSONPreservesExplicitZeroDefaults(t *testing.T) {
	var cfg ToolOptimizersConfig
	if err := json.Unmarshal([]byte(`{
		"enabled": true,
		"output_reduce": {
			"enabled": true,
			"timeout_ms": 0,
			"min_input_bytes": 0,
			"max_input_bytes": 0
		}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}

	got := cfg.withDefaults()
	assertExplicitZeroOutputReduce(t, got)

	got = got.withDefaults()
	assertExplicitZeroOutputReduce(t, got)
}

func TestToolOptimizerOutputConfigYAMLPreservesExplicitZeroDefaults(t *testing.T) {
	var cfg ToolOptimizersConfig
	if err := yaml.Unmarshal([]byte(`
enabled: true
output_reduce:
  enabled: true
  timeout_ms: 0
  min_input_bytes: 0
  max_input_bytes: 0
`), &cfg); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}

	got := cfg.withDefaults()
	assertExplicitZeroOutputReduce(t, got)

	got = got.withDefaults()
	assertExplicitZeroOutputReduce(t, got)
}

func TestToolOptimizerOutputConfigDefaultsWhenUnset(t *testing.T) {
	got := (ToolOptimizersConfig{}).withDefaults()
	if got.OutputReduce.TimeoutMS != defaultToolOptimizerOutputReduceTimeoutMS {
		t.Fatalf("timeout_ms = %d, want %d", got.OutputReduce.TimeoutMS, defaultToolOptimizerOutputReduceTimeoutMS)
	}
	if got.OutputReduce.MinInputBytes != defaultToolOptimizerOutputReduceMinBytes {
		t.Fatalf("min_input_bytes = %d, want %d", got.OutputReduce.MinInputBytes, defaultToolOptimizerOutputReduceMinBytes)
	}
	if got.OutputReduce.MaxInputBytes != defaultToolOptimizerOutputReduceMaxBytes {
		t.Fatalf("max_input_bytes = %d, want %d", got.OutputReduce.MaxInputBytes, defaultToolOptimizerOutputReduceMaxBytes)
	}
}

func TestToolOptimizerRTKCLIProviderDefaultsFromMinimalConfig(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		unmarshal func([]byte, interface{}) error
	}{
		{
			name: "yaml",
			body: `
tool_optimizers:
  enabled: true
  command_rewrite:
    enabled: true
  output_reduce:
    enabled: true
  providers:
    - type: rtk_cli
`,
			unmarshal: yaml.Unmarshal,
		},
		{
			name: "json",
			body: `{
  "tool_optimizers": {
    "enabled": true,
    "command_rewrite": {"enabled": true},
    "output_reduce": {"enabled": true},
    "providers": [{"type": "rtk_cli"}]
  }
}`,
			unmarshal: json.Unmarshal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg ProvidersConfig
			if err := tc.unmarshal([]byte(tc.body), &cfg); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.name, err)
			}

			got := cfg.ToolOptimizers.withDefaults()
			if !got.Enabled {
				t.Fatalf("tool_optimizers.enabled = false, want true")
			}
			if !got.CommandRewrite.Enabled {
				t.Fatalf("command_rewrite.enabled = false, want true")
			}
			if !got.OutputReduce.Enabled {
				t.Fatalf("output_reduce.enabled = false, want true")
			}
			if len(got.Providers) != 1 {
				t.Fatalf("providers count = %d, want 1", len(got.Providers))
			}

			provider := got.Providers[0]
			if provider.Type != "rtk_cli" {
				t.Fatalf("provider type = %q, want rtk_cli", provider.Type)
			}
			if !provider.supportsStage(toolOptimizerStageCommandRewrite) {
				t.Fatalf("provider with omitted stages should support command rewrite")
			}
			if !provider.supportsStage(toolOptimizerStageOutputReduce) {
				t.Fatalf("provider with omitted stages should support output reduce")
			}

			providers := buildConfiguredToolOptimizers(got)
			if len(providers) != 1 {
				t.Fatalf("built providers count = %d, want 1", len(providers))
			}
			if !providers[0].commandRewrite {
				t.Fatalf("built provider commandRewrite = false, want true")
			}
			if !providers[0].outputReduce {
				t.Fatalf("built provider outputReduce = false, want true")
			}
			rtk, ok := providers[0].optimizer.(*rtkToolOptimizer)
			if !ok {
				t.Fatalf("built optimizer = %T, want *rtkToolOptimizer", providers[0].optimizer)
			}
			if rtk.path != "rtk" {
				t.Fatalf("rtk path = %q, want rtk", rtk.path)
			}
		})
	}
}

func assertExplicitZeroOutputReduce(t *testing.T, cfg ToolOptimizersConfig) {
	t.Helper()
	if !cfg.OutputReduce.Enabled {
		t.Fatalf("output_reduce.enabled = false, want true")
	}
	if cfg.OutputReduce.TimeoutMS != 0 {
		t.Fatalf("timeout_ms = %d, want 0", cfg.OutputReduce.TimeoutMS)
	}
	if cfg.OutputReduce.MinInputBytes != 0 {
		t.Fatalf("min_input_bytes = %d, want 0", cfg.OutputReduce.MinInputBytes)
	}
	if cfg.OutputReduce.MaxInputBytes != 0 {
		t.Fatalf("max_input_bytes = %d, want 0", cfg.OutputReduce.MaxInputBytes)
	}
}
