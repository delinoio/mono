// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// stopVM stops the fake VM of the sandbox as a VM that exits on its own.
func (e *env) stopVM(id string) {
	e.t.Helper()
	e.rt.mu.Lock()
	e.rt.vms[id].stop()
	e.rt.mu.Unlock()
}

func TestTickRetriesLostVMAfterRevisionFailure(t *testing.T) {
	const (
		lost    = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
		idle    = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
		max     = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED
		failed  = ownsandv1.SandboxState_SANDBOX_STATE_FAILED
		deleted = ownsandv1.SandboxState_SANDBOX_STATE_DELETED
	)
	for _, tc := range []struct {
		name    string
		pause   bool
		elapsed time.Duration
		state   ownsandv1.SandboxState
		reason  ownsandv1.SandboxEndReason
	}{
		{"running", false, 0, failed, lost},
		{"running past idle", false, 61 * time.Minute, deleted, idle},
		{"paused", true, 0, failed, lost},
		{"paused past idle", true, 61 * time.Minute, failed, lost},
		{"paused past maximum", true, 2 * time.Hour, deleted, max},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := &lockedBuffer{}
				e := newEnvWithLogger(t, slog.New(slog.NewTextHandler(logs, nil)))
				live := e.create(time.Hour, 2*time.Hour)
				id := live.GetSandboxId()
				e.ex.running[id] = true
				if tc.pause {
					var err error
					live, err = e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id})
					if err != nil {
						t.Fatal(err)
					}
				}
				restore := breakFile(t, e.st.Path(store.AgentFile))
				before := e.out.count()
				requireUnchanged := func() {
					t.Helper()
					entry := e.m.StateReport(1, nil).GetSandboxes()[0]
					if entry.GetState() != live.GetState() || entry.GetRevision().GetCounter() != live.GetRevision().GetCounter() || e.out.count() != before {
						t.Fatalf("live revision changed or an update was published while storage was down: %v", entry)
					}
					if !e.rt.hasFiles(id) || e.rt.deleteCallCount() != 0 || e.ex.endedCalls(id) != 0 {
						t.Fatal("cleanup started before the revision was saved")
					}
				}
				requireRevisionError := func(message string) {
					t.Helper()
					for _, line := range strings.Split(logs.String(), "\n") {
						if strings.Contains(line, message) && strings.Contains(line, "sandboxes: persist revision:") {
							return
						}
					}
					t.Fatalf("missing revision error %q in logs:\n%s", message, logs.String())
				}

				e.stopVM(id)
				synctest.Wait()
				requireRevisionError("record lost sandbox failed")
				requireUnchanged()

				e.tickAndWait()
				requireRevisionError("end sandbox failed")
				requireUnchanged()

				e.clk.Advance(tc.elapsed)
				restore()
				e.tickAndWait()
				e.tickAndWait()
				e.tickAndWait()

				entry := e.m.StateReport(2, nil).GetSandboxes()[0]
				if entry.GetState() != tc.state || entry.GetEndReason() != tc.reason || entry.GetRevision().GetCounter() <= live.GetRevision().GetCounter() {
					t.Fatalf("terminal entry = %v, want a newer %v with reason %v", entry, tc.state, tc.reason)
				}
				if e.rt.hasFiles(id) || e.rt.deleteCallCount() != 1 {
					t.Fatal("expected one runtime delete and no remaining files")
				}
				e.ex.mu.Lock()
				calls, reason := e.ex.calls[id], e.ex.ended[id]
				e.ex.mu.Unlock()
				if calls != 1 || reason != tc.reason {
					t.Fatalf("exec tracker notified %d times with %v, want one notification with %v", calls, reason, tc.reason)
				}
				e.out.mu.Lock()
				defer e.out.mu.Unlock()
				updates := e.out.entries[before:]
				if len(updates) != 2 || updates[0].GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETING ||
					updates[0].GetRevision().GetCounter() <= live.GetRevision().GetCounter() || updates[0].GetRevision().GetCounter() >= entry.GetRevision().GetCounter() ||
					updates[1].GetRevision().GetCounter() != entry.GetRevision().GetCounter() || updates[1].GetState() != tc.state || updates[1].GetEndReason() != tc.reason {
					t.Fatalf("published updates = %v, want DELETING followed by the terminal entry", updates)
				}
			})
		})
	}
}

