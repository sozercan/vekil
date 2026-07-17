//go:build !darwin && !linux && !windows

package launch

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestUnsupportedPlatformRejectsLauncherProcess(t *testing.T) {
	_, err := newProcessController(exec.Command("unsupported-launcher-test"), nil)
	if err == nil || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("newProcessController() error = %v, want unsupported %s error", err, runtime.GOOS)
	}
}
