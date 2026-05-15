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
