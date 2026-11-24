// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
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
		// The Hyper-V runtime has no memory-state restore: it boots the
		// snapshot disk cold.
		t.Run("sdk-"+tr.String(), func(t *testing.T) {
			sdkLifecycle(t, s, tr, ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT)
		})
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

func hcsListOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	// Bound output collection if an inherited pipe stays open after the command exits.
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.CombinedOutput()
}

// requireNoComputeSystem checks that hcsdiag no longer lists the sandbox.
func requireNoComputeSystem(t *testing.T, sandboxID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := waitForNoComputeSystem(ctx, sandboxID, func(ctx context.Context) ([]byte, error) {
		return hcsListOutput(ctx, "hcsdiag", "list")
	}); err != nil {
		t.Fatal(err)
	}
}

func waitForNoComputeSystem(ctx context.Context, sandboxID string, list func(context.Context) ([]byte, error)) error {
	matchID := strings.ToLower(sandboxID)
	var lastOutput []byte
	for ctx.Err() == nil {
		out, err := list(ctx)
		if len(out) > 0 {
			lastOutput = out
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			return fmt.Errorf("hcsdiag list for sandbox %s: %w\n%s", sandboxID, err, out)
		}
		if !strings.Contains(strings.ToLower(string(out)), matchID) {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("wait for hcsdiag list to omit sandbox %s: %w\n%s", sandboxID, ctx.Err(), lastOutput)
}

func TestRequireNoComputeSystemCancelsCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	t.Setenv(envHelperMode, string(helperBlock))
	bin, args := helperArgs()
	done := make(chan error, 1)
	go func() {
		done <- waitForNoComputeSystem(ctx, "sandbox-id", func(ctx context.Context) ([]byte, error) {
			return hcsListOutput(ctx, bin, args...)
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "sandbox-id") ||
			!strings.Contains(err.Error(), "out-final") ||
			!strings.Contains(err.Error(), "err-final") {
			t.Fatalf("waitForNoComputeSystem = %v, want deadline, sandbox ID, and both output streams", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked helper process did not return after the deadline")
	}
}

func TestRequireNoComputeSystemStopsPollingAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 1500 * time.Millisecond
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		const output = "running SANDBOX-ID"
		polls := 0
		err := waitForNoComputeSystem(ctx, "sandbox-id", func(ctx context.Context) ([]byte, error) {
			polls++
			if ctx.Err() != nil {
				t.Error("poll started after the deadline")
			}
			return []byte(output), nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), output) {
			t.Fatalf("waitForNoComputeSystem = %v, want deadline and %q", err, output)
		}
		if elapsed := time.Since(start); elapsed != budget || polls != 2 {
			t.Errorf("polls = %d, elapsed = %s, want 2 polls in %s", polls, elapsed, budget)
		}
	})
}

func TestRequireNoComputeSystemReturnsWhenAbsent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, outputs := range [][]string{{"other compute system"}, {"running SANDBOX-ID", "other compute system"}} {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			polls := 0
			err := waitForNoComputeSystem(ctx, "sandbox-id", func(context.Context) ([]byte, error) {
				if polls == len(outputs) {
					t.Fatal("poll continued after the sandbox ID disappeared")
				}
				output := outputs[polls]
				polls++
				return []byte(output), nil
			})
			cancel()
			if err != nil || polls != len(outputs) {
				t.Errorf("output sequence %q: got %d polls, error %v", outputs, polls, err)
			}
		}
	})
}

func TestRequireNoComputeSystemReportsCommandFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	t.Setenv(envHelperMode, string(helperFail))
	bin, args := helperArgs()
	err := waitForNoComputeSystem(ctx, "sandbox-id", func(ctx context.Context) ([]byte, error) {
		return hcsListOutput(ctx, bin, args...)
	})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 ||
		!strings.Contains(err.Error(), "out-final") ||
		!strings.Contains(err.Error(), "err-final") {
		t.Fatalf("waitForNoComputeSystem = %v, want exit code 1 and both output streams", err)
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
