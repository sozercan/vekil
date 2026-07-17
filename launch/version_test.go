package launch

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type failedStartProcessController struct {
	killCalled bool
	waitCalled bool
	killErr    error
	waitErr    error
}

func (*failedStartProcessController) afterStart() error { return nil }
func (c *failedStartProcessController) wait() commandOutcome {
	c.waitCalled = true
	return commandOutcome{err: c.waitErr}
}
func (*failedStartProcessController) signal(os.Signal) error { return nil }
func (c *failedStartProcessController) kill() error {
	c.killCalled = true
	return c.killErr
}
func (*failedStartProcessController) close() error { return nil }

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

func TestReapFailedContainedCommandKillsAndWaits(t *testing.T) {
	cause := errors.New("initialize controller")
	killErr := errors.New("kill child")
	waitErr := errors.New("wait child")
	controller := &failedStartProcessController{killErr: killErr, waitErr: waitErr}
	err := reapFailedContainedCommand(controller, cause)
	if !controller.killCalled || !controller.waitCalled {
		t.Fatalf("killCalled=%v waitCalled=%v, want both true", controller.killCalled, controller.waitCalled)
	}
	for _, want := range []error{cause, killErr, waitErr} {
		if !errors.Is(err, want) {
			t.Fatalf("reap error = %v, missing %v", err, want)
		}
	}
}
