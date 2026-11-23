// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// The opt-in variables of the agent's Virtualization.framework integration
// test: the guest kernel and the root file system of the host architecture
// (from cmds/ownsand-agent/images/linux/build-image.sh).
const (
	envVZKernel = "OWNSAND_VZ_TEST_KERNEL"
	envVZRootfs = "OWNSAND_VZ_TEST_ROOTFS"
)

// virtualizationAgentEnv signs the built agent ad hoc with the
// virtualization entitlement, which needs no developer identity, and
// returns the agent environment of the Virtualization.framework runtime
// with the base image.
func virtualizationAgentEnv(t *testing.T, kernel, rootfs string) []string {
	t.Helper()
	bins := buildBinaries(t)
	entitlements := filepath.Join(moduleRoot(t), "cmds", "ownsand-agent", "entitlements.plist")
	if out, err := exec.Command("codesign", "--sign", "-", "--force", "--entitlements", entitlements,
		filepath.Join(bins, "ownsand-agent")).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v\n%s", err, out)
	}
	return []string{
		"OWNSAND_AGENT_RUNTIME=virtualization",
		"OWNSAND_AGENT_LINUX_KERNEL=" + kernel,
		"OWNSAND_AGENT_LINUX_ROOTFS=" + rootfs,
	}
}

// TestLifecycleVirtualization runs the Go SDK lifecycle with the agent's
// Virtualization.framework runtime on a macOS host when the opt-in
// variables of the agent's integration test name the base image.
func TestLifecycleVirtualization(t *testing.T) {
	requireE2E(t)
	kernel, rootfs := os.Getenv(envVZKernel), os.Getenv(envVZRootfs)
	if runtime.GOOS != "darwin" || kernel == "" || rootfs == "" {
		t.Skip("needs a macOS host and OWNSAND_VZ_TEST_KERNEL and OWNSAND_VZ_TEST_ROOTFS; skipping the Virtualization.framework lifecycle")
	}
	s := newStack(t, stackOptions{agentEnv: virtualizationAgentEnv(t, kernel, rootfs)})
	for _, tr := range transports {
		// Memory-state restore depends on the host architecture (it needs
		// Apple silicon), so the test accepts either restore path of a
		// snapshot and rejects only BASE_IMAGE, which means that the sandbox
		// ignored the snapshot.
		t.Run(tr.String(), func(t *testing.T) {
			sdkLifecycle(t, s, tr,
				ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE,
				ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT)
		})
	}
}
