// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// gateStore wraps an in-memory object store and, while armed, holds
// PresignPut at a barrier before it signs. The signature (and so the URL
// expiry) is taken after the barrier opens, which models a signer that is
// slow after it authorized the request.
type gateStore struct {
	*objectstore.Memory

	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
	expires []time.Time
}

// arm makes the next PresignPut call wait at the barrier.
func (g *gateStore) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.entered = make(chan struct{}, 8)
	g.release = make(chan struct{})
}

// open releases every waiting and future PresignPut call.
func (g *gateStore) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed {
		g.armed = false
		close(g.release)
	}
}

// PresignPut implements objectstore.Store.
func (g *gateStore) PresignPut(key string, ttl time.Duration) (string, time.Time, error) {
	g.mu.Lock()
	armed, entered, release := g.armed, g.entered, g.release
	g.mu.Unlock()
	if armed {
		entered <- struct{}{}
		<-release
	}
	u, exp, err := g.Memory.PresignPut(key, ttl)
	g.mu.Lock()
	g.expires = append(g.expires, exp)
	g.mu.Unlock()
	return u, exp, err
}

// latestExpiry returns the latest expiry of every URL signed so far.
func (g *gateStore) latestExpiry() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var latest time.Time
	for _, e := range g.expires {
		if e.After(latest) {
			latest = e
		}
	}
	return latest, len(g.expires) > 0
}

// signingRaceEnv starts a control plane whose object store is a gateStore.
func signingRaceEnv(t *testing.T) (*testenv.Env, *gateStore) {
	t.Helper()
	gate := &gateStore{}
	env := testenv.New(t, func(o *app.Options) {
		gate.Memory = objectstore.NewMemory(o.Now)
		o.Objects = gate
	})
	t.Cleanup(gate.open) // never leave a signer blocked when a test fails
	return env, gate
}

// exportingSnapshot creates a snapshot and starts its export, and keeps the
// export of the fake agent pending, so the snapshot stays EXPORTING and the
// test sends the signed URL request itself.
func exportingSnapshot(t *testing.T, env *testenv.Env, org *testenv.Org) (snapshotID string, agent *testenv.FakeAgent) {
	t.Helper()
	ctx := context.Background()
	c := org.OwnerClient
	_, agent = setupAgent(t, env, org, nil)
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetExportSnapshot() != nil {
			return nil // the test requests the URLs itself
		}
		return fa.DefaultAnswer(cmd)
	})
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	snapshotID = snap.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "snapshot ready", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapshotID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY
	})
	exp, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapshotID}))
	if err != nil || exp.Msg.GetSnapshot().GetExportState() != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("export: %v %v", exp, err)
	}
	agent.WaitFor("export command", func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand().GetExportSnapshot() != nil })
	return snapshotID, agent
}

// requestUploadURLs sends a signed URL request for parts 1 and 2 and returns
// its request ID.
func requestUploadURLs(agent *testenv.FakeAgent, snapshotID, requestID string) {
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{
		RequestId: requestID, SnapshotId: snapshotID, Operation: ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_UPLOAD, PartNumbers: []int32{1, 2},
	}}})
}

func waitGrant(agent *testenv.FakeAgent, requestID string) *ownsandv1.SignedUrlGrant {
	return agent.WaitFor("signed url grant "+requestID, func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSignedUrlGrant().GetRequestId() == requestID
	}).GetSignedUrlGrant()
}

// waitEntered waits until a signer reached the barrier, which means it holds
// the snapshot row lock.
func (g *gateStore) waitEntered(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	entered := g.entered
	g.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the signer never reached the barrier")
	}
}

// assertBlocked fails when done is closed within a grace period, which means
// the deletion did not wait for the signer.
func assertBlocked(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s finished while a signer held the snapshot row (err = %v)", what, err)
	case <-time.After(400 * time.Millisecond):
	}
}

