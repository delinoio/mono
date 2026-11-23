// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// deleteSnapshotCommands counts the delete_snapshot commands for snapID that
// the agent received.
func deleteSnapshotCommands(agent *testenv.FakeAgent, snapID string) int {
	n := 0
	for _, c := range agent.Commands() {
		if c.GetDeleteSnapshot().GetSnapshotId() == snapID {
			n++
		}
	}
	return n
}

func copyRowCount(t *testing.T, env *testenv.Env, snapID, deviceID string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM snapshot_copies WHERE snapshot_id = $1 AND device_id = $2", snapID, deviceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func snapshotRowCount(t *testing.T, env *testenv.Env, snapID string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM snapshots WHERE snapshot_id = $1", snapID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deleteSnapshotRPC(t *testing.T, org *testenv.Org, snapID string) {
	t.Helper()
	if _, err := org.OwnerClient.Snapshot.DeleteSnapshot(context.Background(), connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
}

// startWithheldImport creates a sandbox from an exported snapshot on devB
// while agentB withholds the import result, and returns the import command
// ID once agentB received the command. agentB already holds the published
// local copy, as the real agent does before it reports success.
func startWithheldImport(t *testing.T, org *testenv.Org, snapID, devID string, agentB *testenv.FakeAgent, importID func() string) string {
	t.Helper()
	_, cmdID := startWithheldImportSandbox(t, org, snapID, devID, agentB, importID)
	return cmdID
}

// startWithheldImportSandbox behaves like startWithheldImport and also returns
// the ID of the sandbox that the create request made.
func startWithheldImportSandbox(t *testing.T, org *testenv.Org, snapID, devID string, agentB *testenv.FakeAgent, importID func() string) (sandboxID, cmdID string) {
	t.Helper()
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devID)))
	if err != nil {
		t.Fatal(err)
	}
	agentB.WaitFor("import command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetImportSnapshot() != nil })
	agentB.AddSnapshot(readySnapshotEntry(snapID))
	return created.Msg.GetSandbox().GetSandboxId(), importID()
}

// importResultLogged reports whether the server logged an agent_command_result
// record for the import command cmdID of snapID with the given result code.
func importResultLogged(env *testenv.Env, cmdID, snapID, result string) bool {
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		var rec struct {
			Msg        string `json:"msg"`
			CommandID  string `json:"command_id"`
			Command    string `json:"command"`
			Result     string `json:"result"`
			SnapshotID string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Msg == "agent_command_result" && rec.Command == "import_snapshot" && rec.CommandID == cmdID && rec.Result == result && rec.SnapshotID == snapID {
			return true
		}
	}
	return false
}

// TestImportResultAfterSnapshotDeletionSendsDelete checks that an import
// result that arrives after the snapshot record was deleted registers no
// copy row and makes the server send delete_snapshot to the reporting
// connection, so the local copy that the deletion fanout could not know is
// removed without a reconnect.
func TestImportResultAfterSnapshotDeletionSendsDelete(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)

	deleteSnapshotRPC(t, org, snapID)
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("the importer got %d delete commands before its result", n)
	}
	if !agentB.HasSnapshot(snapID) {
		t.Fatal("the importer lost its local copy before its result")
	}

	sendImportResult(agentB, cmdID)
	agentB.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	agentB.Eventually("local copy removed", func() bool { return !agentB.HasSnapshot(snapID) })
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after the late result = %d, want 0", n)
	}
	if n := snapshotRowCount(t, env, snapID); n != 0 {
		t.Fatalf("snapshot rows = %d, want 0", n)
	}
	testenv.Eventually(t, "late result logged", func() bool { return strings.Contains(env.Logs.String(), `"snapshot_import_result_late"`) })
	if strings.Contains(env.Logs.String(), "agent_command_result_apply_failed") {
		t.Fatalf("the late result was logged as an apply failure:\n%s", env.Logs.String())
	}
}

