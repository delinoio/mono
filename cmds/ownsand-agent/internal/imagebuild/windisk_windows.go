// SPDX-License-Identifier: Apache-2.0

package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"
)

// Partition layout of the Windows image disk (GPT): the EFI system
// partition, the Microsoft reserved partition and the Windows partition,
// last and without a recovery partition behind it, so a guest extends C:
// into a grown disk.
const (
	efiPartitionMiB = 260
	msrPartitionMiB = 16
)

// attachedDisk is the new image VHDX attached to the host with two drive
// letters: System (the EFI system partition) and Windows.
type attachedDisk struct {
	handle  syscall.Handle
	number  int
	System  string // for example "S:"
	Windows string // for example "W:"
}

// createVHDX creates a dynamic VHDX of size bytes.
func createVHDX(path string, size int64) error {
	h, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone,
		&vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{MaximumSize: uint64(size)}})
	if err != nil {
		return fmt.Errorf("imagebuild: create %s: %w", path, err)
	}
	return syscall.CloseHandle(h)
}

// attachVHDX attaches the VHDX to the host without drive letters. The
// attachment ends when the handle closes, so a crashed build leaves no
// attached disk.
func attachVHDX(path string) (*attachedDisk, error) {
	h, err := vhd.OpenVirtualDiskWithParameters(path, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone,
		&vhd.OpenVirtualDiskParameters{Version: 2})
	if err != nil {
		return nil, fmt.Errorf("imagebuild: open %s: %w", path, err)
	}
	if err := vhd.AttachVirtualDisk(h, vhd.AttachVirtualDiskFlagNoDriveLetter, &vhd.AttachVirtualDiskParameters{Version: 2}); err != nil {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("imagebuild: attach %s: %w", path, err)
	}
	phys, err := vhd.GetVirtualDiskPhysicalPath(h)
	if err == nil {
		d := &attachedDisk{handle: h}
		num, ok := strings.CutPrefix(phys, `\\.\PhysicalDrive`)
		if d.number, err = strconv.Atoi(num); ok && err == nil {
			return d, nil
		}
		err = fmt.Errorf("unexpected physical path %q", phys)
	}
	_ = vhd.DetachVirtualDisk(h)
	syscall.CloseHandle(h)
	return nil, fmt.Errorf("imagebuild: find the attached disk: %w", err)
}

// detach detaches the disk and closes its handle.
func (d *attachedDisk) detach() error {
	if d.handle == 0 {
		return nil
	}
	err := vhd.DetachVirtualDisk(d.handle)
	syscall.CloseHandle(d.handle)
	d.handle = 0
	return err
}

// freeDriveLetters returns n unused drive letters, from Z downwards.
func freeDriveLetters(n int) ([]string, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var out []string
	for c := 'Z'; c >= 'D' && len(out) < n; c-- {
		if mask&(1<<(c-'A')) == 0 {
			out = append(out, string(c)+":")
		}
	}
	if len(out) < n {
		return nil, errors.New("imagebuild: no free drive letters for the image partitions")
	}
	return out, nil
}

// diskpartScript returns the script that partitions disk number as GPT
// and assigns the drive letters (each like "S", without the colon).
func diskpartScript(number int, system, win string) string {
	return strings.Join([]string{
		fmt.Sprintf("select disk %d", number),
		"clean",
		"convert gpt",
		fmt.Sprintf("create partition efi size=%d", efiPartitionMiB),
		`format quick fs=fat32 label="System"`,
		"assign letter=" + system,
		fmt.Sprintf("create partition msr size=%d", msrPartitionMiB),
		"create partition primary",
		`format quick fs=ntfs label="Windows"`,
		"assign letter=" + win,
		"exit",
	}, "\r\n") + "\r\n"
}

// partition partitions the attached disk and mounts its two volumes.
func (d *attachedDisk) partition(ctx context.Context, scratch string) error {
	letters, err := freeDriveLetters(2)
	if err != nil {
		return err
	}
	d.System, d.Windows = letters[0], letters[1]
	script := filepath.Join(scratch, "diskpart.txt")
	if err := os.WriteFile(script, []byte(diskpartScript(d.number, d.System[:1], d.Windows[:1])), 0o600); err != nil {
		return err
	}
	defer os.Remove(script)
	if out, err := runTool(ctx, "diskpart.exe", "/s", script); err != nil {
		return fmt.Errorf("imagebuild: partition the image disk: %w: %s", err, out)
	}
	return nil
}

// runTool runs a Windows tool and returns the end of its output.
func runTool(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", name), args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return tail(out.String(), 4096), err
}
