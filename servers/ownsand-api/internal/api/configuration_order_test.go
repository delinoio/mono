// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestConfigurationSendsKeepTheNewestLimitsLast pauses a configuration send
// after it reads the limits. It commits new limits and checks that the event
// processor waits for the first send before sending the new limits.
func TestConfigurationSendsKeepTheNewestLimitsLast(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	ctx := t.Context()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	agent.WaitFor("initial configuration", func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration() != nil })
	conn := env.App.Service.Hub.Live(dev.ID)
	if conn == nil {
		t.Fatal("the device has no live channel")
	}
	configs := func() []*ownsandv1.AgentConfiguration {
		var out []*ownsandv1.AgentConfiguration
		for _, e := range agent.Received() {
			if c := e.GetConfiguration(); c != nil {
				out = append(out, c)
			}
		}
		return out
	}

	latestDeliveryID := func(phase string) int64 {
		t.Helper()
		lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		id, err := events.LatestID(lookupCtx, env.DB.Pool)
		if err != nil {
			t.Fatalf("read %s delivery ID: %v", phase, err)
		}
		return id
	}
	waitForDelivery := func(phase string, id int64, sequence uint64) {
		t.Helper()
		testenv.Eventually(t, phase+" event processing", func() bool {
			cursor, ok := env.App.Control.ProcessedCursor()
			return ok && cursor >= id
		})
		// The heartbeat acknowledgement follows the queued configurations on
		// the same stream. Its receipt confirms that the agent read them.
		agent.Heartbeat(sequence)
		agent.WaitFor(phase+" heartbeat acknowledgement", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetHeartbeatAck().GetSequence() == sequence
		})
	}
	waitForDelivery("setup", latestDeliveryID("setup"), 1)
	before := len(configs())

	// The first send to read the limits pauses; later sends do not. A later
	// send that reads the limits while the first is still paused did not wait
	// for the configuration lock; the hook runs on the sender goroutine, so it
	// records that for the test goroutine to report.
	var reads atomic.Int32
	var released, unlockedRead atomic.Bool
	paused := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() {
		releaseOnce.Do(func() {
			released.Store(true)
			close(release)
		})
	}
	t.Cleanup(releaseFirst)
	env.App.Service.SetAfterConfigLimitsReadForTest(func(string) {
		if reads.Add(1) == 1 {
			close(paused)
			<-release
			return
		}
		if !released.Load() {
			unlockedRead.Store(true)
		}
	})
	t.Cleanup(func() { env.App.Service.SetAfterConfigLimitsReadForTest(nil) })

	firstDone := make(chan error, 1)
	go func() { firstDone <- env.App.Service.SendConfigurationForTest(ctx, conn) }()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("the first configuration send did not read the limits")
	}

	// The first send holds limits of 4 vCPUs. Commit new limits, which writes
	// the configuration event that the event processor sends.
	if _, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 200}})); err != nil {
		t.Fatal(err)
	}
	updateID := latestDeliveryID("limits update")
	// Wait until the event processor's send is blocked on the configuration
	// lock behind the paused first send. Without the lock it reads the limits
	// at once, which the hook records.
	testenv.Eventually(t, "the second configuration send to wait for the configuration lock", func() bool {
		return conn.ConfigLockWaiters() == 1 || unlockedRead.Load()
	})
	if unlockedRead.Load() {
		t.Fatal("the second configuration send read the limits without the configuration lock")
	}
	releaseFirst()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("manual configuration send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the manual configuration send")
	}

	waitForDelivery("limits update", updateID, 2)
	after := configs()[before:]
	if len(after) != 2 {
		t.Fatalf("received %d configurations after setup, want 2", len(after))
	}
	first := after[0].GetLimits().GetVcpuCount()
	last := after[1].GetLimits().GetVcpuCount()
	if first != 4 || last != 6 {
		t.Fatalf("received configurations with %d then %d vCPUs, want 4 then 6", first, last)
	}
	var stored int32
	if err := env.DB.Pool.QueryRow(ctx, "SELECT limit_vcpu_count FROM devices WHERE device_id = $1", dev.ID).Scan(&stored); err != nil || stored != 6 {
		t.Fatalf("stored vCPUs = %d, %v", stored, err)
	}
}
