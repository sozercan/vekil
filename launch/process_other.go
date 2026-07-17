//go:build !darwin && !linux && !windows

package launch

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

func newProcessController(cmd *exec.Cmd, _ io.Reader) (processController, error) {
	if cmd == nil {
		return nil, fmt.Errorf("agent command is nil")
	}
	return nil, fmt.Errorf("agent launcher process containment is not supported on %s", runtime.GOOS)
}
