package main

import (
	"os"
	"testing"

	"github.com/sozercan/vekil/proxy"
)

func TestMenubarPolicyRoutingModeDefaultsToProvidersConfig(t *testing.T) {
	previous, wasSet := os.LookupEnv("POLICY_ROUTING_MODE")
	if err := os.Unsetenv("POLICY_ROUTING_MODE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv("POLICY_ROUTING_MODE", previous)
			return
		}
		_ = os.Unsetenv("POLICY_ROUTING_MODE")
	})

	mode, err := menubarPolicyRoutingMode()
	if err != nil {
		t.Fatalf("menubarPolicyRoutingMode() error = %v", err)
	}
	if mode != proxy.PolicyRoutingModeConfig {
		t.Fatalf("menubarPolicyRoutingMode() = %q, want %q", mode, proxy.PolicyRoutingModeConfig)
	}
}

func TestMenubarPolicyRoutingModeHonorsExplicitEnvironmentOverride(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  proxy.PolicyRoutingMode
	}{
		{name: "empty follows config", value: "", want: proxy.PolicyRoutingModeConfig},
		{name: "blank follows config", value: "  ", want: proxy.PolicyRoutingModeConfig},
		{name: "config", value: "config", want: proxy.PolicyRoutingModeConfig},
		{name: "off", value: "off", want: proxy.PolicyRoutingModeOff},
		{name: "observe", value: "observe", want: proxy.PolicyRoutingModeObserve},
		{name: "enforce", value: "enforce", want: proxy.PolicyRoutingModeEnforce},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POLICY_ROUTING_MODE", tc.value)
			mode, err := menubarPolicyRoutingMode()
			if err != nil {
				t.Fatalf("menubarPolicyRoutingMode() error = %v", err)
			}
			if mode != tc.want {
				t.Fatalf("menubarPolicyRoutingMode() = %q, want %q", mode, tc.want)
			}
		})
	}
}

func TestMenubarPolicyRoutingModeRejectsInvalidEnvironmentOverride(t *testing.T) {
	t.Setenv("POLICY_ROUTING_MODE", "sometimes")
	if _, err := menubarPolicyRoutingMode(); err == nil {
		t.Fatal("menubarPolicyRoutingMode() error = nil, want invalid mode error")
	}
}
