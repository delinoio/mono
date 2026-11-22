// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// setupAgent enrolls a Linux device with limits and connects a fake agent.
func setupAgent(t *testing.T, env *testenv.Env, org *testenv.Org, limits *ownsandv1.DeviceLimits) (*testenv.Device, *testenv.FakeAgent) {
	t.Helper()
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if limits == nil {
		limits = &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100}
	}
	if _, err := org.OwnerClient.Device.UpdateDeviceLimits(context.Background(), connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
		OrganizationId: org.ID, DeviceId: dev.ID, Limits: limits,
	})); err != nil {
		t.Fatal(err)
	}
	agent := env.NewFakeAgent(dev)
	agent.Connect()
	return dev, agent
}

// TestHeartbeatStoresUpdateState checks that a heartbeat stores the update
// state and starts and clears pending_idle_since.
func TestHeartbeatStoresUpdateState(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	pendingSince := func() (int16, bool) {
		var state int16
		var since *time.Time
		if err := env.DB.Pool.QueryRow(context.Background(), "SELECT update_state, pending_idle_since FROM devices WHERE device_id = $1", dev.ID).Scan(&state, &since); err != nil {
			t.Fatal(err)
		}
		return state, since != nil
	}
	send := func(seq uint64, st ownsandv1.AgentUpdateState) {
		agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: seq, UpdateState: st, UpdateTargetVersion: "1.2.3"}}})
		agent.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == seq })
	}
	send(1, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE)
	testenv.Eventually(t, "pending idle stored", func() bool {
		state, since := pendingSince()
		return state == int16(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE) && since
	})
	send(2, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE)
	testenv.Eventually(t, "pending idle cleared", func() bool {
		state, since := pendingSince()
		return state == int16(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE) && !since
	})
}

func createReq(orgID string) *ownsandv1.CreateSandboxRequest {
	key, _ := uuidv7.NewString()
	return &ownsandv1.CreateSandboxRequest{
		OrganizationId: orgID, IdempotencyKey: key,
		Source:    &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
		Resources: &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 1024, DiskGib: 10},
		IdleTtl:   durationpb.New(time.Hour), MaxTtl: durationpb.New(2 * time.Hour),
		Labels: map[string]string{"purpose": "test"},
	}
}

func getSandbox(t *testing.T, c *testenv.Clients, orgID, id string) *ownsandv1.Sandbox {
	t.Helper()
	resp, err := c.Sandbox.GetSandbox(context.Background(), connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: orgID, SandboxId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetSandbox()
}

func waitState(t *testing.T, c *testenv.Clients, orgID, id string, want ownsandv1.SandboxState) *ownsandv1.Sandbox {
	t.Helper()
	var sb *ownsandv1.Sandbox
	testenv.Eventually(t, "sandbox state "+want.String(), func() bool {
		sb = getSandbox(t, c, orgID, id)
		return sb.GetState() == want
	})
	return sb
}

func TestSandboxLifecycleOverTheAgentChannel(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)

	cfg := agent.WaitFor("configuration", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration() != nil }).GetConfiguration()
	if cfg.GetLimits().GetVcpuCount() != 4 || len(cfg.GetStunUrls()) != 1 {
		t.Fatalf("configuration = %v", cfg)
	}
	agent.Heartbeat(7)
	agent.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 7 })
	agent.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })

	key := env.Clients(org.APIKey)
	created, err := key.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sb := created.Msg.GetSandbox()
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_CREATING || sb.GetDeviceId() != dev.ID || sb.GetGuestOs() != ownsandv1.GuestOs_GUEST_OS_LINUX {
		t.Fatalf("created = %v", sb)
	}
	cmd := agent.WaitFor("create command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetCreateSandbox() != nil }).GetCommand().GetCreateSandbox()
	if cmd.GetLimits().GetVcpuCount() != 4 || cmd.GetIdleTtl().AsDuration() != time.Hour || cmd.GetMaxDeadline() == nil {
		t.Fatalf("create command = %v", cmd)
	}
	sb = waitState(t, key, org.ID, sb.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if sb.GetRestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE || sb.GetLabels()["purpose"] != "test" {
		t.Fatalf("running = %v", sb)
	}

	paused, err := key.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sb.GetSandboxId()}))
	if err != nil || paused.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause: %v %v", paused, err)
	}
	// Pausing a paused sandbox succeeds without a change; resuming works.
	if _, err := key.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sb.GetSandboxId()})); err != nil {
		t.Fatal(err)
	}
	resumed, err := key.Sandbox.ResumeSandbox(ctx, connect.NewRequest(&ownsandv1.ResumeSandboxRequest{OrganizationId: org.ID, SandboxId: sb.GetSandboxId()}))
	if err != nil || resumed.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume: %v %v", resumed, err)
	}
	// Device allocation counts the running sandbox.
	d, _ := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if d.Msg.GetDevice().GetAllocation().GetVcpuCount() != 2 || d.Msg.GetDevice().GetState() != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device = %v", d.Msg.GetDevice())
	}

	deleted, err := key.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: sb.GetSandboxId()}))
	if err != nil || deleted.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED ||
		deleted.Msg.GetSandbox().GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	// Deleting a deleted sandbox succeeds without change.
	if _, err := key.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: sb.GetSandboxId()})); err != nil {
		t.Fatal(err)
	}
	// Ended sandboxes are listed only on request.
	list, _ := key.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID}))
	ended, _ := key.Sandbox.ListSandboxes(ctx, connect.NewRequest(&ownsandv1.ListSandboxesRequest{OrganizationId: org.ID, IncludeEnded: true}))
	if len(list.Msg.GetSandboxes()) != 0 || len(ended.Msg.GetSandboxes()) != 1 {
		t.Fatalf("list = %d, with ended = %d", len(list.Msg.GetSandboxes()), len(ended.Msg.GetSandboxes()))
	}
	var created_, deleted_ int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action = 17), count(*) FILTER (WHERE action = 18) FROM audit_events WHERE target_id = $1", sb.GetSandboxId()).Scan(&created_, &deleted_)
	if created_ != 1 || deleted_ != 1 {
		t.Fatalf("audit created = %d, deleted = %d", created_, deleted_)
	}
	// Another organization's API key sees nothing.
	other := env.NewOrg("mallory")
	if _, err := env.Clients(other.APIKey).Sandbox.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: org.ID, SandboxId: sb.GetSandboxId()})); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("cross organization: %v", err)
	}
}

