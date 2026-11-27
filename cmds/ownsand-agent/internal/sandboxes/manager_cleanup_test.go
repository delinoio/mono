// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestRestartRecoversFailedCreateWithoutCleanupMarker(t *testing.T) {
	e := newEnv(t)
	id, creating, restore := failCreateSave(t, e, func(id string) func() {
		return blockCleanupMarkerSave(t, e.st, id)
	})
	defer restore()
	if got := persistedRecord(t, e.st, id); got != creating {
		t.Fatalf("record changed after marker save failed: %v", got)
	}
	e.rt = newFakeRuntime()
	e.rt.files[id] = true
	e.restart()
	if e.rt.hasFiles(id) || endReason(e.m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatal("startup did not clean up the VM through the saved live record")
	}
}

func TestFailedCreateMarkerClearFailureFencesReplacement(t *testing.T) {
	e := newEnv(t)
	id := retainedFailedCreate(t, e, true)
	marker := e.st.Path(store.SandboxesDir, id+".cleanup")
	restore := breakFile(t, marker)
	// A nonempty directory makes os.Remove fail on all supported platforms.
	blocker := e.st.Path(store.SandboxesDir, id+".cleanup", "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.tickAndWait()
	if e.rt.hasFiles(id) || e.pendingCleanups() != 1 {
		t.Fatal("runtime cleanup should succeed but the marker must stay pending")
	}
	cmd := e.createCmd(1, time.Hour, time.Hour)
	cmd.SandboxId = id
	requireCleanupCreateFence(t, e, cmd)

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	restore()
	e.tickAndWait()
	if got := e.rt.deleteCallCount(); got != 2 || e.pendingCleanups() != 0 {
		t.Fatalf("after retry: Delete calls = %d, pending = %d", got, e.pendingCleanups())
	}
	e.rt.createErr = nil
	if entry, err := e.m.Create(t.Context(), cmd); err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("replacement after confirmed cleanup = %v, %v", entry, err)
	}
}

func TestRestartRetriesUnownedSurvivorOnTick(t *testing.T) {
	e := newEnv(t)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	e.rt.retainFailedCreate(id)
	e.rt.deleteFails = 1
	e.restart()
	requireCleanupCreateFence(t, e, cmd)
	e.tickAndWait()
	e.tickAndWait()
	if e.rt.hasFiles(id) || e.pendingCleanups() != 0 || e.rt.deleteCallCount() != 2 {
		t.Fatal("tick should delete the survivor once and clear the pending ID")
	}
}

func TestRestartRemovalWaitsForAcknowledgedSurvivor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		cmd := e.createCmd(1, time.Hour, time.Hour)
		id := cmd.GetSandboxId()
		rec := Record{SandboxID: id, State: ownsandv1.SandboxState_SANDBOX_STATE_FAILED, Revision: 1}
		if err := e.st.SaveSandbox(id, rec); err != nil {
			t.Fatal(err)
		}
		e.rt.retainFailedCreate(id)
		e.rt.deleteFails = 2
		e.restart()
		e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: rec.Entry(e.m.Epoch()).GetRevision()})
		if _, found := e.m.State(id); found {
			t.Fatal("terminal acknowledgement kept the sandbox record")
		}
		requireCleanupCreateFence(t, e, cmd)
		keep := e.st.Path(store.ImagesDir, "sentinel")
		if err := os.WriteFile(keep, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.m.RemoveAll(t.Context()); err == nil {
			t.Fatal("removal succeeded after a failed runtime Delete")
		}
		started, block := make(chan struct{}), make(chan struct{})
		e.rt.deleteStarted, e.rt.deleteBlock = started, block
		release := sync.OnceFunc(func() { close(block) })
		defer release()
		e.m.Tick(t.Context(), false)
		<-started
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- e.m.RemoveAll(ctx) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("removal returned during the in-flight Delete: %v", err)
		default:
		}
		if e.rt.deleteCallCount() != 3 {
			t.Fatal("removal started an overlapping Delete")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("removal = %v, want canceled", err)
		}
		if _, err := os.Stat(keep); err != nil || !e.rt.hasFiles(id) || e.pendingCleanups() != 1 {
			t.Fatalf("canceled removal lost local data or pending cleanup: %v", err)
		}
		release()
		e.m.bg.Wait()
		if err := e.removeAllAndWipe(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(keep); !os.IsNotExist(err) || e.rt.hasFiles(id) || e.pendingCleanups() != 0 || e.rt.deleteCallCount() != 3 {
			t.Fatalf("removal retry did not reuse cleanup and wipe local data: %v", err)
		}
	})
}

func requireCleanupCreateFence(t *testing.T, e *env, cmd *ownsandv1.CreateSandboxCommand) {
	t.Helper()
	starts := e.rt.startCount()
	_, err := e.m.Create(t.Context(), cmd)
	var commandErr *CommandError
	if !errors.As(err, &commandErr) || commandErr.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION || e.rt.startCount() != starts {
		t.Fatalf("create while cleanup is pending = %v, starts = %d (was %d)", err, e.rt.startCount(), starts)
	}
}

func blockCleanupMarkerSave(t *testing.T, st *store.Store, id string) func() {
	t.Helper()
	path := st.Path(store.SandboxesDir, id+".cleanup")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestartReusesSuccessfulDeleteForFailedCreateMarker(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "terminal"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			var id string
			if live {
				id = e.create(time.Hour, time.Hour).GetSandboxId()
				if err := e.st.SaveSandboxCleanup(id); err != nil {
					t.Fatal(err)
				}
			} else {
				id = retainedFailedCreate(t, e, false)
			}
			e.restart()
			if e.rt.hasFiles(id) || e.rt.deleteCallCount() != 1 || e.pendingCleanups() != 0 {
				t.Fatalf("startup made %d Deletes and left %d cleanup entries", e.rt.deleteCallCount(), e.pendingCleanups())
			}
			if ids, err := e.st.LoadSandboxCleanups(); err != nil || len(ids) != 0 {
				t.Fatalf("markers after cleanup = %v, %v", ids, err)
			}
			if err := e.m.RemoveAll(t.Context()); err != nil {
				t.Fatal(err)
			}
			if e.rt.deleteCallCount() != 1 {
				t.Fatal("removal repeated the confirmed runtime Delete")
			}
		})
	}
}
