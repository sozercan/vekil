package launch

import (
	"strings"
	"testing"
)

func TestValidateVersionOutputRequiresExpectedProduct(t *testing.T) {
	err := validateVersionOutput(
		"copilot version: v1.34.0",
		"GitHub Copilot CLI",
		"GitHub Copilot CLI",
		minimumVersion{major: 1},
	)
	if err == nil || !strings.Contains(err.Error(), "unrecognized output") {
		t.Fatalf("validateVersionOutput() error = %v", err)
	}
}

func TestValidateVersionOutputEnforcesMinimum(t *testing.T) {
	err := validateVersionOutput(
		"codex-cli 0.136.9",
		"Codex CLI",
		"codex",
		minimumVersion{major: 0, minor: 137},
	)
	if err == nil || !strings.Contains(err.Error(), "0.137.0 or newer") {
		t.Fatalf("validateVersionOutput() error = %v", err)
	}
	if err := validateVersionOutput(
		"codex-cli 0.137.0",
		"Codex CLI",
		"codex",
		minimumVersion{major: 0, minor: 137},
	); err != nil {
		t.Fatalf("validateVersionOutput() = %v", err)
	}
}