func TestCreateIdempotency(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	req := createReq(org.ID)
	first, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	// Even with the device offline, a retry returns the original sandbox.
	agent.Close()
	testenv.Eventually(t, "device offline", func() bool { return env.App.Service.Hub.Get(agent.Device.ID) == nil })
	again, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil || again.Msg.GetSandbox().GetSandboxId() != first.Msg.GetSandbox().GetSandboxId() {
		t.Fatalf("retry: %v %v", again, err)
	}
	changed := createReq(org.ID)
	changed.IdempotencyKey = req.IdempotencyKey
	changed.Resources.VcpuCount = 1
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(changed)); testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("other parameters: %v", err)
	}
	// A new key on the offline device fails with unavailable.
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID))); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("offline device: %v", err)
	}
	bad := createReq(org.ID)
	bad.MaxTtl = durationpb.New(8 * 24 * time.Hour)
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(bad)); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("ttl over 7 days: %v", err)
	}
	bad = createReq(org.ID)
	bad.IdleTtl = nil
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(bad)); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing idle ttl: %v", err)
	}
}

func TestOfflineDeleteAndReconciliation(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 300 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	a, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	b, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	idA, idB := a.Msg.GetSandbox().GetSandboxId(), b.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, idA, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	waitState(t, c, org.ID, idB, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	agent.Close()
	sb := waitState(t, c, org.ID, idA, ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE)
	if sb.GetLastKnownState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("last known = %v", sb.GetLastKnownState())
	}
	// Commands for an offline device fail at once; a delete is recorded.
	if _, err := c.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: idA})); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("pause offline: %v", err)
	}
	del, err := c.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: idA}))
	if err != nil || del.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE || del.Msg.GetSandbox().GetLastKnownState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("offline delete: %v %v", del, err)
	}

	// The agent lost sandbox B while offline (for example a host reboot) and
	// still runs A: on reconnect, B fails with LOST_ON_DEVICE and A gets the
	// pending delete.
	agent.ForgetSandbox(idB)
	agent2 := env.NewFakeAgent(dev)
	agent2.OnCommand = func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		return fa.DefaultAnswer(cmd)
	}
	// Carry the records of the first agent process.
	agent2.SetSandbox(idA, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	agent2.Connect()
	sb = waitState(t, c, org.ID, idA, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("A end reason = %v", sb.GetEndReason())
	}
	sb = getSandbox(t, c, org.ID, idB)
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("B = %v", sb)
	}
	var audits int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 18 AND target_id = $1 AND actor_kind = 4", idB).Scan(&audits)
	if audits != 1 {
		t.Fatalf("system audit events for B = %d", audits)
	}
	if n := env.App.Service.WatcherCount(); n != 0 {
		t.Fatalf("watchers after the offline delete = %d", n)
	}
}

// TestOfflineDeleteLeavesNoWatchers checks that DeleteSandbox on an offline
// device, which waits through at least one poll, leaves no sandbox watch
// registration behind and keeps the pending delete.
func TestOfflineDeleteLeavesNoWatchers(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 1500 * time.Millisecond, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agent.Close()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE)
	for i := 0; i < 3; i++ {
		start := time.Now()
		del, err := c.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: id}))
		if err != nil || del.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE {
			t.Fatalf("delete %d: %v %v", i, del, err)
		}
		if time.Since(start) < time.Second {
			t.Fatalf("delete %d returned before a poll: %v", i, time.Since(start))
		}
		if n := env.App.Service.WatcherCount(); n != 0 {
			t.Fatalf("watchers after delete %d = %d", i, n)
		}
	}
	var pending bool
	if err := env.DB.Pool.QueryRow(ctx, "SELECT delete_pending FROM sandboxes WHERE sandbox_id = $1", id).Scan(&pending); err != nil || !pending {
		t.Fatalf("delete_pending = %v, %v", pending, err)
	}
}

// TestWaitSandboxCancelledWaiterLeavesOtherWaiter checks that cancelling one
// waiter unregisters it and that a terminal update still wakes the other.
func TestWaitSandboxCancelledWaiterLeavesOtherWaiter(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: 20 * time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	// The agent never answers the delete, so both calls wait.
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetDeleteSandbox() != nil {
			return nil
		}
		return fa.DefaultAnswer(cmd)
	})
	req := func() *connect.Request[ownsandv1.DeleteSandboxRequest] {
		return connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: id})
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { _, err := c.Sandbox.DeleteSandbox(cancelCtx, req()); firstDone <- err }()
	type result struct {
		resp *connect.Response[ownsandv1.DeleteSandboxResponse]
		err  error
	}
	secondDone := make(chan result, 1)
	go func() { r, err := c.Sandbox.DeleteSandbox(ctx, req()); secondDone <- result{r, err} }()
	testenv.Eventually(t, "two watchers", func() bool { return env.App.Service.WatcherCount() == 2 })
	cancel()
	if err := <-firstDone; err == nil {
		t.Fatal("cancelled delete succeeded")
	}
	testenv.Eventually(t, "one watcher", func() bool { return env.App.Service.WatcherCount() == 1 })
	deleted := agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
	agent.SendUpdate(deleted)
	select {
	case r := <-secondDone:
		if r.err != nil || r.resp.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("second delete: %v %v", r.resp, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second waiter was not woken")
	}
	testenv.Eventually(t, "no watchers", func() bool { return env.App.Service.WatcherCount() == 0 })
}

func TestRevisionRuleAndAbsorbingStates(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	current := agent.Sandbox(id)

	// A stale update is acknowledged without being applied.
	stale := agent.Sandbox(id)
	stale.State = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	stale.Revision.Counter = current.GetRevision().GetCounter() - 1
	agent.SendUpdate(stale)
	agent.WaitFor("stale ack", func(e *ownsandv1.AgentEnvelope) bool {
		a := e.GetStateAck().GetSandboxUpdate()
		return a.GetSandboxId() == id && a.GetRevision().GetCounter() == stale.GetRevision().GetCounter()
	})
	if st := getSandbox(t, c, org.ID, id).GetState(); st != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("stale update applied: %v", st)
	}
	// The agent ends the sandbox at its idle TTL.
	agent.SendUpdate(agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED))
	sb := waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("end reason = %v", sb.GetEndReason())
	}
	// DELETED is absorbing: a later report of a live VM keeps the state and
	// gets a delete command.
	agent.SendUpdate(agent.SetSandbox(id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0))
	agent.WaitFor("delete of a live VM", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSandbox().GetSandboxId() == id
	})
	if sb := getSandbox(t, c, org.ID, id); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("absorbing state changed: %v", sb)
	}
}

func TestCreateRejectedByAgent(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetCreateSandbox() != nil {
			return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: "no disk"}}
		}
		return fa.DefaultAnswer(cmd)
	})
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sb := waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatalf("end reason = %v", sb.GetEndReason())
	}
	d, _ := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: agent.Device.ID}))
	if d.Msg.GetDevice().GetAllocation().GetVcpuCount() != 0 {
		t.Fatalf("reservation not released: %v", d.Msg.GetDevice().GetAllocation())
	}
}

