// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func detachedExportedSnapshotWithCopy(t *testing.T, env *testenv.Env, org *testenv.Org) (string, string) {
	t.Helper()
	original, _ := setupAgent(t, env, org, nil)
	snapshotID := exportedSnapshot(t, org)
	if _, err := org.OwnerClient.Device.RemoveDevice(context.Background(), connect.NewRequest(&ownsandv1.RemoveDeviceRequest{
		OrganizationId: org.ID,
		DeviceId:       original.ID,
	})); err != nil {
		t.Fatal(err)
	}
	importer, agent := setupAgent(t, env, org, nil)
	agent.AddSnapshot(readySnapshotEntry(snapshotID))
	if recovered := reportAndWait(t, env, agent); recovered != 1 {
		t.Fatalf("state report recovered %d copies, want one", recovered)
	}
	return snapshotID, importer.ID
}

func exportedSnapshotOnDevice(t *testing.T, org *testenv.Org, deviceID string) string {
	t.Helper()
	ctx := context.Background()
	device := deviceID
	request := createReq(org.ID)
	request.Placement = &ownsandv1.Placement{DeviceId: &device}
	created, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snapshotID := plainSnapshot(t, org, sandboxID)
	if _, err := org.OwnerClient.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{
		OrganizationId: org.ID,
		SnapshotId:     snapshotID,
	})); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "second snapshot exported", func() bool {
		got, err := org.OwnerClient.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{
			OrganizationId: org.ID,
			SnapshotId:     snapshotID,
		}))
		return err == nil && got.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	return snapshotID
}

func waitForLockWaiterCountOnConn(t *testing.T, observer *pgx.Conn, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
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

func waitForOrganizationShareLockWaiter(t *testing.T, observer *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
			AND query LIKE '%FROM organizations WHERE organization_id = $1 FOR KEY SHARE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("DeleteSnapshot did not wait for the organization FOR KEY SHARE lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConcurrentOrganizationAndSnapshotDeletion(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"organization first", "snapshot first"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			env := testenv.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			org := env.NewOrg("alice")
			snapshotID, importerID := detachedExportedSnapshotWithCopy(t, env, org)
			observer, err := pgx.ConnectConfig(ctx, env.DB.Pool.Config().ConnConfig.Copy())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = observer.Close(context.Background()) }()

			holder, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()

			orgResult := make(chan error, 1)
			snapshotResult := make(chan error, 1)
			startDeleteOrganization := func() {
				go func() {
					_, err := org.OwnerClient.Organization.DeleteOrganization(ctx,
						connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
					orgResult <- err
				}()
			}
			startDeleteSnapshot := func() {
				go func() {
					_, err := org.OwnerClient.Snapshot.DeleteSnapshot(ctx,
						connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapshotID}))
					snapshotResult <- err
				}()
			}

			if order == "organization first" {
				secondSnapshotID := exportedSnapshotOnDevice(t, org, importerID)
				if _, err := holder.Exec(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", secondSnapshotID); err != nil {
					t.Fatal(err)
				}
				startDeleteOrganization()
				waitForLockWaiterCountOnConn(t, observer, 1)
				startDeleteSnapshot()
				waitForOrganizationShareLockWaiter(t, observer)
			} else {
				if _, err := holder.Exec(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR SHARE", snapshotID); err != nil {
					t.Fatal(err)
				}
				startDeleteSnapshot()
				waitForLockWaiterCountOnConn(t, observer, 1)
				startDeleteOrganization()
			}
			waitForLockWaiterCountOnConn(t, observer, 2)
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}

			orgErr := <-orgResult
			snapshotErr := <-snapshotResult
			for name, err := range map[string]error{"DeleteOrganization": orgErr, "DeleteSnapshot": snapshotErr} {
				requireNoDeadlock(t, name, err)
			}
			if orgErr != nil {
				t.Fatalf("DeleteOrganization: %v", orgErr)
			}
			if order == "organization first" {
				if testenv.Code(snapshotErr) != connect.CodeNotFound {
					t.Fatalf("DeleteSnapshot after organization deletion: %v, want not_found", snapshotErr)
				}
			} else if snapshotErr != nil {
				t.Fatalf("DeleteSnapshot before organization deletion: %v", snapshotErr)
			}

			var organizations, snapshots, copies, orgCleanup, snapshotCleanup int
			snapshotPrefix := "organizations/" + org.ID + "/snapshots/" + snapshotID + "/"
			for query, target := range map[string]*int{
				"SELECT count(*) FROM organizations WHERE organization_id = $1": &organizations,
				"SELECT count(*) FROM snapshots WHERE snapshot_id = $1":         &snapshots,
				"SELECT count(*) FROM snapshot_copies WHERE snapshot_id = $1":   &copies,
			} {
				arg := snapshotID
				if target == &organizations {
					arg = org.ID
				}
				if err := env.DB.Pool.QueryRow(ctx, query, arg).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM r2_cleanup_jobs WHERE prefix = $1", snapshotPrefix).Scan(&snapshotCleanup); err != nil {
				t.Fatal(err)
			}
			if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM r2_cleanup_jobs WHERE prefix = $1", "organizations/"+org.ID+"/").Scan(&orgCleanup); err != nil {
				t.Fatal(err)
			}
			if organizations != 0 || snapshots != 0 || copies != 0 || orgCleanup != 1 {
				t.Fatalf("rows after deletion: organizations=%d snapshots=%d copies=%d organization cleanup jobs=%d", organizations, snapshots, copies, orgCleanup)
			}
			var deviceState int16
			if err := env.DB.Pool.QueryRow(ctx, "SELECT state FROM devices WHERE device_id = $1", importerID).Scan(&deviceState); err != nil {
				t.Fatal(err)
			}
			if deviceState != int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED) {
				t.Fatalf("importer device state = %d, want REMOVED tombstone", deviceState)
			}
			if order == "snapshot first" {
				if snapshotCleanup != 1 {
					t.Fatalf("snapshot cleanup jobs = %d, want one", snapshotCleanup)
				}
				var eventCount int
				if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE device_id = $1 AND snapshot_id = $2 AND kind = $3", importerID, snapshotID, int16(events.KindSnapshotDeleted)).Scan(&eventCount); err != nil {
					t.Fatal(err)
				}
				if eventCount != 1 {
					t.Fatalf("snapshot deletion events for importer = %d, want one", eventCount)
				}
			}
		})
	}
}

func TestDeleteSnapshotStillRejectsReferencedSnapshot(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapshotID, importerID := detachedExportedSnapshotWithCopy(t, env, org)
	if _, err := org.OwnerClient.Snapshot.CreateTemplate(context.Background(), connect.NewRequest(&ownsandv1.CreateTemplateRequest{
		OrganizationId: org.ID,
		SnapshotId:     snapshotID,
		Name:           "base",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.Snapshot.DeleteSnapshot(context.Background(), connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{
		OrganizationId: org.ID,
		SnapshotId:     snapshotID,
	})); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("DeleteSnapshot for a referenced snapshot: %v, want failed_precondition", err)
	}
	var snapshots, templates, copies int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM snapshots WHERE snapshot_id = $1", snapshotID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM templates WHERE snapshot_id = $1", snapshotID).Scan(&templates); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM snapshot_copies WHERE snapshot_id = $1 AND device_id = $2", snapshotID, importerID).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || templates != 1 || copies != 1 {
		t.Fatalf("rows after failed deletion: snapshots=%d templates=%d copies=%d", snapshots, templates, copies)
	}
}
