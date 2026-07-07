package proxy

import (
	"testing"
	"time"
)

func TestCodeExecConfig_DefaultsDisabled(t *testing.T) {
	cfg := CodeExecConfig{}.withDefaults()
	if cfg.Enabled {
		t.Error("Enabled = true, want false by default")
	}
	if cfg.active() {
		t.Error("active() = true, want false when disabled")
	}
	if cfg.Backend != defaultCodeExecBackend {
		t.Errorf("Backend = %q, want %q", cfg.Backend, defaultCodeExecBackend)
	}
	if cfg.MaxLoopDepth != defaultCodeExecMaxLoopDepth {
		t.Errorf("MaxLoopDepth = %d, want %d", cfg.MaxLoopDepth, defaultCodeExecMaxLoopDepth)
	}
	if len(cfg.OwnedTools) != 1 || cfg.OwnedTools[0] != defaultCodeExecOwnedTool {
		t.Errorf("OwnedTools = %v, want [%s]", cfg.OwnedTools, defaultCodeExecOwnedTool)
	}
	if cfg.Policy.TimeoutMS != defaultCodeExecTimeoutMS {
		t.Errorf("Policy.TimeoutMS = %d, want %d", cfg.Policy.TimeoutMS, defaultCodeExecTimeoutMS)
	}
}

func TestCodeExecConfig_ActiveRequiresEnabledAndTools(t *testing.T) {
	if (CodeExecConfig{Enabled: true, OwnedTools: nil}).withDefaults().active() == false {
		// withDefaults populates a default owned tool, so this is active.
		t.Error("enabled config with default tools should be active")
	}
	if (CodeExecConfig{Enabled: false, OwnedTools: []string{"Bash"}}).active() {
		t.Error("disabled config must not be active")
	}
	if (CodeExecConfig{Enabled: true, OwnedTools: []string{"  "}}).active() {
		t.Error("enabled config with only blank tools must not be active")
	}
}

func TestCodeExecConfig_OwnsToolCaseInsensitive(t *testing.T) {
	cfg := CodeExecConfig{Enabled: true, OwnedTools: []string{"Bash"}}.withDefaults()
	if !cfg.ownsTool("bash") {
		t.Error("ownsTool(bash) = false, want true (case-insensitive)")
	}
	if !cfg.ownsTool("BASH") {
		t.Error("ownsTool(BASH) = false, want true")
	}
	if cfg.ownsTool("WebSearch") {
		t.Error("ownsTool(WebSearch) = true, want false")
	}
	if cfg.ownsTool("") {
		t.Error("ownsTool(empty) = true, want false")
	}
}

func TestCodeExecConfig_EnvOverrides(t *testing.T) {
	t.Setenv(envCodeExecEnabled, "true")
	t.Setenv(envCodeExecOwnedTools, "Bash, Shell , Bash")
	t.Setenv(envCodeExecBackend, "Local")
	t.Setenv(envCodeExecTimeoutMS, "1500")
	t.Setenv(envCodeExecMaxOutputBytes, "2048")
	t.Setenv(envCodeExecWorkDir, "/tmp/work")
	t.Setenv(envCodeExecEnvAllowlist, "PATH,HOME")
	t.Setenv(envCodeExecMaxLoopDepth, "5")

	cfg := CodeExecConfig{}.withEnvOverrides().withDefaults()
	if !cfg.Enabled {
		t.Error("Enabled = false, want true from env")
	}
	if len(cfg.OwnedTools) != 2 {
		t.Errorf("OwnedTools = %v, want 2 deduped entries", cfg.OwnedTools)
	}
	if cfg.Backend != "local" {
		t.Errorf("Backend = %q, want local (lowercased)", cfg.Backend)
	}
	if cfg.MaxLoopDepth != 5 {
		t.Errorf("MaxLoopDepth = %d, want 5", cfg.MaxLoopDepth)
	}
	if cfg.Policy.TimeoutMS != 1500 {
		t.Errorf("Policy.TimeoutMS = %d, want 1500", cfg.Policy.TimeoutMS)
	}
	if cfg.Policy.MaxOutputBytes != 2048 {
		t.Errorf("Policy.MaxOutputBytes = %d, want 2048", cfg.Policy.MaxOutputBytes)
	}
	if cfg.Policy.WorkingDir != "/tmp/work" {
		t.Errorf("Policy.WorkingDir = %q, want /tmp/work", cfg.Policy.WorkingDir)
	}
	if len(cfg.Policy.EnvAllowlist) != 2 {
		t.Errorf("Policy.EnvAllowlist = %v, want [PATH HOME]", cfg.Policy.EnvAllowlist)
	}
}

func TestCodeExecConfig_EnvInvalidValuesIgnored(t *testing.T) {
	t.Setenv(envCodeExecEnabled, "not-a-bool")
	t.Setenv(envCodeExecTimeoutMS, "abc")
	t.Setenv(envCodeExecMaxLoopDepth, "-3")

	cfg := CodeExecConfig{Policy: CodeExecPolicyConfig{TimeoutMS: 9000}}.withEnvOverrides()
	if cfg.Enabled {
		t.Error("Enabled = true, want false (invalid bool ignored)")
	}
	if cfg.Policy.TimeoutMS != 9000 {
		t.Errorf("Policy.TimeoutMS = %d, want 9000 (invalid override ignored)", cfg.Policy.TimeoutMS)
	}
	if cfg.MaxLoopDepth != 0 {
		t.Errorf("MaxLoopDepth = %d, want 0 (negative override ignored)", cfg.MaxLoopDepth)
	}
}

func TestCodeExecPolicyConfig_ToPolicy(t *testing.T) {
	policy := CodeExecPolicyConfig{
		TimeoutMS:      2500,
		MaxOutputBytes: 4096,
		WorkingDir:     "  /tmp/x  ",
		EnvAllowlist:   []string{"PATH", "  ", "PATH"},
	}.toPolicy()

	if policy.Timeout != 2500*time.Millisecond {
		t.Errorf("Timeout = %v, want 2.5s", policy.Timeout)
	}
	if policy.MaxOutputBytes != 4096 {
		t.Errorf("MaxOutputBytes = %d, want 4096", policy.MaxOutputBytes)
	}
	if policy.WorkingDir != "/tmp/x" {
		t.Errorf("WorkingDir = %q, want /tmp/x (trimmed)", policy.WorkingDir)
	}
	if len(policy.EnvAllowlist) != 1 {
		t.Errorf("EnvAllowlist = %v, want [PATH] (cleaned)", policy.EnvAllowlist)
	}
}

func TestNewCodeExecBackend(t *testing.T) {
	for _, name := range []string{"", "local", "LOCAL"} {
		backend, err := newCodeExecBackend(name)
		if err != nil {
			t.Errorf("newCodeExecBackend(%q) error = %v", name, err)
			continue
		}
		if backend.Name() != "local" {
			t.Errorf("newCodeExecBackend(%q).Name() = %q, want local", name, backend.Name())
		}
	}
	if _, err := newCodeExecBackend("nonexistent"); err == nil {
		t.Error("newCodeExecBackend(nonexistent) error = nil, want error")
	}
}
