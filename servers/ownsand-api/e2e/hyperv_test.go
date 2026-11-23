// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestLifecycleHyperV runs the Go SDK and CLI lifecycles with the agent's
// Hyper-V runtime on a Windows host when the opt-in variables of the
// agent's Hyper-V integration test name the amd64 guest kernel and root
// file system (from cmds/ownsand-agent/images/linux/build-image.sh). The
// test process must run elevated, because the agent calls the Host Compute
// Service. It also checks the reconnect after a control plane restart, the
// reconciliation after an agent crash and the clean-up of a device removal:
// no compute system is left, and the removal leaves the state directory
// empty.
func TestLifecycleHyperV(t *testing.T) {
	requireE2E(t)
	kernel, rootfs := os.Getenv("OWNSAND_HV_TEST_KERNEL"), os.Getenv("OWNSAND_HV_TEST_ROOTFS")
	if runtime.GOOS != "windows" || kernel == "" || rootfs == "" {
		t.Skip("needs an elevated Windows host with Hyper-V and OWNSAND_HV_TEST_KERNEL and OWNSAND_HV_TEST_ROOTFS; skipping the Hyper-V lifecycle")
	}
	s := newStack(t, stackOptions{agentEnv: []string{
		"OWNSAND_AGENT_RUNTIME=hyperv",
		"OWNSAND_AGENT_LINUX_KERNEL=" + kernel,
		"OWNSAND_AGENT_LINUX_ROOTFS=" + rootfs,
	}})
	for _, tr := range transports {
		t.Run("sdk-"+tr.String(), func(t *testing.T) { sdkLifecycle(t, s, tr) })
		t.Run("cli-"+tr.String(), func(t *testing.T) { cliLifecycle(t, s, tr) })
	}
	t.Run("control-plane-restart", func(t *testing.T) { hyperVControlPlaneRestart(t, s) })
	t.Run("agent-crash", func(t *testing.T) { hyperVAgentCrash(t, s) })
	t.Run("device-removal", func(t *testing.T) { hyperVDeviceRemoval(t, s) })
}

// createHyperVSandbox creates a running sandbox and writes a marker file.
func createHyperVSandbox(ctx context.Context, t *testing.T, c *ownsand.Client) *ownsand.Sandbox {
	t.Helper()
	sb, err := c.CreateSandbox(ctx, createParams())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { sb.Close() })
	if info, err := sb.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable = %v, %v", info, err)
	}
	if _, _, res := execCollect(ctx, t, sb, "/bin/sh", "-c", "echo marker > /root/marker"); exitCode(res) != 0 {
		t.Fatalf("write marker exit %d", exitCode(res))
	}
	return sb
}

