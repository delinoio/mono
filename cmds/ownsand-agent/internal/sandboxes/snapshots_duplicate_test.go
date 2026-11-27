// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// requireDuplicateCreateOutcome checks the result of a duplicate create that
// raced a delete: the entry of the ready snapshot (the create ran before the
// deleting mark) or FAILED_PRECONDITION (after it).
func requireDuplicateCreateOutcome(t *testing.T, snapID string, entry *ownsandv1.SnapshotStateEntry, err error) {
	t.Helper()
	if err == nil {
		if entry.GetSnapshotId() != snapID || entry.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			t.Errorf("duplicate create entry = %v, want the ready snapshot %s", entry, snapID)
		}
		return
	}
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Errorf("duplicate create error = %v, want FAILED_PRECONDITION", err)
	}
}

// raceDuplicateCreateAndDelete starts a duplicate create and a delete of the
// same snapshot from one barrier. A reader holds the snapshot, so the delete
// only marks it as deleting, which is the write that the duplicate create
// must not race.
func raceDuplicateCreateAndDelete(t *testing.T, e *env, sandboxID, snapID string) {
	t.Helper()
	release := func() {}
	if _, rel, ok := e.m.AcquireSnapshot(snapID); ok {
		release = rel
	} else {
		t.Fatalf("snapshot %s is not usable", snapID)
	}
	defer release()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID})
		requireDuplicateCreateOutcome(t, snapID, entry, err)
	}()
	go func() {
		defer wg.Done()
		<-start
		if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
			t.Errorf("delete = %v", err)
		}
	}()
	close(start)
	wg.Wait()
}

// TestDuplicateCreateSnapshotDoesNotRaceDelete checks that a duplicate create
// of an existing snapshot reads the snapshot under Manager.mu, so a delete
// that marks it concurrently is either seen or not, without a data race.
func TestDuplicateCreateSnapshotDoesNotRaceDelete(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	for range 50 {
		snapID, _ := uuidv7.NewString()
		if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}); err != nil {
			t.Fatal(err)
		}
		raceDuplicateCreateAndDelete(t, e, sandboxID, snapID)
	}
}

// TestDuplicateCreateSnapshotAfterSandboxLockDoesNotRaceDelete covers the
// second lookup: a duplicate create that waited for the sandbox lock finds
// the snapshot that the first create published and reads it under
// Manager.mu while a delete marks it.
func TestDuplicateCreateSnapshotAfterSandboxLockDoesNotRaceDelete(t *testing.T) {
	e := newEnv(t)
	sandboxID := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	for range 200 {
		snapID, _ := uuidv7.NewString()
		raceWaitingDuplicateCreateAndDelete(t, e, sandboxID, snapID)
	}
}

func raceWaitingDuplicateCreateAndDelete(t *testing.T, e *env, sandboxID, snapID string) {
	t.Helper()
	unlock := lockSandbox(t, e, sandboxID)
	var wg sync.WaitGroup
	var release func()
	defer func() {
		unlock()
		wg.Wait()
		if release != nil {
			release()
		}
	}()

	wg.Go(func() {
		entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID})
		requireDuplicateCreateOutcome(t, snapID, entry, err)
	})
	awaitSnapshotCreateLockWait(t)

	// Publish only after the duplicate misses its first lookup. Keep a
	// reader until both commands finish so delete only marks the snapshot.
	if _, err := e.m.AddImportedSnapshot(context.Background(), &ownsandv1.SnapshotStateEntry{SnapshotId: snapID, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX}); err != nil {
		t.Fatal(err)
	}
	_, release, ok := e.m.AcquireSnapshot(snapID)
	if !ok {
		t.Fatalf("snapshot %s is not usable", snapID)
	}
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
			t.Errorf("delete = %v", err)
		}
	})
	close(start)
}
