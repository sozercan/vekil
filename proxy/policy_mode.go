package proxy

import (
	"fmt"
	"strings"
)

// PolicyRoutingMode controls the process-wide policy safety ceiling. Config
// follows each profile mode; off, observe, and enforce apply explicit ceilings.
type PolicyRoutingMode string

const (
	PolicyRoutingModeConfig  PolicyRoutingMode = "config"
	PolicyRoutingModeOff     PolicyRoutingMode = "off"
	PolicyRoutingModeObserve PolicyRoutingMode = "observe"
	PolicyRoutingModeEnforce PolicyRoutingMode = "enforce"
)

func ParsePolicyRoutingMode(value string) (PolicyRoutingMode, error) {
	if strings.TrimSpace(value) == string(PolicyRoutingModeConfig) {
		return PolicyRoutingModeConfig, nil
	}
	mode, err := parsePolicyMode(value)
	if err != nil {
		return PolicyRoutingModeOff, fmt.Errorf("policy routing mode must be config, off, observe, or enforce: %w", err)
	}
	return PolicyRoutingMode(mode.String()), nil
}

func (m PolicyRoutingMode) internal() policyMode {
	if strings.TrimSpace(string(m)) == string(PolicyRoutingModeConfig) {
		// The highest ceiling leaves each profile's configured mode unchanged.
		return policyModeEnforce
	}
	mode, err := parsePolicyMode(string(m))
	if err != nil {
		return policyModeOff
	}
	return mode
}

func (m PolicyRoutingMode) valid() bool {
	_, err := ParsePolicyRoutingMode(string(m))
	return err == nil
}
