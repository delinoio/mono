// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func requireFailedPrecondition(t *testing.T, err error) {
	t.Helper()
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("error = %v, want FAILED_PRECONDITION", err)
	}
}

// TestRemoveAllWaitsForRegisteredImportsAndRefusesLaterOnes checks the
// device-removal fence: RemoveAll waits for a registered import, a late
// publication is refused and writes no snapshot record, and a new import is
// refused after the fence was set.
func TestRemoveAllWaitsForRegisteredImportsAndRefusesLaterOnes(t *testing.T) {
	e := newEnv(t)
	snapID, _ := uuidv7.NewString()
	end, err := e.m.BeginSnapshotImport()
	if err != nil {
		t.Fatal(err)
	}
	removed := make(chan error, 1)
	go func() { removed <- e.m.RemoveAll(context.Background()) }()
	select {
	case err := <-removed:
		t.Fatalf("RemoveAll finished while an import was registered: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// The fence is set: a new import is refused, and so is the publication
	// of the registered import.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := e.m.BeginSnapshotImport(); err != nil {
			requireFailedPrecondition(t, err)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a new import was accepted after RemoveAll started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = e.m.AddImportedSnapshot(&ownsandv1.SnapshotStateEntry{SnapshotId: snapID})
	requireFailedPrecondition(t, err)
	end()
	end() // idempotent
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("RemoveAll = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RemoveAll did not finish after the import ended")
	}
	if _, _, ok := e.m.SnapshotContent(snapID); ok {
		t.Fatal("a refused import left a snapshot entry")
	}
	if e.m.HasSnapshot(snapID) {
		t.Fatal("a refused import left a snapshot record")
	}
}

// TestRemoveAllWaitEndsWithItsContext checks that a RemoveAll that waits for
// an import returns the context error, keeps the fence, and that a retry
// finishes once the import ended.
func TestRemoveAllWaitEndsWithItsContext(t *testing.T) {
	e := newEnv(t)
	end, err := e.m.BeginSnapshotImport()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := e.m.RemoveAll(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RemoveAll = %v, want the context error", err)
	}
	_, err = e.m.BeginSnapshotImport()
	requireFailedPrecondition(t, err)
	end()
	if err := e.m.RemoveAll(context.Background()); err != nil {
		t.Fatalf("retry = %v", err)
	}
}

// TestSnapshotImportRegistrationWithoutRemoval checks that imports register
// and publish as before while no removal runs.
func TestSnapshotImportRegistrationWithoutRemoval(t *testing.T) {
	e := newEnv(t)
	snapID, _ := uuidv7.NewString()
	end, err := e.m.BeginSnapshotImport()
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	if _, err := e.m.AddImportedSnapshot(&ownsandv1.SnapshotStateEntry{SnapshotId: snapID}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.m.SnapshotContent(snapID); !ok {
		t.Fatal("the imported snapshot is not ready")
	}
}
