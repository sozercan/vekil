//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package proxy

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type unixOptimizerProcessController struct {
	cmd *exec.Cmd
}

func newOptimizerProcessController(cmd *exec.Cmd) (optimizerProcessController, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	return &unixOptimizerProcessController{cmd: cmd}, nil
}

func (*unixOptimizerProcessController) afterStart() error { return nil }

func (c *unixOptimizerProcessController) terminate() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (*unixOptimizerProcessController) close() error { return nil }
