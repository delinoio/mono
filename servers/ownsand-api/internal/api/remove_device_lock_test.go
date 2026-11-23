// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

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

// snapshotOnDevice creates a running sandbox on the device and a READY
// snapshot of it, and returns the snapshot ID.
func snapshotOnDevice(t *testing.T, org *testenv.Org) string {
	t.Helper()
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return plainSnapshot(t, org, sandboxID)
}

// snapshotRow reads the device link and the export state of a snapshot. The
// record is absent when found is false.
func snapshotRow(t *testing.T, env *testenv.Env, snapshotID string) (deviceID *string, export ownsandv1.SnapshotExportState, found bool) {
	t.Helper()
	var state int16
	err := env.DB.Pool.QueryRow(context.Background(), "SELECT device_id::text, export_state FROM snapshots WHERE snapshot_id = $1", snapshotID).Scan(&deviceID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return deviceID, ownsandv1.SnapshotExportState(state), true
}

// TestRemoveDevicePreservesSnapshotExportedDuringRemoval holds the row lock
// of a snapshot whose export is still running, starts the removal of its
// device and then lets the export finish. The removal must keep the exported
// snapshot, without a local copy and without a clean-up job. A snapshot that
// was never exported is deleted with the device.
func TestRemoveDevicePreservesSnapshotExportedDuringRemoval(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, _ := setupAgent(t, env, org, nil)
	exporting := snapshotOnDevice(t, org)
	local := snapshotOnDevice(t, org)
	if _, err := env.DB.Pool.Exec(ctx, "UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1",
		exporting, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING)); err != nil {
		t.Fatal(err)
	}

	// The export result transaction: it holds the snapshot row until it
	// commits the EXPORTED state.
	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", exporting); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- removeDevice(org, dev.ID) }()
	waitForLockWaiterCount(t, env, 1)
	if _, err := holder.Exec(ctx, "UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1",
		exporting, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		requireNoDeadlock(t, "RemoveDevice", err)
		t.Fatalf("RemoveDevice failed: %v", err)
	}

	deviceID, export, found := snapshotRow(t, env, exporting)
	if !found {
		t.Fatal("the snapshot that finished exporting during the removal was deleted")
	}
	if deviceID != nil || export != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED {
		t.Fatalf("snapshot device_id = %v, export state = %s, want no device and EXPORTED", deviceID, export)
	}
	if copies := copyDevices(t, env, exporting); len(copies) != 0 {
		t.Fatalf("exported snapshot keeps local copies on %v", copies)
	}
	if jobs := cleanupJobs(t, env); len(jobs) != 0 {
		t.Fatalf("clean-up jobs = %v, want none for a preserved snapshot", jobs)
	}
	if _, _, found := snapshotRow(t, env, local); found {
		t.Fatal("the local-only snapshot of the removed device was kept")
	}
	if r := readRemovalRecords(t, env, org.ID, dev.ID); r.removeAudits != 1 || r.removeEvents != 1 {
		t.Fatalf("removal records = %+v, want one audit event and one control event", r)
	}
}