// TestImportResultBeforeSnapshotDeletionIsInFanout checks that a result that
// registers its copy first makes the deletion include the importer.
func TestImportResultBeforeSnapshotDeletionIsInFanout(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)

	sendImportResult(agentB, cmdID)
	testenv.Eventually(t, "copy recorded", func() bool { return copyRowCount(t, env, snapID, devB.ID) == 1 })

	deleteSnapshotRPC(t, org, snapID)
	agentB.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	agentB.Eventually("local copy removed", func() bool { return !agentB.HasSnapshot(snapID) })
	// The result path sent nothing: exactly one command comes from the fanout.
	time.Sleep(300 * time.Millisecond)
	if n := deleteSnapshotCommands(agentB, snapID); n != 1 {
		t.Fatalf("delete commands = %d, want 1", n)
	}
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after deletion = %d, want 0", n)
	}
}

// TestImportResultWaitsForInFlightSnapshotDeletion checks the lock order: a
// result that arrives while a deletion holds the snapshot row waits for it
// and, when the deletion commits, finds no snapshot.
func TestImportResultWaitsForInFlightSnapshotDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)

	// A deletion that has removed the row but not committed yet.
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "DELETE FROM snapshots WHERE snapshot_id = $1", snapID); err != nil {
		t.Fatal(err)
	}
	sendImportResult(agentB, cmdID)
	time.Sleep(500 * time.Millisecond)
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("the result was applied while the deletion was uncommitted: %d delete commands", n)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	agentB.WaitFor("delete snapshot command", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetCommand().GetDeleteSnapshot().GetSnapshotId() == snapID
	})
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows = %d, want 0", n)
	}
}

// TestImportResultRecordsOneCopy checks that an ordinary import of an
// existing snapshot records one copy row, creates the sandbox and sends no
// delete command.
func TestImportResultRecordsOneCopy(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)

	created, err := c.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 1 {
		t.Fatalf("copy rows after the import = %d, want 1", n)
	}
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("delete commands for an existing snapshot = %d, want 0", n)
	}
}

// TestImportResultKeepsExistingCopyRow checks that an import result for a
// device that already has a copy row (for example one that a state report
// recovered while the import ran) leaves one row, creates the sandbox and
// sends no delete command. The row is inserted while the import command is
// in flight; a row that exists before the create would skip the import, so
// the result would never reach the registration path.
func TestImportResultKeepsExistingCopyRow(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)

	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	agentB.WaitFor("import command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetImportSnapshot() != nil })
	agentB.AddSnapshot(readySnapshotEntry(snapID))
	cmdID := importID()
	if _, err := env.DB.Pool.Exec(ctx, "INSERT INTO snapshot_copies (snapshot_id, device_id) VALUES ($1, $2)", snapID, devB.ID); err != nil {
		t.Fatal(err)
	}

	sendImportResult(agentB, cmdID)
	waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 1 {
		t.Fatalf("copy rows with an existing row = %d, want 1", n)
	}
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("delete commands for an existing snapshot = %d, want 0", n)
	}
}

// TestFailedImportRegistersNoCopy checks that an import error registers no
// copy row and sends no delete command. The test first waits, within the
// bounded Eventually timeout, until the server processed the error result for
// the import command: it logged the result and failed the requested sandbox
// with CREATE_FAILED. The absence of side effects is asserted only after that,
// so a result that the server dropped or did not match fails the test.
func TestFailedImportRegistersNoCopy(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	sandboxID, cmdID := startWithheldImportSandbox(t, org, snapID, devB.ID, agentB, importID)

	agentB.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: cmdID, Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, Message: "import failed"},
	}}})
	testenv.Eventually(t, "the import error result is logged", func() bool {
		return importResultLogged(env, cmdID, snapID, ownsandv1.ErrorCode_ERROR_CODE_INTERNAL.String())
	})
	sb := waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if got := sb.GetEndReason(); got != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatalf("end reason after a failed import = %v, want CREATE_FAILED", got)
	}
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after a failed import = %d, want 0", n)
	}
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("delete commands after a failed import = %d, want 0", n)
	}
}
