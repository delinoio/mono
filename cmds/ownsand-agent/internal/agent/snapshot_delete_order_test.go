// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// gatedPauseRuntime blocks Pause until release is closed, so that a pause
// command holds the command lock of its sandbox, and counts the captures.
type gatedPauseRuntime struct {
	vmruntime.Runtime
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	snapshots atomic.Int32
}

func (r *gatedPauseRuntime) Pause(ctx context.Context, vm vmruntime.VM) error {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Runtime.Pause(ctx, vm)
}

func (r *gatedPauseRuntime) Snapshot(ctx context.Context, vm vmruntime.VM, dir string) (vmruntime.SnapshotInfo, error) {
	r.snapshots.Add(1)
	return r.Runtime.Snapshot(ctx, vm, dir)
}

// TestDeleteSnapshotReceivedAfterCreateCancelsCreateOnSameChannel checks that
// a delete_snapshot command that the control plane sends right after a
// create_snapshot command, while the create waits for the command lock of its
// sandbox, leaves no snapshot on the same channel: the agent sends its
// snapshot inventory only when a channel opens, so a copy that survived the
// delete would stay until a reconnect.
func TestDeleteSnapshotReceivedAfterCreateCancelsCreateOnSameChannel(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	rt := &gatedPauseRuntime{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releasePause := func() { releaseOnce.Do(func() { close(rt.release) }) }
	t.Cleanup(releasePause)
	h := startWith(t, cp, st, id, func(o *Options) {
		rt.Runtime = o.Runtime
		o.Runtime = rt
	})
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{Limits: limits}}})
	sandboxID, _ := uuidv7.NewString()
	if res := h.command(createCommand(sandboxID)); res.GetError() != nil {
		t.Fatalf("create sandbox = %v", res)
	}
	channels := cp.Channels()

	// The pause holds the sandbox command lock, so the snapshot create that
	// follows waits for it.
	pauseID := h.sendCommand(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: sandboxID}}})
	<-rt.entered
	snapID, _ := uuidv7.NewString()
	createID := h.sendCommand(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: sandboxID}}})
	deleteID := h.sendCommand(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSnapshot{DeleteSnapshot: &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}}})
	if res := h.result(deleteID); res.GetError() != nil {
		t.Fatalf("delete = %v", res)
	}
	releasePause()
	if res := h.result(pauseID); res.GetError() != nil {
		t.Fatalf("pause = %v", res)
	}
	res := h.result(createID)
	if res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED || res.GetCreateSnapshot() != nil {
		t.Fatalf("create after the delete = %v, want ABORTED and no snapshot", res)
	}

	if n := rt.snapshots.Load(); n != 0 {
		t.Errorf("runtime captures = %d, want 0", n)
	}
	if h.agent.Sandboxes.HasSnapshot(snapID) {
		t.Error("snapshot is known to the manager")
	}
	if report := h.agent.Sandboxes.StateReport(2, nil); len(report.GetSnapshots()) != 0 {
		t.Errorf("state report holds %v", report.GetSnapshots())
	}
	dir, err := h.st.SnapshotDir(snapID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot directory after the canceled create: %v", err)
	}
	if got := cp.Channels(); got != channels {
		t.Errorf("channels = %d, want %d: the agent reconnected", got, channels)
	}
}

