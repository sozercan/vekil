//go:build windows

package proxy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsOptimizerProcessController struct {
	cmd *exec.Cmd

	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func newOptimizerProcessController(cmd *exec.Cmd) (optimizerProcessController, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create optimizer job object: %w", err)
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
		return nil, fmt.Errorf("configure optimizer job object: %w", err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Starting suspended closes the assignment race: the optimizer cannot spawn
	// descendants before it is placed in the kill-on-close Job Object.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return &windowsOptimizerProcessController{cmd: cmd, job: job}, nil
}

func (c *windowsOptimizerProcessController) afterStart() error {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(c.cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open suspended optimizer process: %w", err)
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
		return fmt.Errorf("assign optimizer process to job object: %w", err)
	}
	if err := resumeWindowsProcessThreads(uint32(c.cmd.Process.Pid)); err != nil {
		return fmt.Errorf("resume optimizer process: %w", err)
	}
	return nil
}

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

func (c *windowsOptimizerProcessController) terminate() error {
	if c == nil {
		return os.ErrProcessDone
	}
	c.mu.Lock()
	job := c.job
	closed := c.closed
	c.mu.Unlock()

	var jobErr error
	if !closed && job != 0 {
		jobErr = windows.TerminateJobObject(job, 1)
	}
	var processErr error
	if c.cmd != nil && c.cmd.Process != nil {
		processErr = c.cmd.Process.Kill()
		if errors.Is(processErr, os.ErrProcessDone) {
			processErr = nil
		}
	}
	return errors.Join(jobErr, processErr)
}

func (c *windowsOptimizerProcessController) close() error {
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
