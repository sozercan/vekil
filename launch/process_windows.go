//go:build windows

package launch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessController struct {
	cmd *exec.Cmd

	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func newProcessController(cmd *exec.Cmd, _ io.Reader) (processController, error) {
	if cmd == nil {
		return nil, fmt.Errorf("agent command is nil")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create launcher job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure launcher job object: %w", err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	return &windowsProcessController{cmd: cmd, job: job}, nil
}

func (c *windowsProcessController) afterStart() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(c.cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open suspended launcher process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(processHandle) }()

	c.mu.Lock()
	job := c.job
	closed := c.closed
	c.mu.Unlock()
	if closed || job == 0 {
		return os.ErrProcessDone
	}
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		return fmt.Errorf("assign launcher process to job object: %w", err)
	}
	return resumeWindowsProcessThreads(uint32(c.cmd.Process.Pid))
}

func (c *windowsProcessController) wait() commandOutcome { return waitCommand(c.cmd) }

func resumeWindowsProcessThreads(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return openErr
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			resumed++
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ThreadEntry32{}))
		err = windows.Thread32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return err
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no threads found for pid %d", pid)
	}
	return nil
}

func (c *windowsProcessController) signal(os.Signal) error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	// Windows cannot direct CTRL_C_EVENT at an arbitrary process group. A
	// CTRL_BREAK_EVENT targets the CREATE_NEW_PROCESS_GROUP child and gives
	// Node/Claude a chance to clean up; the Job Object remains the timeout
	// fallback for processes that do not exit.
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(c.cmd.Process.Pid))
}

func (c *windowsProcessController) kill() error {
	if c == nil {
		return os.ErrProcessDone
	}
	c.mu.Lock()
	var jobErr error
	if !c.closed && c.job != 0 {
		// Keep the Job Object handle protected through termination so close cannot
		// release or recycle it between the state check and the system call.
		jobErr = windows.TerminateJobObject(c.job, 1)
	}
	c.mu.Unlock()
	var processErr error
	if c.cmd != nil && c.cmd.Process != nil {
		processErr = c.cmd.Process.Kill()
		if errors.Is(processErr, os.ErrProcessDone) {
			processErr = nil
		}
	}
	return errors.Join(jobErr, processErr)
}

func (c *windowsProcessController) close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.job == 0 {
		return nil
	}
	c.closed = true
	err := windows.CloseHandle(c.job)
	c.job = 0
	return err
}
