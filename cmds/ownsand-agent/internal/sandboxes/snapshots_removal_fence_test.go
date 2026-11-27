// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
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
	for _, tc := range []struct {
		name               string
		probeBeforeRemoval bool
	}{
		{name: "first probe before fence", probeBeforeRemoval: true},
		{name: "first probe after fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				snapID, err := uuidv7.NewString()
				if err != nil {
					t.Fatal(err)
				}
				end, err := e.m.BeginSnapshotImport()
				if err != nil {
					t.Fatal(err)
				}
				defer end()

				probeImport := func() error {
					release, err := e.m.BeginSnapshotImport()
					if err == nil {
						release()
					}
					return err
				}
				if tc.probeBeforeRemoval {
					if err := probeImport(); err != nil {
						t.Fatalf("probe before removal = %v", err)
					}
				}

				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				removed := make(chan error, 1)
				go func() { removed <- e.m.RemoveAll(ctx) }()
				// Let removal set the fence and reach the import drain.
				synctest.Wait()
				select {
				case err := <-removed:
					t.Fatalf("RemoveAll finished while an import was registered: %v", err)
				default:
				}

				requireFailedPrecondition(t, probeImport())
				_, err = e.m.AddImportedSnapshot(t.Context(), &ownsandv1.SnapshotStateEntry{SnapshotId: snapID})
				requireFailedPrecondition(t, err)
				if _, _, ok := e.m.SnapshotContent(snapID); ok {
					t.Fatal("a refused import left a snapshot entry")
				}
				if e.m.HasSnapshot(snapID) {
					t.Fatal("a refused import left a snapshot record in the manager")
				}
				dir, err := e.st.SnapshotDir(snapID)
				if err != nil {
					t.Fatal(err)
				}
				// Check before RemoveAll can wipe the snapshot directory.
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stat of refused snapshot directory = %v, want not exist", err)
				}

				end()
				end() // idempotent
				if err := <-removed; err != nil {
					t.Fatalf("RemoveAll after the import ended = %v", err)
				}
			})
		})
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
	if _, err := e.m.AddImportedSnapshot(context.Background(), &ownsandv1.SnapshotStateEntry{SnapshotId: snapID}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.m.SnapshotContent(snapID); !ok {
		t.Fatal("the imported snapshot is not ready")
	}
}
