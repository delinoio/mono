// SPDX-License-Identifier: Apache-2.0

// Package images owns the layout of the locally built base images in the
// state directory:
//
//	images/linux/      vmlinux and rootfs.ext4 (see cmds/ownsand-agent/images/linux)
//	images/macos/      disk.img, aux.img, hardware-model and image.json
//	images/windows/    disk.vhdx and image.json
//
// `ownsand-agent image build` writes a macOS or Windows image into a build
// directory next to its final directory and renames it into place only after
// the image booted and its guest helper answered, so a runtime never sees a
// partial image.
package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Directory names below images/ in the state directory.
const (
	LinuxDir   = "linux"
	MacOSDir   = "macos"
	WindowsDir = "windows"
)

// File names of the macOS base image. A macOS snapshot holds the same
// files (with the disk and the auxiliary storage of the sandbox) plus the
// machine identifier and, for a memory-state snapshot, vm.state.
const (
	MacOSDisk          = "disk.img"
	MacOSAuxStorage    = "aux.img"
	MacOSHardwareModel = "hardware-model"
	MacOSMachineID     = "machine-id"
)

// WindowsDisk is the file name of the Windows base image disk: a dynamic
// VHDX with a GPT layout (EFI system partition, MSR and the Windows
// partition last, so the guest can extend it).
const WindowsDisk = "disk.vhdx"

// MetadataFile describes a built image.
const MetadataFile = "image.json"

// Metadata is the content of image.json. It holds no secrets.
type Metadata struct {
	GuestOS ownsandv1.GuestOs `json:"guest_os"`
	// Source names the IPSW build version or the Windows image name and
	// edition; it never holds a path of the operator.
	Source    string    `json:"source"`
	BuiltAt   time.Time `json:"built_at"`
	DiskBytes int64     `json:"disk_bytes"`
	// MinimumCPUCount and MinimumMemoryMiB are the smallest resources that
	// the guest needs (for macOS, the restore image requirements).
	MinimumCPUCount  int32 `json:"minimum_cpu_count"`
	MinimumMemoryMiB int64 `json:"minimum_memory_mib"`
	// GuestVersion is the guest helper version installed in the image.
	GuestVersion string `json:"guest_version,omitempty"`
}

// Dir returns the image directory of a guest OS below imagesDir.
func Dir(imagesDir string, os ownsandv1.GuestOs) (string, error) {
	switch os {
	case ownsandv1.GuestOs_GUEST_OS_LINUX:
		return filepath.Join(imagesDir, LinuxDir), nil
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		return filepath.Join(imagesDir, MacOSDir), nil
	case ownsandv1.GuestOs_GUEST_OS_WINDOWS:
		return filepath.Join(imagesDir, WindowsDir), nil
	}
	return "", fmt.Errorf("images: no image directory for guest OS %v", os)
}

// ReadMetadata reads image.json of an image directory.
func ReadMetadata(dir string) (Metadata, error) {
	var m Metadata
	b, err := os.ReadFile(filepath.Join(dir, MetadataFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("images: parse %s: %w", MetadataFile, err)
	}
	return m, nil
}

// WriteMetadata writes image.json into an image directory.
func WriteMetadata(dir string, m Metadata) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MetadataFile), append(b, '\n'), 0o600)
}

// BuildDir returns the build directory of an image directory.
func BuildDir(dir string) string { return dir + ".build" }

// Install replaces the image directory dir with the finished build
// directory. The old image moves aside first and is removed after the
// rename, so dir always holds either the old or the new complete image.
func Install(dir string) error {
	build := BuildDir(dir)
	if _, err := os.Stat(filepath.Join(build, MetadataFile)); err != nil {
		return fmt.Errorf("images: the build directory has no %s: %w", MetadataFile, err)
	}
	old := dir + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(build, dir); err != nil {
		// Put the old image back so a failed install keeps it.
		_ = os.Rename(old, dir)
		return err
	}
	return os.RemoveAll(old)
}
