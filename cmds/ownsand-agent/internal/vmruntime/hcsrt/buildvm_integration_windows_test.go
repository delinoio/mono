// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Microsoft/go-winio/vhd"

	"github.com/delinoio/mono/go/uuidv7"
)

// This test uses real HCS with disposable blank disks. Run it elevated on a
// Hyper-V host with OWNSAND_HV_TEST_BUILD_VM=1.
func TestBuildVMDuplicateIDKeepsExistingVM(t *testing.T) {
	if os.Getenv("OWNSAND_HV_TEST_BUILD_VM") != "1" {
		t.Skip("set OWNSAND_HV_TEST_BUILD_VM=1 on an elevated Hyper-V host")
	}
	if err := CheckHost(); err != nil {
		t.Fatal(err)
	}

	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	opts := BuildVMOptions{ID: "ownsand-image-build-windows-" + id, VCPUs: 1, MemoryMiB: 512}
	newDisk := func() string {
		path := filepath.Join(t.TempDir(), "disk.vhdx")
		handle, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone,
			&vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{MaximumSize: 1 << 30}})
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.CloseHandle(handle); err != nil {
			t.Fatal(err)
		}
		return path
	}
	opts.Disk = newDisk()
	opts.Dir = filepath.Dir(opts.Disk)
	other := opts
	other.Disk = newDisk()
	other.Dir = filepath.Dir(other.Disk)

	first, err := StartBuildVM(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Pause at UEFI so the original VM cannot exit on its own during the test.
	if err := first.sys.pause(); err != nil {
		t.Fatalf("pause original VM: %v", err)
	}

	second, err := StartBuildVM(other)
	if second != nil {
		defer second.Close()
		t.Error("startup returned a VM for the duplicate ID")
	}
	var createErr *hcsError
	const (
		hrSystemAlreadyExists    = 0x8037010f
		hrVmcomputeAlreadyExists = 0xc037010f
	)
	if !errors.As(err, &createErr) || createErr.op != "HcsCreateComputeSystem" ||
		(createErr.hr != hrSystemAlreadyExists && createErr.hr != hrVmcomputeAlreadyExists) {
		t.Errorf("start with duplicate ID: %v, want HCS create collision", err)
	}
	if err := first.sys.resume(); err != nil {
		t.Fatalf("resume original VM after the create error: %v", err)
	}
	select {
	case <-first.Exited():
		t.Fatal("original VM exited after the create error")
	default:
	}
}
