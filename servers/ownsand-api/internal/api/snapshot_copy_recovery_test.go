// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// exportedSnapshot creates a snapshot of a new sandbox, exports it and waits until
// it is EXPORTED.
func exportedSnapshot(t *testing.T, org *testenv.Org) string {
	t.Helper()
	ctx := context.Background()
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snapID := plainSnapshot(t, org, sandboxID)
	if _, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "snapshot exported", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	return snapID
}

// plainSnapshot creates a snapshot of sandboxID and waits until it is READY.
func plainSnapshot(t *testing.T, org *testenv.Org, sandboxID string) string {
	t.Helper()
	ctx := context.Background()
	c := org.OwnerClient
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	return snapID
}

func readySnapshotEntry(id string) *ownsandv1.SnapshotStateEntry {
	return &ownsandv1.SnapshotStateEntry{SnapshotId: id, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY,
		GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX, HasMemoryState: true, SizeBytes: 3 << 20}
}

// copyDevices returns the device IDs of the snapshot_copies rows of a snapshot.
func copyDevices(t *testing.T, env *testenv.Env, snapID string) []string {
	t.Helper()
	rows, err := env.DB.Pool.Query(context.Background(), "SELECT device_id::text FROM snapshot_copies WHERE snapshot_id = $1 ORDER BY device_id", snapID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// stateReportLogs returns the recovered_copies field of every agent_state_report
// log line of deviceID, oldest first.
func stateReportLogs(t *testing.T, env *testenv.Env, deviceID string) []int {
	t.Helper()
	var out []int
	for _, line := range bytes.Split([]byte(env.Logs.String()), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"agent_state_report"`)) {
			continue
		}
		var rec struct {
			DeviceID        string `json:"device_id"`
			RecoveredCopies *int   `json:"recovered_copies"`
		}
		if err := json.Unmarshal(line, &rec); err != nil || rec.DeviceID != deviceID {
			continue
		}
		if rec.RecoveredCopies == nil {
			t.Fatalf("agent_state_report without recovered_copies: %s", line)
		}
		out = append(out, *rec.RecoveredCopies)
	}
	return out
}

// reportAndWait sends a state report of agent and waits until the server
// logged it as applied. It returns the recovered copy count of that report.
func reportAndWait(t *testing.T, env *testenv.Env, agent *testenv.FakeAgent) int {
	t.Helper()
	before := len(stateReportLogs(t, env, agent.Device.ID))
	agent.SendStateReport()
	testenv.Eventually(t, "state report applied", func() bool { return len(stateReportLogs(t, env, agent.Device.ID)) > before })
	logs := stateReportLogs(t, env, agent.Device.ID)
	return logs[before]
}

func hasImportCommand(agent *testenv.FakeAgent, snapID string) bool {
	for _, c := range agent.Commands() {
		if c.GetImportSnapshot().GetSnapshot().GetSnapshotId() == snapID {
			return true
		}
	}
	return false
}

// TestStateReportRecoversImportedSnapshotCopy checks that a state report
// that lists a READY copy of an exported snapshot without a snapshot_copies
// row creates the row once, so a targeted create uses the copy without an
// import and DeleteSnapshot reaches the device.
func TestStateReportRecoversImportedSnapshotCopy(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent1 := setupAgent(t, env, org, nil)
	snapID := exportedSnapshot(t, org)
	dev2, agent2 := setupAgent(t, env, org, nil)
	c := org.OwnerClient

	// The import result was lost: the device holds the copy, the server has
	// no row.
	if got := copyDevices(t, env, snapID); len(got) != 0 {
		t.Fatalf("copy rows before the report = %v", got)
	}
	agent2.AddSnapshot(readySnapshotEntry(snapID))
	if n := reportAndWait(t, env, agent2); n != 1 {
		t.Fatalf("recovered_copies = %d, want 1", n)
	}
	if got := copyDevices(t, env, snapID); len(got) != 1 || got[0] != dev2.ID {
		t.Fatalf("copy rows after the report = %v, want [%s]", got, dev2.ID)
	}
	// Repeating the report leaves one row and recovers nothing.
	if n := reportAndWait(t, env, agent2); n != 0 {
		t.Fatalf("recovered_copies of the repeated report = %d, want 0", n)
	}
	if got := copyDevices(t, env, snapID); len(got) != 1 {
		t.Fatalf("copy rows after the repeated report = %v", got)
	}

	// A create that targets the device uses the copy and sends no import.
	req := createReq(org.ID)
	req.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}}
	devID := dev2.ID
	req.Placement = &ownsandv1.Placement{DeviceId: &devID}
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req)); err != nil {
		t.Fatalf("targeted create: %v", err)
	}
	agent2.WaitFor("create from snapshot", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetCreateSandbox().GetSnapshotId() == snapID
	})
	if hasImportCommand(agent2, snapID) {
		t.Fatal("the targeted create imported a snapshot that the device already holds")
	}

	// DeleteSnapshot reaches the connected device that holds the recovered copy.
	if _, err := c.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	agent2.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	agent2.Eventually("local copy deleted", func() bool { return !agent2.HasSnapshot(snapID) })
	agent1.WaitFor("delete snapshot command of the original", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
}

// TestStateReportOmittedCopyLosesRow checks that a tracked copy that a
// report omits still loses its row.
func TestStateReportOmittedCopyLosesRow(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	_, _ = setupAgent(t, env, org, nil)
	snapID := exportedSnapshot(t, org)
	dev2, agent2 := setupAgent(t, env, org, nil)
	agent2.AddSnapshot(readySnapshotEntry(snapID))
	reportAndWait(t, env, agent2)
	if got := copyDevices(t, env, snapID); len(got) != 1 || got[0] != dev2.ID {
		t.Fatalf("copy rows = %v", got)
	}
	agent2.ForgetSnapshot(snapID)
	if n := reportAndWait(t, env, agent2); n != 0 {
		t.Fatalf("recovered_copies = %d", n)
	}
	if got := copyDevices(t, env, snapID); len(got) != 0 {
		t.Fatalf("copy rows after the omitting report = %v", got)
	}
}

// TestStateReportCreatesNoCopyRow checks the cases in which a listed copy
// is not recovered: a snapshot of another organization, a removed device,
// an incompatible host, and a snapshot that is not exported.
func TestStateReportCreatesNoCopyRow(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, _ = setupAgent(t, env, org, nil)
	snapID := exportedSnapshot(t, org)

	t.Run("foreign organization", func(t *testing.T) {
		other := env.NewOrg("bob")
		_, foreign := setupAgent(t, env, other, nil)
		foreign.AddSnapshot(readySnapshotEntry(snapID))
		if n := reportAndWait(t, env, foreign); n != 0 {
			t.Fatalf("recovered_copies = %d", n)
		}
		if got := copyDevices(t, env, snapID); len(got) != 0 {
			t.Fatalf("copy rows = %v", got)
		}
	})

	t.Run("incompatible host", func(t *testing.T) {
		dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64)
		mac := env.NewFakeAgent(dev)
		mac.HostOS, mac.Arch = ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64
		mac.AddSnapshot(readySnapshotEntry(snapID))
		mac.Connect()
		if n := reportAndWait(t, env, mac); n != 0 {
			t.Fatalf("recovered_copies = %d", n)
		}
		if got := copyDevices(t, env, snapID); len(got) != 0 {
			t.Fatalf("copy rows = %v", got)
		}
	})

	t.Run("removed device", func(t *testing.T) {
		dev, removed := setupAgent(t, env, org, nil)
		if _, err := env.DB.Pool.Exec(ctx, "UPDATE devices SET state = $2 WHERE device_id = $1", dev.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)); err != nil {
			t.Fatal(err)
		}
		removed.AddSnapshot(readySnapshotEntry(snapID))
		if n := reportAndWait(t, env, removed); n != 0 {
			t.Fatalf("recovered_copies = %d", n)
		}
		if got := copyDevices(t, env, snapID); len(got) != 0 {
			t.Fatalf("copy rows = %v", got)
		}
	})

	t.Run("not exported", func(t *testing.T) {
		created, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
		if err != nil {
			t.Fatal(err)
		}
		sandboxID := created.Msg.GetSandbox().GetSandboxId()
		waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		local := plainSnapshot(t, org, sandboxID)
		_, other := setupAgent(t, env, org, nil)
		other.AddSnapshot(readySnapshotEntry(local))
		if n := reportAndWait(t, env, other); n != 0 {
			t.Fatalf("recovered_copies = %d", n)
		}
		if got := copyDevices(t, env, local); len(got) != 0 {
			t.Fatalf("copy rows = %v", got)
		}
	})
}

// lockWaiters counts the sessions that wait for a lock in the test database.
func lockWaiters(t *testing.T, env *testenv.Env) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitLockWaiters(t *testing.T, env *testenv.Env, want int) {
	t.Helper()
	testenv.Eventually(t, "lock waiters", func() bool { return lockWaiters(t, env) >= want })
}

// TestStateReportCopyRecoveryCommitOrder holds the snapshot row lock from a
// test transaction and queues a state report and a snapshot deletion behind
// it in both orders.
func TestStateReportCopyRecoveryCommitOrder(t *testing.T) {
	setup := func(t *testing.T) (*testenv.Env, *testenv.Org, *testenv.FakeAgent, string, pgx.Tx) {
		env := testenv.New(t)
		org := env.NewOrg("alice")
		_, _ = setupAgent(t, env, org, nil)
		snapID := exportedSnapshot(t, org)
		_, agent2 := setupAgent(t, env, org, nil)
		agent2.AddSnapshot(readySnapshotEntry(snapID))
		tx, err := env.DB.Pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(context.Background(), "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", snapID); err != nil {
			t.Fatal(err)
		}
		return env, org, agent2, snapID, tx
	}
	deleteSnapshot := func(org *testenv.Org, snapID string) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := org.OwnerClient.Snapshot.DeleteSnapshot(context.Background(), connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
			done <- err
		}()
		return done
	}

	t.Run("report commits first", func(t *testing.T) {
		env, org, agent2, snapID, tx := setup(t)
		before := len(stateReportLogs(t, env, agent2.Device.ID))
		agent2.SendStateReport()
		waitLockWaiters(t, env, 1)
		done := deleteSnapshot(org, snapID)
		waitLockWaiters(t, env, 2)
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("delete snapshot: %v", err)
		}
		testenv.Eventually(t, "state report applied", func() bool { return len(stateReportLogs(t, env, agent2.Device.ID)) > before })
		if got := stateReportLogs(t, env, agent2.Device.ID)[before]; got != 1 {
			t.Fatalf("recovered_copies = %d, want 1", got)
		}
		// The deletion read the recovered row and recorded an event for the device.
		agent2.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
		})
		agent2.Eventually("local copy deleted", func() bool { return !agent2.HasSnapshot(snapID) })
		if got := copyDevices(t, env, snapID); len(got) != 0 {
			t.Fatalf("copy rows after the deletion = %v", got)
		}
	})

	t.Run("deletion commits first", func(t *testing.T) {
		env, org, agent2, snapID, tx := setup(t)
		before := len(stateReportLogs(t, env, agent2.Device.ID))
		done := deleteSnapshot(org, snapID)
		waitLockWaiters(t, env, 1)
		agent2.SendStateReport()
		waitLockWaiters(t, env, 2)
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("delete snapshot: %v", err)
		}
		testenv.Eventually(t, "state report applied", func() bool { return len(stateReportLogs(t, env, agent2.Device.ID)) > before })
		if got := stateReportLogs(t, env, agent2.Device.ID)[before]; got != 0 {
			t.Fatalf("recovered_copies = %d, want 0", got)
		}
		// The report finds no record: it creates no row and schedules the
		// stale-copy clean-up.
		agent2.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
		})
		agent2.Eventually("local copy deleted", func() bool { return !agent2.HasSnapshot(snapID) })
		if got := copyDevices(t, env, snapID); len(got) != 0 {
			t.Fatalf("copy rows = %v", got)
		}
	})
}
