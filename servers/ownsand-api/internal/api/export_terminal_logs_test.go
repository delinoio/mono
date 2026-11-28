// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type exportLog struct {
	Message          string `json:"msg"`
	Level            string `json:"level"`
	SnapshotID       string `json:"snapshot_id"`
	CommandID        string `json:"command_id"`
	CurrentCommandID string `json:"current_command_id"`
	Reason           string `json:"reason"`
	Parts            int    `json:"parts"`
}

func snapshotExportLogs(env *testenv.Env, snapshotID string, messages ...string) []exportLog {
	var records []exportLog
	for line := range strings.SplitSeq(env.Logs.String(), "\n") {
		var record exportLog
		if json.Unmarshal([]byte(line), &record) == nil && record.SnapshotID == snapshotID && slices.Contains(messages, record.Message) {
			records = append(records, record)
		}
	}
	return records
}

func waitExportFailureLog(t *testing.T, env *testenv.Env, snapshotID, commandID, reason string) {
	t.Helper()
	want := exportLog{Message: "export_failed", Level: "WARN", SnapshotID: snapshotID, CommandID: commandID, Reason: reason}
	testenv.Eventually(t, "export failure log for "+commandID, func() bool {
		return slices.Contains(snapshotExportLogs(env, snapshotID, "export_failed"), want)
	})
}

func checkTerminalExportLogs(t *testing.T, env *testenv.Env, snapshotID string, want ...exportLog) {
	t.Helper()
	got := snapshotExportLogs(env, snapshotID, "export_completed", "export_failed")
	if !slices.Equal(got, want) {
		t.Fatalf("snapshot %s terminal export logs = %+v, want %+v", snapshotID, got, want)
	}
}

func TestSnapshotExportTerminalLogsAfterCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		state  ownsandv1.SnapshotExportState
		result bool
	}{
		{"result completion", ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED, true},
		{"result failure", ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testExportResultCommitLogs(t, tc.state, tc.result)
		})
	}
	t.Run("replacement claim and stale result", testReplacementClaimExportLogs)
	t.Run("replacement report", testReplacementReportExportLogs)
}

