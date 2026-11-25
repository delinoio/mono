// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// blockSnapshotRemoval makes the removal of the stored snapshot fail: it makes
// the snapshots directory read-only, so the snapshot directory itself cannot be
// unlinked. A read-only directory inside the snapshot would not do: the
// removal makes the directories of the tree writable again, because a copied
// read-only disk must stay deletable. The returned function restores the
// permissions. A test that needs the failure is skipped where permissions do
// not stop the owner (Windows, or a root user).
func blockSnapshotRemoval(t *testing.T, e *env, snapID string) (unblock func()) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop the removal for this user or OS")
	}
	dir, err := e.st.SnapshotDir(snapID)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(dir)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unblock = func() { once.Do(func() { _ = os.Chmod(parent, 0o700) }) }
	t.Cleanup(unblock)
	return unblock
}

func snapshotInReport(e *env, seq uint64, snapID string) bool {
	for _, s := range e.m.StateReport(seq, nil).GetSnapshots() {
		if s.GetSnapshotId() == snapID {
			return true
		}
	}
	return false
}

func newReadySnapshot(t *testing.T, e *env) string {
	t.Helper()
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	return snapID
}

func requireErrCode(t *testing.T, err error, code ownsandv1.ErrorCode, what string) {
	t.Helper()
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("%s = %v, want %v", what, err, code)
	}
}

// TestDeleteSnapshotRemovalFailureKeepsEntryAndRetries checks that an
// immediate delete whose directory removal fails reports INTERNAL, keeps the
// snapshot unusable but in the state report, and that a repeated delete in
// the same process removes the remaining directory and the entry.
func TestDeleteSnapshotRemovalFailureKeepsEntryAndRetries(t *testing.T) {
	e := newEnv(t)
	snapID := newReadySnapshot(t, e)
	unblock := blockSnapshotRemoval(t, e, snapID)
	del := &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}

	requireErrCode(t, e.m.DeleteSnapshot(del), ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "delete with a failing removal")
	if !e.m.HasSnapshot(snapID) || !snapshotInReport(e, 2, snapID) {
		t.Fatal("snapshot with a failed removal is not kept in the state report")
	}
	if _, _, ok := e.m.AcquireSnapshot(snapID); ok {
		t.Fatal("AcquireSnapshot of a snapshot with a failed removal succeeded")
	}
	if _, _, ok := e.m.SnapshotContent(snapID); ok {
		t.Fatal("SnapshotContent of a snapshot with a failed removal succeeded")
	}
	// A create for the same ID does not hand out the half-removed copy.
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	_, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	requireErrCode(t, err, ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "create of a snapshot being deleted")
	// The retry fails the same way while the directory stays blocked.
	requireErrCode(t, e.m.DeleteSnapshot(del), ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "second delete with a failing removal")

	unblock()
	if err := e.m.DeleteSnapshot(del); err != nil {
		t.Fatalf("retried delete = %v", err)
	}
	if e.m.HasSnapshot(snapID) || snapshotInReport(e, 3, snapID) {
		t.Fatal("snapshot entry remains after a successful removal")
	}
	dir, _ := e.st.SnapshotDir(snapID)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory after the retried delete: %v", err)
	}
	if err := e.m.DeleteSnapshot(del); err != nil {
		t.Fatalf("delete of an absent snapshot = %v", err)
	}
}

