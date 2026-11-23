// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// saturationQueueFullWait is the shortened queue-full wait of these tests.
const saturationQueueFullWait = 300 * time.Millisecond

// saturationConfirmations is the number of consecutive timed-out fill sends
// that saturate requires before it reports a full queue.
const saturationConfirmations = 4

// committedStateQueueFullWait is the queue-full wait of the tests that cancel
// their request themselves. It is far longer than the tests, so a command
// send on the saturated channel waits for room that never comes until the
// test cancels the request, and the channel never closes on its own.
const committedStateQueueFullWait = 30 * time.Second

// rpcTimeout bounds the wait for a canceled RPC to return.
const rpcTimeout = 10 * time.Second

// failRPCAfterCommit calls rpc in a goroutine with a cancellable context,
// waits until committed reports the state that the request commits before it
// blocks on the saturated channel, then cancels the request and requires the
// RPC to fail. The wait for the committed state polls through the caller's
// own clients, so it does not depend on the request context or on how fast
// the request authorizes and writes. The channel must be saturated, and its
// queue-full wait must be longer than the test (committedStateQueueFullWait),
// so the send is still waiting for room when the cancellation arrives. The
// cancellation is also registered for cleanup.
func failRPCAfterCommit(t *testing.T, what string, rpc func(ctx context.Context) error, committed func() bool) {
	t.Helper()
	rpcCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rpcDone := make(chan error, 1)
	go func() { rpcDone <- rpc(rpcCtx) }()
	testenv.Eventually(t, what+" committed", committed)
	cancel()
	select {
	case err := <-rpcDone:
		if err == nil {
			t.Fatalf("%s on a saturated channel succeeded", what)
		}
	case <-time.After(rpcTimeout):
		t.Fatalf("%s did not return after its request was canceled", what)
	}
}

// exportFailsAfterClaim calls ExportSnapshot on a saturated channel, waits
// for the committed EXPORTING claim, then cancels the request, so the
// command send fails before enqueue and the handler reaches its compensation.
func exportFailsAfterClaim(t *testing.T, c *testenv.Clients, orgID, snapshotID string) {
	t.Helper()
	failRPCAfterCommit(t, "ExportSnapshot",
		func(ctx context.Context) error {
			_, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: orgID, SnapshotId: snapshotID}))
			return err
		},
		func() bool {
			return snapshotExportState(t, c, orgID, snapshotID) == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING
		})
}

// saturate stops the agent from reading its channel and fills the transport
// and the outbound queue of its channel, so that the next Send waits for
// room that never comes. The fill uses a caller deadline that is shorter
// than the queue-full wait, so it does not close the channel itself.
func saturate(t *testing.T, env *testenv.Env, agent *testenv.FakeAgent) {
	t.Helper()
	// The acknowledgement is the last envelope that the agent reads. The
	// stall is armed by the read loop itself when it receives that
	// acknowledgement, so it does not depend on whether the loop is already
	// blocked in a receive when the stall is requested (it can still be
	// processing an earlier configuration or state acknowledgement).
	agent.StallReadsAfter(func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 900 })
	agent.Heartbeat(900)
	agent.WaitFor("last heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 900 })
	conn := env.App.Service.Hub.Get(agent.Device.ID)
	if conn == nil {
		t.Fatal("the device has no channel")
	}
	// Large envelopes fill the HTTP/2 flow-control window of the stalled
	// stream, then the 256 envelope queue. The payload is random because
	// Connect compresses the messages and a repeated byte would shrink to
	// almost nothing.
	payload := make([]byte, 32<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	big := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{
		StunUrls: []string{hex.EncodeToString(payload)},
	}}}
	// One send that times out is not proof of a full queue: on a loaded
	// machine the writer goroutine can be descheduled for longer than the
	// send deadline while it still drains, and the room that it frees later
	// would let the send of the test succeed. The channel counts as
	// saturated after several consecutive timeouts.
	timeouts := 0
	for i := 0; ; i++ {
		if i > 2000+saturationConfirmations {
			t.Fatal("the stalled channel never filled")
		}
		ctx, cancel := context.WithTimeout(context.Background(), saturationQueueFullWait/4)
		err := conn.Send(ctx, big)
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			if timeouts++; timeouts >= saturationConfirmations {
				return
			}
		case err != nil:
			t.Fatalf("fill send %d: %v", i, err)
		default:
			timeouts = 0
		}
	}
}

// TestSaturatedChannelHeartbeatClosesChannel checks that a heartbeat on a
// channel whose agent stopped reading does not block the channel handler
// for ever: the channel closes with the queue-full cause, the device goes
// OFFLINE and its sandboxes show UNREACHABLE.
func TestSaturatedChannelHeartbeatClosesChannel(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	conn := env.App.Service.Hub.Get(dev.ID)

	saturate(t, env, agent)
	// The agent still sends; its heartbeat needs an acknowledgement that
	// cannot be queued.
	agent.Heartbeat(901)

	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the saturated channel stayed open")
	}
	if cause := context.Cause(conn.Context()); !errors.Is(cause, hub.ErrQueueFull) {
		t.Fatalf("close cause = %v, want ErrQueueFull", cause)
	}
	testenv.Eventually(t, "device offline", func() bool {
		d, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
		return err == nil && d.Msg.GetDevice().GetState() == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	})
	if sb := getSandbox(t, org.OwnerClient, org.ID, id); sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE {
		t.Fatalf("sandbox on the offline device = %v", sb.GetState())
	}
	if logs := env.Logs.String(); !strings.Contains(logs, "agent_channel_disconnected") || !strings.Contains(logs, "outbound queue stayed full") {
		t.Fatalf("no disconnect record with the queue-full cause in the logs")
	}
}

