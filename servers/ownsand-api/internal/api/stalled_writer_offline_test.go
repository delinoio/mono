// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// stalledWriterDrainTimeout is the writer drain timeout of the tests that
// take the device row lock after the channel closed. It is longer than the
// time that the test needs to take the lock, so the lock is held before the
// drain wait ends and the offline write starts.
const stalledWriterDrainTimeout = time.Second

// holdDeviceLock takes the row lock of a device in a separate transaction,
// the lock that the offline write of a closing channel waits for. The
// returned function releases it.
func holdDeviceLock(t *testing.T, env *testenv.Env, deviceID string) (release func()) {
	t.Helper()
	ctx := context.Background()
	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = holder.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	if _, err := holder.Exec(ctx, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE", deviceID); err != nil {
		t.Fatal(err)
	}
	return release
}

func deviceState(t *testing.T, org *testenv.Org, deviceID string) ownsandv1.DeviceState {
	t.Helper()
	d, err := org.OwnerClient.Device.GetDevice(context.Background(), connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: deviceID}))
	if err != nil {
		t.Fatal(err)
	}
	return d.Msg.GetDevice().GetState()
}

// closeSaturatedChannelUnderLock saturates the channel of the agent, closes
// it with a heartbeat and takes the device row lock as soon as the channel
// closed. It returns the release function of the lock.
func closeSaturatedChannelUnderLock(t *testing.T, env *testenv.Env, dev *testenv.Device, agent *testenv.FakeAgent) (release func()) {
	t.Helper()
	conn := env.App.Service.Hub.Get(dev.ID)
	saturate(t, env, agent)
	agent.Heartbeat(901)
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the saturated channel stayed open")
	}
	return holdDeviceLock(t, env, dev.ID)
}

// TestStalledWriterAbortedBeforeOfflineUpdate checks that the handler aborts
// the blocked transport write after the writer drain timeout even while the
// offline write waits for the device row lock, and that the offline write
// then completes once the lock is released. Before the abort was moved ahead
// of the offline write, a delayed offline write kept the blocked write, the
// handler and the outbound queue for as long as the lock was held.
func TestStalledWriterAbortedBeforeOfflineUpdate(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	env.App.Service.Timing.WriterDrainTimeout = stalledWriterDrainTimeout
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)

	release := closeSaturatedChannelUnderLock(t, env, dev, agent)
	testenv.Eventually(t, "the blocked write was aborted while the device row is locked", func() bool {
		return strings.Contains(env.Logs.String(), "agent_channel_writer_stalled")
	})
	waitForLockWaiter(t, env, "FROM devices WHERE device_id", "FOR UPDATE")
	// The offline write is still waiting for the lock: the device is ONLINE
	// and the handler has not finished.
	logs := env.Logs.String()
	if strings.Contains(logs, "agent_channel_writer_abort_failed") {
		t.Fatalf("the abort of the blocked write failed:\n%s", logs)
	}
	if strings.Contains(logs, `"msg":"device_offline"`) || strings.Contains(logs, "agent_channel_writer_ended") {
		t.Fatalf("the offline write or the handler finished under the device lock:\n%s", logs)
	}
	if state := deviceState(t, org, dev.ID); state != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device state under the lock = %v, want ONLINE", state)
	}

	release()
	testenv.Eventually(t, "device offline after the lock release", func() bool {
		return deviceState(t, org, dev.ID) == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	})
	testenv.Eventually(t, "the handler waited for the writer", func() bool {
		return strings.Contains(env.Logs.String(), "agent_channel_writer_ended")
	})
}

// TestStalledWriterReplacementChannelKeepsDeviceOnline checks that the
// protection of a replacement channel stays intact when the stalled writer is
// aborted before the offline write: a replacement channel that registers
// while the offline write waits for the device row lock keeps the device
// from going OFFLINE.
func TestStalledWriterReplacementChannelKeepsDeviceOnline(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	env.App.Service.Timing.WriterDrainTimeout = stalledWriterDrainTimeout
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)

	// The replacement authenticates before the lock is taken: the
	// authentication writes the device row.
	cred := env.SessionCredential(dev)

	release := closeSaturatedChannelUnderLock(t, env, dev, agent)
	// The old channel is unregistered as the current channel, then its
	// offline write waits for the lock.
	testenv.Eventually(t, "the old channel is unregistered", func() bool {
		return strings.Contains(env.Logs.String(), `"current":true`)
	})
	waitForLockWaiter(t, env, "FROM devices WHERE device_id", "FOR UPDATE")

	// The replacement registers before the offline write runs. It sends no
	// state report, so the device state stays the one of the old channel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := env.Clients(cred).Agent.Channel(ctx)
	defer func() { _ = stream.CloseRequest() }()
	// The stream opens on its first message. An envelope without a message
	// registers the channel and changes nothing else.
	if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: &ownsandv1.AgentEnvelope{}}); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "the replacement channel registered", func() bool {
		return env.App.Service.Hub.Get(dev.ID) != nil
	})
	release()
	testenv.Eventually(t, "the offline write was skipped", func() bool {
		return strings.Contains(env.Logs.String(), "device_offline_skipped")
	})
	if strings.Contains(env.Logs.String(), `"msg":"device_offline"`) {
		t.Fatal("the stale channel took the device offline under a replacement channel")
	}
	testenv.Eventually(t, "the handler waited for the writer", func() bool {
		return strings.Contains(env.Logs.String(), "agent_channel_writer_ended")
	})
	if state := deviceState(t, org, dev.ID); state != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		t.Fatalf("device state = %v, want ONLINE", state)
	}
}

// TestGracefulChannelCloseDoesNotAbortWriter checks that a channel whose
// writer is not blocked drains without an abort: the offline write runs and
// no stalled-writer record appears.
func TestGracefulChannelCloseDoesNotAbortWriter(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Timing.WriterDrainTimeout = stalledWriterDrainTimeout
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)

	agent.Close()
	testenv.Eventually(t, "device offline", func() bool {
		return deviceState(t, org, dev.ID) == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	})
	logs := env.Logs.String()
	for _, record := range []string{"agent_channel_writer_stalled", "agent_channel_writer_abort_failed", "agent_channel_writer_ended"} {
		if strings.Contains(logs, record) {
			t.Fatalf("a graceful close logged %s:\n%s", record, logs)
		}
	}
}
