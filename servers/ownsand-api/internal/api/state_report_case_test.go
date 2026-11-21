// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// epochAt returns a UUID v7 epoch whose timestamp is ms Unix milliseconds.
func epochAt(t *testing.T, ms int64) string {
	t.Helper()
	id, err := uuidv7.FromTime(time.UnixMilli(ms), strings.NewReader(strings.Repeat("\x11", 16)))
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

type sandboxRecord struct {
	state     ownsandv1.SandboxState
	endReason int16
	epoch     string
	counter   int64
}

func readSandboxRecord(t *testing.T, env *testenv.Env, id string) sandboxRecord {
	t.Helper()
	var r sandboxRecord
	var state int16
	var reason *int16
	if err := env.DB.Pool.QueryRow(context.Background(),
		"SELECT agent_state, end_reason, revision_epoch, revision_counter FROM sandboxes WHERE sandbox_id = $1", id).
		Scan(&state, &reason, &r.epoch, &r.counter); err != nil {
		t.Fatal(err)
	}
	r.state = ownsandv1.SandboxState(state)
	if reason != nil {
		r.endReason = *reason
	}
	return r
}

// TestStateReportMatchesSandboxIDsAndEpochsByValue checks that a report that
// spells a sandbox ID or a revision epoch in uppercase hex is the same report
// as the lowercase one: the listed sandbox is not ended for omission, a later
// epoch wins over an earlier one whatever the case, and the stored epoch is
// lowercase. A sandbox that the report omits still ends as lost.
func TestStateReportMatchesSandboxIDsAndEpochsByValue(t *testing.T) {
	f := newStateReportLockFixture(t)
	ctx := context.Background()
	created, err := f.org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(f.org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	omittedID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, f.org.OwnerClient, f.org.ID, omittedID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	// Both epochs are lowercase-letter spellings that sort the wrong way
	// once the newer one is uppercase: "01AB..." < "01aa..." byte-wise.
	older := epochAt(t, 0x01aa00000000)
	newer := epochAt(t, 0x01ab00000000)
	if !(strings.ToUpper(newer) < older) || !strings.Contains(older, "a") {
		t.Fatalf("test epochs do not exercise the case ordering: %s %s", older, newer)
	}
	if _, err := f.env.DB.Pool.Exec(ctx, "UPDATE sandboxes SET revision_epoch = $2, revision_counter = 7 WHERE sandbox_id = $1", f.sandboxID, older); err != nil {
		t.Fatal(err)
	}

	entry := f.agent.Sandbox(f.sandboxID)
	entry.SandboxId = strings.ToUpper(f.sandboxID)
	entry.State = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	entry.Revision = &ownsandv1.SandboxRevision{Epoch: strings.ToUpper(newer), Counter: 1}
	report := &ownsandv1.StateReport{StateEpoch: epochAt(t, 0x01ac00000000), Sandboxes: []*ownsandv1.SandboxStateEntry{entry}}
	if removed, err := f.env.App.Service.ReconcileStateReportForTest(ctx, f.dev.ID, report); err != nil || removed {
		t.Fatalf("report: removed=%v err=%v", removed, err)
	}

	got := readSandboxRecord(t, f.env, f.sandboxID)
	if got.state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("listed sandbox state = %v, want PAUSED (the uppercase later epoch must win and the ID must match)", got.state)
	}
	if got.epoch != newer || got.counter != 1 {
		t.Fatalf("stored revision = %s/%d, want %s/1 in lowercase", got.epoch, got.counter, newer)
	}
	omitted := readSandboxRecord(t, f.env, omittedID)
	if omitted.state != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || omitted.endReason != int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE) {
		t.Fatalf("omitted sandbox = %v/%d, want FAILED/LOST_ON_DEVICE", omitted.state, omitted.endReason)
	}
}

