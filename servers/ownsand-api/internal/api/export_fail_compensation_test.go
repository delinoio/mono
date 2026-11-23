// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// exportFailFault makes the guarded update that ends an export attempt whose
// command send failed (EXPORTING to EXPORT_FAILED, matched by its query text)
// fail with a chosen SQLSTATE for its first calls. It injects the failure in
// the database itself, so the server code under test has no test seam. The
// trigger counts the calls in a sequence, which survives the rollback of the
// failed statement, and it ignores every other writer of the transition (the
// state report of a replacement channel).
type exportFailFault struct {
	t   *testing.T
	env *testenv.Env
}

func newExportFailFault(t *testing.T, env *testenv.Env) *exportFailFault {
	t.Helper()
	for _, stmt := range []string{
		"CREATE TABLE export_fail_state (code text, max_failures int)",
		"CREATE SEQUENCE export_fail_calls",
		`CREATE FUNCTION export_fail_raise() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE c text; lim int; n bigint;
			BEGIN
				IF position('export_command_id = $2 AND export_state = $4' in current_query()) = 0 THEN
					RETURN NEW;
				END IF;
				n := nextval('export_fail_calls');
				SELECT code, max_failures INTO c, lim FROM export_fail_state LIMIT 1;
				IF c IS NOT NULL AND n <= lim THEN
					RAISE EXCEPTION 'injected fault' USING ERRCODE = c;
				END IF;
				RETURN NEW;
			END $$`,
		`CREATE TRIGGER export_fail_fault BEFORE UPDATE ON snapshots FOR EACH ROW
			WHEN (OLD.export_state = 2 AND NEW.export_state = 4) EXECUTE FUNCTION export_fail_raise()`,
	} {
		if _, err := env.DB.Pool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &exportFailFault{t: t, env: env}
}

// on makes the first max calls of the update fail with the SQLSTATE code.
func (f *exportFailFault) on(code string, max int) {
	f.t.Helper()
	f.off()
	if _, err := f.env.DB.Pool.Exec(context.Background(), "INSERT INTO export_fail_state (code, max_failures) VALUES ($1, $2)", code, max); err != nil {
		f.t.Fatal(err)
	}
}

func (f *exportFailFault) off() {
	f.t.Helper()
	if _, err := f.env.DB.Pool.Exec(context.Background(), "DELETE FROM export_fail_state"); err != nil {
		f.t.Fatal(err)
	}
}

// calls returns how many times the update reached a matching row.
func (f *exportFailFault) calls() int {
	f.t.Helper()
	var n int
	if err := f.env.DB.Pool.QueryRow(context.Background(), "SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM export_fail_calls").Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func exportFailTiming(budget, backoff time.Duration) testenv.Option {
	return func(o *app.Options) {
		o.Timing.ExportFailTimeout = budget
		o.Timing.ExportApplyRetryBackoff = backoff
	}
}

// failedExportSend sets up a device with a ready snapshot, saturates the
// agent channel and calls ExportSnapshot; it cancels the request once the
// EXPORTING claim is committed, so the command send fails before enqueue
// while the channel is still open. It returns after the RPC failed; the
// failure compensation of the handler may still run (it does not depend on
// the request context).
func failedExportSend(t *testing.T, env *testenv.Env) (org *testenv.Org, snapID string, agent *testenv.FakeAgent, conn *hub.Conn) {
	t.Helper()
	env.App.Service.Hub.QueueFullWait = committedStateQueueFullWait
	ctx := context.Background()
	org = env.NewOrg("alice")
	c := org.OwnerClient
	dev, agent := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snapID = plainSnapshot(t, org, sandboxID)
	live := env.App.Service.Hub.Get(dev.ID)
	if live == nil {
		t.Fatal("the device has no channel")
	}

	saturate(t, env, agent)
	exportFailsAfterClaim(t, c, org.ID, snapID)
	return org, snapID, agent, live
}

func waitConnClosed(t *testing.T, conn *hub.Conn) {
	t.Helper()
	select {
	case <-conn.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open")
	}
}

func connStaysOpen(t *testing.T, conn *hub.Conn) {
	t.Helper()
	select {
	case <-conn.Done():
		t.Fatal("the channel closed")
	default:
	}
}

// TestExportFailUpdateRetriesTransientError checks that a transient database
// error of the update that ends an unsent export attempt is retried inside
// the budget: the attempt ends as EXPORT_FAILED on the same channel, the
// channel stays open, and a repeat export dispatches exactly one command.
func TestExportFailUpdateRetriesTransientError(t *testing.T) {
	env := testenv.New(t, exportFailTiming(10*time.Second, 20*time.Millisecond))
	fault := newExportFailFault(t, env)
	fault.on("40001", 2) // serialization_failure for the first two calls
	org, snapID, agent, conn := failedExportSend(t, env)
	c := org.OwnerClient

	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	waitLog(t, env, `"reason":"send_failed"`)
	logs := env.Logs.String()
	for _, want := range []string{`"msg":"export_fail_transition_retry"`, `"applied":true`, `"attempts":3`, `"reason":"send_failed"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `"msg":"export_fail_transition_failed"`) {
		t.Fatal("the compensation was reported as failed")
	}
	if got := fault.calls(); got != 3 {
		t.Fatalf("update calls = %d, want 3 (two injected failures and the success)", got)
	}
	connStaysOpen(t, conn)
	if ids := exportCommandIDs(agent); len(ids) != 0 {
		t.Fatalf("export commands before the retry = %v", ids)
	}

	// The channel is still open: the agent reads again and the retry runs.
	agent.ResumeReads()
	if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("retry state = %v", got)
	}
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	if ids := exportCommandIDs(agent); len(ids) != 1 {
		t.Fatalf("export commands after the retry = %v, want exactly one", ids)
	}
}

// TestExportFailUpdateUnresolvedClosesChannel checks that an update that a
// permanent error or the end of the budget leaves unresolved closes the
// channel that still owns the attempt, keeps the unavailable response, and
// that the state report of the replacement channel then fails the abandoned
// attempt so that an explicit retry sends one new command.
func TestExportFailUpdateUnresolvedClosesChannel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code         string
		budget       time.Duration
		moreThanOnce bool
	}{
		{name: "permanent error", code: "23514", budget: 10 * time.Second},
		{name: "budget ends", code: "40001", budget: 400 * time.Millisecond, moreThanOnce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, exportFailTiming(tc.budget, 20*time.Millisecond))
			fault := newExportFailFault(t, env)
			fault.on(tc.code, 1<<30)
			org, snapID, agent, conn := failedExportSend(t, env)
			c := org.OwnerClient

			waitConnClosed(t, conn)
			waitLog(t, env, `"msg":"export_fail_transition_failed"`)
			logs := env.Logs.String()
			for _, want := range []string{`"channel_closed":true`, `"applied":false`, `"reason":"send_failed"`, `"send_error"`} {
				if !strings.Contains(logs, want) {
					t.Fatalf("logs lack %s:\n%s", want, logs)
				}
			}
			if tc.moreThanOnce != strings.Contains(logs, `"msg":"export_fail_transition_retry"`) {
				t.Fatalf("retry logged = %v, want %v:\n%s", !tc.moreThanOnce, tc.moreThanOnce, logs)
			}
			if got := snapshotExportState(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
				t.Fatalf("state while the compensation failed = %v", got)
			}
			if jobs := cleanupJobs(t, env); len(jobs) != 0 {
				t.Fatalf("the direct path recorded a clean-up job: %v", jobs)
			}
			fault.off()

			// The replacement channel's state report fails the abandoned
			// attempt, and a retry sends exactly one command.
			agent.ResumeReads()
			reconnect(agent)
			waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
			if ids := exportCommandIDs(agent); len(ids) != 0 {
				t.Fatalf("export commands before the retry = %v", ids)
			}
			if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
				t.Fatalf("retry state = %v", got)
			}
			waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
			if ids := exportCommandIDs(agent); len(ids) != 1 {
				t.Fatalf("export commands after the retry = %v, want exactly one", ids)
			}
		})
	}
}

// TestExportFailUpdateZeroRowsIsResolved checks that an update that affects
// no row ends the compensation without a close: the row names a newer
// attempt, was deleted, or the replacement channel's state report already
// failed the attempt. None of them changes another attempt's row and none
// closes the current channel.
func TestExportFailUpdateZeroRowsIsResolved(t *testing.T) {
	type scenario struct {
		name string
		// act changes the world while the compensation waits for its next
		// attempt, and returns the channel that must stay open (nil when
		// the old channel is the only one).
		act func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string, agent *testenv.FakeAgent) (stay *hub.Conn)
		// check verifies the row afterwards.
		check func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string)
	}
	const newerCommand = "0190a000-0000-7000-8000-000000000604"
	for _, sc := range []scenario{
		{
			name: "newer attempt",
			act: func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string, agent *testenv.FakeAgent) *hub.Conn {
				if _, err := env.DB.Pool.Exec(context.Background(), "UPDATE snapshots SET export_command_id = $2 WHERE snapshot_id = $1", snapID, newerCommand); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			check: func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string) {
				if got := snapshotExportState(t, org.OwnerClient, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
					t.Fatalf("the newer attempt's state = %v, want EXPORTING", got)
				}
				if cmd, _ := exportColumns(t, env, snapID); cmd != newerCommand {
					t.Fatalf("export_command_id = %q, want %q", cmd, newerCommand)
				}
			},
		},
		{
			name: "deleted snapshot",
			act: func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string, agent *testenv.FakeAgent) *hub.Conn {
				if _, err := org.OwnerClient.Snapshot.DeleteSnapshot(context.Background(), connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			check: func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string) {
				var n int
				if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM snapshots WHERE snapshot_id = $1", snapID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("snapshot rows = %d, %v", n, err)
				}
			},
		},
		{
			name: "settled by the replacement channel",
			act: func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string, agent *testenv.FakeAgent) *hub.Conn {
				// The replacement channel replaces the saturated one, and its
				// state report fails the attempt (owner_channel_replaced).
				agent.ResumeReads()
				reconnect(agent)
				return env.App.Service.Hub.Get(agent.Device.ID)
			},
			check: func(t *testing.T, env *testenv.Env, org *testenv.Org, snapID string) {
				waitExportState(t, org.OwnerClient, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
			},
		},
	} {
		t.Run(sc.name, func(t *testing.T) {
			// A long backoff leaves time to change the row before the next attempt.
			env := testenv.New(t, exportFailTiming(10*time.Second, 600*time.Millisecond))
			fault := newExportFailFault(t, env)
			fault.on("40001", 1<<30)
			ctx := context.Background()
			env.App.Service.Hub.QueueFullWait = committedStateQueueFullWait
			org := env.NewOrg("alice")
			c := org.OwnerClient
			_, agent := setupAgent(t, env, org, nil)
			created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			if err != nil {
				t.Fatal(err)
			}
			sandboxID := created.Msg.GetSandbox().GetSandboxId()
			waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			snapID := plainSnapshot(t, org, sandboxID)
			saturate(t, env, agent)
			exportFailsAfterClaim(t, c, org.ID, snapID)
			waitLog(t, env, `"msg":"export_fail_transition_retry"`)

			stay := sc.act(t, env, org, snapID, agent)
			waitLog(t, env, `"reason":"send_failed"`)
			logs := env.Logs.String()
			if !strings.Contains(logs, `"applied":false`) {
				t.Fatalf("the zero-row update was reported as applied:\n%s", logs)
			}
			if strings.Contains(logs, `"msg":"export_fail_transition_failed"`) {
				t.Fatalf("a zero-row update was reported as failed:\n%s", logs)
			}
			sc.check(t, env, org, snapID)
			if stay != nil {
				connStaysOpen(t, stay)
			}
			if got := fault.calls(); got < 1 {
				t.Fatalf("update calls = %d", got)
			}
		})
	}
}
