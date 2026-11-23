// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// exportVerifyWait bounds the wait for the export verifier and the R2
// clean-up worker, which poll every 30 seconds.
const exportVerifyWait = 2 * time.Minute

// databaseURL returns the DATABASE_URL of the control plane process.
func (s *stack) databaseURL() string {
	for _, kv := range s.apiEnv {
		if v, ok := strings.CutPrefix(kv, "DATABASE_URL="); ok {
			return v
		}
	}
	s.t.Fatal("the control plane has no DATABASE_URL")
	return ""
}

// exportOneSnapshot snapshots the sandbox sb with the real agent, exports it
// to the fake S3 store and waits until it is EXPORTED. It returns the ID of
// the snapshot and the key prefix of its only export attempt.
func exportOneSnapshot(ctx context.Context, t *testing.T, s *stack, c *ownsand.Client, sb *ownsand.Sandbox) (snapID, attemptPrefix string) {
	t.Helper()
	snap, err := sb.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snapID = snap.GetSnapshotId()
	waitSnapshotReady(ctx, t, c, snapID)
	if _, err := c.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	waitExported(ctx, t, c, snapID)
	attempts := s.exportAttempts(snapID)
	if len(attempts) != 1 {
		t.Fatalf("export attempts %v, want one", attempts)
	}
	for id := range attempts {
		attemptPrefix = "organizations/" + s.orgID + "/snapshots/" + snapID + "/attempts/" + id + "/"
	}
	return snapID, attemptPrefix
}

// TestSnapshotExportVerification exports two snapshots with the real agent
// and the fake S3 store. The control plane verifies the parts of the first
// against the store and keeps it EXPORTED; the second has a part replaced by
// a larger object and an extra object under its attempt prefix before the
// verification, so the control plane fails the export, deletes its manifest
// (the storage usage stops) and cleans up the attempt prefix. The wait for
// the verification (15 minutes after the export) is skipped by moving
// export_verify_after into the past in the database.
func TestSnapshotExportVerification(t *testing.T) {
	s := newStack(t, stackOptions{agents: 1, objects: true})
	ctx := s.ctx(8 * time.Minute)
	c := s.apiKeyClient(ownsand.TransportRelay)
	p := createParams()
	p.DeviceID = s.agents[0].deviceID
	sb, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer sb.Close()
	if info, err := sb.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable = (%v, %v)", info.GetState(), err)
	}
	conn, err := pgx.Connect(ctx, s.databaseURL())
	if err != nil {
		t.Fatalf("connect to the database: %v", err)
	}
	defer conn.Close(ctx)

	good, goodPrefix := exportOneSnapshot(ctx, t, s, c, sb)
	bad, badPrefix := exportOneSnapshot(ctx, t, s, c, sb)

	// Both rows wait for their verification time (the upload URL lifetime).
	for _, id := range []string{good, bad} {
		var verified bool
		var after *time.Time
		if err := conn.QueryRow(ctx, "SELECT export_verified, export_verify_after FROM snapshots WHERE snapshot_id = $1", id).Scan(&verified, &after); err != nil {
			t.Fatal(err)
		}
		if verified || after == nil || !after.After(time.Now().Add(10*time.Minute)) {
			t.Fatalf("snapshot %s right after the export: verified %v, export_verify_after %v; want unverified and about 15 minutes ahead", id, verified, after)
		}
	}

	// Tamper with the second export: the first part grows by one byte and an
	// object appears under the attempt prefix that the manifest does not list.
	s.objects.mu.Lock()
	var tampered string
	for k, v := range s.objects.objects {
		if strings.HasPrefix(k, badPrefix) && tampered == "" {
			tampered = k
			s.objects.objects[k] = append(append([]byte(nil), v...), 'x')
		}
	}
	s.objects.objects[badPrefix+"extra"] = []byte("unlisted")
	s.objects.mu.Unlock()
	if tampered == "" {
		t.Fatalf("no object under %s", badPrefix)
	}

	// Skip the 15-minute wait.
	if _, err := conn.Exec(ctx, "UPDATE snapshots SET export_verify_after = now() - interval '1 minute' WHERE snapshot_id = ANY($1)", []string{good, bad}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(exportVerifyWait)
	for {
		var verified bool
		if err := conn.QueryRow(ctx, "SELECT export_verified FROM snapshots WHERE snapshot_id = $1", good).Scan(&verified); err != nil {
			t.Fatal(err)
		}
		bs, err := c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{SnapshotId: bad})
		if err != nil {
			t.Fatalf("GetSnapshot: %v", err)
		}
		if verified && bs.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %v: matching export verified = %v, tampered export state = %v; want verified and EXPORT_FAILED", exportVerifyWait, verified, bs.GetSnapshot().GetExportState())
		}
		time.Sleep(time.Second)
	}

	gs, err := c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{SnapshotId: good})
	if err != nil || gs.GetSnapshot().GetExportState() != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED {
		t.Fatalf("matching export = (%v, %v), want EXPORTED", gs.GetSnapshot().GetExportState(), err)
	}
	if got := len(s.objects.keys(goodPrefix)); got == 0 {
		t.Fatal("the verified export lost its objects")
	}
	var parts int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM snapshot_parts WHERE snapshot_id = $1", bad).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if parts != 0 {
		t.Fatalf("the failed export keeps %d manifest parts, want none (they would still be billed)", parts)
	}
	// The clean-up worker deletes the failed attempt's objects.
	for len(s.objects.keys(badPrefix)) != 0 {
		if time.Now().After(deadline.Add(exportVerifyWait)) {
			t.Fatalf("objects of the failed attempt remain: %v", s.objects.keys(badPrefix))
		}
		time.Sleep(time.Second)
	}
}