func TestTickRetriesCallerDeleteAfterVMExitsDuringCleanup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fails int
	}{
		{"first delete succeeds", 0},
		{"next tick succeeds", 1},
		{"next tick also fails", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				ctx := context.Background()
				id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
				started, block := make(chan struct{}), make(chan struct{})
				e.rt.mu.Lock()
				e.rt.deleteStarted, e.rt.deleteBlock, e.rt.deleteFails = started, block, tc.fails
				e.rt.mu.Unlock()

				done := make(chan error, 1)
				go func() {
					_, err := e.m.Delete(ctx, &ownsandv1.DeleteSandboxCommand{SandboxId: id})
					done <- err
				}()
				<-started
				e.stopVM(id)
				// Let the watcher handle the exit while Delete still owns the end.
				synctest.Wait()
				close(block)
				deleteErr := <-done
				e.rt.mu.Lock()
				e.rt.deleteBlock = nil
				e.rt.mu.Unlock()
				if tc.fails > 0 {
					requireInternal(t, deleteErr)
				} else if deleteErr != nil {
					t.Fatal(deleteErr)
				}

				for attempt := 0; attempt < tc.fails; attempt++ {
					if state, _ := e.m.State(id); state != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
						t.Fatalf("state after failed cleanup %d = %v, want DELETING", attempt+1, state)
					}
					if !e.rt.hasFiles(id) || e.ex.endedCalls(id) != 0 {
						t.Fatalf("cleanup %d removed files or notified the exec tracker despite failure", attempt+1)
					}
					e.tickAndWait()
					if calls := e.rt.deleteCallCount(); calls != attempt+2 {
						t.Fatalf("runtime Delete called %d times after tick %d, want %d", calls, attempt+1, attempt+2)
					}
				}
				e.tickAndWait()
				if calls := e.rt.deleteCallCount(); calls != tc.fails+1 {
					t.Fatalf("runtime Delete called %d times, want %d", calls, tc.fails+1)
				}
				if e.rt.hasFiles(id) {
					t.Fatal("runtime files remain after cleanup")
				}
				if state, _ := e.m.State(id); state != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
					t.Fatalf("final state = %v, want DELETED", state)
				}
				const caller = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
				e.out.mu.Lock()
				var published []ownsandv1.SandboxEndReason
				for _, entry := range e.out.entries {
					if entry.GetSandboxId() == id && entry.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
						published = append(published, entry.GetEndReason())
					}
				}
				e.out.mu.Unlock()
				if len(published) != 1 || published[0] != caller {
					t.Fatalf("published DELETED reasons = %v, want one DELETED_BY_CALLER", published)
				}
				e.ex.mu.Lock()
				reason := e.ex.ended[id]
				e.ex.mu.Unlock()
				if calls := e.ex.endedCalls(id); calls != 1 || reason != caller {
					t.Fatalf("exec tracker notified %d times with %v, want one DELETED_BY_CALLER", calls, reason)
				}
			})
		})
	}
}

// TestVMExitAfterFailedCallerDeleteEndsDeletedByCaller checks that a VM exit
// while a failed caller delete leaves the record in DELETING completes the
// delete with the recorded reason and does not report a lost VM.
func TestVMExitAfterFailedCallerDeleteEndsDeletedByCaller(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.failCallerDelete(id, false)
	e.stopVM(id)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason %v, want DELETED_BY_CALLER", r)
	}
	e.ex.mu.Lock()
	r := e.ex.ended[id]
	e.ex.mu.Unlock()
	if r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("exec tracker end reason %v, want DELETED_BY_CALLER", r)
	}
	if n := e.ex.endedCalls(id); n != 1 {
		t.Fatalf("exec tracker notified %d times, want 1", n)
	}
}

// TestVMExitAfterFailedCallerDeleteKeepsReasonWhenDeadlinePassed checks that a
// passed deadline does not replace the caller reason recorded by the failed
// delete when the VM exits.
func TestVMExitAfterFailedCallerDeleteKeepsReasonWhenDeadlinePassed(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.failCallerDelete(id, false)
	e.clk.Advance(61 * time.Minute)
	e.stopVM(id)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason %v, want DELETED_BY_CALLER", r)
	}
}

// TestVMExitAfterPassedIdleDeadlineEndsWithTTLReason checks that a running
// sandbox whose VM exits after the idle deadline passed ends with the TTL
// reason and not LOST_ON_DEVICE.
func TestVMExitAfterPassedIdleDeadlineEndsWithTTLReason(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.clk.Advance(61 * time.Minute)
	e.stopVM(id)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("end reason %v, want IDLE_TTL_EXPIRED", r)
	}
}
