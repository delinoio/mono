// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/ownsand/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// stateReportLockFixture is a connected device with one running sandbox.
type stateReportLockFixture struct {
	env       *testenv.Env
	org       *testenv.Org
	dev       *testenv.Device
	agent     *testenv.FakeAgent
	sandboxID string
}

func newStateReportLockFixture(t *testing.T) *stateReportLockFixture {
	t.Helper()
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return &stateReportLockFixture{env: env, org: org, dev: dev, agent: agent, sandboxID: id}
}

// hold starts a transaction that runs lockQuery (with the device ID as $1)
// and keeps its locks until the returned function rolls it back.
func (f *stateReportLockFixture) hold(t *testing.T, lockQuery string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, lockQuery, f.dev.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = tx.Rollback(ctx)
		}
	})
	return func() {
		released = true
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Fatal(err)
		}
	}
}

// report builds a full state report of the agent's records with a fresh
// state epoch, so the device row shows whether the report was applied.
func (f *stateReportLockFixture) report(t *testing.T) *ownsandv1.StateReport {
	t.Helper()
	epoch, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return &ownsandv1.StateReport{StateEpoch: epoch, Sandboxes: []*ownsandv1.SandboxStateEntry{f.agent.Sandbox(f.sandboxID)}}
}

type reportOutcome struct {
	removed bool
	err     error
}

// reportAsync runs the report transaction and delivers its outcome.
func (f *stateReportLockFixture) reportAsync(report *ownsandv1.StateReport) <-chan reportOutcome {
	done := make(chan reportOutcome, 1)
	go func() {
		removed, err := f.env.App.Service.ReconcileStateReportForTest(context.Background(), f.dev.ID, report)
		done <- reportOutcome{removed: removed, err: err}
	}()
	return done
}

func waitReport(t *testing.T, done <-chan reportOutcome) reportOutcome {
	t.Helper()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("state report transaction failed: %v", out.err)
		}
		return out
	case <-time.After(15 * time.Second):
		t.Fatal("the state report transaction did not finish")
		return reportOutcome{}
	}
}

// removeAsync calls RemoveDevice and delivers its error.
func (f *stateReportLockFixture) removeAsync() <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := f.org.OwnerClient.Device.RemoveDevice(context.Background(), connect.NewRequest(&ownsandv1.RemoveDeviceRequest{
			OrganizationId: f.org.ID, DeviceId: f.dev.ID,
		}))
		done <- err
	}()
	return done
}

func (f *stateReportLockFixture) waitRemoval(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		requireNoDeadlock(t, "RemoveDevice", err)
		if err != nil {
			t.Fatalf("RemoveDevice failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RemoveDevice did not finish")
	}
}

// requireRemoved checks the removed device and its ended sandbox, and
// returns the stored state epoch of the device.
func (f *stateReportLockFixture) requireRemoved(t *testing.T) (stateEpoch string) {
	t.Helper()
	var state int16
	if err := f.env.DB.Pool.QueryRow(context.Background(), "SELECT state, state_epoch::text FROM devices WHERE device_id = $1", f.dev.ID).Scan(&state, &stateEpoch); err != nil {
		t.Fatal(err)
	}
	if ownsandv1.DeviceState(state) != ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
		t.Fatalf("device state = %v, want REMOVED", ownsandv1.DeviceState(state))
	}
	sb := getSandbox(t, f.org.OwnerClient, f.org.ID, f.sandboxID)
	if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || sb.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED {
		t.Fatalf("sandbox after removal = %v", sb)
	}
	return stateEpoch
}

// requireNoDeadlockLog fails when the server logged a deadlock.
func (f *stateReportLockFixture) requireNoDeadlockLog(t *testing.T) {
	t.Helper()
	logs := f.env.Logs.String()
	if strings.Contains(logs, "40P01") || strings.Contains(strings.ToLower(logs), "deadlock detected") {
		t.Fatalf("the server logged a deadlock:\n%s", logs)
	}
}

