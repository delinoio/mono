// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestRemovalFencesInFlightSnapshotImport pauses an import after it wrote the
// snapshot content and before it publishes the ready entry, runs the device
// removal to the point where it can finish, and releases the import. The
// import must not create snapshot metadata or directories after the clean-up
// boundary, so the state directory is empty when both ended.
func TestRemovalFencesInFlightSnapshotImport(t *testing.T) {
	f := newImportFixture(t, nil)
	a := f.h.agent
	id, _ := uuidv7.NewString()
	staged := f.stage(t, id, false)

	entered := make(chan struct{})
	release := make(chan struct{})
	a.beforeSnapshotPublishHook = func() {
		close(entered)
		<-release
	}
	// A failed assertion must not leave the import blocked: the agent
	// cleanup waits for its commands.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	cmdID := f.h.sendCommand(staged)
	waitClosed(t, entered, "the import before its publication")

	f.h.cp.MarkRemoved(f.h.id.DeviceID)
	removed := make(chan error, 1)
	go func() { removed <- a.removeDevice(context.Background()) }()
	// Without the fence the removal finishes here and wipes the state
	// directory while the import still holds its content.
	settle()
	close(release)

	res := f.h.result(cmdID)
	if res.GetError() == nil && res.GetImportSnapshot().GetSnapshot() == nil {
		t.Fatalf("import result has neither an entry nor an error: %v", res)
	}
	select {
	case err := <-removed:
		if !errors.Is(err, ErrRemoved) {
			t.Fatalf("removal returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("removal did not finish")
	}
	// Whether the import was refused or published before the boundary, the
	// removal leaves nothing behind.
	requireEmptyStateDir(t, f.h.st)
	if dir, err := f.h.st.SnapshotDir(id); err == nil {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("snapshot directory %q exists after the removal", dir)
		}
	}
	if a.Sandboxes.HasSnapshot(id) {
		t.Error("snapshot record exists after the removal")
	}
}

// TestImportRefusedAfterRemovalStarted checks that an import that starts
// after the removal fenced the device is refused and writes nothing.
func TestImportRefusedAfterRemovalStarted(t *testing.T) {
	f := newImportFixture(t, nil)
	a := f.h.agent
	id, _ := uuidv7.NewString()
	staged := f.stage(t, id, false)

	f.h.cp.MarkRemoved(f.h.id.DeviceID)
	// Fence the manager without finishing the removal.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Sandboxes.RemoveAll(ctx); err != nil {
		t.Fatalf("RemoveAll = %v", err)
	}
	res := f.h.command(staged)
	if res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("import after the fence = %v, want FAILED_PRECONDITION", res)
	}
	if dir, err := f.h.st.SnapshotDir(id); err == nil {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("refused import created %q", dir)
		}
	}
}
