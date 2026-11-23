// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// importCopyFault faults the INSERT of every snapshot_copies row. The tests
// install it after their fixture, and every insert that it matches is the
// registration of an import result, because a state report only inserts rows
// after the fault was lifted.
func importCopyFault(t *testing.T, env *testenv.Env) *updateFault {
	return newInsertFault(t, env, "import_copy", "snapshot_copies", "true")
}

// importCommands counts the import commands for snapID that the agent got.
func importCommands(agent *testenv.FakeAgent, snapID string) int {
	n := 0
	for _, c := range agent.Commands() {
		if c.GetImportSnapshot().GetSnapshot().GetSnapshotId() == snapID {
			n++
		}
	}
	return n
}

// createSandboxCommands counts the create-sandbox commands that the agent got.
func createSandboxCommands(agent *testenv.FakeAgent) int {
	n := 0
	for _, c := range agent.Commands() {
		if c.GetCreateSandbox() != nil {
			n++
		}
	}
	return n
}

// TestImportResultAppliesAfterTransientError checks that a transient failure
// of the write of the copy row recovers on the same channel: exactly one row,
// one import command, the waiting create completes, and a later deletion of
// the snapshot includes the importing device.
func TestImportResultAppliesAfterTransientError(t *testing.T) {
	env := testenv.New(t, retryTiming(10*time.Second, 20*time.Millisecond))
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	fault := importCopyFault(t, env)
	fault.on("40001", 2) // serialization_failure for the first two calls

	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, org.OwnerClient, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	waitLog(t, env, `"msg":"import_result_apply_recovered"`)
	if got := fault.calls(); got != 3 {
		t.Fatalf("copy inserts = %d, want 3 (two failures and one success)", got)
	}
	if n := copyRowCount(t, env, snapID, devB.ID); n != 1 {
		t.Fatalf("copy rows after the retry = %d, want 1", n)
	}
	if n := importCommands(agentB, snapID); n != 1 {
		t.Fatalf("import commands = %d, want exactly one", n)
	}
	logs := env.Logs.String()
	for _, want := range []string{`"msg":"import_result_apply_retry"`, `"attempts":3`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `"msg":"import_result_apply_abandoned"`) || strings.Contains(logs, "agent_command_result_apply_failed") {
		t.Fatalf("the result was logged as a failure:\n%s", logs)
	}

	// The tracked copy puts the importer in the fanout of the deletion.
	deleteSnapshotRPC(t, org, snapID)
	agentB.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	agentB.Eventually("local copy removed", func() bool { return !agentB.HasSnapshot(snapID) })
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after the deletion = %d, want 0", n)
	}
	if n := deleteSnapshotCommands(agentB, snapID); n != 1 {
		t.Fatalf("delete commands = %d, want 1", n)
	}
}

// TestImportResultRetryAfterSnapshotDeletionSendsDelete checks that a retry
// that finds the snapshot deleted registers no row and makes the server send
// delete_snapshot to the importer, like a result that arrives late.
func TestImportResultRetryAfterSnapshotDeletionSendsDelete(t *testing.T) {
	// A long backoff leaves time to delete the snapshot before the next attempt.
	env := testenv.New(t, retryTiming(10*time.Second, 600*time.Millisecond))
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)
	fault := importCopyFault(t, env)
	fault.on("40001", 1<<30)

	sendImportResult(agentB, cmdID)
	waitLog(t, env, `"msg":"import_result_apply_retry"`)
	deleteSnapshotRPC(t, org, snapID)
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("the importer got %d delete commands before the retry", n)
	}
	fault.off()

	agentB.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	agentB.Eventually("local copy removed", func() bool { return !agentB.HasSnapshot(snapID) })
	waitLog(t, env, `"msg":"import_result_apply_recovered"`)
	waitLog(t, env, `"snapshot_import_result_late"`)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after the late retry = %d, want 0", n)
	}
	time.Sleep(300 * time.Millisecond)
	if n := deleteSnapshotCommands(agentB, snapID); n != 1 {
		t.Fatalf("delete commands = %d, want 1", n)
	}
	if strings.Contains(env.Logs.String(), `"msg":"import_result_apply_abandoned"`) {
		t.Fatal("the result was abandoned")
	}
}

// TestImportResultAbandonedClosesChannelAndRecoversCopy checks that a result
// that a permanent error or the end of the retry window leaves unapplied
// closes the channel that owns it, before any create command reaches the
// device, and that the state report of the replacement channel then records
// the copy that the agent lists.
func TestImportResultAbandonedClosesChannelAndRecoversCopy(t *testing.T) {
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
			env := testenv.New(t, retryTiming(tc.window, 20*time.Millisecond))
			org := env.NewOrg("alice")
			snapID, devB, agentB := importFixture(t, env, org)
			conn := env.App.Service.Hub.Get(devB.ID)
			if conn == nil {
				t.Fatal("the device has no channel")
			}
			importID := withholdImportResult(agentB)
			cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)
			fault := importCopyFault(t, env)
			fault.on(tc.code, 1<<30)

			sendImportResult(agentB, cmdID)
			select {
			case <-conn.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the channel stayed open")
			}
			if cause := causeText(conn); !strings.Contains(cause, "import-snapshot result could not be applied") {
				t.Fatalf("close cause = %q", cause)
			}
			waitLog(t, env, `"msg":"import_result_apply_abandoned"`)
			logs := env.Logs.String()
			for _, want := range []string{`"channel_current":true`, `"attempts":`} {
				if !strings.Contains(logs, want) {
					t.Fatalf("logs lack %s:\n%s", want, logs)
				}
			}
			if got := strings.Contains(logs, `"msg":"import_result_apply_retry"`); got != tc.retried {
				t.Fatalf("retry logged = %v, want %v", got, tc.retried)
			}
			if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
				t.Fatalf("copy rows while the write failed = %d, want 0", n)
			}
			if n := createSandboxCommands(agentB); n != 0 {
				t.Fatalf("create commands after an unapplied import = %d, want 0", n)
			}
			fault.off()

			// The agent still holds the copy, so the replacement channel's
			// state report records it.
			reconnect(agentB)
			testenv.Eventually(t, "copy recovered", func() bool { return copyRowCount(t, env, snapID, devB.ID) == 1 })
			if n := importCommands(agentB, snapID); n != 1 {
				t.Fatalf("import commands = %d, want exactly one", n)
			}
		})
	}
}

// TestImportResultRetryStopsOnReplacedChannel checks that the retry of a
// result whose channel was replaced stops without closing the replacement
// channel, and that the replacement's state report records the copy.
func TestImportResultRetryStopsOnReplacedChannel(t *testing.T) {
	env := testenv.New(t, retryTiming(10*time.Second, 1500*time.Millisecond))
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)
	fault := importCopyFault(t, env)
	fault.on("40001", 1<<30)

	sendImportResult(agentB, cmdID)
	waitLog(t, env, `"msg":"import_result_apply_retry"`)
	fault.off()
	reconnect(agentB)
	testenv.Eventually(t, "copy recovered", func() bool { return copyRowCount(t, env, snapID, devB.ID) == 1 })
	replacement := env.App.Service.Hub.Get(devB.ID)
	if replacement == nil {
		t.Fatal("no replacement channel")
	}
	time.Sleep(2 * time.Second) // a retry loop that outlived its channel would have closed the replacement by now
	if strings.Contains(env.Logs.String(), `"msg":"import_result_apply_abandoned"`) {
		t.Fatal("the result was abandoned although its channel closed")
	}
	connStaysOpen(t, replacement)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 1 {
		t.Fatalf("copy rows = %d, want 1", n)
	}
}