func TestHeartbeatTimeoutAndReplacement(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 400 * time.Millisecond, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	// A second channel of the same device replaces the first; the close of
	// the old channel does not change the device state.
	agent2 := env.NewFakeAgent(dev)
	agent2.Connect()
	select {
	case <-agent.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced channel stayed open")
	}
	for i := 0; i < 3; i++ {
		agent2.Heartbeat(uint64(i))
		time.Sleep(200 * time.Millisecond)
	}
	d, _ := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if d.Msg.GetDevice().GetState() != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device after replacement = %v", d.Msg.GetDevice().GetState())
	}
	// No heartbeat: the server closes the channel and marks the device
	// offline.
	select {
	case <-agent2.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat timeout")
	}
	testenv.Eventually(t, "device offline", func() bool {
		d, _ := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
		return d.Msg.GetDevice().GetState() == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	})
}

// TestOfflineWriteOfClosedChannelSkipsReplacement checks the per-device fence:
// a channel that closed as the current channel, whose clean-up is paused
// after its removal from the hub, must not take the device offline once a
// replacement channel registered and applied its state report. The last
// channel's close still marks the device offline.
func TestOfflineWriteOfClosedChannelSkipsReplacement(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agentA := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	deviceState := func() ownsandv1.DeviceState {
		d, err := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
		if err != nil {
			t.Fatal(err)
		}
		return d.Msg.GetDevice().GetState()
	}

	// Pause the clean-up of channel A after the hub removed it as the
	// current channel and before its offline write.
	connA := env.App.Service.Hub.Get(dev.ID)
	if connA == nil {
		t.Fatal("no current channel")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	env.App.Service.Hub.OnClose(func(closed *hub.Conn) {
		if closed == connA {
			close(entered)
			<-release
		}
	})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()
	agentA.Close()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the close of channel A did not reach its listeners")
	}

	// Replacement B connects and its state report is applied while A's
	// offline write is still pending.
	agentB := env.NewFakeAgent(dev)
	agentB.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED)
	agentB.Connect()
	agentB.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })
	if st := deviceState(); st != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device before the release = %v", st)
	}

	// Let A's clean-up run to its end.
	releaseOnce()
	testenv.Eventually(t, "the clean-up of channel A decided its offline write", func() bool {
		return strings.Contains(env.Logs.String(), `"msg":"device_offline"`) || strings.Contains(env.Logs.String(), `"msg":"device_offline_skipped"`) ||
			strings.Contains(env.Logs.String(), `"msg":"device_offline_update_failed"`)
	})
	if strings.Contains(env.Logs.String(), `"msg":"device_offline"`) {
		t.Fatal("the closed channel A took the device offline")
	}
	if st := deviceState(); st != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device after the clean-up of A = %v", st)
	}
	if sb := getSandbox(t, c, org.ID, sandboxID); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox after the clean-up of A = %v", sb.GetState())
	}
	agentB.Heartbeat(42)
	agentB.WaitFor("heartbeat ack on B", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 42 })

	// The close of the last channel still marks the device offline.
	agentB.Close()
	testenv.Eventually(t, "device offline", func() bool { return deviceState() == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE })
	sb := waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE)
	if sb.GetLastKnownState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("last known state = %v", sb.GetLastKnownState())
	}
}

