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
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// exportFault makes the insert of snapshot parts, which the application of
// an export result runs inside its transaction, fail with a chosen SQLSTATE
// while it is on. It injects the database failure in the database itself, so
// the server code under test has no test seam.
type exportFault struct {
	t   *testing.T
	env *testenv.Env
}

func newExportFault(t *testing.T, env *testenv.Env) *exportFault {
	t.Helper()
	for _, stmt := range []string{
		"CREATE TABLE export_fault_state (code text)",
		`CREATE FUNCTION export_fault_raise() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE c text;
			BEGIN
				SELECT code INTO c FROM export_fault_state LIMIT 1;
				IF c IS NOT NULL THEN
					RAISE EXCEPTION 'injected fault' USING ERRCODE = c;
				END IF;
				RETURN NEW;
			END $$`,
		"CREATE TRIGGER export_fault BEFORE INSERT ON snapshot_parts FOR EACH ROW EXECUTE FUNCTION export_fault_raise()",
	} {
		if _, err := env.DB.Pool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &exportFault{t: t, env: env}
}

// on makes the insert fail with the SQLSTATE code.
func (f *exportFault) on(code string) {
	f.t.Helper()
	f.off()
	if _, err := f.env.DB.Pool.Exec(context.Background(), "INSERT INTO export_fault_state (code) VALUES ($1)", code); err != nil {
		f.t.Fatal(err)
	}
}

func (f *exportFault) off() {
	f.t.Helper()
	if _, err := f.env.DB.Pool.Exec(context.Background(), "DELETE FROM export_fault_state"); err != nil {
		f.t.Fatal(err)
	}
}

func retryTiming(window, backoff time.Duration) testenv.Option {
	return func(o *app.Options) {
		o.Timing.ExportApplyRetryWindow = window
		o.Timing.ExportApplyRetryBackoff = backoff
	}
}

func waitLog(t *testing.T, env *testenv.Env, needle string) {
	t.Helper()
	testenv.Eventually(t, "log "+needle, func() bool { return strings.Contains(env.Logs.String(), needle) })
}

// uploadedParts requests the upload URLs of the owning channel and returns
// the parts that its result would report.
func uploadedParts(t *testing.T, agent *testenv.FakeAgent, snapID, requestID string) []*ownsandv1.SnapshotPart {
	t.Helper()
	requestUploadURLs(agent, snapID, requestID)
	grant := waitGrant(agent, requestID)
	if grant.GetError() != nil || len(grant.GetPartUrls()) != 2 {
		t.Fatalf("grant = %v", grant)
	}
	return partsFor(grant)
}

// TestExportResultAppliesAfterTransientDatabaseError checks that a transient
// failure of the application transaction followed by a recovered database
// applies the original result on the same channel without a second export
// command.
func TestExportResultAppliesAfterTransientDatabaseError(t *testing.T) {
	env := testenv.New(t, retryTiming(10*time.Second, 20*time.Millisecond))
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]
	_, channel := exportColumns(t, env, snapID)
	parts := uploadedParts(t, agent, snapID, "urls")

	fault := newExportFault(t, env)
	fault.on("40001") // serialization_failure
	sendExportResult(agent, first, parts)
	waitLog(t, env, `"msg":"export_result_apply_retry"`)
	if got := snapshotExportState(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("state while the database fails = %v", got)
	}
	fault.off()

	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	waitLog(t, env, `"msg":"export_result_apply_recovered"`)
	if ids := exportCommandIDs(agent); len(ids) != 1 || ids[0] != first {
		t.Fatalf("export commands = %v, want only %s", ids, first)
	}
	if cmd, ch := exportColumns(t, env, snapID); cmd != first || ch != channel {
		t.Fatalf("owner columns = %q %q, want %q %q", cmd, ch, first, channel)
	}
	if got := len(manifestHashes(t, env, snapID)); got != 2 {
		t.Fatalf("part manifest has %d parts, want 2", got)
	}
	select {
	case <-agent.Closed():
		t.Fatal("the channel closed although the result was applied")
	default:
	}
	if strings.Contains(env.Logs.String(), `"msg":"export_result_apply_abandoned"`) {
		t.Fatal("the result was abandoned")
	}
}

