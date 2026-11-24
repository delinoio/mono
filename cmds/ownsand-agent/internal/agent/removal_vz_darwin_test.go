// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package agent

import (
	"os"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Sign the test binary with cmds/ownsand-agent/entitlements.plist before
// running this test with an OwnSand Linux kernel and root file system.
func TestRemovalRetriesRetainedFailedCreateVirtualization(t *testing.T) {
	kernel, rootfs := os.Getenv("OWNSAND_VZ_TEST_KERNEL"), os.Getenv("OWNSAND_VZ_TEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("needs OWNSAND_VZ_TEST_KERNEL and OWNSAND_VZ_TEST_ROOTFS")
	}
	testRemovalRetriesRetainedFailedCreate(t, func(st *store.Store, o *Options) vmruntime.Runtime {
		rt, err := vzrt.New(vzrt.Options{Kernel: kernel, Rootfs: rootfs, VMsDir: st.Path(store.VMsDir), Logger: o.Logger})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}, func(cmd *ownsandv1.CreateSandboxCommand) {
		cmd.GuestOs = ownsandv1.GuestOs_GUEST_OS_LINUX
		cmd.Source = &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}
		cmd.Resources.MemoryMib = 512
		cmd.Resources.DiskGib = 2
	})
}