// TestSaturatedChannelEndsStalledWrite checks that a channel whose agent
// keeps its HTTP/2 stream open but stops reading does not keep the handler,
// the writer and the full outbound queue until the agent disconnects: after
// the writer drain timeout the handler aborts the blocked transport write
// and returns. The server sets no write timeout, so only that abort ends
// the write; the agent stays stalled and connected throughout.
func TestSaturatedChannelEndsStalledWrite(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	env.App.Service.Timing.WriterDrainTimeout = saturationQueueFullWait
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	conn := env.App.Service.Hub.Get(dev.ID)

	saturate(t, env, agent)
	agent.Heartbeat(901)

	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the saturated channel stayed open")
	}
	testenv.Eventually(t, "the blocked writer ended", func() bool {
		logs := env.Logs.String()
		return strings.Contains(logs, "agent_channel_writer_stalled") && strings.Contains(logs, "agent_channel_writer_ended")
	})
	if logs := env.Logs.String(); strings.Contains(logs, "agent_channel_writer_abort_failed") {
		t.Fatalf("the abort of the blocked write failed:\n%s", logs)
	}
}

// TestSaturatedChannelDoesNotBlockOtherDevices checks that a control event
// for a device whose agent stopped reading does not hold back the event of
// a healthy device, and that the saturated channel closes.
func TestSaturatedChannelDoesNotBlockOtherDevices(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	ctx := context.Background()
	org := env.NewOrg("alice")
	stalledDev, stalled := setupAgent(t, env, org, nil)
	healthyDev, healthy := setupAgent(t, env, org, nil)
	conn := env.App.Service.Hub.Get(stalledDev.ID)

	saturate(t, env, stalled)
	update := func(deviceID string, vcpu int32) {
		t.Helper()
		if _, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
			OrganizationId: org.ID, DeviceId: deviceID,
			Limits: &ownsandv1.DeviceLimits{VcpuCount: vcpu, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 100},
		})); err != nil {
			t.Fatal(err)
		}
	}
	// The event of the stalled device comes first on the single event
	// worker; the event of the healthy device follows.
	start := time.Now()
	update(stalledDev.ID, 5)
	update(healthyDev.ID, 6)
	healthy.WaitFor("configuration of the healthy device", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration().GetLimits().GetVcpuCount() == 6 })
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the healthy device waited %v for the stalled stream", elapsed)
	}

	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the saturated channel stayed open")
	}
	if cause := context.Cause(conn.Context()); !errors.Is(cause, hub.ErrQueueFull) {
		t.Fatalf("close cause = %v, want ErrQueueFull", cause)
	}
	testenv.Eventually(t, "stalled device offline", func() bool {
		d, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: stalledDev.ID}))
		return err == nil && d.Msg.GetDevice().GetState() == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	})
	// The healthy device stays online and keeps its acknowledgements.
	healthy.Heartbeat(2)
	healthy.WaitFor("heartbeat ack", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 2 })
	d, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: healthyDev.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if state := d.Msg.GetDevice().GetState(); state != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("healthy device = %v", state)
	}
}

// assertImportCreateFailed checks that the sandbox ended FAILED with
// CREATE_FAILED and that the device allocation no longer counts it.
func assertImportCreateFailed(t *testing.T, org *testenv.Org, sandboxID, deviceID string) {
	t.Helper()
	sb := waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatalf("end reason = %v", sb.GetEndReason())
	}
	d, err := org.OwnerClient.Device.GetDevice(context.Background(), connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: deviceID}))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Msg.GetDevice().GetAllocation().GetVcpuCount(); got != 0 {
		t.Fatalf("reservation not released: %v", d.Msg.GetDevice().GetAllocation())
	}
}

// TestImportSendDeadlineFailsCreate checks that an import command that a
// saturated, still connected channel cannot take before the import deadline
// fails the create and releases its capacity.
func TestImportSendDeadlineFailsCreate(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: 400 * time.Millisecond}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	env.App.Service.Hub.QueueFullWait = 30 * time.Second
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	saturate(t, env, agentB)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	assertImportCreateFailed(t, org, created.Msg.GetSandbox().GetSandboxId(), devB.ID)
	if conn := env.App.Service.Hub.Get(devB.ID); conn == nil || conn.Context().Err() != nil {
		t.Fatal("the channel must stay connected")
	}
}

// TestImportCreateSendDeadlineFailsCreate checks the same for the create
// command that follows a successful import: the import result arrives, the
// channel is then saturated, and the create send outlasts the deadline.
func TestImportCreateSendDeadlineFailsCreate(t *testing.T) {
	timing := api.Timing{HeartbeatTimeout: 45 * time.Second, CommandWait: time.Second, EventPoll: 50 * time.Millisecond, OfflineSweep: time.Hour, CleanupPoll: time.Hour, ImportWait: 6 * time.Second}
	env := testenv.New(t, func(o *app.Options) { o.Timing = &timing })
	env.App.Service.Hub.QueueFullWait = 30 * time.Second
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	var importCmdID atomic.Value
	agentB.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetImportSnapshot() != nil {
			importCmdID.Store(cmd.GetCommandId())
			return nil // the test answers once the channel is saturated
		}
		return fa.DefaultAnswer(cmd)
	})
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "import command received", func() bool { return importCmdID.Load() != nil })
	saturate(t, env, agentB)
	agentB.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: importCmdID.Load().(string), Result: &ownsandv1.AgentCommandResult_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotResult{}},
	}}})
	assertImportCreateFailed(t, org, created.Msg.GetSandbox().GetSandboxId(), devB.ID)
	// The create send itself reported the deadline, not the wait timer.
	if !strings.Contains(env.Logs.String(), "agent_command_send_failed") {
		t.Fatal("the create send did not fail on the import deadline")
	}
}