// TestExportResultAbandonedClosesChannelAndRetries checks that a database
// that stays unavailable beyond the retry window closes the owning channel,
// that the replacement channel's state report fails the abandoned attempt
// with a clean-up job, and that an explicit retry sends a new command and
// reaches EXPORTED.
func TestExportResultAbandonedClosesChannelAndRetries(t *testing.T) {
	env := testenv.New(t, retryTiming(400*time.Millisecond, 20*time.Millisecond))
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]
	parts := uploadedParts(t, agent, snapID, "urls")

	fault := newExportFault(t, env)
	fault.on("08006") // connection_failure
	sendExportResult(agent, first, parts)
	select {
	case <-agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open after the retry window")
	}
	waitLog(t, env, `"msg":"export_result_apply_abandoned"`)
	if got := manifestHashes(t, env, snapID); len(got) != 0 {
		t.Fatalf("the failed application stored %d parts", len(got))
	}
	fault.off()

	agent.Connect()
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, snapID, first)]; !ok {
		t.Fatalf("no clean-up job for the abandoned attempt: %v", cleanupJobs(t, env))
	}

	answerExports(agent)
	if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("retry state = %v", got)
	}
	ids := waitExportCommands(t, agent, 2)
	if ids[1] == first {
		t.Fatal("the retry reused the command ID")
	}
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	if got := len(manifestHashes(t, env, snapID)); got != 2 {
		t.Fatalf("part manifest has %d parts, want 2", got)
	}
}

// TestExportResultPermanentErrorIsNotRetried checks that a permanent
// database error is applied once and logged, without retries and without
// closing the channel.
func TestExportResultPermanentErrorIsNotRetried(t *testing.T) {
	env := testenv.New(t, retryTiming(10*time.Second, 20*time.Millisecond))
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]
	parts := uploadedParts(t, agent, snapID, "urls")

	newExportFault(t, env).on("23514") // check_violation
	sendExportResult(agent, first, parts)
	waitLog(t, env, `"msg":"agent_command_result_apply_failed"`)
	time.Sleep(300 * time.Millisecond) // a wrongly scheduled retry would have logged by now
	logs := env.Logs.String()
	for _, unwanted := range []string{`"msg":"export_result_apply_retry"`, `"msg":"export_result_apply_abandoned"`} {
		if strings.Contains(logs, unwanted) {
			t.Fatalf("logs contain %s", unwanted)
		}
	}
	select {
	case <-agent.Closed():
		t.Fatal("the channel closed after a permanent error")
	default:
	}
}

// TestExportResultRetryStopsWhenSnapshotIsDeleted checks that a retry after
// the snapshot was deleted stores no manifest and ends without closing the
// channel.
func TestExportResultRetryStopsWhenSnapshotIsDeleted(t *testing.T) {
	env := testenv.New(t, retryTiming(10*time.Second, 20*time.Millisecond))
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]
	parts := uploadedParts(t, agent, snapID, "urls")

	fault := newExportFault(t, env)
	fault.on("40P01") // deadlock_detected
	sendExportResult(agent, first, parts)
	waitLog(t, env, `"msg":"export_result_apply_retry"`)
	if _, err := org.OwnerClient.Snapshot.DeleteSnapshot(context.Background(), connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	fault.off()
	waitLog(t, env, `"msg":"export_result_apply_recovered"`)
	if got := manifestHashes(t, env, snapID); len(got) != 0 {
		t.Fatalf("the deleted snapshot has %d parts", len(got))
	}
	select {
	case <-agent.Closed():
		t.Fatal("the channel closed")
	default:
	}
}
