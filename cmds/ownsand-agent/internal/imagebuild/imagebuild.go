// SPDX-License-Identifier: FSL-1.1-ALv2

// Package imagebuild builds the macOS and Windows base images on the device
// (`ownsand-agent image build`). Delino distributes no macOS or Windows
// image: a Mac installs macOS from an Apple restore image (IPSW), and a
// Windows host applies the Windows image of an ISO file that an admin
// supplies. Each build installs the guest helper, boots the image once
// until the guest helper answers, shuts the guest down and then installs
// the image directory (see package images).
//
// See docs/cmds-ownsand-agent-foundation.md#images.
package imagebuild

import (
	"context"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ErrUnsupported reports an image build that this host cannot run.
var ErrUnsupported = errors.New("imagebuild: not supported on this host")

// Options configure a build.
type Options struct {
	// ImagesDir is images/ in the state directory.
	ImagesDir string
	GuestOS   ownsandv1.GuestOs
	// IPSW is the macOS restore image; empty downloads the latest restore
	// image that this Mac supports.
	IPSW string
	// ISO is the Windows installation ISO file (required for Windows).
	ISO string
	// Edition selects the Windows image in the ISO by its index or name;
	// empty selects the first image.
	Edition string
	// DiskGiB is the disk size of the image; zero selects the default of
	// the guest OS.
	DiskGiB int64
	// CPUCount and MemoryMiB are the resources of the build VM; zero
	// selects the default of the guest OS.
	CPUCount  int32
	MemoryMiB int64
	// GuestBinary is the guest helper executable for the guest OS; empty
	// finds it next to the agent executable, then on PATH.
	GuestBinary string
	Logger      *slog.Logger
}

// DefaultGuestOS returns the guest OS whose image this host builds: macOS on
// macOS hosts and Windows on Windows hosts.
func DefaultGuestOS() ownsandv1.GuestOs {
	switch runtime.GOOS {
	case "darwin":
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	case "windows":
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS
	}
	return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
}

// ParseGuestOS parses the --os flag value.
func ParseGuestOS(s string) (ownsandv1.GuestOs, error) {
	switch s {
	case "":
		if os := DefaultGuestOS(); os != ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED {
			return os, nil
		}
		return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED, fmt.Errorf("%w: this host builds no local base image; Linux base images come from cmds/ownsand-agent/images/linux", ErrUnsupported)
	case "macos":
		return ownsandv1.GuestOs_GUEST_OS_MACOS, nil
	case "windows":
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS, nil
	case "linux":
		return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED, fmt.Errorf("%w: Linux base images are not built by the agent; use cmds/ownsand-agent/images/linux/build-image.sh", ErrUnsupported)
	}
	return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED, fmt.Errorf("imagebuild: unknown guest OS %q (want macos or windows)", s)
}

// Build builds the image of opts.GuestOS and installs it.
func Build(ctx context.Context, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ImagesDir == "" {
		return errors.New("imagebuild: the images directory is required")
	}
	if err := ValidateResources(opts.DiskGiB, int64(opts.CPUCount), opts.MemoryMiB); err != nil {
		return err
	}
	dir, err := images.Dir(opts.ImagesDir, opts.GuestOS)
	if err != nil {
		return err
	}
	switch opts.GuestOS {
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		guest, err := guestBinary(opts.GuestBinary, "ownsand-guest", checkDarwinARM64)
		if err != nil {
			return err
		}
		opts.GuestBinary = guest
		return buildMacOS(ctx, dir, opts)
	case ownsandv1.GuestOs_GUEST_OS_WINDOWS:
		if opts.ISO == "" {
			return errors.New("imagebuild: a Windows image build needs --iso <path to a Windows ISO>")
		}
		guest, err := guestBinary(opts.GuestBinary, "ownsand-guest.exe", checkWindowsAMD64)
		if err != nil {
			return err
		}
		opts.GuestBinary = guest
		return buildWindows(ctx, dir, opts)
	}
	return fmt.Errorf("%w: guest OS %v", ErrUnsupported, opts.GuestOS)
}

// guestBinary finds the guest helper for the guest OS and checks that it is
// an executable of the guest platform.
func guestBinary(path, name string, check func(string) error) (string, error) {
	if path == "" {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), name)
			if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
				path = candidate
			}
		}
	}
	if path == "" {
		if p, err := exec.LookPath(name); err == nil {
			path = p
		}
	}
	if path == "" {
		return "", fmt.Errorf("imagebuild: the guest helper %s was not found next to the agent or on PATH; pass --guest-binary", name)
	}
	if err := check(path); err != nil {
		return "", err
	}
	return path, nil
}

func checkDarwinARM64(path string) error {
	f, err := macho.Open(path)
	if err != nil {
		return fmt.Errorf("imagebuild: the guest helper %s is not a macOS executable: %w", path, err)
	}
	defer f.Close()
	if f.Cpu != macho.CpuArm64 {
		return fmt.Errorf("imagebuild: the guest helper %s is built for %v; macOS guests need arm64 (GOOS=darwin GOARCH=arm64)", path, f.Cpu)
	}
	return nil
}

func checkWindowsAMD64(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("imagebuild: the guest helper %s is not a Windows executable: %w", path, err)
	}
	defer f.Close()
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return fmt.Errorf("imagebuild: the guest helper %s is not an amd64 executable; Windows guests need GOOS=windows GOARCH=amd64", path)
	}
	return nil
}