// TestOfflineSweepWithEmptyRegistry checks that the sweep marks a stale
// ONLINE device OFFLINE when the lease holder has no channel at all, and
// leaves a recent device and terminal sandboxes unchanged.
func TestOfflineSweepWithEmptyRegistry(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 400 * time.Millisecond, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	stale := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	recent := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	now := env.Clock.Now()
	// No agent connects, so the channel registry stays empty. Seed the
	// devices ONLINE directly: one not heard from for longer than the
	// heartbeat timeout, one heard from just now.
	for id, seen := range map[string]time.Time{stale.ID: now.Add(-time.Hour), recent.ID: now.Add(time.Hour)} {
		if _, err := env.DB.Pool.Exec(ctx, "UPDATE devices SET state = $2, last_seen_time = $3 WHERE device_id = $1",
			id, int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE), seen); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(deviceID string, state ownsandv1.SandboxState, endReason int16) string {
		id, _ := uuidv7.NewString()
		var reason *int16
		if endReason != 0 {
			reason = &endReason
		}
		if _, err := env.DB.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, end_reason, vcpu_count, memory_mib, disk_gib,
			guest_os, host_os, arch, network_mode, idle_ttl_ms, max_ttl_ms, max_deadline, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, $4, $5, 1, 512, 1, 1, 1, 1, 1, 60000, 60000, now() + interval '1 hour', 1, 'alice', now())`,
			id, org.ID, deviceID, int16(state), reason); err != nil {
			t.Fatal(err)
		}
		return id
	}
	running := seed(stale.ID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	deleted := seed(stale.ID, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, 1)
	recentRunning := seed(recent.ID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	deviceState := func(id string) ownsandv1.DeviceState {
		d, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: id}))
		if err != nil {
			t.Fatal(err)
		}
		return d.Msg.GetDevice().GetState()
	}
	testenv.Eventually(t, "stale device offline", func() bool {
		return deviceState(stale.ID) == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	})
	// Several more sweeps pass before the other devices are checked.
	time.Sleep(400 * time.Millisecond)
	if got := deviceState(recent.ID); got != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("recent device = %v", got)
	}
	sb := getSandbox(t, org.OwnerClient, org.ID, running)
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE || sb.GetLastKnownState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox on stale device = %v, last known %v", sb.GetState(), sb.GetLastKnownState())
	}
	if sb := getSandbox(t, org.OwnerClient, org.ID, recentRunning); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox on recent device = %v", sb.GetState())
	}
	if sb := getSandbox(t, org.OwnerClient, org.ID, deleted); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || sb.GetEndReason() != ownsandv1.SandboxEndReason(1) {
		t.Fatalf("terminal sandbox = %v, end reason %v", sb.GetState(), sb.GetEndReason())
	}
}

func TestConcurrentReservation(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: 100})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[connect.Code]int{}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			mu.Lock()
			codes[testenv.Code(err)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[0] != 1 || codes[connect.CodeUnavailable] != 5 {
		t.Fatalf("codes = %v", codes)
	}
	// A request over the device limits themselves is failed_precondition.
	big := createReq(org.ID)
	big.Resources.VcpuCount = 3
	if _, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(big)); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("over the limit: %v", err)
	}
}

func TestConfigurationPushAndAgentUpdates(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, false)"); err != nil {
		t.Fatal(err)
	}
	dev, agent := setupAgent(t, env, org, nil)
	up := agent.WaitFor("update command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetUpdateAgent() != nil }).GetCommand().GetUpdateAgent()
	if up.GetTargetVersion() != "1.1.0" || up.GetRevocationRecovery() {
		t.Fatalf("update = %v", up)
	}
	if _, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 200}})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("new configuration", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration().GetLimits().GetVcpuCount() == 6 })

	// HOLD stops updates, but a revoked running version forces the default
	// target as a revocation recovery.
	if _, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN, PinnedVersion: "1.0.0"})); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Pool.Exec(ctx, "UPDATE agent_releases SET revoked = true WHERE version = '1.0.0'"); err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("recovery update", func(e *ownsandv1.AgentEnvelope) bool {
		u := e.GetCommand().GetUpdateAgent()
		return u.GetRevocationRecovery() && u.GetTargetVersion() == "1.1.0"
	})
	// The revoked version makes the device ineligible (transient).
	if _, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID))); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("create on a revoked agent: %v", err)
	}
}

// TestHoldWithdrawsQueuedAgentUpdate checks the withdrawal of a queued update
// under HOLD: a live change to HOLD sends one withdrawal and no target, a
// change back to FOLLOW_DEFAULT sends the target again, a revoked running
// version still gets the revocation recovery instead of a withdrawal, and an
// older agent that answers UNIMPLEMENTED only produces a log line.
func TestHoldWithdrawsQueuedAgentUpdate(t *testing.T) {
	const (
		follow = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT
		hold   = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD
	)
	setPolicy := func(t *testing.T, org *testenv.Org, mode ownsandv1.AgentVersionPolicyMode) {
		t.Helper()
		if _, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(context.Background(), connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: mode})); err != nil {
			t.Fatal(err)
		}
	}
	isWithdraw := func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetWithdrawAgentUpdate() != nil }
	// settle waits until the lease holder evaluated every earlier policy
	// event: it processes control events in order, so the configuration that a
	// later limits change pushes shows that the earlier policy events ran. A
	// heartbeat then shows that the channel handled its earlier envelopes.
	var settleN int32
	settle := func(t *testing.T, org *testenv.Org, dev *testenv.Device, agent *testenv.FakeAgent) {
		settleN++
		if _, err := org.OwnerClient.Device.UpdateDeviceLimits(context.Background(), connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID,
			Limits: &ownsandv1.DeviceLimits{VcpuCount: 10 + settleN, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 200}})); err != nil {
			t.Fatal(err)
		}
		agent.WaitFor("new configuration", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetConfiguration().GetLimits().GetVcpuCount() == 10+settleN
		})
		agent.Heartbeat(uint64(settleN))
		agent.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == uint64(settleN) })
	}
	count := func(agent *testenv.FakeAgent) (updates, withdrawals int) {
		for _, c := range agent.Commands() {
			if c.GetUpdateAgent() != nil {
				updates++
			}
			if c.GetWithdrawAgentUpdate() != nil {
				withdrawals++
			}
		}
		return
	}

	t.Run("hold withdraws and follow default resends the target", func(t *testing.T) {
		env := testenv.New(t)
		ctx := context.Background()
		org := env.NewOrg("alice")
		if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, false)"); err != nil {
			t.Fatal(err)
		}
		dev, agent := setupAgent(t, env, org, nil)
		agent.WaitFor("update command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetUpdateAgent() != nil })

		setPolicy(t, org, hold)
		agent.WaitFor("withdrawal", isWithdraw)
		// A repeated evaluation under HOLD sends no second withdrawal.
		setPolicy(t, org, hold)
		settle(t, org, dev, agent)
		if u, w := count(agent); u != 1 || w != 1 {
			t.Fatalf("under HOLD: %d updates, %d withdrawals, want 1 and 1", u, w)
		}

		setPolicy(t, org, follow)
		agent.Eventually("the target again", func() bool { u, _ := count(agent); return u == 2 })
		var last *ownsandv1.UpdateAgentCommand
		for _, c := range agent.Commands() {
			if c.GetUpdateAgent() != nil {
				last = c.GetUpdateAgent()
			}
		}
		if last.GetTargetVersion() != "1.1.0" || last.GetRevocationRecovery() {
			t.Fatalf("resent update = %v", last)
		}

		setPolicy(t, org, hold)
		agent.Eventually("a second withdrawal", func() bool { _, w := count(agent); return w == 2 })
		if !strings.Contains(env.Logs.String(), `"command":"withdraw_agent_update"`) {
			t.Fatalf("the withdrawal is not in the command logs")
		}
	})

	t.Run("a reconnect under hold sends one withdrawal", func(t *testing.T) {
		env := testenv.New(t)
		ctx := context.Background()
		org := env.NewOrg("alice")
		if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, false)"); err != nil {
			t.Fatal(err)
		}
		setPolicy(t, org, hold)
		dev, agent := setupAgent(t, env, org, nil)
		agent.WaitFor("withdrawal", isWithdraw)
		settle(t, org, dev, agent)
		if u, w := count(agent); u != 0 || w != 1 {
			t.Fatalf("after reconciliation: %d updates, %d withdrawals, want 0 and 1", u, w)
		}
	})

	t.Run("a revoked running version recovers instead of withdrawing", func(t *testing.T) {
		env := testenv.New(t)
		ctx := context.Background()
		org := env.NewOrg("alice")
		if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, true)"); err != nil {
			t.Fatal(err)
		}
		setPolicy(t, org, hold)
		dev, agent := setupAgent(t, env, org, nil)
		up := agent.WaitFor("recovery update", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetUpdateAgent() != nil }).GetCommand().GetUpdateAgent()
		if up.GetTargetVersion() != "1.1.0" || !up.GetRevocationRecovery() {
			t.Fatalf("update = %v", up)
		}
		settle(t, org, dev, agent)
		if _, w := count(agent); w != 0 {
			t.Fatalf("%d withdrawals under a revocation recovery", w)
		}
	})

	t.Run("an older agent answers UNIMPLEMENTED", func(t *testing.T) {
		env := testenv.New(t)
		ctx := context.Background()
		org := env.NewOrg("alice")
		if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, false)"); err != nil {
			t.Fatal(err)
		}
		dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
		agent := env.NewFakeAgent(dev)
		agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
			if cmd.GetWithdrawAgentUpdate() != nil {
				return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "unknown command"}}
			}
			return a.DefaultAnswer(cmd)
		})
		agent.Connect()
		agent.WaitFor("update command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetUpdateAgent() != nil })
		setPolicy(t, org, hold)
		agent.WaitFor("withdrawal", isWithdraw)
		settle(t, org, dev, agent)
		testenv.Eventually(t, "the UNIMPLEMENTED result in the logs", func() bool {
			logs := env.Logs.String()
			return strings.Contains(logs, "agent_command_result") && strings.Contains(logs, `"result":"ERROR_CODE_UNIMPLEMENTED"`)
		})
		// The channel stays open and the server does not retry.
		select {
		case <-agent.Closed():
			t.Fatal("the channel closed after UNIMPLEMENTED")
		default:
		}
		if _, w := count(agent); w != 1 {
			t.Fatalf("%d withdrawals, want 1", w)
		}
	})

	// The channel handler and the control event processor both evaluate the
	// update of one channel. While another decision for the channel runs, a
	// policy change must wait: a decision that read an older policy could
	// otherwise queue its command after the command of a newer decision.
	t.Run("an update decision waits for the decision that runs on the channel", func(t *testing.T) {
		env := testenv.New(t)
		ctx := context.Background()
		org := env.NewOrg("alice")
		if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ('1.1.0', 1, 1, true, false), ('1.0.0', 1, 1, false, false)"); err != nil {
			t.Fatal(err)
		}
		dev, agent := setupAgent(t, env, org, nil)
		agent.WaitFor("update command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetUpdateAgent() != nil })
		conn := env.App.Service.Hub.Live(dev.ID)
		if conn == nil {
			t.Fatal("the device has no live channel")
		}

		unlock := conn.LockUpdates()
		unlocked := false
		defer func() {
			if !unlocked {
				unlock()
			}
		}()
		setPolicy(t, org, hold)
		time.Sleep(500 * time.Millisecond)
		if _, w := count(agent); w != 0 {
			t.Fatalf("%d withdrawals while another decision held the channel", w)
		}
		unlock()
		unlocked = true
		agent.WaitFor("withdrawal after the decision ended", isWithdraw)
	})
}

// TestAgentUpdateTargetOrdering checks which update target the control
// plane sends for a running version, the releases of the platform and the
// organization policy: an ordinary default is a target only when it is
// strictly newer, a pin is sent as it is, and a revocation recovery selects
// the default even when it is lower.
func TestAgentUpdateTargetOrdering(t *testing.T) {
	type release struct {
		version            string
		isDefault, revoked bool
	}
	const (
		follow = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT
		pin    = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN
		hold   = ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD
	)
	cases := []struct {
		name         string
		running      string
		releases     []release
		mode         ownsandv1.AgentVersionPolicyMode
		pinned       string
		wantTarget   string // empty: no update command
		wantRecovery bool
		wantWarning  bool
	}{
		{name: "lower default is not a downgrade", running: "1.10.0", releases: []release{{"1.9.0", true, false}, {"1.10.0", false, false}}, mode: follow},
		{name: "numeric minor ordering sends a newer default", running: "1.9.0", releases: []release{{"1.10.0", true, false}, {"1.9.0", false, false}}, mode: follow, wantTarget: "1.10.0"},
		{name: "numeric patch ordering sends a newer default", running: "1.2.9", releases: []release{{"1.2.10", true, false}}, mode: follow, wantTarget: "1.2.10"},
		{name: "equal default with a v prefix", running: "1.9.0", releases: []release{{"v1.9.0", true, false}}, mode: follow},
		{name: "a release is newer than its pre-release", running: "1.9.0-rc.1", releases: []release{{"1.9.0", true, false}}, mode: follow, wantTarget: "1.9.0"},
		{name: "a pre-release default is older than the release", running: "1.9.0", releases: []release{{"1.9.0-rc.1", true, false}}, mode: follow},
		{name: "unrecognized running version", running: "dev-build", releases: []release{{"1.9.0", true, false}}, mode: follow, wantWarning: true},
		{name: "unrecognized default version", running: "1.9.0", releases: []release{{"latest", true, false}}, mode: follow, wantWarning: true},
		{name: "zero-padded running version", running: "01.0.0", releases: []release{{"1.1.0", true, false}}, mode: follow, wantWarning: true},
		{name: "zero-padded default version", running: "1.0.0", releases: []release{{"01.1.0", true, false}}, mode: follow, wantWarning: true},
		{name: "signed default version", running: "1.0.0", releases: []release{{"+1.1.0", true, false}}, mode: follow, wantWarning: true},
		{name: "no default release", running: "1.9.0", releases: []release{{"1.8.0", false, false}}, mode: follow},
		{name: "lower pin is sent without recovery", running: "1.10.0", releases: []release{{"1.10.0", true, false}, {"1.9.0", false, false}}, mode: pin, pinned: "1.9.0", wantTarget: "1.9.0"},
		{name: "healthy hold sends nothing", running: "1.0.0", releases: []release{{"1.1.0", true, false}}, mode: hold},
		{name: "revoked running version recovers to a lower default", running: "1.10.0", releases: []release{{"1.9.0", true, false}, {"1.10.0", false, true}}, mode: follow, wantTarget: "1.9.0", wantRecovery: true},
		{name: "revoked running version recovers under hold", running: "1.10.0", releases: []release{{"1.9.0", true, false}, {"1.10.0", false, true}}, mode: hold, wantTarget: "1.9.0", wantRecovery: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			org := env.NewOrg("alice")
			for _, r := range c.releases {
				if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO agent_releases (version, host_os, arch, is_default, revoked) VALUES ($1, 1, 1, $2, $3)", r.version, r.isDefault, r.revoked); err != nil {
					t.Fatal(err)
				}
			}
			if c.mode != follow {
				if _, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: c.mode, PinnedVersion: c.pinned})); err != nil {
					t.Fatal(err)
				}
			}
			dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
			agent := env.NewFakeAgent(dev)
			agent.AgentVersion = c.running
			agent.Connect()
			if c.wantTarget != "" {
				up := agent.WaitFor("update command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetUpdateAgent() != nil }).GetCommand().GetUpdateAgent()
				if up.GetTargetVersion() != c.wantTarget || up.GetRevocationRecovery() != c.wantRecovery {
					t.Fatalf("update = %v, want target %q recovery %t", up, c.wantTarget, c.wantRecovery)
				}
			}
			// The channel handles envelopes in order, so the acknowledgement
			// of a heartbeat that follows the capability and state reports
			// shows that their update evaluation finished.
			agent.Heartbeat(1)
			agent.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 1 })
			var updates []*ownsandv1.UpdateAgentCommand
			for _, e := range agent.Received() {
				if u := e.GetCommand().GetUpdateAgent(); u != nil {
					updates = append(updates, u)
				}
			}
			wantCount := 0
			if c.wantTarget != "" {
				wantCount = 1
			}
			if len(updates) != wantCount {
				t.Fatalf("update commands = %v, want %d", updates, wantCount)
			}
			logs := env.Logs.String()
			warned := strings.Contains(logs, "agent_update_skipped") && strings.Contains(logs, fmt.Sprintf(`"device_id":%q`, dev.ID)) && strings.Contains(logs, "version_not_comparable")
			if warned != c.wantWarning || (!c.wantWarning && strings.Contains(logs, "agent_update_skipped")) {
				t.Fatalf("warning logged = %t, want %t", warned, c.wantWarning)
			}
		})
	}
}

func TestDeviceRemovalOverTheChannel(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	created, _ := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if _, err := org.OwnerClient.Device.RemoveDevice(ctx, connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("clean-up command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetCleanUp() != nil })
	select {
	case <-agent.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open after the removal")
	}
	sb := getSandbox(t, org.OwnerClient, org.ID, id)
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED {
		t.Fatalf("sandbox after removal = %v", sb)
	}
}

// TestDeleteSnapshotDeletesLocalCopy checks that DeleteSnapshot sends the
// delete-snapshot command to the device, and that a state report that
// lists a snapshot without a record gets the command too.
func TestDeleteSnapshotDeletesLocalCopy(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	agent.Eventually("snapshot on the device", func() bool { return agent.HasSnapshot(snapID) })
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	if _, err := c.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	agent.Eventually("local copy deleted", func() bool { return !agent.HasSnapshot(snapID) })

	// A copy that the device kept while it missed the command (offline, or
	// an older agent) is deleted after its next state report.
	agent.Close()
	stale, _ := uuidv7.NewString()
	agent2 := env.NewFakeAgent(dev)
	agent2.OnCommand = func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		return fa.DefaultAnswer(cmd)
	}
	agent2.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	agent2.AddSnapshot(&ownsandv1.SnapshotStateEntry{SnapshotId: stale, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX})
	agent2.Connect()
	agent2.WaitFor("delete stale snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == stale
	})
	agent2.Eventually("stale copy deleted", func() bool { return !agent2.HasSnapshot(stale) })
}

func TestSnapshotsTemplatesAndExport(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	c := org.OwnerClient
	created, _ := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil || snap.Msg.GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
		t.Fatalf("create snapshot: %v %v", snap, err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY && s.Msg.GetSnapshot().GetHasMemoryState()
	})
	tmpl, err := c.Snapshot.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: org.ID, SnapshotId: snapID, Name: "base"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: org.ID, SnapshotId: snapID, Name: "base"})); testenv.Code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate template: %v", err)
	}
	// A sandbox from the template restores memory state on the same device
	// with equal resources.
	fromTemplate := createReq(org.ID)
	fromTemplate.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: tmpl.Msg.GetTemplate().GetTemplateId()}}
	fromTemplate.Resources.VcpuCount = 1
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(fromTemplate)); err != nil {
		t.Fatalf("from template: %v", err)
	}
	cmd := agent.WaitFor("create from snapshot", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetCreateSandbox().GetSnapshotId() == snapID
	}).GetCommand().GetCreateSandbox()
	if cmd.GetMemoryRestoreAllowed() {
		t.Fatal("memory restore with different resources")
	}
	small := createReq(org.ID)
	small.Source = fromTemplate.Source
	small.Resources.DiskGib = 5
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(small)); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("smaller disk: %v", err)
	}
	if _, err := c.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete with a template: %v", err)
	}

	exp, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
	if err != nil || exp.Msg.GetSnapshot().GetExportState() != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("export: %v %v", exp, err)
	}
	grant := agent.WaitFor("signed urls", func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignedUrlGrant() != nil }).GetSignedUrlGrant()
	if len(grant.GetPartUrls()) != 2 || grant.GetError() != nil {
		t.Fatalf("grant = %v", grant)
	}
	testenv.Eventually(t, "snapshot exported", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	if logs := env.Logs.String(); containsAny(logs, grant.GetPartUrls()[0].GetUrl()) {
		t.Fatal("a signed URL appears in the logs")
	}
	// Deleting the exported snapshot starts the clean-up of its prefix, which
	// sweeps again after the signed URLs expired.
	if _, err := c.Snapshot.DeleteTemplate(ctx, connect.NewRequest(&ownsandv1.DeleteTemplateRequest{OrganizationId: org.ID, TemplateId: tmpl.Msg.GetTemplate().GetTemplateId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	prefix := "organizations/" + org.ID + "/snapshots/" + snapID + "/"
	env.App.Control.RunCleanupOnce(ctx)
	if got := env.Objects.DeletedPrefixes(); len(got) != 1 || got[0] != prefix {
		t.Fatalf("first sweep = %v", got)
	}
	env.Clock.Advance(16 * time.Minute)
	env.App.Control.RunCleanupOnce(ctx)
	var jobs int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM r2_cleanup_jobs").Scan(&jobs)
	if got := env.Objects.DeletedPrefixes(); len(got) != 2 || jobs != 0 {
		t.Fatalf("final sweep = %v, jobs left = %d", got, jobs)
	}
}

func TestOrganizationDeletionFinalSweepRetries(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	if _, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	env.Objects.SetFail(errFake)
	env.App.Control.RunCleanupOnce(ctx)
	var attempts int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT attempts FROM r2_cleanup_jobs").Scan(&attempts)
	if attempts != 1 || len(env.Objects.DeletedPrefixes()) != 0 {
		t.Fatalf("attempts = %d", attempts)
	}
	env.Objects.SetFail(nil)
	env.Clock.Advance(20 * time.Minute)
	env.App.Control.RunCleanupOnce(ctx)
	var jobs int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM r2_cleanup_jobs").Scan(&jobs)
	if got := env.Objects.DeletedPrefixes(); len(got) != 2 || got[0] != "organizations/"+org.ID+"/" || jobs != 0 {
		t.Fatalf("sweeps = %v, jobs = %d", got, jobs)
	}
}

type fakeError struct{}

func (fakeError) Error() string { return "fake R2 failure" }

var errFake = fakeError{}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

func TestChannelNeedsLease(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if err := env.App.Lease.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A second instance on the same database does not get the lease and
	// refuses agent channels.
	second, err := app.New(app.Options{DB: env.DB, Logger: env.Logger, Now: env.Clock.Now, Logto: nil})
	if err != nil {
		t.Fatal(err)
	}
	held, err := second.Lease.TryAcquire(context.Background())
	if err != nil || held {
		t.Fatalf("second instance lease: %v %v", held, err)
	}
	secondServer := httptest.NewUnstartedServer(second.Handler())
	secondServer.EnableHTTP2 = true
	secondServer.StartTLS()
	t.Cleanup(secondServer.Close)
	secondInstance := &instance{app: second, server: secondServer}
	cred := env.SessionCredential(dev)

	// A valid device stream to the instance without the lease ends
	// unavailable before any configuration arrives.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := secondInstance.channel(ctx, cred)
	if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}}); err != nil {
		t.Fatal(err)
	}
	if r := awaitReceive(t, "the refusal", receive(stream)); testenv.Code(r.err) != connect.CodeUnavailable || r.msg != nil {
		t.Fatalf("device stream on the instance without the lease: %v %v", r.msg, r.err)
	}

	// Control: the same credential on the lease holder receives the
	// configuration.
	holder := (&instance{app: env.App, server: env.Server}).channel(ctx, cred)
	if err := holder.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}}); err != nil {
		t.Fatal(err)
	}
	cfg := awaitReceive(t, "the configuration from the holder", receive(holder))
	if cfg.err != nil || cfg.msg.GetEnvelope().GetConfiguration() == nil {
		t.Fatalf("first message from the lease holder: %v %v", cfg.msg, cfg.err)
	}

	// The API key of the device's own organization is rejected on the
	// channel of the lease holder.
	keyStream := env.Clients(org.APIKey).Agent.Channel(context.Background())
	_ = keyStream.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}})
	if _, err := keyStream.Receive(); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("api key on the channel: %v", err)
	}
}

// gateLease is a LeaseHolder test double with the ownership semantics of
// the real lease: WhileHeld holds the ownership lock while fn runs and the
// loss transition takes the same lock before it closes the channels. The
// gate pauses an admission before it reaches the lock.
type gateLease struct {
	mu   sync.Mutex
	held bool
	hub  interface{ CloseAll(error) }

	// entered receives once when an admission reaches the gate; the
	// admission continues after proceed is closed.
	entered chan struct{}
	proceed chan struct{}
}

func (g *gateLease) Held() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

func (g *gateLease) WhileHeld(fn func()) bool {
	if g.entered != nil {
		g.entered <- struct{}{}
		<-g.proceed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		return false
	}
	fn()
	return true
}

// lose clears ownership under the lock and then closes every channel, like
// Lease.set(false) followed by OnLose.
func (g *gateLease) lose() {
	g.mu.Lock()
	g.held = false
	g.mu.Unlock()
	g.hub.CloseAll(errors.New("lease lost"))
}

// alwaysHeld is the lease of a successor instance that holds the lease.
type alwaysHeld struct{}

func (alwaysHeld) Held() bool { return true }

func (alwaysHeld) WhileHeld(fn func()) bool {
	fn()
	return true
}

// instance is a second control plane instance on the database of env. It is
// not run (no lease loop, no workers); lease replaces its lease.
type instance struct {
	app    *app.App
	server *httptest.Server
}

func newInstance(t *testing.T, env *testenv.Env, lease api.LeaseHolder) *instance {
	t.Helper()
	a, err := app.New(app.Options{DB: env.DB, Logger: env.Logger, Now: env.Clock.Now, STUNURLs: []string{"stun:127.0.0.1:3478"}})
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := lease.(*gateLease); ok {
		g.hub = a.Service.Hub
	}
	a.Service.Lease = lease
	srv := httptest.NewUnstartedServer(a.Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &instance{app: a, server: srv}
}

// channel opens the agent channel of the device on the instance.
func (i *instance) channel(ctx context.Context, cred string) *connect.BidiStreamForClient[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse] {
	client := ownsandv1connect.NewAgentServiceClient(i.server.Client(), i.server.URL)
	stream := client.Channel(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+cred)
	return stream
}

type receiveResult struct {
	msg *ownsandv1.ChannelResponse
	err error
}

// receive reads the next message in the background so a test can time out.
func receive(stream *connect.BidiStreamForClient[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]) <-chan receiveResult {
	out := make(chan receiveResult, 1)
	go func() {
		msg, err := stream.Receive()
		out <- receiveResult{msg, err}
	}()
	return out
}

func awaitReceive(t *testing.T, what string, ch <-chan receiveResult) receiveResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return receiveResult{}
	}
}

// TestChannelAdmissionAfterLeaseLoss checks that an admission that passed
// the pre-lock stage after ownership was cleared and the channels were
// closed is answered unavailable, sends no configuration and leaves the hub
// unchanged.
func TestChannelAdmissionAfterLeaseLoss(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	cred := env.SessionCredential(dev)

	gate := &gateLease{held: true, entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	former := newInstance(t, env, gate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := former.channel(ctx, cred)
	_ = stream.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}})
	first := receive(stream)

	// The admission is paused at the ownership step; the instance loses the
	// lease and finishes its loss transition meanwhile.
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the admission did not reach the ownership step")
	}
	gate.lose()
	close(gate.proceed)

	r := awaitReceive(t, "the admission result", first)
	if r.err == nil {
		t.Fatalf("the former holder admitted the channel and sent %v", r.msg)
	}
	if testenv.Code(r.err) != connect.CodeUnavailable {
		t.Fatalf("admission after lease loss: %v", r.err)
	}
	if former.app.Service.Hub.Get(dev.ID) != nil || len(former.app.Service.Hub.All()) != 0 {
		t.Fatal("the hub holds a channel that was admitted after lease loss")
	}
}

// TestChannelRegisteredBeforeLeaseLossCloses checks that a channel that is
// registered when the real lease is lost closes during the loss transition,
// and that the device then connects to the successor and gets its
// configuration and heartbeat acknowledgements.
func TestChannelRegisteredBeforeLeaseLossCloses(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if err := env.App.Lease.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent := env.NewFakeAgent(dev)
	agent.Connect()
	conn := env.App.Service.Hub.Get(dev.ID)
	if conn == nil {
		t.Fatal("the holder has no channel for the device")
	}

	// The holder releases the lease, for example when it starts to drain.
	env.App.Lease.Release()
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open after the lease was lost")
	}
	select {
	case <-agent.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not see the channel close")
	}
	// A new admission to the former holder is refused.
	late := env.Clients(env.SessionCredential(dev)).Agent.Channel(context.Background())
	_ = late.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}})
	if _, err := late.Receive(); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("admission to the former holder: %v", err)
	}

	// The successor holds the lease: the device reconnects there.
	successor := newInstance(t, env, alwaysHeld{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := successor.channel(ctx, env.SessionCredential(dev))
	// The client request starts with its first message.
	if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}}); err != nil {
		t.Fatal(err)
	}
	cfg := awaitReceive(t, "the configuration", receive(stream))
	if cfg.err != nil || cfg.msg.GetEnvelope().GetConfiguration() == nil {
		t.Fatalf("first message from the successor: %v %v", cfg.msg, cfg.err)
	}
	if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{
		Sequence: 7, UpdateState: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE,
	}}}}); err != nil {
		t.Fatal(err)
	}
	ack := awaitReceive(t, "the heartbeat acknowledgement", receive(stream))
	if ack.err != nil || ack.msg.GetEnvelope().GetHeartbeatAck().GetSequence() != 7 {
		t.Fatalf("heartbeat acknowledgement from the successor: %v %v", ack.msg, ack.err)
	}
}

// importFixture exports a snapshot from device A and returns a second
// connected device B that has no copy of it, so a create on B imports first.
func importFixture(t *testing.T, env *testenv.Env, org *testenv.Org) (snapID string, devB *testenv.Device, agentB *testenv.FakeAgent) {
	t.Helper()
	ctx := context.Background()
	c := org.OwnerClient
	_, agentA := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	snapID = snap.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	if _, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "snapshot exported", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	_ = agentA
	devB, agentB = setupAgent(t, env, org, nil)
	return snapID, devB, agentB
}

func importCreateReq(orgID, snapID, deviceID string) *ownsandv1.CreateSandboxRequest {
	req := createReq(orgID)
	req.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}}
	req.Placement = &ownsandv1.Placement{DeviceId: &deviceID}
	return req
}

// TestImportWaitExpiryFailsCreate checks that an expired import wait on a
// connected device fails the create, releases its reservation and keeps a
// late import success from reviving it.
func TestImportWaitExpiryFailsCreate(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: 300 * time.Millisecond}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)

	var mu sync.Mutex
	var importCmdID string
	agentB.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetImportSnapshot() != nil {
			mu.Lock()
			importCmdID = cmd.GetCommandId()
			mu.Unlock()
			return nil // withhold the result while the channel stays connected
		}
		return fa.DefaultAnswer(cmd)
	})
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	sb := waitState(t, c, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatalf("end reason = %v", sb.GetEndReason())
	}
	d, _ := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: devB.ID}))
	if d.Msg.GetDevice().GetAllocation().GetVcpuCount() != 0 {
		t.Fatalf("reservation not released: %v", d.Msg.GetDevice().GetAllocation())
	}
	// A late success records the copy but does not create or revive.
	mu.Lock()
	cmdID := importCmdID
	mu.Unlock()
	if cmdID == "" {
		t.Fatal("agent never received the import command")
	}
	agentB.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: cmdID, Result: &ownsandv1.AgentCommandResult_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotResult{}},
	}}})
	testenv.Eventually(t, "late copy recorded", func() bool {
		var n int
		_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM snapshot_copies WHERE snapshot_id = $1 AND device_id = $2", snapID, devB.ID).Scan(&n)
		return n == 1
	})
	time.Sleep(300 * time.Millisecond)
	for _, cmd := range agentB.Commands() {
		if cmd.GetCreateSandbox().GetSandboxId() == id {
			t.Fatal("create dispatched for the failed sandbox")
		}
	}
	if got := getSandbox(t, c, org.ID, id); got.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		t.Fatalf("state after late result = %v", got.GetState())
	}
	// The released capacity accepts a new request (the copy now exists).
	again, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatalf("create after release: %v", err)
	}
	_ = again
}

// TestImportBeforeWaitExpiryCreates is the control: a timely import result
// leads to the create command and RUNNING.
func TestImportBeforeWaitExpiryCreates(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: 100 * time.Millisecond, CleanupPoll: time.Hour, ImportWait: 5 * time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	agentB.WaitFor("create after import", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetCreateSandbox().GetSandboxId() == created.Msg.GetSandbox().GetSandboxId()
	})
}

// offsetClock is the real clock plus an offset that a test moves forward.
type offsetClock struct{ offset atomic.Int64 }

func (c *offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *offsetClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

// TestChannelClosesAtLeaseExpiry checks that a holder that cannot confirm a
// renewal stops serving at the confirmed expiry: the connected agent channel
// closes, the instance no longer holds the lease and a new admission is
// refused. The test moves the local lease deadline clock past the TTL; the
// next scheduled renewal is 10 seconds away, so nothing renews in between.
func TestChannelClosesAtLeaseExpiry(t *testing.T) {
	clock := &offsetClock{}
	env := testenv.New(t, func(o *app.Options) { o.LeaseDeadlineNow = clock.Now })
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	agent := env.NewFakeAgent(dev)
	agent.Connect()
	conn := env.App.Service.Hub.Get(dev.ID)
	if conn == nil {
		t.Fatal("the holder has no channel for the device")
	}

	clock.Advance(lease.TTL)
	if env.App.Lease.Held() {
		t.Fatal("the instance holds the lease at the confirmed expiry")
	}
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open after the lease expired")
	}
	select {
	case <-agent.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not see the channel close")
	}
	late := env.Clients(env.SessionCredential(dev)).Agent.Channel(context.Background())
	_ = late.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}})
	if _, err := late.Receive(); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("admission after the lease expired: %v", err)
	}
}

// TestAggregateVCPUAllocationAboveInt32 checks that reservations whose vCPU
// sum exceeds the int32 range are reported exactly, and that placement uses
// the exact sum instead of failing in SQL.
func TestAggregateVCPUAllocationAboveInt32(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: math.MaxInt32, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 400})
	agent.WaitFor("state report ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetStateReport() != nil })
	// Two RUNNING sandboxes of 2147483647 vCPUs each sum to 4294967294.
	const total = 2 * int64(math.MaxInt32)
	for range 2 {
		id, _ := uuidv7.NewString()
		if _, err := env.DB.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, vcpu_count, memory_mib, disk_gib,
			guest_os, host_os, arch, network_mode, idle_ttl_ms, max_ttl_ms, max_deadline, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, 2, $4, 100, 1, 1, 1, 1, 1, 60000, 60000, now(), 1, 'alice', now())`, id, org.ID, dev.ID, int32(math.MaxInt32)); err != nil {
			t.Fatal(err)
		}
	}
	c := org.OwnerClient
	got, err := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	list, err := c.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID}))
	if err != nil || len(list.Msg.GetDevices()) != 1 {
		t.Fatalf("list devices: %v, %v", list, err)
	}
	for name, d := range map[string]*ownsandv1.Device{"get": got.Msg.GetDevice(), "list": list.Msg.GetDevices()[0]} {
		a := d.GetAllocation()
		if a.VcpuTotalCount == nil || a.GetVcpuTotalCount() != total || a.GetVcpuCount() != math.MaxInt32 {
			t.Errorf("%s allocation = %v, want total %d and a capped count", name, a, total)
		}
	}
	// The effective limit is 8589934588, so a sandbox of 2 vCPUs fits.
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID))); err != nil {
		t.Fatalf("create with room: %v", err)
	}
	// A limit below the exact allocation is a capacity outcome, not an SQL
	// integer overflow.
	if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
		OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: math.MaxInt32, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID))); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("create above the limit: %v", err)
	}
}
