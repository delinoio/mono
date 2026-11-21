// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// waitForLockWaiterCount waits until count transactions wait for a lock.
func waitForLockWaiterCount(t *testing.T, env *testenv.Env, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := env.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d transactions wait for a lock, want %d", waiting, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type removalRecords struct {
	removeTime, tombstoneExpireTime time.Time
	removeAudits, removeEvents      int
}

func readRemovalRecords(t *testing.T, env *testenv.Env, orgID, deviceID string) removalRecords {
	t.Helper()
	ctx := context.Background()
	var r removalRecords
	if err := env.DB.Pool.QueryRow(ctx, "SELECT remove_time, tombstone_expire_time FROM devices WHERE organization_id = $1 AND device_id = $2", orgID, deviceID).Scan(&r.removeTime, &r.tombstoneExpireTime); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND target_id = $2 AND action = $3",
		orgID, deviceID, int16(ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_REMOVED)).Scan(&r.removeAudits); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE device_id = $1 AND kind = $2", deviceID, int16(events.KindDeviceRemoved)).Scan(&r.removeEvents); err != nil {
		t.Fatal(err)
	}
	return r
}

func removeDevice(org *testenv.Org, deviceID string) error {
	_, err := org.OwnerClient.Device.RemoveDevice(context.Background(), connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: deviceID}))
	return err
}

// TestConcurrentRemoveDeviceWritesOneRemoval holds the device row lock so
// that two RemoveDevice calls are in flight at once, then lets both run.
// Both succeed, and the removal runs once: one audit event and one control
// event.
func TestConcurrentRemoveDeviceWritesOneRemoval(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE", dev.ID); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = removeDevice(org, dev.ID)
		}()
	}
	waitForLockWaiterCount(t, env, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, err := range errs {
		requireNoDeadlock(t, "RemoveDevice", err)
		if err != nil {
			t.Fatalf("RemoveDevice %d failed: %v", i, err)
		}
	}
	if r := readRemovalRecords(t, env, org.ID, dev.ID); r.removeAudits != 1 || r.removeEvents != 1 {
		t.Fatalf("removal records = %+v, want one audit event and one control event", r)
	}
}

// TestRemoveDeviceRepeatKeepsTombstone checks that a repeated removal of a
// visible tombstone succeeds, returns the tombstone and writes nothing.
func TestRemoveDeviceRepeatKeepsTombstone(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if err := removeDevice(org, dev.ID); err != nil {
		t.Fatal(err)
	}
	first := readRemovalRecords(t, env, org.ID, dev.ID)
	env.Clock.Advance(time.Minute)
	resp, err := org.OwnerClient.Device.RemoveDevice(context.Background(), connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if err != nil {
		t.Fatalf("repeated RemoveDevice: %v", err)
	}
	if got := resp.Msg.GetDevice(); got.GetState() != ownsandv1.DeviceState_DEVICE_STATE_REMOVED || got.GetDeviceId() != dev.ID {
		t.Fatalf("repeated RemoveDevice returned %v", got)
	}
	if second := readRemovalRecords(t, env, org.ID, dev.ID); second != first || first.removeAudits != 1 || first.removeEvents != 1 {
		t.Fatalf("records after the repeat = %+v, after the first call = %+v", second, first)
	}
}

// TestRemoveDeviceAfterOrganizationDeletion holds the device row lock so
// that DeleteOrganization stops after it took the organization lock, starts
// RemoveDevice for the same device, and lets the deletion commit. RemoveDevice
// must return not_found and must neither write a device removal audit event
// nor overwrite the tombstone timestamps the deletion wrote.
func TestRemoveDeviceAfterOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE", dev.ID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		deleted <- err
	}()
	waitForLockWaiterCount(t, env, 1)
	// A later clock makes an overwrite of the tombstone visible.
	env.Clock.Advance(time.Minute)
	removed := make(chan error, 1)
	go func() { removed <- removeDevice(org, dev.ID) }()
	waitForLockWaiterCount(t, env, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteOrganization: %v", err)
	}
	err = <-removed
	requireNoDeadlock(t, "RemoveDevice", err)
	if testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("RemoveDevice after the deletion: %v, want not_found", err)
	}
	r := readRemovalRecords(t, env, org.ID, dev.ID)
	if r.removeAudits != 0 || r.removeEvents != 1 {
		t.Fatalf("removal records = %+v, want no device removal audit event and the deletion's control event", r)
	}
	if want := env.Clock.Now().Add(-time.Minute); !r.removeTime.Equal(want) {
		t.Fatalf("remove_time = %v, want the deletion's %v", r.removeTime, want)
	}
}