// cleanupJobs returns the final sweep deadline of every clean-up job.
func cleanupJobs(t *testing.T, env *testenv.Env) map[string]time.Time {
	t.Helper()
	rows, err := env.DB.Pool.Query(context.Background(), "SELECT prefix, final_sweep_after FROM r2_cleanup_jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	jobs := map[string]time.Time{}
	for rows.Next() {
		var prefix string
		var after time.Time
		if err := rows.Scan(&prefix, &after); err != nil {
			t.Fatal(err)
		}
		jobs[prefix] = after
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return jobs
}

// assertSweepCoversURLs checks that no signed URL outlives a final sweep.
func assertSweepCoversURLs(t *testing.T, env *testenv.Env, gate *gateStore, wantPrefix string) {
	t.Helper()
	latest, ok := gate.latestExpiry()
	if !ok {
		t.Fatal("the signer issued no URL")
	}
	jobs := cleanupJobs(t, env)
	if _, ok := jobs[wantPrefix]; !ok {
		t.Fatalf("no clean-up job for %s: %v", wantPrefix, jobs)
	}
	for prefix, after := range jobs {
		if after.Before(latest) {
			t.Fatalf("job %s: final sweep %s is before the latest URL expiry %s", prefix, after, latest)
		}
	}
}

// assertCleanupKeepsJobUntil runs the worker before and after the final
// sweep deadline: the job stays until the deadline, then the prefix is swept
// once more and the job is removed.
func assertCleanupKeepsJobUntil(t *testing.T, env *testenv.Env, gate *gateStore, prefix string) {
	t.Helper()
	ctx := context.Background()
	after := cleanupJobs(t, env)[prefix]
	env.App.Control.RunCleanupOnce(ctx) // first sweep
	if len(cleanupJobs(t, env)) == 0 {
		t.Fatal("the first sweep removed a job before its final sweep")
	}
	env.Clock.Advance(after.Sub(env.Clock.Now()) - time.Second)
	env.App.Control.RunCleanupOnce(ctx)
	if _, ok := cleanupJobs(t, env)[prefix]; !ok {
		t.Fatal("the job was removed before its final sweep deadline")
	}
	env.Clock.Advance(2 * time.Second)
	env.App.Control.RunCleanupOnce(ctx)
	if got := cleanupJobs(t, env); len(got) != 0 {
		t.Fatalf("jobs left after the final sweep: %v", got)
	}
	var sweeps int
	for _, p := range gate.DeletedPrefixes() {
		if p == prefix {
			sweeps++
		}
	}
	if sweeps != 2 {
		t.Fatalf("prefix %s swept %d times, want 2 (first and final): %v", prefix, sweeps, gate.DeletedPrefixes())
	}
}

// TestSnapshotDeletionWaitsForSigner: signing wins the race. DeleteSnapshot
// waits for the signer that holds the snapshot row, then records its job
// after the signer's last expiry.
func TestSnapshotDeletionWaitsForSigner(t *testing.T) {
	env, gate := signingRaceEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	prefix := "organizations/" + org.ID + "/snapshots/" + snapID + "/"

	gate.arm()
	requestUploadURLs(agent, snapID, "sign-first")
	gate.waitEntered(t)
	done := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		done <- err
	}()
	assertBlocked(t, "DeleteSnapshot", done)
	env.Clock.Advance(5 * time.Minute) // the signer is slow: it signs 5 minutes after it authorized
	gate.open()
	grant := waitGrant(agent, "sign-first")
	if grant.GetError() != nil || len(grant.GetPartUrls()) != 2 {
		t.Fatalf("grant = %v", grant)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertSweepCoversURLs(t, env, gate, prefix)
	assertCleanupKeepsJobUntil(t, env, gate, prefix)
}

// TestSnapshotDeletionBeforeSigner: deletion wins the race. A signer that
// waits behind the deleting transaction finds no row and returns no URL, and
// so does every later request.
func TestSnapshotDeletionBeforeSigner(t *testing.T) {
	env, gate := signingRaceEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)

	// A transaction that deletes the row stands for DeleteSnapshot between its
	// row lock and its commit.
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", snapID); err != nil {
		t.Fatal(err)
	}
	requestUploadURLs(agent, snapID, "sign-second")
	testenv.Eventually(t, "the signer waits for the row lock", func() bool {
		var n int
		_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR SHARE%'").Scan(&n)
		return n == 1
	})
	if _, err := tx.Exec(ctx, "DELETE FROM snapshots WHERE snapshot_id = $1", snapID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	grant := waitGrant(agent, "sign-second")
	if grant.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || len(grant.GetPartUrls()) != 0 {
		t.Fatalf("grant after deletion = %v", grant)
	}
	if _, ok := gate.latestExpiry(); ok {
		t.Fatal("a URL was signed for a deleted snapshot")
	}
}

// TestSignedURLRequestAfterSnapshotDeletion: a request after DeleteSnapshot
// gets no URL.
func TestSignedURLRequestAfterSnapshotDeletion(t *testing.T) {
	env, gate := signingRaceEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	if _, err := org.OwnerClient.Snapshot.DeleteSnapshot(ctx, connect.NewRequest(&ownsandv1.DeleteSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	requestUploadURLs(agent, snapID, "after-delete")
	grant := waitGrant(agent, "after-delete")
	if grant.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || len(grant.GetPartUrls()) != 0 {
		t.Fatalf("grant after deletion = %v", grant)
	}
	if _, ok := gate.latestExpiry(); ok {
		t.Fatal("a URL was signed for a deleted snapshot")
	}
}

// TestOrganizationDeletionWaitsForSigner: signing wins the race against
// DeleteOrganization, whose cascade deletes the snapshot row. Every job of
// the deletion is recorded after the signer's last expiry.
func TestOrganizationDeletionWaitsForSigner(t *testing.T) {
	env, gate := signingRaceEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)
	orgPrefix := "organizations/" + org.ID + "/"

	gate.arm()
	requestUploadURLs(agent, snapID, "sign-org")
	gate.waitEntered(t)
	done := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		done <- err
	}()
	assertBlocked(t, "DeleteOrganization", done)
	env.Clock.Advance(5 * time.Minute)
	gate.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertSweepCoversURLs(t, env, gate, orgPrefix)
	for prefix := range cleanupJobs(t, env) {
		if !strings.HasPrefix(prefix, orgPrefix) {
			t.Fatalf("job prefix %s is outside the organization", prefix)
		}
	}
	assertCleanupKeepsJobUntil(t, env, gate, orgPrefix)
}

// TestOrganizationDeletionBeforeSigner: deletion wins the race against the
// signer, which waits behind the cascade and returns no URL.
func TestOrganizationDeletionBeforeSigner(t *testing.T) {
	env, gate := signingRaceEnv(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, agent := exportingSnapshot(t, env, org)

	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", snapID); err != nil {
		t.Fatal(err)
	}
	requestUploadURLs(agent, snapID, "sign-org-late")
	testenv.Eventually(t, "the signer waits for the row lock", func() bool {
		var n int
		_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR SHARE%'").Scan(&n)
		return n == 1
	})
	// The cascade of the organization deletes the snapshot row.
	if _, err := tx.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", org.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	grant := waitGrant(agent, "sign-org-late")
	if grant.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND || len(grant.GetPartUrls()) != 0 {
		t.Fatalf("grant after deletion = %v", grant)
	}
	if _, ok := gate.latestExpiry(); ok {
		t.Fatal("a URL was signed for a deleted organization")
	}
}
