// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func snapshotExportState(t *testing.T, c *testenv.Clients, orgID, snapshotID string) ownsandv1.SnapshotExportState {
	t.Helper()
	s, err := c.Snapshot.GetSnapshot(context.Background(), connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: orgID, SnapshotId: snapshotID}))
	if err != nil {
		t.Fatal(err)
	}
	return s.Msg.GetSnapshot().GetExportState()
}

func waitExportState(t *testing.T, c *testenv.Clients, orgID, snapshotID string, want ownsandv1.SnapshotExportState) {
	t.Helper()
	testenv.Eventually(t, "export state "+want.String(), func() bool { return snapshotExportState(t, c, orgID, snapshotID) == want })
}

func exportSnapshotRPC(t *testing.T, c *testenv.Clients, orgID, snapshotID string) ownsandv1.SnapshotExportState {
	t.Helper()
	resp, err := c.Snapshot.ExportSnapshot(context.Background(), connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: orgID, SnapshotId: snapshotID}))
	if err != nil {
		t.Fatalf("export snapshot: %v", err)
	}
	return resp.Msg.GetSnapshot().GetExportState()
}

// exportCommandIDs returns the IDs of the export commands that the agent
// received, oldest first.
func exportCommandIDs(agent *testenv.FakeAgent) []string {
	var ids []string
	for _, cmd := range agent.Commands() {
		if cmd.GetExportSnapshot() != nil {
			ids = append(ids, cmd.GetCommandId())
		}
	}
	return ids
}

// holdExports makes the fake agent accept export commands without an answer.
func holdExports(agent *testenv.FakeAgent) {
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetExportSnapshot() != nil {
			return nil
		}
		return fa.DefaultAnswer(cmd)
	})
}

// answerExports restores the default behavior, which uploads and answers.
func answerExports(agent *testenv.FakeAgent) { agent.SetOnCommand(nil) }

// waitExportCommands waits until the agent received n export commands.
func waitExportCommands(t *testing.T, agent *testenv.FakeAgent, n int) []string {
	t.Helper()
	var ids []string
	testenv.Eventually(t, "export commands", func() bool {
		ids = exportCommandIDs(agent)
		return len(ids) >= n
	})
	if len(ids) != n {
		t.Fatalf("export commands = %v, want %d", ids, n)
	}
	return ids
}

// reconnect replaces the channel of the agent: the old one closes and the
// agent connects again with a full state report.
func reconnect(agent *testenv.FakeAgent) {
	agent.Close()
	agent.Connect()
}

func snapshotPrefixOf(orgID, snapshotID string) string {
	return "organizations/" + orgID + "/snapshots/" + snapshotID + "/"
}

func attemptPrefixOf(orgID, snapshotID, attemptID string) string {
	return snapshotPrefixOf(orgID, snapshotID) + "attempts/" + attemptID + "/"
}

func manifestHashes(t *testing.T, env *testenv.Env, snapshotID string) [][]byte {
	t.Helper()
	rows, err := env.DB.Pool.Query(context.Background(), "SELECT sha256 FROM snapshot_parts WHERE snapshot_id = $1 ORDER BY part_number", snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func exportColumns(t *testing.T, env *testenv.Env, snapshotID string) (command, channel string) {
	t.Helper()
	if err := env.DB.Pool.QueryRow(context.Background(),
		"SELECT COALESCE(export_command_id::text, ''), COALESCE(export_channel_id::text, '') FROM snapshots WHERE snapshot_id = $1", snapshotID).Scan(&command, &channel); err != nil {
		t.Fatal(err)
	}
	return command, channel
}

// partsFor builds the result parts that a fake agent reports for the URLs of
// a grant: the hash of a part is the hash of its URL, so the manifest tells
// which attempt's URLs produced it.
func partsFor(grant *ownsandv1.SignedUrlGrant) []*ownsandv1.SnapshotPart {
	var parts []*ownsandv1.SnapshotPart
	for _, u := range grant.GetPartUrls() {
		sum := sha256.Sum256([]byte(u.GetUrl()))
		parts = append(parts, &ownsandv1.SnapshotPart{PartNumber: u.GetPartNumber(), SizeBytes: 1 << 20, Sha256: sum[:]})
	}
	return parts
}

func sendExportResult(agent *testenv.FakeAgent, commandID string, parts []*ownsandv1.SnapshotPart) {
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: &ownsandv1.AgentCommandResult{
		CommandId: commandID, Result: &ownsandv1.AgentCommandResult_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotResult{Parts: parts, SizeBytes: 2 << 20}},
	}}})
}

