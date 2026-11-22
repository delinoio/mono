// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// createSnapshotCommands counts the create-snapshot commands that a fake
// agent received.
func createSnapshotCommands(agent *testenv.FakeAgent) int {
	n := 0
	for _, cmd := range agent.Commands() {
		if cmd.GetCreateSnapshot() != nil {
			n++
		}
	}
	return n
}

// rejectCreateSnapshots makes the fake agent answer a create-snapshot command
// with an error, and forget the snapshot.
func rejectCreateSnapshots(agent *testenv.FakeAgent) *atomic.Int64 {
	var n atomic.Int64
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetCreateSnapshot() != nil {
			n.Add(1)
			return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "rejected by the test"}}
		}
		return fa.DefaultAnswer(cmd)
	})
	return &n
}

// snapshotResultEnv starts an environment with a running sandbox on one
// device and returns what a create-snapshot test needs. The fault is created
// by the caller after this returns, so it does not affect the set-up.
func snapshotResultEnv(t *testing.T, retryWindow, backoff time.Duration) (env *testenv.Env, org *testenv.Org, sandboxID string, agent *testenv.FakeAgent, conn *hub.Conn) {
	t.Helper()
	env = testenv.New(t, retryTiming(retryWindow, backoff))
	org = env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID = created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	conn = env.App.Service.Hub.Get(dev.ID)
	if conn == nil {
		t.Fatal("the device has no channel")
	}
	return env, org, sandboxID, agent, conn
}

func startSnapshot(t *testing.T, org *testenv.Org, sandboxID string) string {
	t.Helper()
	snap, err := org.OwnerClient.Snapshot.CreateSnapshot(context.Background(), connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	return snap.Msg.GetSnapshot().GetSnapshotId()
}

func waitSnapshotState(t *testing.T, org *testenv.Org, snapID string, want ownsandv1.SnapshotState) {
	t.Helper()
	testenv.Eventually(t, "snapshot "+want.String(), func() bool {
		return snapshotStateOf(t, org.OwnerClient, org.ID, snapID) == want
	})
}

// TestSnapshotResultAppliesAfterTransientError checks that a transient
// failure of the write of a create-snapshot result recovers to the state that
// the agent reported (READY for a success, FAILED for an error) on the same
// channel, without a second create command.
func TestSnapshotResultAppliesAfterTransientError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*testing.T, *testenv.Env) *updateFault
		setup func(*testenv.FakeAgent)
		want  ownsandv1.SnapshotState
	}{
		{name: "success result", fault: snapshotReadyFault, setup: func(*testenv.FakeAgent) {}, want: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY},
		{name: "error result", fault: snapshotFailFault, setup: func(a *testenv.FakeAgent) { rejectCreateSnapshots(a) }, want: ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, org, sandboxID, agent, conn := snapshotResultEnv(t, 10*time.Second, 20*time.Millisecond)
			tc.setup(agent)
			fault := tc.fault(t, env)
			fault.on("40001", 2) // serialization_failure for the first two calls

			snapID := startSnapshot(t, org, sandboxID)
			waitSnapshotState(t, org, snapID, tc.want)
			waitLog(t, env, `"msg":"snapshot_result_apply_recovered"`)
			logs := env.Logs.String()
			for _, want := range []string{`"msg":"snapshot_result_apply_retry"`, `"attempts":3`} {
				if !strings.Contains(logs, want) {
					t.Fatalf("logs lack %s:\n%s", want, logs)
				}
			}
			if strings.Contains(logs, `"msg":"snapshot_result_apply_abandoned"`) {
				t.Fatal("the result was abandoned")
			}
			if got := fault.calls(); got != 3 {
				t.Fatalf("update calls = %d, want 3", got)
			}
			if got := createSnapshotCommands(agent); got != 1 {
				t.Fatalf("create-snapshot commands = %d, want exactly one", got)
			}
			connStaysOpen(t, conn)
		})
	}
}

