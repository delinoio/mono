// SPDX-License-Identifier: Apache-2.0

package hcsrt

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"
)

// The Windows guest disk is a dynamic VHDX. go-winio binds creation,
// opening and attachment of virtual disks; the size query and the resize
// of virtdisk.dll are bound here.
var (
	modVirtDisk                   = windows.NewLazySystemDLL("virtdisk.dll")
	procGetVirtualDiskInformation = modVirtDisk.NewProc("GetVirtualDiskInformation")
	procResizeVirtualDisk         = modVirtDisk.NewProc("ResizeVirtualDisk")
)

// getVirtualDiskInfoSize is GET_VIRTUAL_DISK_INFO_SIZE.
const getVirtualDiskInfoSize = 1

// virtualDiskSizeInfo is GET_VIRTUAL_DISK_INFO with its Size member.
type virtualDiskSizeInfo struct {
	Version      uint32
	_            uint32
	VirtualSize  uint64
	PhysicalSize uint64
	BlockSize    uint32
	SectorSize   uint32
}

// resizeVirtualDiskParameters is RESIZE_VIRTUAL_DISK_PARAMETERS version 1.
type resizeVirtualDiskParameters struct {
	Version uint32
	_       uint32
	NewSize uint64
}

// openVHDX opens a virtual disk for metadata operations. Resize needs a
// handle that OpenVirtualDisk version 2 opened with no access mask.
func openVHDX(path string) (syscall.Handle, error) {
	h, err := vhd.OpenVirtualDiskWithParameters(path, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone,
		&vhd.OpenVirtualDiskParameters{Version: 2})
	if err != nil {
		return 0, fmt.Errorf("hcsrt: open %s: %w", path, err)
	}
	return h, nil
}

// vhdxSize returns the virtual size of an open virtual disk.
func vhdxSize(h syscall.Handle) (uint64, error) {
	info := virtualDiskSizeInfo{Version: getVirtualDiskInfoSize}
	size := uint32(unsafe.Sizeof(info))
	r, _, _ := procGetVirtualDiskInformation.Call(uintptr(h), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&info)), 0)
	if r != 0 {
		return 0, fmt.Errorf("hcsrt: GetVirtualDiskInformation: %w", syscall.Errno(r))
	}
	return info.VirtualSize, nil
}

// growVHDX grows the virtual disk at path to at least size bytes (rounded
// up to 1 MiB) and returns its virtual size. A smaller size keeps the disk:
// a VHDX cannot shrink below its partitions, and the guest grows its
// Windows partition into the new space at boot.
func growVHDX(path string, size int64) (int64, error) {
	h, err := openVHDX(path)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	cur, err := vhdxSize(h)
	if err != nil {
		return 0, err
	}
	want := (uint64(size) + vhdAlign - 1) / vhdAlign * vhdAlign
	if want <= cur {
		return int64(cur), nil
	}
	params := resizeVirtualDiskParameters{Version: 1, NewSize: want}
	// RESIZE_VIRTUAL_DISK_FLAG_NONE, synchronous (no OVERLAPPED).
	r, _, _ := procResizeVirtualDisk.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&params)), 0)
	if r != 0 {
		return 0, fmt.Errorf("hcsrt: resize %s to %d bytes: %w", path, want, syscall.Errno(r))
	}
	return int64(want), nil
}

// VHDXSize returns the virtual size of the virtual disk at path.
func VHDXSize(path string) (int64, error) {
	h, err := openVHDX(path)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	n, err := vhdxSize(h)
	return int64(n), err
}