// waitSandboxState polls the sandbox until cond holds for its state.
func waitSandboxState(ctx context.Context, t *testing.T, sb *ownsand.Sandbox, what string, cond func(*ownsandv1.Sandbox) bool) *ownsandv1.Sandbox {
	t.Helper()
	var last *ownsandv1.Sandbox
	for {
		info, err := sb.Get(ctx)
		if err == nil {
			last = info
			if cond(info) {
				return info
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("sandbox %s is not %s: last %v, error %v", sb.ID(), what, last, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// hcsSystems returns the output of hcsdiag list, which names every compute
// system of the host.
func hcsSystems(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("hcsdiag", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("hcsdiag list: %v\n%s", err, out)
	}
	return string(out)
}

// requireNoComputeSystem fails when a compute system of the sandbox exists.
func requireNoComputeSystem(t *testing.T, sandboxID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := hcsSystems(t)
		if !strings.Contains(strings.ToLower(out), strings.ToLower(sandboxID)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a compute system of sandbox %s is left:\n%s", sandboxID, out)
		}
		time.Sleep(time.Second)
	}
}

// hyperVControlPlaneRestart restarts the control plane while a sandbox
// runs: the agent reconnects, and the same VM serves exec again.
func hyperVControlPlaneRestart(t *testing.T, s *stack) {
	ctx := s.ctx(5 * time.Minute)
	c := s.apiKeyClient(ownsand.TransportRelay)
	sb := createHyperVSandbox(ctx, t, c)
	s.killAPI()
	s.restartAPI()
	// After a crash the device stays ONLINE in the database until the
	// offline sweep of the new process, so a RUNNING sandbox does not show
	// that the agent reconnected: wait until the new process reconciled the
	// agent's connection, which it logs as agent_state_report.
	for !s.logs.hasStateReport(s.api.name, s.deviceID) {
		select {
		case <-ctx.Done():
			t.Fatalf("the agent did not reconnect to the restarted control plane")
		case <-time.After(200 * time.Millisecond):
		}
	}
	waitSandboxState(ctx, t, sb, "RUNNING after the reconnect", func(info *ownsandv1.Sandbox) bool {
		return info.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	})
	c = s.apiKeyClient(ownsand.TransportRelay)
	again, err := c.Sandbox(s.orgID, sb.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	out, _, res := execCollect(ctx, t, again, "/bin/cat", "/root/marker")
	if out != "marker\n" || exitCode(res) != 0 {
		t.Fatalf("exec after the reconnect = %q exit %d", out, exitCode(res))
	}
	if _, err := again.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	requireNoComputeSystem(t, sb.ID())
}

// hyperVAgentCrash kills the agent while a sandbox runs: the sandbox shows
// as UNREACHABLE, and the restarted agent stops the VM of the earlier
// process and reports the sandbox as FAILED with LOST_ON_DEVICE.
func hyperVAgentCrash(t *testing.T, s *stack) {
	ctx := s.ctx(5 * time.Minute)
	c := s.apiKeyClient(ownsand.TransportRelay)
	sb := createHyperVSandbox(ctx, t, c)
	s.killAgent(0)
	// The compute system is bound to the agent process
	// (ShouldTerminateOnLastHandleClosed), so the VM ends with the agent
	// even before a restarted agent stops leftover VMs.
	requireNoComputeSystem(t, sb.ID())
	waitSandboxState(ctx, t, sb, "UNREACHABLE", func(info *ownsandv1.Sandbox) bool {
		return info.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE
	})
	s.restartAgent(0)
	// The reconciliation may report the end reason a moment after the state,
	// so the wait covers both and a different end reason fails at the
	// deadline with the last observed sandbox in the message.
	info := waitSandboxState(ctx, t, sb, "FAILED with LOST_ON_DEVICE", func(info *ownsandv1.Sandbox) bool {
		return info.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_FAILED &&
			info.GetEndReason() == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
	})
	t.Logf("sandbox after the agent restart: %v", info)
	if got := info.GetEndReason(); got != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("end reason of the sandbox after the agent restart = %v, want %v", got, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE)
	}
	requireNoComputeSystem(t, sb.ID())
}

// hyperVDeviceRemoval removes the device while a sandbox runs: the agent
// deletes every VM and its local data, confirms the removal and exits.
func hyperVDeviceRemoval(t *testing.T, s *stack) {
	ctx := s.ctx(5 * time.Minute)
	c := s.apiKeyClient(ownsand.TransportRelay)
	sb := createHyperVSandbox(ctx, t, c)
	user := s.userClient(ownsand.TransportAuto)
	if _, err := user.Devices().Remove(ctx, &ownsandv1.RemoveDeviceRequest{OrganizationId: s.orgID, DeviceId: s.deviceID}); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	select {
	case <-s.agents[0].proc.done:
	case <-ctx.Done():
		t.Fatal("the agent did not exit after the device removal")
	}
	if code := s.agents[0].proc.cmd.ProcessState.ExitCode(); code != 3 {
		t.Fatalf("agent exit code after the removal = %d, want 3", code)
	}
	requireNoComputeSystem(t, sb.ID())
	// A finished removal leaves the state directory empty: no VM disk,
	// snapshot, image, record or device key.
	left, err := remainingStatePaths(s.agents[0].stateDir)
	if err != nil {
		t.Fatalf("inspect the state directory %s: %v", s.agents[0].stateDir, err)
	}
	if len(left) != 0 {
		t.Fatalf("the state directory is not empty after the removal: %v", left)
	}
}

// remainingStatePaths returns every path below root, without root itself. A
// lookup or read error of root or of any entry is returned, so that an
// unreadable or missing directory is never mistaken for an empty one.
func remainingStatePaths(root string) ([]string, error) {
	var left []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			left = append(left, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return left, nil
}
