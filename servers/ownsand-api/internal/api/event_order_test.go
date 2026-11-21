// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestDelayedDeleteEventIsDeliveredAfterALaterCommit holds the transaction
// of a pending sandbox delete open (the way a slow DeleteSandbox request on
// another instance would), commits a configuration event of another device
// and waits until the lease holder has processed it, then commits the delete.
// The delete event has the lower insert-time ID, and the connected agent must
// still receive the delete command and be able to report its completion on
// the same channel, without a new state report.
func TestDelayedDeleteEventIsDeliveredAfterALaterCommit(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 300 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	dev, agent := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	// The second device joins after the sandbox was placed on the first.
	dev2, agent2 := setupAgent(t, env, org, nil)

	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `UPDATE sandboxes SET delete_pending = true, delete_end_reason = $2, delete_actor_kind = 1, delete_actor_id = 'test'
		WHERE sandbox_id = $1`, id, int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)); err != nil {
		t.Fatal(err)
	}
	if err := events.Write(ctx, tx, events.Event{Kind: events.KindSandboxDeleteRequested, OrganizationID: org.ID, SandboxID: id, DeviceID: dev.ID}); err != nil {
		t.Fatal(err)
	}

	// A later event commits and is processed while the delete is open.
	if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev2.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 200}})); err != nil {
		t.Fatal(err)
	}
	agent2.WaitFor("new configuration", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration().GetLimits().GetVcpuCount() == 6 })

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("delayed delete command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetDeleteSandbox().GetSandboxId() == id })
	sb := waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason = %v", sb.GetEndReason())
	}
}

// TestRolledBackEventDoesNotStallTheEventLoop checks that an event whose
// transaction rolls back is never dispatched and that later events still are.
func TestRolledBackEventDoesNotStallTheEventLoop(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 300 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)

	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Write(ctx, tx, events.Event{Kind: events.KindDeviceRemoved, OrganizationID: org.ID, DeviceID: dev.ID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 200}})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("new configuration", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration().GetLimits().GetVcpuCount() == 6 })
	select {
	case <-agent.Closed():
		t.Fatal("the rolled-back device removal reached the agent")
	default:
	}
	if env.App.Service.Hub.Get(dev.ID) == nil {
		t.Fatal("the channel closed")
	}
}
