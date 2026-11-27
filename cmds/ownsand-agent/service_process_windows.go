// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// Assign the job during CreateProcess. AssignProcessToJobObject after
	// creation would leave a process outside the job if the launcher died.
	processAttributeJobList = 0x0002000d
	payloadStopTimeout      = 20 * time.Second
	payloadWaitIntervalMS   = 1000
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	attachPayloadConsole  = kernel32.NewProc("AttachConsole")
	freePayloadConsole    = kernel32.NewProc("FreeConsole")
	setConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")

	ignorePayloadConsoleBreak = windows.NewCallback(func(event uint32) uintptr {
		if event == windows.CTRL_BREAK_EVENT {
			return 1
		}
		return 0
	})
)

type payloadProcessOps struct {
	start       func(windows.Handle, string, []string, *os.File) (windows.ProcessInformation, error)
	wait        func(windows.Handle, <-chan struct{}) (uint32, error)
	signal      func(uint32) error
	freeConsole func()
	terminate   func(windows.Handle, uint32) error
	closeHandle func(windows.Handle) error
	stopTimeout time.Duration
}

func windowsPayloadProcessOps() payloadProcessOps {
	return payloadProcessOps{
		start:       startPayloadProcess,
		wait:        waitPayloadProcess,
		signal:      stopPayloadConsole,
		freeConsole: func() { freePayloadConsole.Call() },
		terminate:   windows.TerminateJobObject,
		closeHandle: windows.CloseHandle,
		stopTimeout: payloadStopTimeout,
	}
}

func runPayloadProcess(ctx context.Context, exe string, args []string, stderr *os.File) (int, error) {
	return runPayloadProcessWith(ctx, exe, args, stderr, windowsPayloadProcessOps())
}

func runPayloadProcessWith(ctx context.Context, exe string, args []string, stderr *os.File, ops payloadProcessOps) (int, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create the service payload job: %w", err)
	}
	closeJob := func() error {
		if job == 0 {
			return nil
		}
		err := ops.closeHandle(job)
		job = 0
		return err
	}
	defer closeJob()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return 0, fmt.Errorf("set the service payload job limits: %w", err)
	}
	child, err := ops.start(job, exe, args, stderr)
	if err != nil {
		return 0, fmt.Errorf("launch the service payload: %w", err)
	}
	defer ops.closeHandle(child.Process)
	defer ops.closeHandle(child.Thread)
	cancelWait := make(chan struct{})
	done := make(chan struct{})
	var code uint32
	var waitErr error
	go func() {
		defer close(done)
		code, waitErr = ops.wait(child.Process, cancelWait)
	}()
	select {
	case <-done:
		return int(code), waitErr
	case <-ctx.Done():
	}
	forceStop := func(stopErr error) (int, error) {
		if err := ops.terminate(job, 1); err != nil {
			stopErr = fmt.Errorf("%w: terminate the service payload job: %w", stopErr, err)
			// Kill-on-close gives Windows another way to stop the job. Join the
			// waiter even if closing the job does not make the payload exit.
			if err := closeJob(); err != nil {
				stopErr = fmt.Errorf("%w: close the service payload job: %w", stopErr, err)
			}
			close(cancelWait)
		}
		<-done
		return 0, stopErr
	}

	// Keep the console handler registered while stopping the payload.
	defer ops.freeConsole()
	if err := ops.signal(child.ProcessId); err != nil {
		// The process can exit before AttachConsole. Terminate the job if
		// the payload has already lost its console and is still running.
		select {
		case <-done:
			return int(code), waitErr
		default:
			return forceStop(fmt.Errorf("signal the service payload to stop: %w", err))
		}
	}
	timer := time.NewTimer(ops.stopTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return int(code), waitErr
	case <-timer.C:
		return forceStop(fmt.Errorf("the service payload did not stop within %s", ops.stopTimeout))
	}
}

func waitPayloadProcess(process windows.Handle, cancel <-chan struct{}) (uint32, error) {
	for {
		select {
		case <-cancel:
			return 0, context.Canceled
		default:
		}
		// A bounded wait lets the launcher cancel and join this waiter even
		// if the process is still running and Windows rejected termination.
		state, err := windows.WaitForSingleObject(process, payloadWaitIntervalMS)
		if err != nil {
			return 0, err
		}
		switch state {
		case windows.WAIT_OBJECT_0:
			var code uint32
			err := windows.GetExitCodeProcess(process, &code)
			return code, err
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			return 0, fmt.Errorf("unexpected service payload wait status: %#x", state)
		}
	}
}

func startPayloadProcess(job windows.Handle, exe string, args []string, stderr *os.File) (windows.ProcessInformation, error) {
	var child windows.ProcessInformation
	app, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return child, err
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return child, err
	}

	attr, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return child, err
	}
	defer attr.Delete()
	if err := attr.Update(processAttributeJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return child, err
	}
	null, err := os.OpenFile("NUL", os.O_RDWR, 0)
	if err != nil {
		return child, err
	}
	defer null.Close()
	// The SCM process has no standard streams. Give the child the log
	// that the service opened, using NUL if the log could not be opened.
	if stderr == nil {
		stderr = null
	}
	var handles [2]windows.Handle
	for i, f := range []*os.File{null, stderr} {
		current := windows.CurrentProcess()
		if err := windows.DuplicateHandle(current, windows.Handle(f.Fd()), current, &handles[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return child, err
		}
		defer windows.CloseHandle(handles[i])
	}
	if err := attr.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), unsafe.Sizeof(handles)); err != nil {
		return child, err
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Flags: windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE,
			StdInput: handles[0], StdOutput: handles[0], StdErr: handles[1],
		},
		ProcThreadAttributeList: attr.List(),
	}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	err = windows.CreateProcess(app, line, nil, nil, true, windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_NEW_CONSOLE,
		nil, nil, &startup.StartupInfo, &child)
	runtime.KeepAlive(stderr)
	return child, err
}

// The payload uses the normal interactive run command, including in releases
// that predate the launcher. Give it a private console so a stop request can
// deliver the same interrupt that the foreground command handles.
func stopPayloadConsole(pid uint32) error {
	// AttachConsole fails if the launcher already has a console.
	freePayloadConsole.Call()
	if ok, _, err := attachPayloadConsole.Call(uintptr(pid)); ok == 0 {
		return err
	}
	// Windows propagates the setting that ignores Ctrl+C to child processes.
	// Ctrl+Break still reaches the payload's interrupt handler. The event also
	// reaches the launcher while it is attached, so consume it here.
	if ok, _, err := setConsoleCtrlHandler.Call(ignorePayloadConsoleBreak, 1); ok == 0 {
		return err
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0)
}
