//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package proxy

import (
	"errors"
	"os"
	"os/exec"
)

// Unsupported targets use direct-process termination plus Cmd.WaitDelay's
// bounded pipe cleanup. Unix and Windows builds use real process-tree
// containment instead; this fallback exists only so non-server targets compile.
type fallbackOptimizerProcessController struct {
	cmd *exec.Cmd
}

func newOptimizerProcessController(cmd *exec.Cmd) (optimizerProcessController, error) {
	return &fallbackOptimizerProcessController{cmd: cmd}, nil
}

func (*fallbackOptimizerProcessController) afterStart() error { return nil }

func (c *fallbackOptimizerProcessController) terminate() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := c.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (*fallbackOptimizerProcessController) close() error { return nil }
