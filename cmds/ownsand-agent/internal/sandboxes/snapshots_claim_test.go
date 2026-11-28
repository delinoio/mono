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
	cmd := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}
	finish, done := startSnapshotCreatesWaitingForLock(t, e, cmd, 1)
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete = %v", err)
	}
	finish()
	res := <-done
	requireErrCode(t, res.err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create after a delete")
	requireNoSnapshotTrace(t, e, snapID)
}

func TestDeleteSnapshotBeforeCaptureAbortsCreate(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	cmd := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}
	e.rt.snapStarted = make(chan struct{})
	e.rt.snapBlock = make(chan struct{})
	close(e.rt.snapBlock)

	e.m.beforeSnapshotCapture = func() {
		if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
			t.Fatalf("delete before capture = %v", err)
		}
	}
	entry, err := e.m.CreateSnapshot(context.Background(), cmd)
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create after a delete")
	if entry != nil {
		t.Errorf("entry = %v, want nil", entry)
	}
	select {
	case <-e.rt.snapStarted:
		t.Fatal("runtime started the deleted snapshot capture")
	default:
	}
	requireNoSnapshotTrace(t, e, snapID)

	e.m.beforeSnapshotCapture = nil
	entry, err = e.m.CreateSnapshot(context.Background(), cmd)
	if err != nil || entry.GetSnapshotId() != snapID || entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("create after the aborted create = %v, %v; want the ready snapshot", entry, err)
	}
	select {
	case <-e.rt.snapStarted:
	default:
		t.Fatal("runtime did not start the fresh snapshot capture")
	}
}

func TestDeleteSnapshotDuringRuntimeError(t *testing.T) {
	for _, scenario := range []string{"cleanup succeeds", "cleanup retries"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnv(t)
			sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
			snapID, _ := uuidv7.NewString()
			e.rt.snapStarted = make(chan struct{})
			e.rt.snapBlock = make(chan struct{})
			e.rt.snapshotErr = errors.New("injected snapshot runtime error")
			e.rt.snapshotSourceState = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
			if scenario == "cleanup retries" {
				e.m.SetSnapshotRemovalHookForTest(func(string) error {
					return errors.New("injected snapshot removal error")
				})
			}

			release := sync.OnceFunc(func() { close(e.rt.snapBlock) })
			defer release()
			done := make(chan claimedCreateResult, 1)
			go func() {
				entry, err := e.m.CreateSnapshot(t.Context(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID})
				done <- claimedCreateResult{entry, err}
			}()
			select {
			case <-e.rt.snapStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("snapshot did not enter the runtime")
			}
			deleteCmd := &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}
			if err := e.m.DeleteSnapshot(deleteCmd); err != nil {
				t.Fatalf("delete during capture: %v", err)
			}
			release()
			select {
			case res := <-done:
				requireErrCode(t, res.err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "capture after delete")
				if res.entry != nil {
					t.Fatalf("capture returned a snapshot: %v", res.entry)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("snapshot did not return after the runtime error")
			}
			if got, _ := e.m.State(sandboxID); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
				t.Errorf("source state = %s, want PAUSED", got)
			}

			if scenario == "cleanup retries" {
				snaps := e.m.StateReport(2, nil).GetSnapshots()
				if len(snaps) != 1 || snaps[0].GetSnapshotId() != snapID || snaps[0].GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_DELETING {
					t.Fatalf("snapshot report = %v, want the DELETING snapshot", snaps)
				}
				if _, release, ok := e.m.AcquireSnapshot(snapID); ok {
					release()
					t.Fatal("capture was available to a reader")
				}
				e.m.SetSnapshotRemovalHookForTest(nil)
				if err := e.m.DeleteSnapshot(deleteCmd); err != nil {
					t.Fatalf("delete retry: %v", err)
				}
			}
			requireNoSnapshotTrace(t, e, snapID)
		})
	}
}

// TestDeleteCancelsEveryReceivedCreateOfTheSnapshot checks that two create
// commands of one snapshot, which wait for the sandbox lock, are both
// canceled by a delete that arrived after both.
func TestDeleteCancelsEveryReceivedCreateOfTheSnapshot(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	cmd := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}
	finish, done := startSnapshotCreatesWaitingForLock(t, e, cmd, 2)
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete = %v", err)
	}
	finish()
	for range 2 {
		requireErrCode(t, (<-done).err, ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "create after a delete")
	}
	requireNoSnapshotTrace(t, e, snapID)
}

// TestDuplicateClaimedCreatesPublishOneSnapshot checks that two creates
// waiting for the same sandbox lock return one published snapshot after a
// single runtime capture.
func TestDuplicateClaimedCreatesPublishOneSnapshot(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	cmd := &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}
	finish, done := startSnapshotCreatesWaitingForLock(t, e, cmd, 2)
	finish()
	for range 2 {
		res := <-done
		if res.err != nil || res.entry.GetSnapshotId() != snapID || res.entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			t.Errorf("duplicate create = %v, %v; want the ready snapshot", res.entry, res.err)
		}
	}
	if calls := e.rt.snapshotCallCount(); calls != 1 {
		t.Errorf("runtime Snapshot calls = %d, want 1", calls)
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
