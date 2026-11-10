package main

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/sozercan/vekil/proxy"
)

func TestStateBindingsCLIOverridePrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bindings.db")
	t.Setenv("STATE_BINDINGS_MODE", "durable")
	t.Setenv("STATE_BINDINGS_FILE", file)
	t.Setenv("STATE_BINDINGS_MAX_ENTRIES", "23")
	fromEnv, err := proxy.StateBindingsEnvironmentOverrides()
	if err != nil {
		t.Fatal(err)
	}
	flags := parseServeFlagsForTest(t)
	got, err := flags.parsedStateBindingsConfig()
	if err != nil || got != fromEnv {
		t.Fatalf("CLI/env mismatch: %+v, %+v, %v", got, fromEnv, err)
	}
	flags = parseServeFlagsForTest(t, "--state-bindings-mode=memory", "--state-bindings-max-entries=31")
	got, err = flags.parsedStateBindingsConfig()
	if err != nil || got.Mode != "memory" || got.MaxEntries != 31 || got.File != file {
		t.Fatalf("flag overrides = %+v, %v", got, err)
	}
	// Parse the selected value, so valid flags can replace invalid environment settings.
	t.Setenv("STATE_BINDINGS_MAX_ENTRIES", "invalid")
	if _, err := proxy.StateBindingsEnvironmentOverrides(); err == nil {
		t.Fatal("invalid environment ignored")
	}
	flags = parseServeFlagsForTest(t, "--state-bindings-max-entries=5", "--state-bindings-mode=config", "--state-bindings-file=")
	got, err = flags.parsedStateBindingsConfig()
	if err != nil || got.Mode != "config" || got.MaxEntries != 5 || got.File != "" {
		t.Fatalf("selected overrides = %+v, %v", got, err)
	}
	flags = parseServeFlagsForTest(t)
	if _, err := flags.parsedStateBindingsConfig(); err == nil {
		t.Fatal("invalid selected value ignored")
	}
}

func TestStateBindingsLaunchUsesSharedOverrides(t *testing.T) {
	t.Setenv("STATE_BINDINGS_MODE", "memory")
	t.Setenv("STATE_BINDINGS_FILE", "")
	t.Setenv("STATE_BINDINGS_MAX_ENTRIES", "0")
	target, _ := launchTarget("codex")
	opts, err := parseLaunchAgentOptions(target, []string{"--state-bindings-mode=durable", "--state-bindings-max-entries=41"}, io.Discard)
	if err != nil || opts.stateBindings.Mode != "durable" || opts.stateBindings.MaxEntries != 41 {
		t.Fatalf("launch state config = %+v, %v", opts.stateBindings, err)
	}
	opts, err = parseLaunchAgentOptions(target, nil, io.Discard)
	if err != nil || opts.stateBindings.Mode != "memory" {
		t.Fatalf("launch memory override = %+v, %v", opts.stateBindings, err)
	}
}
