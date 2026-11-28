// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// The Windows guest image runs the guest helper as the auto-start
// LocalSystem service serviceName (see
// cmds/ownsand-agent/internal/imagebuild/windows_windows.go).
const serviceName = "ownsand-guest"

// serviceStopTimeout bounds the wait for the server after a stop request.
const serviceStopTimeout = 20 * time.Second

// Subcommands of the guest helper on Windows guests. The device agent runs
// them through the guest helper's own exec.
const (
	// subcommandFlushVolumes flushes the file system caches of every mounted fixed
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
	h := &serviceHandler{args: args, reportStartupError: writeStartupEvent}
	if err := svc.Run(serviceName, h); err != nil {
		return 1
	}
	return h.code
}

type serviceHandler struct {
	args               []string
	code               int
	reportStartupError func(string) error
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, h.args, h.openServiceLog) }()
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

// runFlushVolumes flushes mounted fixed volumes. Flushing a volume requires
// administrator rights. The guest helper service and its execs run as LocalSystem.
func runFlushVolumes() int {
	api := volumeAPI{
		findFirst:     windows.FindFirstVolume,
		findNext:      windows.FindNextVolume,
		findClose:     windows.FindVolumeClose,
		getMountPaths: windows.GetVolumePathNamesForVolumeName,
		getDriveType:  windows.GetDriveType,
		open:          windows.CreateFile,
		flush:         windows.FlushFileBuffers,
		close:         windows.CloseHandle,
	}
	return api.flushMountedFixedVolumes(os.Stderr)
}

type volumeAPI struct {
	findFirst     func(*uint16, uint32) (windows.Handle, error)
	findNext      func(windows.Handle, *uint16, uint32) error
	findClose     func(windows.Handle) error
	getMountPaths func(*uint16, *uint16, uint32, *uint32) error
	getDriveType  func(*uint16) uint32
	open          func(*uint16, uint32, uint32, *windows.SecurityAttributes, uint32, uint32, windows.Handle) (windows.Handle, error)
	flush         func(windows.Handle) error
	close         func(windows.Handle) error
}

func (api volumeAPI) flushMountedFixedVolumes(stderr io.Writer) int {
	var name [windows.MAX_PATH]uint16
	handle, err := api.findFirst(&name[0], uint32(len(name)))
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-guest: list volumes:", err)
		return 1
	}
	defer api.findClose(handle)

	code := 0
	for {
		guid := windows.UTF16ToString(name[:])
		fixed, err := api.hasFixedMount(&name[0])
		if err != nil {
			fmt.Fprintf(stderr, "ownsand-guest: list mount paths for %s: %v\n", guid, err)
			code = 1
		} else if fixed {
			if err := api.flushVolume(guid); err != nil {
				fmt.Fprintf(stderr, "ownsand-guest: flush %s: %v\n", guid, err)
				code = 1
			}
		}

		err = api.findNext(handle, &name[0], uint32(len(name)))
		if err == windows.ERROR_NO_MORE_FILES {
			return code
		}
		if err != nil {
			fmt.Fprintln(stderr, "ownsand-guest: list volumes:", err)
			return 1
		}
	}
}

func (api volumeAPI) hasFixedMount(volume *uint16) (bool, error) {
	paths := make([]uint16, windows.MAX_PATH)
	for {
		var length uint32
		err := api.getMountPaths(volume, &paths[0], uint32(len(paths)), &length)
		if err == windows.ERROR_MORE_DATA && length > uint32(len(paths)) {
			paths = make([]uint16, length)
			continue
		}
		if err != nil {
			return false, err
		}
		paths = paths[:length]
		break
	}
	// Windows returns consecutive NUL-terminated roots with a final empty
	// string. Pass each root as UTF-16 and keep its trailing backslash.
	for end := slices.Index(paths, 0); end > 0; end = slices.Index(paths, 0) {
		if api.getDriveType(&paths[0]) == windows.DRIVE_FIXED {
			return true, nil
		}
		paths = paths[end+1:]
	}
	return false, nil
}

func (api volumeAPI) flushVolume(guid string) error {
	// CreateFile opens the volume itself when its GUID path has no trailing backslash.
	p, err := windows.UTF16PtrFromString(strings.TrimSuffix(guid, "\\"))
	if err != nil {
		return err
	}
	h, err := api.open(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("open volume: %w", err)
	}
	defer api.close(h)
	if err := api.flush(h); err != nil {
		return fmt.Errorf("FlushFileBuffers: %w", err)
	}
	return nil
}
