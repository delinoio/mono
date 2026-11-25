// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestRemovalFencesInFlightSnapshotImport verifies that canceled removal
// preserves snapshot storage while an admitted import is still active.
func TestRemovalFencesInFlightSnapshotImport(t *testing.T) {
	source := newImportFixture(t, nil)
	id, _ := uuidv7.NewString()
	staged := source.stage(t, id, false)

	// Import onto a fresh device so sandbox deletion cannot delay the wipe.
	cp := testcp.New(t)
	cp.SetSignedURLBase(source.b.URL)
	st, identity := enrollDevice(t, cp)
	h := start(t, cp, st, identity)
	a := h.agent
	sentinel := st.Path(store.SnapshotsDir, "removal-sentinel")
	if err := os.WriteFile(sentinel, []byte("keep until the import exits"), 0o600); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	hookRelease := make(chan struct{})
	release := sync.OnceFunc(func() { close(hookRelease) })
	a.beforeSnapshotPublishHook = func() {
		close(entered)
		<-hookRelease
	}
	t.Cleanup(release)
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	cmdID := h.sendCommand(staged)
	waitClosed(t, entered, "the import before its publication")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	removed := make(chan error, 1)
	go func() { removed <- a.removeDevice(ctx) }()

	// Wait for the admission fence before canceling. Any probe admitted
	// before the fence releases its reservation without writing data.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	for {
		endImport, err := a.Sandboxes.BeginSnapshotImport()
		if err != nil {
			var refusal *sandboxes.CommandError
			if !errors.As(err, &refusal) || refusal.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
				t.Fatalf("snapshot import admission returned %v, want FAILED_PRECONDITION", err)
			}
			break
		}
		endImport()
		select {
		case err := <-removed:
			t.Fatalf("removal returned %v before it fenced import admission", err)
		case <-deadline:
			t.Fatal("removal did not fence import admission")
		case <-ticker.C:
		}
	}

	cancel()
	select {
	case err := <-removed:
		if _, statErr := os.Stat(sentinel); statErr != nil {
			t.Fatalf("snapshot sentinel while the import is held: %v", statErr)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("removal returned %v while the import is held, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("removal did not return after cancellation while the import is held")
	}

	release()
	if res := h.result(cmdID); res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("import after the fence = %v, want FAILED_PRECONDITION", res)
	}
	h.requestDeviceRemoval(false)
	h.requireRunResult(ErrRemoved)
	requireEmptyStateDir(t, st)
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
