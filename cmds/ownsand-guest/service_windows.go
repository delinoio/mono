// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// The Windows guest image runs the guest helper as the auto-start
// LocalSystem service serviceName (see
// cmds/ownsand-agent/internal/imagebuild/windows_windows.go).
const serviceName = "ownsand-guest"

// Log file of the service in the state directory. A service has no
// standard error, so the service logs to this file; at start a file larger
// than logRotateBytes moves to logFile + ".1".
const (
	logFile        = "guest.log"
	logRotateBytes = 16 << 20
)

// serviceStopTimeout bounds the wait for the server after a stop request.
const serviceStopTimeout = 20 * time.Second

// Subcommands of the guest helper on Windows guests. The device agent runs
// them through the guest helper's own exec.
const (
	// subcommandFlushVolumes flushes the file system caches of every fixed
	// volume to the disk (the Windows counterpart of `sync`).
	subcommandFlushVolumes = "flush-volumes"
)

func init() { platformMain = windowsMain }

func windowsMain(args []string) (int, bool) {
	if len(args) > 0 && args[0] == subcommandFlushVolumes {
		return runFlushVolumes(), true
	}
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return 0, false
	}
	return runService(args), true
}

// runService serves the GuestService as a Windows service until the
// Service Control Manager stops it. A server that ends by itself returns a
// service-specific exit code, so the restart-on-failure actions of the
// service apply.
func runService(args []string) int {
	h := &serviceHandler{args: args}
	if err := svc.Run(serviceName, h); err != nil {
		return 1
	}
	return h.code
}

type serviceHandler struct {
	args []string
	code int
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, h.args, openServiceLog) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case code := <-done:
			h.code = code
			if code != 0 {
				return true, uint32(code)
			}
			return false, 0
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case h.code = <-done:
				case <-time.After(serviceStopTimeout):
				}
				return false, 0
			}
		}
	}
}

// openServiceLog opens the service log in the state directory.
func openServiceLog(stateDir string) (io.Writer, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, logFile)
	if st, err := os.Stat(path); err == nil && st.Size() > logRotateBytes {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// runFlushVolumes flushes every fixed volume with FlushFileBuffers on the
// volume handle, which writes the cached data of all open files of the
// volume. It needs administrator rights; the guest helper service runs as
// LocalSystem, and so do its execs.
func runFlushVolumes() int {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ownsand-guest: list volumes:", err)
		return 1
	}
	failed := 0
	for i := range 26 {
		if mask&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rootp, err := windows.UTF16PtrFromString(root)
		if err != nil || windows.GetDriveType(rootp) != windows.DRIVE_FIXED {
			continue
		}
		if err := flushVolume(strings.TrimSuffix(root, `\`)); err != nil {
			fmt.Fprintf(os.Stderr, "ownsand-guest: flush %s: %v\n", root, err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func flushVolume(drive string) error {
	p, err := windows.UTF16PtrFromString(`\\.\` + drive)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.FlushFileBuffers(h)
}
