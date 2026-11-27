// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestControlLeaseHandoverCancelsBlockedLookup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	env.WaitForLease()
	env.App.Lease.Release()

	former := env.StartPeer()
	stopFormer := runHandoverLease(t, former)
	agent := former.NewFakeAgent(dev)
	agent.Connect()

	// Hold a PostgreSQL lock that the delete lookup will wait for. The
	// lock is independent of lease acquisition and device reconciliation.
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const lock = "SELECT pg_advisory_xact_lock(22780001)"
	if _, err := tx.Exec(ctx, lock); err != nil {
		t.Fatal(err)
	}
	lookupErr := make(chan error, 1)
	former.App.Control.SetLoadDeleteTargetForTest(func(deliveryCtx context.Context, _, _ string) (*api.SandboxRowForTest, error) {
		_, err := former.DB.Pool.Exec(deliveryCtx, lock)
		lookupErr <- err
		return nil, err
	})
	for _, kind := range []events.Kind{events.KindSandboxDeleteRequested, events.KindSnapshotDeleted} {
		if err := events.Write(ctx, env.DB.Pool, events.Event{
			Kind: kind, OrganizationID: org.ID, DeviceID: dev.ID, SandboxID: dev.ID, SnapshotID: dev.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		former.App.Control.PollEventsForTest(ctx)
	}()
	waitForLockWaiter(t, env, lock)

	released := make(chan struct{})
	go func() {
		stopFormer()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("lease release waited for the blocked delivery")
	}
	successor := env.StartPeer()
	runHandoverLease(t, successor)
	newAgent := successor.NewFakeAgent(dev)
	newAgent.Connect()
	t.Log("successor accepted and reconciled the agent on TLS HTTP/2 while PostgreSQL still held the lookup lock")

	select {
	case err := <-lookupErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked lookup returned %v, want lease cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease loss did not cancel the blocked delete lookup")
	}
	select {
	case <-pollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded poll did not stop")
	}
	for _, cmd := range agent.Commands() {
		if cmd.GetDeleteSnapshot() != nil {
			t.Fatal("old poll dispatched its next event")
		}
	}
	if commands := newAgent.Commands(); len(commands) != 0 {
		t.Fatalf("successor received old commands: %v", commands)
	}
	baseline, err := events.LatestID(ctx, env.DB.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := successor.App.Control.ProcessedCursor(); !ok || id != baseline {
		t.Fatalf("successor baseline = %d, %v, want %d, true", id, ok, baseline)
	}
	newAgent.Heartbeat(42)
	newAgent.WaitFor("successor heartbeat", func(e *ownsandv1.AgentEnvelope) bool { return e.GetHeartbeatAck().GetSequence() == 42 })
}
