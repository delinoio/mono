// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func runHandoverLease(t *testing.T, env *testenv.Env) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.App.Lease.Run(ctx)
	}()
	stop = func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	env.WaitForLease()
	return stop
}

func pauseOfflineCleanup(t *testing.T, env *testenv.Env, deviceID string) (<-chan struct{}, func()) {
	t.Helper()
	conn := env.App.Service.Hub.Get(deviceID)
	if conn == nil {
		t.Fatal("device has no channel to close")
	}
	entered := make(chan struct{})
	resume := make(chan struct{})
	env.App.Service.Hub.OnClose(func(closed *hub.Conn) {
		if closed == conn {
			close(entered)
			<-resume
		}
	})
	return entered, sync.OnceFunc(func() { close(resume) })
}

func waitForOfflineLeaseSkip(t *testing.T, env *testenv.Env, deviceID string) {
	t.Helper()
	testenv.Eventually(t, "the old channel's offline write to skip", func() bool {
		for _, rec := range deviceOfflineLogs(t, env, deviceID) {
			switch rec.Msg {
			case "device_offline", "device_offline_update_failed":
				t.Fatalf("offline cleanup should skip instead of logging %s", rec.Msg)
			case "device_offline_skipped":
				if rec.Reason != "the channel's lease period is no longer current" {
					t.Fatalf("offline skip reason = %q", rec.Reason)
				}
				return true
			}
		}
		return false
	})
}

// TestOfflineWriteAfterLeaseHandover keeps the old handler alive until the
// new holder has acknowledged the same device's full state report.
func TestOfflineWriteAfterLeaseHandover(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, _ := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	entered, resume := pauseOfflineCleanup(t, env, dev.ID)
	defer resume()
	env.App.Lease.Release()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old channel did not reach the close listener")
	}

	successor := env.StartPeer()
	runHandoverLease(t, successor)
	agent := successor.NewFakeAgent(dev)
	agent.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	agent.Connect() // Waits for the successor's StateReportAck.
	t.Log("successor acknowledged its state report while the former holder's cleanup was paused")

	resume()
	waitForOfflineLeaseSkip(t, env, dev.ID)
	if got := deviceState(t, org, dev.ID); got != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device state after the former holder's cleanup = %s, want ONLINE", got)
	}
	if got := getSandbox(t, org.OwnerClient, org.ID, sandboxID).GetState(); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox state after the former holder's cleanup = %s, want RUNNING", got)
	}
	agent.Heartbeat(42)
	agent.WaitFor("successor heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 42 })
	t.Log("former holder skipped its write; device stayed ONLINE, sandbox stayed RUNNING, and successor acknowledged a heartbeat")
}

func TestOfflineWriteDuringLeaseLossWhileDeviceRowHeld(t *testing.T) {
	for _, sameInstance := range []bool{false, true} {
		name := "takeover"
		if sameInstance {
			name = "reacquisition"
		}
		t.Run(name, func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
			env.WaitForLease()
			env.App.Lease.Release()
			holder := env.StartPeer()
			stop := runHandoverLease(t, holder)
			agent := holder.NewFakeAgent(dev)
			agent.Connect()

			releaseDevice := holdDeviceLock(t, env, dev.ID)
			defer releaseDevice()
			agent.Close()
			waitForLockWaiter(t, env, "FROM devices WHERE device_id", "FOR UPDATE")

			stopped := make(chan struct{})
			go func() {
				stop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("lease release waited for the device row")
			}
			successor := holder
			if !sameInstance {
				successor = env.StartPeer()
			}
			runHandoverLease(t, successor)
			t.Log("lease released and acquired while the device row remained locked")
			waitForOfflineLeaseSkip(t, env, dev.ID)
			releaseDevice()
			if got := deviceState(t, org, dev.ID); got != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
				t.Fatalf("device state after loss while waiting = %s, want ONLINE", got)
			}
		})
	}
}

func TestOfflineWriteFromEarlierLeasePeriodAfterReacquisition(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	env.WaitForLease()
	env.App.Lease.Release()
	holder := env.StartPeer()
	stop := runHandoverLease(t, holder)
	agent := holder.NewFakeAgent(dev)
	agent.Connect()
	entered, resume := pauseOfflineCleanup(t, holder, dev.ID)
	defer resume()
	stop()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old channel did not reach the close listener")
	}

	runHandoverLease(t, holder)
	resume()
	waitForOfflineLeaseSkip(t, env, dev.ID)
	if got := deviceState(t, org, dev.ID); got != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device state after cleanup from the earlier period = %s, want ONLINE", got)
	}
	t.Log("the old cleanup skipped even though the same instance held a new lease period")
}

func TestOfflineWriteChecksExpiryAfterLeaseRowWait(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)

	ctx := context.Background()
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM instance_leases WHERE lease_name = $1 FOR UPDATE", lease.Name); err != nil {
		t.Fatal(err)
	}
	agent.Close()
	waitForLockWaiter(t, env, "FROM instance_leases", "FOR SHARE")
	// Expire the shared row after the cleanup transaction started, while
	// the server's local deadline is still current.
	if _, err := tx.Exec(ctx, "UPDATE instance_leases SET expire_time = clock_timestamp() WHERE lease_name = $1", lease.Name); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitForOfflineLeaseSkip(t, env, dev.ID)
	if got := deviceState(t, org, dev.ID); got != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device state after database lease expiry = %s, want ONLINE", got)
	}
}
