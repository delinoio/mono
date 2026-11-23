// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"os"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestLifecycleFirecracker runs the Go SDK lifecycle with the agent's
// Firecracker runtime and network NONE when the opt-in variables of the
// agent's Firecracker integration test name the Firecracker binary, the
// guest kernel and a root file system built with
// cmds/ownsand-agent/images/linux/build-rootfs.sh. It needs /dev/kvm
// access but no root.
func TestLifecycleFirecracker(t *testing.T) {
	requireE2E(t)
	binary, kernel, rootfs := os.Getenv("OWNSAND_FC_TEST_BINARY"), os.Getenv("OWNSAND_FC_TEST_KERNEL"), os.Getenv("OWNSAND_FC_TEST_ROOTFS")
	if binary == "" || kernel == "" || rootfs == "" {
		t.Skip("OWNSAND_FC_TEST_BINARY, OWNSAND_FC_TEST_KERNEL and OWNSAND_FC_TEST_ROOTFS are not set; skipping the Firecracker lifecycle")
	}
	s := newStack(t, stackOptions{agentEnv: []string{
		"OWNSAND_AGENT_RUNTIME=firecracker",
		"OWNSAND_AGENT_FIRECRACKER_BINARY=" + binary,
		"OWNSAND_AGENT_LINUX_KERNEL=" + kernel,
		"OWNSAND_AGENT_LINUX_ROOTFS=" + rootfs,
	}})
	for _, tr := range transports {
		// The restored sandbox has the vCPU count, memory, disk and network
		// mode of the snapshot, so the runtime must restore the memory state;
		// a cold boot of the snapshot disk fails the test.
		t.Run(tr.String(), func(t *testing.T) {
			sdkLifecycle(t, s, tr, ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE)
		})
	}
}
