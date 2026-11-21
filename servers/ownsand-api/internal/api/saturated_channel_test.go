// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// saturationQueueFullWait is the shortened queue-full wait of these tests.
const saturationQueueFullWait = 300 * time.Millisecond

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
	for i := 0; ; i++ {
		if i > 2000 {
			t.Fatal("the stalled channel never filled")
		}
		ctx, cancel := context.WithTimeout(context.Background(), saturationQueueFullWait/4)
		err := conn.Send(ctx, big)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatalf("fill send %d: %v", i, err)
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
