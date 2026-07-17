//go:build !darwin && !linux && !windows

package launch

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

type fallbackProcessController struct {
	cmd *exec.Cmd
}

func newProcessController(cmd *exec.Cmd, _ io.Reader) (processController, error) {
	if cmd == nil {
		return nil, fmt.Errorf("agent command is nil")
	}
	return &fallbackProcessController{cmd: cmd}, nil
}

func (*fallbackProcessController) afterStart() error { return nil }

func (c *fallbackProcessController) wait() commandOutcome { return waitCommand(c.cmd) }

func (c *fallbackProcessController) signal(signalValue os.Signal) error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return c.cmd.Process.Signal(signalValue)
}

func (c *fallbackProcessController) kill() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return c.cmd.Process.Kill()
}

func (*fallbackProcessController) close() error { return nil }
