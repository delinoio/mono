// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

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
