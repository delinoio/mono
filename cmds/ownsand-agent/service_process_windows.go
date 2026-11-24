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
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	attachPayloadConsole  = kernel32.NewProc("AttachConsole")
	freePayloadConsole    = kernel32.NewProc("FreeConsole")
	setConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

func runPayloadProcess(ctx context.Context, exe string, args []string, stderr *os.File) (int, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create the service payload job: %w", err)
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return 0, fmt.Errorf("set the service payload job limits: %w", err)
	}
	child, err := startPayloadProcess(job, exe, args, stderr)
	if err != nil {
		return 0, fmt.Errorf("launch the service payload: %w", err)
	}
	defer windows.CloseHandle(child.Process)
	defer windows.CloseHandle(child.Thread)
	type result struct {
		code uint32
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, err := windows.WaitForSingleObject(child.Process, windows.INFINITE)
		var code uint32
		if err == nil {
			err = windows.GetExitCodeProcess(child.Process, &code)
		}
		done <- result{code, err}
	}()
	select {
	case res := <-done:
		return int(res.code), res.err
	case <-ctx.Done():
	}
	if err := stopPayloadConsole(child.ProcessId); err != nil {
		// The process can exit before AttachConsole. Terminate the job if
		// the payload has already lost its console and is still running.
		select {
		case res := <-done:
			return int(res.code), res.err
		default:
			_ = windows.TerminateJobObject(job, 1)
			<-done
			return 0, fmt.Errorf("signal the service payload to stop: %w", err)
		}
	}
	timer := time.NewTimer(payloadStopTimeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return int(res.code), res.err
	case <-timer.C:
		_ = windows.TerminateJobObject(job, 1)
		<-done
		return 0, fmt.Errorf("the service payload did not stop within %s", payloadStopTimeout)
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
	defer freePayloadConsole.Call()
	if ok, _, err := setConsoleCtrlHandler.Call(0, 1); ok == 0 {
		return err
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
}
