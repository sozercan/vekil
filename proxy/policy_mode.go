package proxy

import "fmt"

// PolicyRoutingMode is the process-wide safety ceiling for policy profiles.
type PolicyRoutingMode string

const (
	PolicyRoutingModeOff     PolicyRoutingMode = "off"
	PolicyRoutingModeObserve PolicyRoutingMode = "observe"
	PolicyRoutingModeEnforce PolicyRoutingMode = "enforce"
)

func ParsePolicyRoutingMode(value string) (PolicyRoutingMode, error) {
	mode, err := parsePolicyMode(value)
	if err != nil {
		return PolicyRoutingModeOff, fmt.Errorf("policy routing mode must be off, observe, or enforce: %w", err)
	}
	return PolicyRoutingMode(mode.String()), nil
}

func (m PolicyRoutingMode) internal() policyMode {
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
