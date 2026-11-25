// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

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

	driveLetterMutexName   = `Global\Delino.OwnSand.ImageBuild.DriveLetters`
	driveLetterMutexWaitMS = 50
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

// withDriveLetterMutex serializes drive-letter selection and assignment
// across builder processes, including those in different Windows sessions.
func withDriveLetterMutex(ctx context.Context, name string, partition func() error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	mutexName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	// Grant SYNCHRONIZE and MUTEX_MODIFY_STATE to SYSTEM, administrators,
	// and the object owner so elevated builders can share the mutex.
	descriptor, err := windows.SecurityDescriptorFromString("D:(A;;0x00100001;;;SY)(A;;0x00100001;;;BA)(A;;0x00100001;;;OW)")
	if err != nil {
		return fmt.Errorf("imagebuild: create the drive-letter mutex security descriptor: %w", err)
	}
	attrs := windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	mutex, err := windows.CreateMutexEx(&attrs, mutexName, 0, windows.SYNCHRONIZE|windows.MUTEX_MODIFY_STATE)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("imagebuild: open the drive-letter mutex: %w", err)
	}
	defer windows.CloseHandle(mutex)

	// Windows assigns mutex ownership to the thread that waited on it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for ctx.Err() == nil {
		result, waitErr := windows.WaitForSingleObject(mutex, driveLetterMutexWaitMS)
		if waitErr != nil {
			return fmt.Errorf("imagebuild: wait for the drive-letter mutex: %w", waitErr)
		}
		switch result {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			// Partitioning reads a fresh mask even if the previous owner exited.
			defer func() {
				if releaseErr := windows.ReleaseMutex(mutex); releaseErr != nil {
					err = errors.Join(err, fmt.Errorf("imagebuild: release the drive-letter mutex: %w", releaseErr))
				}
			}()
			if err := ctx.Err(); err != nil {
				return err
			}
			return partition()
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			return fmt.Errorf("imagebuild: unexpected drive-letter mutex wait result %#x", result)
		}
	}
	return ctx.Err()
}

// freeDriveLetters returns n unused drive letters, from Z downwards.
func freeDriveLetters(mask uint32, n int) ([]string, error) {
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
	return d.partitionWith(ctx, scratch, driveLetterMutexName, windows.GetLogicalDrives, runTool)
}

func (d *attachedDisk) partitionWith(ctx context.Context, scratch, mutexName string, readMask func() (uint32, error), run func(context.Context, string, ...string) (string, error)) error {
	return withDriveLetterMutex(ctx, mutexName, func() error {
		mask, err := readMask()
		if err != nil {
			return err
		}
		letters, err := freeDriveLetters(mask, 2)
		if err != nil {
			return err
		}
		d.System, d.Windows = letters[0], letters[1]
		script := filepath.Join(scratch, "diskpart.txt")
		if err := os.WriteFile(script, []byte(diskpartScript(d.number, d.System[:1], d.Windows[:1])), 0o600); err != nil {
			return err
		}
		defer os.Remove(script)
		if out, err := run(ctx, "diskpart.exe", "/s", script); err != nil {
			return fmt.Errorf("imagebuild: partition the image disk: %w: %s", err, out)
		}
		return nil
	})
}

// runTool runs a Windows tool and returns the end of its output.
func runTool(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", name), args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return tail(out.String(), 4096), err
}
