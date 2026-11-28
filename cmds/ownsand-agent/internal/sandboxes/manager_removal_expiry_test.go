// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

func TestTickDoesNotClaimLiveEndsAfterRemovalFence(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	expired := e.create(time.Minute, time.Hour).GetSandboxId()
	retry := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.clk.Advance(time.Minute)

	e.m.mu.Lock()
	e.m.sandboxes[retry].vmExitPending = true
	e.m.mu.Unlock()
	if err := e.m.fenceSnapshotImports(ctx); err != nil {
		t.Fatal(err)
	}

	e.m.Tick(ctx, false)
	e.m.bg.Wait()
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	for _, id := range []string{expired, retry} {
		if e.m.sandboxes[id].ending {
			t.Errorf("Tick claimed sandbox %s after the removal fence", id)
		}
	}
	if calls := e.rt.deleteCallCount(); calls != 0 {
		t.Errorf("runtime Delete calls = %d, want 0", calls)
	}
}

// expiryLogGate holds Tick after it queues the expiry, before the worker
// takes the sandbox command lock.
type expiryLogGate struct {
	queued  chan struct{}
	release chan struct{}
}

func (g expiryLogGate) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "sandbox TTL expired") {
		close(g.queued)
		<-g.release
	}
	return len(p), nil
}

func (g expiryLogGate) hold(t *testing.T, m *Manager) func() {
	t.Helper()
	release := sync.OnceFunc(func() { close(g.release) })
	t.Cleanup(release)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Tick(context.Background(), false)
	}()
	select {
	case <-g.queued:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick did not queue the expiry")
	}
	return func() {
		release()
		<-done
		m.bg.Wait()
	}
}

func TestTickDropsQueuedExpiryAfterRemoval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
	}{
		{name: "after wipe"},
		{name: "after reset", reset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			gate := expiryLogGate{queued: make(chan struct{}), release: make(chan struct{})}
			e := newEnvWithLogger(t, slog.New(slog.NewTextHandler(gate, nil)))
			e.create(time.Minute, time.Hour)
			e.clk.Advance(time.Minute)
			updates := e.out.count()

			finishExpiry := gate.hold(t, e.m)

			cleanup, err := e.st.LockCleanup(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup.Close()
			if err := e.m.RemoveAll(ctx); err != nil {
				t.Fatal(err)
			}
			if err := cleanup.WipeLocalData(); err != nil {
				t.Fatal(err)
			}
			before, err := e.st.LoadAgent()
			if err != nil {
				t.Fatal(err)
			}
			if tc.reset {
				if err := cleanup.Reset(); err != nil {
					t.Fatal(err)
				}
			}

			finishExpiry()
			if calls := e.rt.deleteCallCount(); calls != 1 {
				t.Errorf("runtime Delete calls = %d, want 1", calls)
			}
			if after := e.out.count(); after != updates {
				t.Errorf("updates after delayed expiry = %d, want %d", after, updates)
			}
			if tc.reset {
				entries, err := os.ReadDir(e.st.Dir())
				if err != nil || len(entries) != 1 || entries[0].Name() != store.EnrollmentLockFile {
					t.Fatalf("state directory after reset = %v, %v, want only %s", entries, err, store.EnrollmentLockFile)
				}
				return
			}
			entries, err := os.ReadDir(e.st.Path(store.SandboxesDir))
			if err != nil || len(entries) != 0 {
				t.Errorf("sandbox records after wipe = %v, %v, want an empty directory", entries, err)
			}
			after, err := e.st.LoadAgent()
			if err != nil {
				t.Fatal(err)
			}
			if after.RevisionCounter != before.RevisionCounter {
				t.Errorf("revision after wipe = %d, want %d", after.RevisionCounter, before.RevisionCounter)
			}
		})
	}
}

func TestTickDropsQueuedExpiryOfReplacedSandbox(t *testing.T) {
	gate := expiryLogGate{queued: make(chan struct{}), release: make(chan struct{})}
	e := newEnvWithLogger(t, slog.New(slog.NewTextHandler(gate, nil)))
	entry := e.create(time.Minute, time.Hour)
	id := entry.GetSandboxId()
	e.clk.Advance(time.Minute)
	updates := e.out.count()
	finishExpiry := gate.hold(t, e.m)

	e.m.mu.Lock()
	e.m.sandboxes[id] = &sandbox{rec: e.m.sandboxes[id].rec}
	e.m.mu.Unlock()

	finishExpiry()
	if calls := e.rt.deleteCallCount(); calls != 0 {
		t.Errorf("runtime Delete calls = %d, want 0", calls)
	}
	after, err := e.st.LoadAgent()
	if err != nil {
		t.Fatal(err)
	}
	if after.RevisionCounter != entry.GetRevision().GetCounter() || e.out.count() != updates {
		t.Errorf("a queued expiry wrote a revision or update for a replaced sandbox")
	}
}
