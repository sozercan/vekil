//go:build darwin || linux

package launch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const remainingProcessGrace = 200 * time.Millisecond

type unixProcessController struct {
	cmd *exec.Cmd

	interactive bool
	grouped     bool
	childPgrp   int
	terminalFD  int
	parentPgrp  int
	closeOnce   sync.Once
	closeErr    error
}

func newProcessController(cmd *exec.Cmd, stdin io.Reader) (processController, error) {
	if cmd == nil {
		return nil, fmt.Errorf("agent command is nil")
	}
	controller := &unixProcessController{cmd: cmd, terminalFD: -1}
	stdinFile, stdinOK := stdin.(*os.File)
	_, stdoutOK := cmd.Stdout.(*os.File)
	_, stderrOK := cmd.Stderr.(*os.File)
	if stdinOK && stdoutOK && stderrOK && term.IsTerminal(int(stdinFile.Fd())) {
		terminalFD := int(stdinFile.Fd())
		foregroundPgrp, err := unix.IoctlGetInt(terminalFD, unix.TIOCGPGRP)
		if err != nil {
			return nil, fmt.Errorf("read terminal foreground process group: %w", err)
		}
		if foregroundPgrp != unix.Getpgrp() {
			return nil, fmt.Errorf("interactive agent launch must run in the foreground; resume the shell job with fg")
		}
		controller.interactive = true
		controller.grouped = true
		controller.terminalFD = terminalFD
		controller.parentPgrp = foregroundPgrp
		signal.Ignore(syscall.SIGTTOU)
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = controller.terminalFD
		return controller, nil
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	controller.grouped = true
	return controller, nil
}

func (c *unixProcessController) afterStart() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	c.childPgrp = c.cmd.Process.Pid
	return nil
}

func (c *unixProcessController) wait() commandOutcome {
	if !c.interactive {
		return waitCommand(c.cmd)
	}
	return c.waitInteractive()
}

func (c *unixProcessController) waitInteractive() commandOutcome {
	pid := c.childPgrp
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(pid, &status, syscall.WUNTRACED|syscall.WCONTINUED, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return commandOutcome{code: 1, err: fmt.Errorf("wait for interactive agent process: %w", err)}
		}
		switch {
		case status.Exited():
			_ = c.cmd.Process.Release()
			return commandOutcome{code: status.ExitStatus()}
		case status.Signaled():
			_ = c.cmd.Process.Release()
			return commandOutcome{code: 128 + int(status.Signal())}
		case status.Stopped():
			if err := c.setForeground(c.parentPgrp); err != nil {
				return commandOutcome{code: 1, err: fmt.Errorf("restore terminal after agent stop: %w", err)}
			}
			// Stop Vekil's shell-visible job. When the shell resumes it with fg,
			// execution continues here, the child group regains the terminal, and
			// every process in that group receives SIGCONT.
			if err := syscall.Kill(0, status.StopSignal()); err != nil {
				return commandOutcome{code: 1, err: fmt.Errorf("suspend launcher job: %w", err)}
			}
			// Resume once before and once after the terminal handoff. If the
			// process wakes while still in the background, a blocking terminal read
			// can immediately stop it with SIGTTIN; the second SIGCONT resumes it
			// after it owns the terminal.
			_ = signalUnixGroup(pid, syscall.SIGCONT)
			if err := c.setForeground(pid); err != nil {
				return commandOutcome{code: 1, err: fmt.Errorf("restore agent terminal after continue: %w", err)}
			}
			if err := signalUnixGroup(pid, syscall.SIGCONT); err != nil {
				return commandOutcome{code: 1, err: fmt.Errorf("continue agent process group: %w", err)}
			}
		}
	}
}

func (c *unixProcessController) signal(signalValue os.Signal) error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	sig, err := unixSignal(signalValue)
	if err != nil {
		return err
	}
	if c.grouped && c.childPgrp > 0 {
		return signalUnixGroup(c.childPgrp, sig)
	}
	err = c.cmd.Process.Signal(signalValue)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (c *unixProcessController) kill() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	if c.grouped && c.childPgrp > 0 {
		return signalUnixGroup(c.childPgrp, syscall.SIGKILL)
	}
	err := c.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (c *unixProcessController) close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.grouped && c.childPgrp > 0 {
			c.closeErr = terminateUnixGroup(c.childPgrp)
		}
		if c.interactive {
			// Best effort: some Darwin terminals return EPERM after the child
			// group disappears even though the shell regains control correctly.
			_ = c.setForeground(c.parentPgrp)
			signal.Reset(syscall.SIGTTOU)
		}
	})
	return c.closeErr
}

func (c *unixProcessController) setForeground(pgrp int) error {
	if c.terminalFD < 0 {
		return nil
	}
	return unix.IoctlSetPointerInt(c.terminalFD, unix.TIOCSPGRP, pgrp)
}

func unixSignal(signalValue os.Signal) (syscall.Signal, error) {
	if sig, ok := signalValue.(syscall.Signal); ok {
		return sig, nil
	}
	if signalValue == os.Interrupt {
		return syscall.SIGINT, nil
	}
	return 0, fmt.Errorf("unsupported process signal %v", signalValue)
}

func signalUnixGroup(pgid int, sig syscall.Signal) error {
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

func terminateUnixGroup(pgid int) error {
	if err := signalUnixGroup(pgid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(remainingProcessGrace)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return signalUnixGroup(pgid, syscall.SIGKILL)
}