// TestSnapshotResultAbandonedClosesChannelAndReconciles checks that a result
// that a permanent error or the end of the retry window leaves unapplied
// closes the channel that owns it, and that the state report of the
// replacement channel then applies the agent's report of the snapshot.
func TestSnapshotResultAbandonedClosesChannelAndReconciles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    string
		window  time.Duration
		retried bool
	}{
		{name: "permanent error", code: "23514", window: 10 * time.Second},
		{name: "window ends", code: "40001", window: 400 * time.Millisecond, retried: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, org, sandboxID, agent, conn := snapshotResultEnv(t, tc.window, 20*time.Millisecond)
			fault := snapshotReadyFault(t, env)
			fault.on(tc.code, 1<<30)

			snapID := startSnapshot(t, org, sandboxID)
			select {
			case <-conn.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the channel stayed open")
			}
			if cause := causeText(conn); !strings.Contains(cause, "create-snapshot result could not be applied") {
				t.Fatalf("close cause = %q", cause)
			}
			waitLog(t, env, `"msg":"snapshot_result_apply_abandoned"`)
			logs := env.Logs.String()
			for _, want := range []string{`"channel_current":true`, `"attempts":`} {
				if !strings.Contains(logs, want) {
					t.Fatalf("logs lack %s:\n%s", want, logs)
				}
			}
			if got := strings.Contains(logs, `"msg":"snapshot_result_apply_retry"`); got != tc.retried {
				t.Fatalf("retry logged = %v, want %v", got, tc.retried)
			}
			if got := snapshotStateOf(t, org.OwnerClient, org.ID, snapID); got != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
				t.Fatalf("state while the write failed = %v", got)
			}
			fault.off()

			// The agent still holds the snapshot, so the replacement
			// channel's state report lists it as READY.
			agent.ResumeReads()
			reconnect(agent)
			waitSnapshotState(t, org, snapID, ownsandv1.SnapshotState_SNAPSHOT_STATE_READY)
			if got := createSnapshotCommands(agent); got != 1 {
				t.Fatalf("create-snapshot commands = %d, want exactly one", got)
			}
		})
	}
}

// TestSnapshotResultRetryKeepsOtherWriters checks that a retry after another
// writer changed the record does not overwrite it: a snapshot that was
// deleted stays deleted, and a snapshot that left CREATING keeps its state.
// Neither ends with a close of the channel.
func TestSnapshotResultRetryKeepsOtherWriters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		act   func(t *testing.T, env *testenv.Env, snapID string)
		check func(t *testing.T, env *testenv.Env, snapID string)
	}{
		{
			name: "deleted",
			act: func(t *testing.T, env *testenv.Env, snapID string) {
				if _, err := env.DB.Pool.Exec(context.Background(), "DELETE FROM snapshots WHERE snapshot_id = $1", snapID); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, env *testenv.Env, snapID string) {
				var n int
				if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM snapshots WHERE snapshot_id = $1", snapID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("snapshot rows = %d, %v", n, err)
				}
			},
		},
		{
			name: "superseded",
			act: func(t *testing.T, env *testenv.Env, snapID string) {
				if _, err := env.DB.Pool.Exec(context.Background(), "UPDATE snapshots SET state = $2 WHERE snapshot_id = $1", snapID, int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_DELETING)); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, env *testenv.Env, snapID string) {
				var state int16
				if err := env.DB.Pool.QueryRow(context.Background(), "SELECT state FROM snapshots WHERE snapshot_id = $1", snapID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_DELETING) {
					t.Fatalf("the replayed result overwrote the state: %d", state)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A long backoff leaves time to change the row before the next attempt.
			env, org, sandboxID, _, conn := snapshotResultEnv(t, 10*time.Second, 600*time.Millisecond)
			fault := snapshotReadyFault(t, env)
			fault.on("40001", 1<<30)
			snapID := startSnapshot(t, org, sandboxID)
			waitLog(t, env, `"msg":"snapshot_result_apply_retry"`)

			tc.act(t, env, snapID)
			fault.off()
			waitLog(t, env, `"msg":"snapshot_result_apply_recovered"`)
			tc.check(t, env, snapID)
			if strings.Contains(env.Logs.String(), `"msg":"snapshot_result_apply_abandoned"`) {
				t.Fatal("the result was abandoned")
			}
			connStaysOpen(t, conn)
		})
	}
}

// TestSnapshotResultRetryStopsOnReplacedChannel checks that the retry of a
// result whose channel was replaced stops without closing the replacement
// channel, and that the replacement's state report applies the agent's
// report of the snapshot.
func TestSnapshotResultRetryStopsOnReplacedChannel(t *testing.T) {
	env, org, sandboxID, agent, _ := snapshotResultEnv(t, 10*time.Second, 300*time.Millisecond)
	fault := snapshotReadyFault(t, env)
	fault.on("40001", 1<<30)
	snapID := startSnapshot(t, org, sandboxID)
	waitLog(t, env, `"msg":"snapshot_result_apply_retry"`)

	fault.off()
	reconnect(agent)
	waitSnapshotState(t, org, snapID, ownsandv1.SnapshotState_SNAPSHOT_STATE_READY)
	replacement := env.App.Service.Hub.Get(agent.Device.ID)
	if replacement == nil {
		t.Fatal("no replacement channel")
	}
	time.Sleep(time.Second) // a retry loop that outlived its channel would have closed the replacement by now
	if strings.Contains(env.Logs.String(), `"msg":"snapshot_result_apply_abandoned"`) {
		t.Fatal("the result was abandoned although its channel closed")
	}
	connStaysOpen(t, replacement)
}