func testExportResultCommitLogs(t *testing.T, terminal ownsandv1.SnapshotExportState, complete bool) {
	env := testenv.New(t, retryTiming(15*time.Second, 100*time.Millisecond))
	org := env.NewOrg("alice")
	snapshotID, agent := exportingSnapshot(t, env, org)
	commandID := waitExportCommands(t, agent, 1)[0]
	_, channelID := exportColumns(t, env, snapshotID)
	oldHash := sha256.Sum256([]byte("old manifest"))
	if _, err := env.DB.Pool.Exec(t.Context(), "INSERT INTO snapshot_parts (snapshot_id, part_number, size_bytes, sha256) VALUES ($1, 1, 1, $2)", snapshotID, oldHash[:]); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE reject_export_commit_enabled (enabled boolean)`,
		`INSERT INTO reject_export_commit_enabled VALUES (true)`,
		`CREATE FUNCTION reject_export_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF EXISTS (SELECT 1 FROM reject_export_commit_enabled WHERE enabled) THEN
					RAISE EXCEPTION 'test: export commit rejected' USING ERRCODE = '40001';
				END IF;
				RETURN NEW;
			END $$`,
		fmt.Sprintf(`CREATE CONSTRAINT TRIGGER reject_export_commit AFTER UPDATE ON snapshots
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
			WHEN (NEW.snapshot_id = '%s' AND NEW.export_state = %d)
			EXECUTE FUNCTION reject_export_commit()`, snapshotID, terminal),
	} {
		if _, err := env.DB.Pool.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}

	event := exportLog{Message: "export_failed", Level: "WARN", SnapshotID: snapshotID, CommandID: commandID, Reason: "no_parts"}
	var parts []*ownsandv1.SnapshotPart
	wantHash := oldHash
	if complete {
		wantHash = sha256.Sum256([]byte("new manifest"))
		parts = []*ownsandv1.SnapshotPart{{PartNumber: 1, SizeBytes: 1024, Sha256: wantHash[:]}}
		event = exportLog{Message: "export_completed", Level: "INFO", SnapshotID: snapshotID, CommandID: commandID, Parts: 1}
	}

	sendExportResult(agent, commandID, parts)
	waitLog(t, env, "test: export commit rejected")
	testenv.Eventually(t, "retry of the rejected commit", func() bool {
		records := snapshotExportLogs(env, snapshotID, "export_result_apply_retry")
		return len(records) > 0 && records[0].CommandID == commandID
	})
	checkRolledBackExport(t, env, org, snapshotID, commandID, channelID)
	checkExportManifest(t, env, snapshotID, oldHash[:])

	if _, err := env.DB.Pool.Exec(t.Context(), "DELETE FROM reject_export_commit_enabled"); err != nil {
		t.Fatal(err)
	}
	waitExportState(t, org.OwnerClient, org.ID, snapshotID, terminal)
	testenv.Eventually(t, "recovery of the original result", func() bool {
		records := snapshotExportLogs(env, snapshotID, "export_result_apply_recovered")
		return len(records) == 1 && records[0].CommandID == commandID
	})
	checkTerminalExportLogs(t, env, snapshotID, event)
	checkExportManifest(t, env, snapshotID, wantHash[:])
	if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, snapshotID, commandID)]; ok != !complete {
		t.Fatalf("cleanup job exists = %t after %s", ok, terminal)
	}
	waitExportCommands(t, agent, 1)
	t.Logf("rejected COMMIT preserved EXPORTING, the old manifest and no cleanup job or terminal log. Retry committed %s and logged one %s", terminal, event.Message)
}

func checkRolledBackExport(t *testing.T, env *testenv.Env, org *testenv.Org, snapshotID, commandID, channelID string) {
	t.Helper()
	if got := snapshotExportState(t, org.OwnerClient, org.ID, snapshotID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("rolled back export state = %v, want EXPORTING", got)
	}
	if gotCommand, gotChannel := exportColumns(t, env, snapshotID); gotCommand != commandID || gotChannel != channelID {
		t.Fatalf("rolled back export owner = (%s, %s), want (%s, %s)", gotCommand, gotChannel, commandID, channelID)
	}
	if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, snapshotID, commandID)]; ok {
		t.Fatal("rolled back export left a cleanup job")
	}
	checkTerminalExportLogs(t, env, snapshotID)
}

func checkExportManifest(t *testing.T, env *testenv.Env, snapshotID string, hash []byte) {
	t.Helper()
	if got := manifestHashes(t, env, snapshotID); len(got) != 1 || !bytes.Equal(got[0], hash) {
		t.Fatalf("manifest = %x, want [%x]", got, hash)
	}
}

func testReplacementClaimExportLogs(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapshotID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]

	oldChannelID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Pool.Exec(t.Context(), "UPDATE snapshots SET export_channel_id = $2 WHERE snapshot_id = $1", snapshotID, oldChannelID); err != nil {
		t.Fatal(err)
	}
	fault := newUpdateFault(t, env, "reject_export_claim", "snapshots",
		fmt.Sprintf("NEW.snapshot_id = '%s' AND NEW.export_command_id IS DISTINCT FROM OLD.export_command_id", snapshotID))
	fault.on("23514", 1)

	_, err = org.OwnerClient.Snapshot.ExportSnapshot(t.Context(), connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapshotID}))
	if connect.CodeOf(err) != connect.CodeInternal || fault.calls() != 1 {
		t.Fatalf("replacement RPC = %v, trigger calls = %d, want internal and one trigger call", err, fault.calls())
	}
	checkRolledBackExport(t, env, org, snapshotID, first, oldChannelID)
	waitExportCommands(t, agent, 1)

	fault.off()
	if got := exportSnapshotRPC(t, org.OwnerClient, org.ID, snapshotID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("replacement state = %v, want EXPORTING", got)
	}
	second := waitExportCommands(t, agent, 2)[1]
	if first == second {
		t.Fatal("replacement reused the abandoned command ID")
	}
	failureEvent := exportLog{Message: "export_failed", Level: "WARN", SnapshotID: snapshotID, CommandID: first, Reason: "owner_channel_replaced"}
	startEvent := exportLog{Message: "export_started", Level: "INFO", SnapshotID: snapshotID, CommandID: second}
	events := snapshotExportLogs(env, snapshotID, "export_started", "export_failed")
	if len(events) != 3 || events[1] != failureEvent || events[2] != startEvent {
		t.Fatalf("export logs = %+v, want the original start, old attempt failure and replacement start in order", events)
	}
	if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, snapshotID, first)]; !ok {
		t.Fatal("committed replacement did not record the old attempt's cleanup job")
	}

	hash := sha256.Sum256([]byte("replacement manifest"))
	if _, err := env.DB.Pool.Exec(t.Context(), "INSERT INTO snapshot_parts (snapshot_id, part_number, size_bytes, sha256) VALUES ($1, 1, 1, $2)", snapshotID, hash[:]); err != nil {
		t.Fatal(err)
	}
	lateHash := sha256.Sum256([]byte("late result"))
	sendExportResult(agent, first, []*ownsandv1.SnapshotPart{{PartNumber: 1, SizeBytes: 1024, Sha256: lateHash[:]}})
	// The stream handles the result before the following heartbeat.
	agent.Heartbeat(3033)
	waitHeartbeatAck(agent, 3033)
	stale := snapshotExportLogs(env, snapshotID, "export_result_stale")
	if len(stale) != 1 || stale[0].CommandID != first || stale[0].CurrentCommandID != second {
		t.Fatalf("stale result logs = %+v, want one for %s with current command %s", stale, first, second)
	}
	checkTerminalExportLogs(t, env, snapshotID, failureEvent)
	checkExportManifest(t, env, snapshotID, hash[:])
	if got := snapshotExportState(t, org.OwnerClient, org.ID, snapshotID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("state after the stale result = %v, want EXPORTING", got)
	}
	t.Log("rejected replacement claim produced no terminal log or cleanup job. Committed replacement logged the old failure before the new start. A late result logged only the stale diagnostic")
}

func testReplacementReportExportLogs(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	firstSnapshotID, agent := exportingSnapshot(t, env, org)
	firstCommand := waitExportCommands(t, agent, 1)[0]
	_, oldChannel := exportColumns(t, env, firstSnapshotID)
	client := org.OwnerClient.Snapshot

	first, err := client.GetSnapshot(t.Context(), connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: firstSnapshotID}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.CreateSnapshot(t.Context(), connect.NewRequest(&ownsandv1.CreateSnapshotRequest{
		OrganizationId: org.ID, SandboxId: first.Msg.GetSnapshot().GetSourceSandboxId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshotID := second.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "second snapshot ready", func() bool {
		resp, err := client.GetSnapshot(t.Context(), connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: secondSnapshotID}))
		return err == nil && resp.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	exportSnapshotRPC(t, org.OwnerClient, org.ID, secondSnapshotID)
	secondCommand := waitExportCommands(t, agent, 2)[1]

	agent.Close()
	// Disconnect and offline sweeps only move state away from ONLINE.
	// Authentication and capability reports do not assign state.
	for _, stmt := range []string{
		`CREATE FUNCTION reject_export_report() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'test: final device update rejected';
			END $$`,
		fmt.Sprintf(`CREATE TRIGGER reject_export_report BEFORE UPDATE OF state ON devices FOR EACH ROW
			WHEN (NEW.device_id = '%s' AND NEW.state = %d)
			EXECUTE FUNCTION reject_export_report()`, agent.Device.ID, ownsandv1.DeviceState_DEVICE_STATE_ONLINE),
	} {
		if _, err := env.DB.Pool.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	agent.OpenChannel()
	select {
	case <-agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("replacement channel stayed open after its failed initial report")
	}
	waitLog(t, env, "test: final device update rejected")
	checkRolledBackExport(t, env, org, firstSnapshotID, firstCommand, oldChannel)
	checkRolledBackExport(t, env, org, secondSnapshotID, secondCommand, oldChannel)

	if _, err := env.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_export_report ON devices"); err != nil {
		t.Fatal(err)
	}
	agent.Connect()
	for _, attempt := range []struct{ snapshotID, commandID string }{
		{firstSnapshotID, firstCommand},
		{secondSnapshotID, secondCommand},
	} {
		waitExportState(t, org.OwnerClient, org.ID, attempt.snapshotID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
		checkTerminalExportLogs(t, env, attempt.snapshotID, exportLog{
			Message: "export_failed", Level: "WARN", SnapshotID: attempt.snapshotID, CommandID: attempt.commandID, Reason: "owner_channel_replaced",
		})
		if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, attempt.snapshotID, attempt.commandID)]; !ok {
			t.Fatalf("committed report did not record cleanup for snapshot %s", attempt.snapshotID)
		}
	}
	t.Log("rejected state report preserved both exporting attempts without terminal logs or cleanup jobs. Acknowledged replacement report failed both attempts and logged each once")
}