// TestSnapshotRemovalFailureAfterLastReaderStaysReported checks that a failed
// removal after the last reader ended keeps the snapshot unavailable and in
// the next state report, and that a resent delete completes it.
func TestSnapshotRemovalFailureAfterLastReaderStaysReported(t *testing.T) {
	e := newEnv(t)
	snapID := newReadySnapshot(t, e)
	unblock := blockSnapshotRemoval(t, e, snapID)
	del := &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}

	_, release, ok := e.m.AcquireSnapshot(snapID)
	if !ok {
		t.Fatal("AcquireSnapshot failed")
	}
	if err := e.m.DeleteSnapshot(del); err != nil {
		t.Fatalf("delete with a reader = %v", err)
	}
	release() // The removal fails here; the release only logs it.
	if !e.m.HasSnapshot(snapID) || !snapshotInReport(e, 2, snapID) {
		t.Fatal("snapshot whose removal failed after its last reader is not in the state report")
	}
	if _, _, ok := e.m.AcquireSnapshot(snapID); ok {
		t.Fatal("AcquireSnapshot succeeded after a failed removal")
	}
	unblock()
	if err := e.m.DeleteSnapshot(del); err != nil {
		t.Fatalf("resent delete = %v", err)
	}
	if e.m.HasSnapshot(snapID) || snapshotInReport(e, 3, snapID) {
		t.Fatal("snapshot entry remains after the resent delete")
	}
	dir, _ := e.st.SnapshotDir(snapID)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory after the resent delete: %v", err)
	}
}

// TestSuccessfulRemovalAfterLastReaderDropsEntry checks that the release path
// drops the entry from the reports when the removal succeeds.
func TestSuccessfulRemovalAfterLastReaderDropsEntry(t *testing.T) {
	e := newEnv(t)
	snapID := newReadySnapshot(t, e)
	_, release, _ := e.m.AcquireSnapshot(snapID)
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatal(err)
	}
	if !snapshotInReport(e, 2, snapID) {
		t.Fatal("snapshot with a reader left the report before its removal")
	}
	release()
	if snapshotInReport(e, 3, snapID) || e.m.HasSnapshot(snapID) {
		t.Fatal("entry remains after a successful removal")
	}
}

// TestConcurrentSnapshotRemovalNeverDeletesReplacement checks that concurrent
// deletes all succeed, and that a cleanup of an old copy that runs after a
// replacement copy was published for the same ID leaves the replacement alone.
func TestConcurrentSnapshotRemovalNeverDeletesReplacement(t *testing.T) {
	e := newEnv(t)
	snapID := newReadySnapshot(t, e)
	e.m.mu.Lock()
	old := e.m.snapshots[snapID]
	e.m.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent delete = %v", err)
		}
	}
	if e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot remains after concurrent deletes")
	}

	// A replacement copy of the same ID is published; a stale cleanup of the
	// old entry (for example a release that raced the deletes) must not
	// remove its entry or its content.
	dir, err := e.st.SnapshotDir(snapID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, contentDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AddImportedSnapshot(context.Background(), &ownsandv1.SnapshotStateEntry{SnapshotId: snapID, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.removeSnapshotEntry(old); err != nil {
		t.Fatalf("stale cleanup = %v", err)
	}
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("stale cleanup removed the replacement entry")
	}
	if _, err := os.Stat(filepath.Join(dir, contentDir)); err != nil {
		t.Fatalf("stale cleanup removed the replacement content: %v", err)
	}
}

// TestPrepareSnapshotImportFinishesRemoval checks that an import first
// completes the removal of a marked copy, and fails while the removal fails
// or a reader still uses the copy.
func TestPrepareSnapshotImportFinishesRemoval(t *testing.T) {
	e := newEnv(t)
	snapID := newReadySnapshot(t, e)
	if err := e.m.PrepareSnapshotImport("00000000-0000-7000-8000-000000000000"); err != nil {
		t.Fatalf("prepare without a copy = %v", err)
	}
	requireErrCode(t, e.m.PrepareSnapshotImport(snapID), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "prepare over a ready copy")

	_, release, _ := e.m.AcquireSnapshot(snapID)
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatal(err)
	}
	requireErrCode(t, e.m.PrepareSnapshotImport(snapID), ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "prepare while a reader reads")
	unblock := blockSnapshotRemoval(t, e, snapID)
	release() // Fails to remove; the entry stays.
	requireErrCode(t, e.m.PrepareSnapshotImport(snapID), ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "prepare with a failing removal")
	unblock()
	if err := e.m.PrepareSnapshotImport(snapID); err != nil {
		t.Fatalf("prepare after the directory was unblocked = %v", err)
	}
	if e.m.HasSnapshot(snapID) {
		t.Fatal("entry remains after the prepared removal")
	}
}