func TestDeleteSnapshotAfterStagingWithFailedCleanup(t *testing.T) {
	for _, scenario := range []string{"retry", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			cp := testcp.New(t)
			st, id := enrollDevice(t, cp)
			h := start(t, cp, st, id)
			isReport := func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil }
			h.wait(0, "initial state report", isReport)
			sandboxID, _ := uuidv7.NewString()
			snapID, _ := uuidv7.NewString()
			readyID, _ := uuidv7.NewString()
			create := func(id, source string) *ownsandv1.AgentCommand {
				return &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{
					CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SnapshotId: id, SandboxId: source},
				}}
			}
			deleteCmd := &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSnapshot{
				DeleteSnapshot: &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID},
			}}
			if res := h.command(createCommand(sandboxID)); res.GetError() != nil {
				t.Fatalf("create sandbox: %v", res)
			}
			if res := h.command(create(readyID, sandboxID)); res.GetCreateSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
				t.Fatalf("create ordinary snapshot: %v", res)
			}

			staged, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			h.agent.Sandboxes.SetSnapshotPublishHookForTest(func(id string) {
				if id == snapID {
					close(staged)
					<-resume
				}
			})
			var removals atomic.Int32
			h.agent.Sandboxes.SetSnapshotRemovalHookForTest(func(id string) error {
				if id == snapID && removals.Add(1) == 1 {
					return errors.New("injected snapshot removal failure")
				}
				return nil
			})
			createID := h.sendCommand(create(snapID, sandboxID))
			select {
			case <-staged:
			case <-time.After(10 * time.Second):
				t.Fatal("capture did not stage its record")
			}
			if res := h.command(deleteCmd); res.GetError() != nil {
				t.Fatalf("delete during staging: %v", res)
			}
			release()
			res := h.result(createID)
			if res.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED || res.GetCreateSnapshot() != nil {
				t.Fatalf("overtaken create: %v, want ABORTED and no snapshot", res)
			}
			if removals.Load() != 1 {
				t.Fatalf("cleanup attempts = %d, want 1", removals.Load())
			}
			dir, err := st.SnapshotDir(snapID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "content", "disk")); err != nil {
				t.Fatalf("expected surviving capture content: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".record.pending")); err != nil {
				t.Fatalf("expected surviving staged record: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "record.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published record after aborted create: %v, want absent", err)
			}
			checkUnavailable := func() {
				t.Helper()
				if _, release, ok := h.agent.Sandboxes.AcquireSnapshot(snapID); ok {
					release()
					t.Fatal("abandoned snapshot was acquirable")
				}
			}
			checkUnavailable()
			reported := func(report *ownsandv1.StateReport, id string) ownsandv1.SnapshotState {
				for _, snap := range report.GetSnapshots() {
					if snap.GetSnapshotId() == id {
						return snap.GetState()
					}
				}
				return ownsandv1.SnapshotState_SNAPSHOT_STATE_UNSPECIFIED
			}
			if state := reported(h.agent.Sandboxes.StateReport(2, nil), snapID); state != ownsandv1.SnapshotState_SNAPSHOT_STATE_DELETING {
				t.Fatalf("cleanup state = %v, want DELETING", state)
			}
			if scenario == "restart" {
				h.stop()
				select {
				case err := <-h.errc:
					h.errc <- err // The harness cleanup still waits for this result.
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Fatalf("agent stopped with an error: %v", err)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("agent did not stop")
				}
				from := cp.Received()
				h = start(t, cp, st, id)
				env, _ := h.wait(from, "state report after restart", isReport)
				if snapshots := env.GetStateReport().GetSnapshots(); len(snapshots) != 1 || reported(env.GetStateReport(), readyID) != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
					t.Fatalf("snapshots after restart = %v, want only the ordinary READY snapshot", snapshots)
				}
				checkUnavailable()
				sandboxID, _ = uuidv7.NewString()
				if res := h.command(createCommand(sandboxID)); res.GetError() != nil {
					t.Fatalf("create sandbox after restart: %v", res)
				}
			} else {
				if res := h.command(deleteCmd); res.GetError() != nil || removals.Load() != 2 {
					t.Fatalf("retried delete: %v, cleanup attempts = %d", res, removals.Load())
				}
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capture directory after cleanup: %v, want absent", err)
			}
			h.agent.Sandboxes.SetSnapshotPublishHookForTest(nil)
			if res := h.command(create(snapID, sandboxID)); res.GetCreateSnapshot().GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
				t.Fatalf("fresh create for the same ID: %v", res)
			}
		})
	}
}
