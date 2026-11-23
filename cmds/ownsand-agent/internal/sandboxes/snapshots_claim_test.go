// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type claimedCreateResult struct {
	entry *ownsandv1.SnapshotStateEntry
	err   error
}

// pendingCount returns the number of snapshot IDs with a received create.
func (e *env) pendingCount() int {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return len(e.m.pending)
}

// requireNoSnapshotTrace checks that an aborted create left no record, no
// report entry, no content directory and no pending entry.
func requireNoSnapshotTrace(t *testing.T, e *env, snapID string) {
	t.Helper()
	if e.m.HasSnapshot(snapID) {
		t.Error("snapshot is known to the manager")
	}
	if report := e.m.StateReport(2, nil); len(report.GetSnapshots()) != 0 {
		t.Errorf("state report holds %v", report.GetSnapshots())
	}
	if dir, err := e.m.ImportDir(snapID); err != nil {
		t.Error(err)
	} else if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot directory after the aborted create: %v", err)
	}
	if n := e.pendingCount(); n != 0 {
		t.Errorf("pending snapshots = %d, want 0", n)
	}
}

// lockSandbox holds the command lock of a sandbox, as a running command of
// the sandbox does, until the returned function runs. The cleanup also
// releases it, so a failed test cannot leave the create blocked.
func lockSandbox(t *testing.T, e *env, sandboxID string) (unlock func()) {
	t.Helper()
	e.m.mu.Lock()
	sb := e.m.sandboxes[sandboxID]
	e.m.mu.Unlock()
	sb.cmdMu.Lock()
	var once sync.Once
	unlock = func() { once.Do(sb.cmdMu.Unlock) }
	t.Cleanup(unlock)
	return unlock
}

// TestDeleteAfterClaimBeforeCreateRunsAbortsCreate covers the window between
// the receipt of a create command and the start of its goroutine: a delete
// that runs first finds the claim, and the create then returns ABORTED
// without it capturing anything.
func TestDeleteAfterClaimBeforeCreateRunsAbortsCreate(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	claim := e.m.ClaimSnapshotCreate(snapID)
	defer claim.Release()
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete = %v", err)
	}
	entry, err := e.m.CreateClaimedSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}, claim)
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create after a delete")
	if entry != nil {
		t.Errorf("entry = %v, want nil", entry)
	}
	claim.Release()
	requireNoSnapshotTrace(t, e, snapID)
	// The snapshot ID stays usable for a later create.
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}); err != nil {
		t.Fatalf("create after the aborted create = %v", err)
	}
}

// TestDeleteWhileCreateWaitsForSandboxLockAbortsCreate covers a create that
// waits for the command lock of its sandbox while the delete arrives.
func TestDeleteWhileCreateWaitsForSandboxLockAbortsCreate(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	unlock := lockSandbox(t, e, sandboxID)
	claim := e.m.ClaimSnapshotCreate(snapID)
	defer claim.Release()
	done := make(chan claimedCreateResult, 1)
	go func() {
		entry, err := e.m.CreateClaimedSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}, claim)
		done <- claimedCreateResult{entry, err}
	}()
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete = %v", err)
	}
	unlock()
	res := <-done
	requireErrCode(t, res.err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create after a delete")
	claim.Release()
	requireNoSnapshotTrace(t, e, snapID)
}

// TestDeleteCancelsEveryReceivedCreateOfTheSnapshot checks that two create
// commands of one snapshot, which wait for the sandbox lock, are both
// canceled by a delete that arrived after both.
func TestDeleteCancelsEveryReceivedCreateOfTheSnapshot(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	unlock := lockSandbox(t, e, sandboxID)
	done := make(chan claimedCreateResult, 2)
	claims := []*SnapshotCreateClaim{e.m.ClaimSnapshotCreate(snapID), e.m.ClaimSnapshotCreate(snapID)}
	for _, claim := range claims {
		defer claim.Release()
		go func() {
			entry, err := e.m.CreateClaimedSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}, claim)
			done <- claimedCreateResult{entry, err}
		}()
	}
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete = %v", err)
	}
	unlock()
	for range claims {
		requireErrCode(t, (<-done).err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create after a delete")
	}
	for _, claim := range claims {
		claim.Release()
	}
	requireNoSnapshotTrace(t, e, snapID)
}

// TestDuplicateClaimedCreatesPublishOneSnapshot checks that two create
// commands of one snapshot that wait for the sandbox lock both succeed with
// the one snapshot, as a duplicate create did before the claims existed.
func TestDuplicateClaimedCreatesPublishOneSnapshot(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	unlock := lockSandbox(t, e, sandboxID)
	done := make(chan claimedCreateResult, 2)
	claims := []*SnapshotCreateClaim{e.m.ClaimSnapshotCreate(snapID), e.m.ClaimSnapshotCreate(snapID)}
	for _, claim := range claims {
		defer claim.Release()
		go func() {
			entry, err := e.m.CreateClaimedSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}, claim)
			done <- claimedCreateResult{entry, err}
		}()
	}
	unlock()
	for range claims {
		res := <-done
		if res.err != nil || res.entry.GetSnapshotId() != snapID || res.entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			t.Fatalf("duplicate create = %v, %v; want the ready snapshot", res.entry, res.err)
		}
	}
	for _, claim := range claims {
		claim.Release()
	}
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot is not published")
	}
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("pending snapshots = %d, want 0", n)
	}
}

// TestCreateReceivedAfterDeleteIsNotCanceled checks that a delete cancels only
// the creates that were received before it: a create of the same snapshot ID
// received after the delete, while the canceled create still holds its claim,
// publishes the snapshot.
func TestCreateReceivedAfterDeleteIsNotCanceled(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	before := e.m.ClaimSnapshotCreate(snapID)
	defer before.Release()
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete = %v", err)
	}
	after := e.m.ClaimSnapshotCreate(snapID)
	defer after.Release()
	cmd := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}
	_, err := e.m.CreateClaimedSnapshot(context.Background(), cmd, before)
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create received before the delete")
	entry, err := e.m.CreateClaimedSnapshot(context.Background(), cmd, after)
	if err != nil || entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("create received after the delete = %v, %v", entry, err)
	}
}

// TestDeleteOfUnknownSnapshotWithoutCreateIsIdempotent checks that a delete
// of a snapshot without a received create succeeds, repeatedly, and leaves no
// pending entry that would cancel a later create.
func TestDeleteOfUnknownSnapshotWithoutCreateIsIdempotent(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	for range 2 {
		if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
			t.Fatalf("delete = %v", err)
		}
	}
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("pending snapshots = %d, want 0", n)
	}
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}); err != nil {
		t.Fatalf("create after the deletes = %v", err)
	}
}

// TestFailedCreatesReleaseTheirClaims checks that every early return of a
// create ends its claim, so no pending entry outlives the command.
func TestFailedCreatesReleaseTheirClaims(t *testing.T) {
	e := newEnv(t)
	snapID, _ := uuidv7.NewString()
	_, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: "unknown"})
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "create on an unknown sandbox")
	_, err = e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: "not-a-uuid", SandboxId: "unknown"})
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "create with an invalid ID")
	if n := e.pendingCount(); n != 0 {
		t.Fatalf("pending snapshots = %d, want 0", n)
	}
}
