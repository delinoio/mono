// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestControlBaselinePrecedesChannelAdmission drives a lease period whose
// first control event cursor read fails, with real Postgres and a real agent
// channel. A channel that connects before the baseline exists is refused. A
// sandbox delete and a limits update that commit while the baseline is unset
// still reach an agent that connects after it, through the pending delete
// reconciliation and the limits read at admission. Later events reach the
// connected agent through the event processor without another report,
// reconnect or update.
func TestControlBaselinePrecedesChannelAdmission(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 500 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	dev, agent := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agent.Close()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE)

	// Start a new lease period whose baseline read fails until the test
	// allows it: the lease stays held, the cursor stays unset.
	control := env.App.Control
	testenv.Eventually(t, "the baseline of the first lease period", func() bool { _, ok := control.ProcessedCursor(); return ok })
	var failRead atomic.Bool
	failRead.Store(true)
	var reads atomic.Int32
	var realLatestID func(context.Context) (int64, error)
	realLatestID = control.SetLatestIDForTest(func(ctx context.Context) (int64, error) {
		reads.Add(1)
		if failRead.Load() {
			return 0, errors.New("injected baseline read failure")
		}
		return realLatestID(ctx)
	})
	control.LeaseLost()
	control.LeaseAcquired(ctx)
	if _, ok := control.ProcessedCursor(); ok {
		t.Fatal("the baseline was set although its read failed")
	}
	testenv.Eventually(t, "a retry of the baseline read by the event processor", func() bool { return reads.Load() >= 3 })

	// A channel that connects now is refused, and the hub stays empty.
	refused := env.NewFakeAgent(dev)
	refused.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	refused.OpenChannel()
	select {
	case <-refused.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open although the control event baseline was unset")
	}
	if code := testenv.Code(refused.CloseErr()); code != connect.CodeUnavailable {
		t.Fatalf("the refused channel ended with %v, want unavailable", refused.CloseErr())
	}
	if env.App.Service.Hub.Get(dev.ID) != nil {
		t.Fatal("a refused channel was registered in the hub")
	}

	// Commit a delete and a limits update while the baseline is unset.
	deleteDone := make(chan error, 1)
	go func() {
		_, err := c.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
		deleteDone <- err
	}()
	testenv.Eventually(t, "the pending delete", func() bool {
		var pending bool
		return env.DB.Pool.QueryRow(ctx, "SELECT delete_pending FROM sandboxes WHERE sandbox_id = $1", sandboxID).Scan(&pending) == nil && pending
	})
	if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100}})); err != nil {
		t.Fatal(err)
	}
	if _, ok := control.ProcessedCursor(); ok {
		t.Fatal("the baseline was set before the test allowed its read")
	}

	// Allow the baseline. The newest event is after both writes, so the
	// event processor never delivers them: the reconnecting agent must get
	// them through its own admission.
	failRead.Store(false)
	testenv.Eventually(t, "the baseline of the new lease period", func() bool { _, ok := control.ProcessedCursor(); return ok })
	connected := env.NewFakeAgent(dev)
	connected.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	connected.Connect()
	first := connected.WaitFor("configuration at admission", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration() != nil })
	if got := first.GetConfiguration().GetLimits().GetVcpuCount(); got != 6 {
		t.Fatalf("the first configuration has %d vCPUs, want the 6 committed while the baseline was unset", got)
	}
	connected.WaitFor("the delete that predates the baseline", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == sandboxID
	})
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)

	// Events after the baseline reach the connected agent without a report,
	// a reconnect or an update: a limits change and the delete of a sandbox
	// that was created after the reconnect.
	if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100}})); err != nil {
		t.Fatal(err)
	}
	connected.WaitFor("the limits committed after the baseline", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetConfiguration().GetLimits().GetVcpuCount() == 8
	})
	second, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	secondID := second.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, secondID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if _, err := c.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: secondID})); err != nil {
		t.Fatal(err)
	}
	connected.WaitFor("the delete after the baseline", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == secondID
	})
}
