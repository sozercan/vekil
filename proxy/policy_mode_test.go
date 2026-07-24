package proxy

import "testing"

func TestPolicyRoutingModeConfigFollowsProfileMode(t *testing.T) {
	mode, err := ParsePolicyRoutingMode("config")
	if err != nil {
		t.Fatalf("ParsePolicyRoutingMode(config) error = %v", err)
	}
	if mode != PolicyRoutingModeConfig {
		t.Fatalf("ParsePolicyRoutingMode(config) = %q, want %q", mode, PolicyRoutingModeConfig)
	}

	profiles := []policyMode{policyModeOff, policyModeObserve, policyModeEnforce}
	for _, profile := range profiles {
		if got := effectivePolicyMode(mode.internal(), profile); got != profile {
			t.Errorf("config mode with profile %q = %q, want profile mode", profile, got)
		}
	}
}

func TestParsePolicyRoutingModeTrimsConfig(t *testing.T) {
	mode, err := ParsePolicyRoutingMode("  config  ")
	if err != nil {
		t.Fatalf("ParsePolicyRoutingMode(spaced config) error = %v", err)
	}
	if mode != PolicyRoutingModeConfig {
		t.Fatalf("ParsePolicyRoutingMode(spaced config) = %q, want %q", mode, PolicyRoutingModeConfig)
	}
}

func TestPolicyRoutingModeZeroValueRemainsOff(t *testing.T) {
	var mode PolicyRoutingMode
	if !mode.valid() {
		t.Fatal("zero PolicyRoutingMode should remain a valid safe-off value")
	}
	if got := mode.internal(); got != policyModeOff {
		t.Fatalf("zero PolicyRoutingMode internal mode = %q, want off", got)
	}
}

func TestPolicyRoutingModeConfigTypedValueTrimsWhitespace(t *testing.T) {
	mode := PolicyRoutingMode("  config  ")
	if !mode.valid() {
		t.Fatal("whitespace-padded config mode should be valid")
	}
	if got := effectivePolicyMode(mode.internal(), policyModeObserve); got != policyModeObserve {
		t.Fatalf("whitespace-padded config mode with observe profile = %q, want observe", got)
	}
}
