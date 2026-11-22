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
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestConfigurationSendsKeepTheNewestLimitsLast pauses the configuration send
// of the channel handler right after it read the limits, commits new limits
// and lets the control event processor send the change, then releases the
// first send. The channel handler and the event processor send to one channel
// without any other order, so without the per-channel configuration lock the
// older limits reach the agent last and the agent keeps them.
func TestConfigurationSendsKeepTheNewestLimitsLast(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
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
	before := len(configs())

	// The first send to read the limits pauses; later sends do not.
	var reads atomic.Int32
	paused := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	env.App.Service.SetAfterConfigLimitsReadForTest(func(string) {
		if reads.Add(1) == 1 {
			close(paused)
			<-release
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
	// Give the event processor time to run its send while the first one is
	// paused. With the lock it waits for the first send; without it, it
	// sends the new limits at once.
	time.Sleep(500 * time.Millisecond)
	releaseFirst()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	// Both sends deliver one configuration each.
	deadline := time.Now().Add(10 * time.Second)
	for len(configs()) < before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("received %d configurations, want %d", len(configs())-before, 2)
		}
		time.Sleep(10 * time.Millisecond)
	}
	all := configs()
	if last := all[len(all)-1].GetLimits().GetVcpuCount(); last != 6 {
		t.Fatalf("the last configuration has %d vCPUs, want the committed 6", last)
	}
	var stored int32
	if err := env.DB.Pool.QueryRow(ctx, "SELECT limit_vcpu_count FROM devices WHERE device_id = $1", dev.ID).Scan(&stored); err != nil || stored != 6 {
		t.Fatalf("stored vCPUs = %d, %v", stored, err)
	}
}