// TestStateReportMatchesSnapshotIDsByValue checks that a report that lists a
// snapshot in uppercase hex keeps the original copy of an exported snapshot
// on its device, keeps an unexported snapshot's record and keeps the copy row
// of another device, while a snapshot that the report omits keeps the
// existing omission rules.
func TestStateReportMatchesSnapshotIDsByValue(t *testing.T) {
	f := newStateReportLockFixture(t)
	ctx := context.Background()
	c := f.org.OwnerClient
	exported := plainSnapshot(t, f.org, f.sandboxID)
	if _, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: f.org.ID, SnapshotId: exported})); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "snapshot exported", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: f.org.ID, SnapshotId: exported}))
		return err == nil && s.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	plain := plainSnapshot(t, f.org, f.sandboxID)
	omitted := plainSnapshot(t, f.org, f.sandboxID)

	dev2, _ := setupAgent(t, f.env, f.org, nil)
	copyReport := &ownsandv1.StateReport{StateEpoch: epochAt(t, 0x01ac00000000), Snapshots: []*ownsandv1.SnapshotStateEntry{readySnapshotEntry(exported)}}
	if _, err := f.env.App.Service.ReconcileStateReportForTest(ctx, dev2.ID, copyReport); err != nil {
		t.Fatal(err)
	}
	if got := copyDevices(t, f.env, exported); len(got) != 1 || got[0] != dev2.ID {
		t.Fatalf("copy rows before the uppercase report = %v", got)
	}

	// The device of the original lists the exported and the plain snapshot in
	// uppercase; the second device lists its copy in uppercase.
	upper := func(id string) *ownsandv1.SnapshotStateEntry {
		return readySnapshotEntry(strings.ToUpper(id))
	}
	report := &ownsandv1.StateReport{
		StateEpoch: epochAt(t, 0x01ad00000000),
		Sandboxes:  []*ownsandv1.SandboxStateEntry{f.agent.Sandbox(f.sandboxID)},
		Snapshots:  []*ownsandv1.SnapshotStateEntry{upper(exported), upper(plain)},
	}
	if _, err := f.env.App.Service.ReconcileStateReportForTest(ctx, f.dev.ID, report); err != nil {
		t.Fatal(err)
	}
	copyReport = &ownsandv1.StateReport{StateEpoch: epochAt(t, 0x01ae00000000), Snapshots: []*ownsandv1.SnapshotStateEntry{upper(exported)}}
	if _, err := f.env.App.Service.ReconcileStateReportForTest(ctx, dev2.ID, copyReport); err != nil {
		t.Fatal(err)
	}

	snapshotDevice := func(id string) (device *string, found bool) {
		rows, err := f.env.DB.Pool.Query(ctx, "SELECT device_id::text FROM snapshots WHERE snapshot_id = $1", id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			found = true
			if err := rows.Scan(&device); err != nil {
				t.Fatal(err)
			}
		}
		return device, found
	}
	if d, ok := snapshotDevice(exported); !ok || d == nil || *d != f.dev.ID {
		t.Fatalf("the exported snapshot lost its device: found=%v device=%v", ok, d)
	}
	if d, ok := snapshotDevice(plain); !ok || d == nil || *d != f.dev.ID {
		t.Fatalf("the listed snapshot was deleted or lost its device: found=%v device=%v", ok, d)
	}
	if got := copyDevices(t, f.env, exported); len(got) != 1 || got[0] != dev2.ID {
		t.Fatalf("copy rows after the uppercase report = %v, want [%s]", got, dev2.ID)
	}
	// The omitted snapshot is unexported and READY: its record is deleted.
	if _, ok := snapshotDevice(omitted); ok {
		t.Fatal("the omitted snapshot still has a record")
	}
}

// TestStateReportOrdersRevisionEpochsByValue checks the revision rule alone:
// with the sandbox ID in canonical case, an uppercase spelling of a later
// epoch still wins, and an uppercase spelling of an earlier epoch still loses.
func TestStateReportOrdersRevisionEpochsByValue(t *testing.T) {
	f := newStateReportLockFixture(t)
	ctx := context.Background()
	older := epochAt(t, 0x01aa00000000)
	newer := epochAt(t, 0x01ab00000000)
	if !(strings.ToUpper(newer) < older) {
		t.Fatalf("test epochs do not exercise the case ordering: %s %s", older, newer)
	}
	if _, err := f.env.DB.Pool.Exec(ctx, "UPDATE sandboxes SET revision_epoch = $2, revision_counter = 7 WHERE sandbox_id = $1", f.sandboxID, older); err != nil {
		t.Fatal(err)
	}
	send := func(state ownsandv1.SandboxState, epoch string, counter uint64) {
		t.Helper()
		entry := f.agent.Sandbox(f.sandboxID)
		entry.State = state
		entry.Revision = &ownsandv1.SandboxRevision{Epoch: epoch, Counter: counter}
		report := &ownsandv1.StateReport{StateEpoch: epochAt(t, 0x01ac00000000), Sandboxes: []*ownsandv1.SandboxStateEntry{entry}}
		if _, err := f.env.App.Service.ReconcileStateReportForTest(ctx, f.dev.ID, report); err != nil {
			t.Fatal(err)
		}
	}
	send(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, strings.ToUpper(newer), 1)
	if got := readSandboxRecord(t, f.env, f.sandboxID); got.state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || got.epoch != newer || got.counter != 1 {
		t.Fatalf("after the later uppercase epoch: %v %s/%d, want PAUSED %s/1", got.state, got.epoch, got.counter, newer)
	}
	send(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, strings.ToUpper(older), 99)
	if got := readSandboxRecord(t, f.env, f.sandboxID); got.state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || got.epoch != newer {
		t.Fatalf("an earlier uppercase epoch changed the record to %v %s", got.state, got.epoch)
	}
}
