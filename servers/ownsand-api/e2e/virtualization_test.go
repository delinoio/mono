// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestLifecycleVirtualization runs the Go SDK lifecycle with the agent's
// Virtualization.framework runtime on a macOS host when the opt-in
// variables of the agent's integration test name the guest kernel and the
// root file system of the host architecture (from
// cmds/ownsand-agent/images/linux/build-image.sh). It signs the agent
// binary ad hoc with the virtualization entitlement, which needs no
// developer identity.
func TestLifecycleVirtualization(t *testing.T) {
	requireE2E(t)
	kernel, rootfs := os.Getenv("OWNSAND_VZ_TEST_KERNEL"), os.Getenv("OWNSAND_VZ_TEST_ROOTFS")
	if runtime.GOOS != "darwin" || kernel == "" || rootfs == "" {
		t.Skip("needs a macOS host and OWNSAND_VZ_TEST_KERNEL and OWNSAND_VZ_TEST_ROOTFS; skipping the Virtualization.framework lifecycle")
	}
	bins := buildBinaries(t)
	entitlements := filepath.Join(moduleRoot(t), "cmds", "ownsand-agent", "entitlements.plist")
	if out, err := exec.Command("codesign", "--sign", "-", "--force", "--entitlements", entitlements,
		filepath.Join(bins, "ownsand-agent")).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v\n%s", err, out)
	}
	s := newStack(t, stackOptions{agentEnv: []string{
		"OWNSAND_AGENT_RUNTIME=virtualization",
		"OWNSAND_AGENT_LINUX_KERNEL=" + kernel,
		"OWNSAND_AGENT_LINUX_ROOTFS=" + rootfs,
	}})
	for _, tr := range transports {
		t.Run(tr.String(), func(t *testing.T) { sdkLifecycle(t, s, tr) })
	}
}