// TestStateReportAfterDeviceRemovalWins runs a removal that holds the device
// row lock while a full state report starts. The report and the removal
// used to lock the device row and the sandbox rows in opposite orders, which
// deadlocks with one running sandbox. The test pauses the removal after it
// updated the device row (it waits for the device's session row, which the
// test holds), lets the report start, and then releases the removal. The
// removal must finish without a deadlock, and the report, which waits for
// the device row first, must find the removed device and change nothing.
func TestStateReportAfterDeviceRemovalWins(t *testing.T) {
	f := newStateReportLockFixture(t)
	release := f.hold(t, "SELECT 1 FROM device_sessions WHERE device_id = $1 FOR UPDATE")
	removal := f.removeAsync()
	waitForLockWaiter(t, f.env, "DELETE FROM device_sessions")

	report := f.report(t)
	reported := f.reportAsync(report)
	waitForLockWaiter(t, f.env, "FROM devices WHERE device_id = $1 FOR UPDATE")
	release()
	f.waitRemoval(t, removal)

	if out := waitReport(t, reported); !out.removed {
		t.Fatal("the report that waited for the removal did not see the removed device")
	}
	f.requireNoDeadlockLog(t)
	if got := f.requireRemoved(t); got == report.GetStateEpoch() {
		t.Fatalf("the report of a removed device changed the device row (state_epoch %s)", got)
	}
}

// TestRemovalAfterStateReportWins is the other order: the report takes the
// device row lock first. The removal waits, then ends the sandbox with
// DEVICE_REMOVED. The report is applied before it.
func TestRemovalAfterStateReportWins(t *testing.T) {
	f := newStateReportLockFixture(t)
	release := f.hold(t, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE")

	report := f.report(t)
	reported := f.reportAsync(report)
	waitForLockWaiter(t, f.env, "FROM devices WHERE device_id = $1 FOR UPDATE")
	removal := f.removeAsync()
	waitForLockWaiter(t, f.env, "UPDATE devices SET state")
	release()
	f.waitRemoval(t, removal)

	if out := waitReport(t, reported); out.removed {
		t.Fatal("the report found a removed device although it took the device lock first")
	}
	f.requireNoDeadlockLog(t)
	if got := f.requireRemoved(t); got != report.GetStateEpoch() {
		t.Fatalf("state_epoch = %s, want %s: the report that took the device lock first was not applied", got, report.GetStateEpoch())
	}
}

// TestStateReportOfRemovedDeviceChangesNothing checks that a report that
// arrives after the removal committed neither restores the device nor
// replaces the end reason of its sandbox, and that the channel closes
// without an acknowledgement. The test marks the device removed in the
// database instead of calling RemoveDevice, which would close the channel
// itself.
func TestStateReportOfRemovedDeviceChangesNothing(t *testing.T) {
	f := newStateReportLockFixture(t)
	ctx := context.Background()
	if _, err := f.env.DB.Pool.Exec(ctx, "UPDATE devices SET state = $2 WHERE device_id = $1", f.dev.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)); err != nil {
		t.Fatal(err)
	}
	var epochBefore string
	if err := f.env.DB.Pool.QueryRow(ctx, "SELECT state_epoch::text FROM devices WHERE device_id = $1", f.dev.ID).Scan(&epochBefore); err != nil {
		t.Fatal(err)
	}
	sandboxBefore := getSandbox(t, f.org.OwnerClient, f.org.ID, f.sandboxID)
	acksBefore := countStateReportAcks(f.agent)

	// The agent reports a sandbox that ended on its own, with a newer revision.
	f.agent.SetSandbox(f.sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE)
	f.dev.Epoch = f.report(t).GetStateEpoch()
	f.agent.SendStateReport()

	select {
	case <-f.agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open after a state report of a removed device")
	}
	if !strings.Contains(f.env.Logs.String(), "agent_state_report_device_removed") {
		t.Fatal("the server did not log the report of a removed device")
	}
	var state int16
	var epochAfter string
	if err := f.env.DB.Pool.QueryRow(ctx, "SELECT state, state_epoch::text FROM devices WHERE device_id = $1", f.dev.ID).Scan(&state, &epochAfter); err != nil {
		t.Fatal(err)
	}
	if ownsandv1.DeviceState(state) != ownsandv1.DeviceState_DEVICE_STATE_REMOVED || epochAfter != epochBefore {
		t.Fatalf("the report changed the device row: state %v, state_epoch %s (was %s)", ownsandv1.DeviceState(state), epochAfter, epochBefore)
	}
	sandboxAfter := getSandbox(t, f.org.OwnerClient, f.org.ID, f.sandboxID)
	if sandboxAfter.GetState() != sandboxBefore.GetState() || sandboxAfter.GetEndReason() != sandboxBefore.GetEndReason() {
		t.Fatalf("the report changed the sandbox: %v, was %v", sandboxAfter, sandboxBefore)
	}
	if got := countStateReportAcks(f.agent); got != acksBefore {
		t.Fatalf("the agent got %d state report acknowledgements for a removed device, want none", got-acksBefore)
	}
}

func countStateReportAcks(a *testenv.FakeAgent) int {
	n := 0
	for _, e := range a.Received() {
		if e.GetStateAck().GetStateReport() != nil {
			n++
		}
	}
	return n
}