// TestInterruptedExportFailsOnReplacementChannelAndRetries checks that a
// ready snapshot whose export lost its agent channel becomes EXPORT_FAILED
// when the device reports on the replacement channel, that the abandoned
// attempt's objects are cleaned up, and that a retry sends one command and
// reaches EXPORTED.
func TestInterruptedExportFailsOnReplacementChannelAndRetries(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]
	if cmd, channel := exportColumns(t, env, snapID); cmd != first || channel == "" {
		t.Fatalf("owner columns = %q %q, want the command %q and a channel", cmd, channel, first)
	}

	reconnect(agent)
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	abandoned := attemptPrefixOf(org.ID, snapID, first)
	if _, ok := cleanupJobs(t, env)[abandoned]; !ok {
		t.Fatalf("no clean-up job for %s: %v", abandoned, cleanupJobs(t, env))
	}
	env.App.Control.RunCleanupOnce(context.Background())
	if got := env.Objects.DeletedPrefixes(); len(got) != 1 || got[0] != abandoned {
		t.Fatalf("first sweep = %v, want only %s", got, abandoned)
	}
	logs := env.Logs.String()
	for _, want := range []string{`"msg":"export_failed"`, `"command_id":"` + first + `"`, `"reason":"owner_channel_replaced"`, `"snapshot_id":"` + snapID + `"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s", want)
		}
	}

	// The explicit retry sends exactly one new command.
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
	if cmd, _ := exportColumns(t, env, snapID); cmd != ids[1] {
		t.Fatalf("successful attempt = %q, want %q", cmd, ids[1])
	}
	grant := agent.WaitFor("retry grant", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSignedUrlGrant().GetRequestId() == ids[1]
	}).GetSignedUrlGrant()
	for _, u := range grant.GetPartUrls() {
		if !strings.Contains(u.GetUrl(), attemptPrefixOf(org.ID, snapID, ids[1])) {
			t.Fatalf("retry URL %q does not address its attempt prefix", u.GetUrl())
		}
	}

	// Deleting the snapshot still cleans up the whole snapshot prefix.
	if _, err := c.Snapshot.DeleteSnapshot(context.Background(), connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	env.App.Control.RunCleanupOnce(context.Background())
	if got := env.Objects.DeletedPrefixes(); got[len(got)-1] != snapshotPrefixOf(org.ID, snapID) {
		t.Fatalf("sweeps = %v, want the whole snapshot prefix last", got)
	}
}

// TestInterruptedExportFailsAfterControlPlaneRestart checks that a control
// plane that restarts over the same database, which forgot every remembered
// command, fails the lost export when the device reconnects, and that a
// retry reaches EXPORTED.
func TestInterruptedExportFailsAfterControlPlaneRestart(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]

	env.Restart()
	select {
	case <-agent.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the channel stayed open after the control plane restarted")
	}
	c := env.User(org.Owner)
	// Until the device reports, the restarted control plane cannot tell
	// that the export lost its owner.
	if got := snapshotExportState(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("state before the device reconnects = %v", got)
	}
	agent.Connect()
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, snapID, first)]; !ok {
		t.Fatalf("no clean-up job for the abandoned attempt: %v", cleanupJobs(t, env))
	}

	answerExports(agent)
	if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("retry state = %v", got)
	}
	waitExportCommands(t, agent, 2)
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	if got := len(manifestHashes(t, env, snapID)); got != 2 {
		t.Fatalf("part manifest has %d parts, want 2", got)
	}
}

// TestStaleExportResultCannotChangeRetry checks that the first attempt's
// late result leaves the retry's state and part manifest alone, that the two
// attempts' upload URLs address different objects, and that the download
// URLs address the objects of the successful attempt.
func TestStaleExportResultCannotChangeRetry(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]
	requestUploadURLs(agent, snapID, "first-urls")
	firstGrant := waitGrant(agent, "first-urls")
	if firstGrant.GetError() != nil || len(firstGrant.GetPartUrls()) != 2 {
		t.Fatalf("first grant = %v", firstGrant)
	}

	reconnect(agent)
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("retry state = %v", got)
	}
	second := waitExportCommands(t, agent, 2)[1]
	requestUploadURLs(agent, snapID, "second-urls")
	secondGrant := waitGrant(agent, "second-urls")
	if secondGrant.GetError() != nil || len(secondGrant.GetPartUrls()) != 2 {
		t.Fatalf("second grant = %v", secondGrant)
	}
	for i, u := range secondGrant.GetPartUrls() {
		if u.GetUrl() == firstGrant.GetPartUrls()[i].GetUrl() ||
			!strings.Contains(firstGrant.GetPartUrls()[i].GetUrl(), attemptPrefixOf(org.ID, snapID, first)) ||
			!strings.Contains(u.GetUrl(), attemptPrefixOf(org.ID, snapID, second)) {
			t.Fatalf("attempt URLs overlap: first %q second %q", firstGrant.GetPartUrls()[i].GetUrl(), u.GetUrl())
		}
	}

	// The first attempt's result arrives late, with its own parts.
	sendExportResult(agent, first, partsFor(firstGrant))
	testenv.Eventually(t, "stale result logged", func() bool { return strings.Contains(env.Logs.String(), `"msg":"export_result_stale"`) })
	if got := snapshotExportState(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("state after the stale result = %v, want EXPORTING", got)
	}
	if got := manifestHashes(t, env, snapID); len(got) != 0 {
		t.Fatalf("the stale result stored %d parts", len(got))
	}
	// The stale attempt can no longer get URLs on the new channel to write
	// to its own prefix: URLs always address the current attempt.
	requestUploadURLs(agent, snapID, "third-urls")
	for _, u := range waitGrant(agent, "third-urls").GetPartUrls() {
		if !strings.Contains(u.GetUrl(), attemptPrefixOf(org.ID, snapID, second)) {
			t.Fatalf("URL %q addresses another attempt than the current one", u.GetUrl())
		}
	}

	sendExportResult(agent, second, partsFor(secondGrant))
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	want := partsFor(secondGrant)
	got := manifestHashes(t, env, snapID)
	if len(got) != len(want) {
		t.Fatalf("manifest = %d parts, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i].GetSha256()) {
			t.Fatalf("manifest part %d does not come from the second attempt", i+1)
		}
	}

	// A device that imports the snapshot downloads from the successful
	// attempt.
	answerExports(agent)
	devB, agentB := setupAgent(t, env, org, nil)
	withholdImportResult(agentB)
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID))); err != nil {
		t.Fatal(err)
	}
	agentB.WaitFor("import command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetImportSnapshot() != nil })
	grant := requestDownloadURLs(agentB, snapID, "download")
	if grant.GetError() != nil || len(grant.GetPartUrls()) != 2 {
		t.Fatalf("download grant = %v", grant)
	}
	for _, u := range grant.GetPartUrls() {
		if !strings.Contains(u.GetUrl(), attemptPrefixOf(org.ID, snapID, second)) {
			t.Fatalf("download URL %q does not address the successful attempt", u.GetUrl())
		}
	}
}

// TestHealthyExportRepeatDispatchesOnce checks that a repeat ExportSnapshot
// and a state report on the owning channel leave a healthy export alone.
func TestHealthyExportRepeatDispatchesOnce(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)
	first := waitExportCommands(t, agent, 1)[0]

	for range 2 {
		if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
			t.Fatalf("repeat state = %v", got)
		}
	}
	acks := func() int {
		n := 0
		for _, e := range agent.Received() {
			if e.GetStateAck().GetStateReport() != nil {
				n++
			}
		}
		return n
	}
	before := acks()
	agent.SendStateReport()
	agent.Eventually("state report acknowledged", func() bool { return acks() > before })
	time.Sleep(200 * time.Millisecond) // a wrongly dispatched command would have arrived by now
	if ids := exportCommandIDs(agent); len(ids) != 1 || ids[0] != first {
		t.Fatalf("export commands = %v, want only %s", ids, first)
	}
	if got := snapshotExportState(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("state = %v", got)
	}
	if cmd, _ := exportColumns(t, env, snapID); cmd != first {
		t.Fatalf("the repeat changed the attempt to %q", cmd)
	}
}

// TestLegacyExportingRowCountsAsOrphaned checks that an EXPORTING row
// without an owner (written before owners were recorded) fails when the
// device reports and gets no upload URL before that.
func TestLegacyExportingRowCountsAsOrphaned(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)
	if _, err := env.DB.Pool.Exec(ctx, "UPDATE snapshots SET export_command_id = NULL, export_channel_id = NULL WHERE snapshot_id = $1", snapID); err != nil {
		t.Fatal(err)
	}
	requestUploadURLs(agent, snapID, "legacy-urls")
	requireDenied(t, waitGrant(agent, "legacy-urls"))

	reconnect(agent)
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	// The abandoned legacy objects live under parts/ of the snapshot prefix;
	// the job must not cover the prefix of later attempts.
	legacy := snapshotPrefixOf(org.ID, snapID) + "parts/"
	if _, ok := cleanupJobs(t, env)[legacy]; !ok {
		t.Fatalf("no clean-up job for %s: %v", legacy, cleanupJobs(t, env))
	}
	answerExports(agent)
	exportSnapshotRPC(t, c, org.ID, snapID)
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
}

// TestUploadURLsNeedOwningChannel checks that a channel that does not own the
// export gets no upload URL.
func TestUploadURLsNeedOwningChannel(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	other, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Pool.Exec(ctx, "UPDATE snapshots SET export_channel_id = $2 WHERE snapshot_id = $1", snapID, other); err != nil {
		t.Fatal(err)
	}
	requestUploadURLs(agent, snapID, "not-owner")
	requireDenied(t, waitGrant(agent, "not-owner"))
}

// TestLegacyExportedSnapshotStaysDownloadable checks that an exported
// snapshot without attempt columns resolves its parts under the legacy keys.
func TestLegacyExportedSnapshotStaysDownloadable(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, agentB := importFixture(t, env, org)
	if _, err := env.DB.Pool.Exec(ctx, "UPDATE snapshots SET export_command_id = NULL, export_channel_id = NULL WHERE snapshot_id = $1", snapID); err != nil {
		t.Fatal(err)
	}
	withholdImportResult(agentB)
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID))); err != nil {
		t.Fatal(err)
	}
	agentB.WaitFor("import command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetImportSnapshot() != nil })
	grant := requestDownloadURLs(agentB, snapID, "legacy-download")
	if grant.GetError() != nil || len(grant.GetPartUrls()) != 2 {
		t.Fatalf("download grant = %v", grant)
	}
	for _, u := range grant.GetPartUrls() {
		if !strings.Contains(u.GetUrl(), "memory://GET/"+snapshotPrefixOf(org.ID, snapID)+"parts/") || strings.Contains(u.GetUrl(), "attempts/") {
			t.Fatalf("download URL %q is not a legacy key", u.GetUrl())
		}
	}
}
