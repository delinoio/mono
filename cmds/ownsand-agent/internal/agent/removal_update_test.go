// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func (h *harness) requestDeviceRemoval(viaAuthentication bool) {
	h.t.Helper()
	h.cp.MarkRemoved(h.id.DeviceID)
	if viaAuthentication {
		h.cp.Current().Close()
		return
	}
	id, _ := uuidv7.NewString()
	cmd := &ownsandv1.AgentCommand{CommandId: id, Command: &ownsandv1.AgentCommand_CleanUp{CleanUp: &ownsandv1.CleanUpCommand{}}}
	if !h.cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}) {
		h.t.Fatal("could not send the clean-up command")
	}
}

func (h *harness) requireRunResult(want error) {
	h.t.Helper()
	select {
	case err := <-h.errc:
		h.errc <- err
		if !errors.Is(err, want) {
			h.t.Fatalf("Run returned %v, want %v", err, want)
		}
	case <-time.After(10 * time.Second):
		h.t.Fatal("agent did not exit")
	}
}

func queueRemovalTestUpdate(h *harness, recovery bool) {
	h.t.Helper()
	cmd := &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0", RevocationRecovery: recovery}
	if res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_UpdateAgent{UpdateAgent: cmd}}); res.GetError() != nil {
		h.t.Fatalf("update command: %v", res.GetError())
	}
}

func TestRemovalDiscardsQueuedUpdate(t *testing.T) {
	for _, entry := range []struct {
		name              string
		viaAuthentication bool
	}{
		{name: "cleanup-command"},
		{name: "authenticate-notice", viaAuthentication: true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, release := range []struct {
				name     string
				revoked  []string
				expected ownsandv1.AgentUpdateState
			}{
				{name: "ordinary", expected: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE},
				{name: "revocation-recovery", revoked: []string{"1.0.0"}, expected: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE},
			} {
				t.Run(release.name, func(t *testing.T) {
					cp := testcp.New(t)
					st, id := enrollDevice(t, cp)
					env := newUpdateEnv(t, false, release.revoked...)
					confirm := confirmRemovalGate{entered: make(chan struct{}), resume: make(chan struct{})}
					h := startWith(t, cp, st, id, func(o *Options) {
						env.tweak(o)
						confirm.AgentServiceClient = o.Control.Agent
						o.Control.Agent = &confirm
					})
					h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
					sandboxID, _ := uuidv7.NewString()
					if res := h.command(createCommand(sandboxID)); res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
						t.Fatalf("create before removal: %v", res)
					}
					queueRemovalTestUpdate(h, len(release.revoked) > 0)
					if state, target := h.agent.updater.State(); state != release.expected || target != "1.1.0" {
						t.Fatalf("queued update state: %v %q", state, target)
					}

					h.requestDeviceRemoval(entry.viaAuthentication)
					waitClosed(t, confirm.entered, "removal confirmation")
					if h.agent.Sandboxes.Busy() {
						t.Fatal("device is still busy at removal confirmation")
					}
					if installed, err := h.agent.updater.TryInstall(); installed || err != nil || env.installs.Load() != 0 {
						t.Fatalf("installed during removal: %t, %v, calls=%d", installed, err, env.installs.Load())
					}
					close(confirm.resume)
					h.requireRunResult(ErrRemoved)
					requireEmptyStateDir(t, st)
				})
			}
		})
	}
}

func waitForRemovalNotice(t *testing.T, st *store.Store) *store.AgentState {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if state, err := st.LoadAgent(); err == nil && state.Removal.NoticeReceived {
				return state
			}
		case <-deadline.C:
			t.Fatal("removal notice was not recorded")
		}
	}
}

func TestRemovalDrainsAdmittedInstaller(t *testing.T) {
	installError := errors.New("injected install failure")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "failure", err: installError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := testcp.New(t)
			st, id := enrollDevice(t, cp)
			env := newUpdateEnv(t, true)
			executable := filepath.Join(t.TempDir(), "ownsand-agent")
			if err := os.WriteFile(executable, []byte("old agent binary"), 0o700); err != nil {
				t.Fatal(err)
			}
			env.install = func(path string) error {
				if tc.err != nil {
					return tc.err
				}
				return (update.ExecutableInstaller{Path: executable}).Install(path)
			}
			confirm := confirmRemovalGate{entered: make(chan struct{}), resume: make(chan struct{})}
			h := startWith(t, cp, st, id, func(o *Options) {
				env.tweak(o)
				confirm.AgentServiceClient = o.Control.Agent
				o.Control.Agent = &confirm
			})
			unblock := sync.OnceFunc(func() { close(env.unblock) })
			t.Cleanup(unblock)
			h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
			queueRemovalTestUpdate(h, false)
			waitClosed(t, env.entered, "an admitted installation")
			h.requestDeviceRemoval(false)
			state := waitForRemovalNotice(t, st)
			if state.PendingUpdate == nil || state.Removal.CleanupDone {
				t.Fatalf("state while the installer is held: %+v", state)
			}
			artifacts, err := os.ReadDir(st.Path(store.UpdateDir))
			if err != nil || len(artifacts) != 1 {
				t.Fatalf("installer artifact while removal waits: %v, %v", artifacts, err)
			}
			select {
			case <-confirm.entered:
				t.Fatal("removal reached confirmation with the installer still held")
			case <-time.After(100 * time.Millisecond):
			}
			unblock()
			waitClosed(t, confirm.entered, "removal confirmation")
			state, err = st.LoadAgent()
			if err != nil || !state.Removal.CleanupDone || (state.PendingUpdate == nil) != (tc.err != nil) {
				t.Fatalf("terminal update state at removal confirmation: %+v, %v", state, err)
			}
			close(confirm.resume)
			h.requireRunResult(ErrRemoved)
			requireEmptyStateDir(t, st)
			select {
			case <-h.agent.restart:
				t.Fatal("installer signaled restart during removal")
			default:
			}
			got, err := os.ReadFile(executable)
			want := "new agent binary"
			if tc.err != nil {
				want = "old agent binary"
			}
			if err != nil || string(got) != want {
				t.Fatalf("executable contents: %q, %v, want %q", got, err, want)
			}
		})
	}
}

func TestInterruptedRemovalKeepsUpdaterFenced(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	env := newUpdateEnv(t, true)
	a := newAgent(t, cp, st, id, env.tweak)
	t.Cleanup(a.shutdown)
	unblock := sync.OnceFunc(func() { close(env.unblock) })
	t.Cleanup(unblock)
	cmd := &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}
	if err := a.updater.Handle(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	installDone := make(chan error, 1)
	go func() { _, err := a.updater.TryInstall(); installDone <- err }()
	waitClosed(t, env.entered, "an admitted installation")
	cp.MarkRemoved(id.DeviceID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	removed := make(chan error, 1)
	go func() { removed <- a.removeDevice(ctx) }()
	waitForRemovalNotice(t, st)
	cancel()
	select {
	case err := <-removed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted removal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("removal cancellation waited for the held installer")
	}
	if state, err := st.LoadAgent(); err != nil || !state.Removal.NoticeReceived || state.Removal.CleanupDone {
		t.Fatalf("removal state after cancellation: %+v, %v", state, err)
	}
	if _, err := st.LoadIdentity(); err != nil || cp.Confirms() != 0 {
		t.Fatalf("interrupted removal lost retry state: identity error %v, confirmations %d", err, cp.Confirms())
	}
	unblock()
	select {
	case err := <-installDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("installer did not finish")
	}
	if err := a.updater.Handle(t.Context(), cmd); !errors.Is(err, update.ErrRemoving) {
		t.Fatalf("update after interrupted removal: %v", err)
	}
	if err := a.updater.Withdraw(); !errors.Is(err, update.ErrRemoving) {
		t.Fatalf("withdrawal after interrupted removal: %v", err)
	}
	select {
	case <-a.restart:
		t.Fatal("installer signaled restart after interrupted removal")
	default:
	}
	retry := newAgent(t, cp, st, id, nil)
	if err := retry.Run(t.Context()); !errors.Is(err, ErrRemoved) {
		t.Fatalf("removal retry returned %v", err)
	}
	if cp.Confirms() != 1 {
		t.Fatalf("removal confirmations after retry: %d", cp.Confirms())
	}
	requireEmptyStateDir(t, st)
}

func TestQueuedUpdateInstallsAfterSandboxStops(t *testing.T) {
	cp := testcp.New(t)
	st, id := enrollDevice(t, cp)
	env := newUpdateEnv(t, false)
	h := startWith(t, cp, st, id, env.tweak)
	h.wait(0, "state report", func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateReport() != nil })
	sandboxID, _ := uuidv7.NewString()
	if res := h.command(createCommand(sandboxID)); res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create before update: %v", res)
	}
	queueRemovalTestUpdate(h, false)
	release, err := h.agent.updater.BeginTransfer()
	if err != nil {
		t.Fatal(err)
	}
	del := &ownsandv1.DeleteSandboxCommand{SandboxId: sandboxID}
	if res := h.command(&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: del}}); res.GetError() != nil {
		t.Fatalf("delete before update: %v", res.GetError())
	}
	release() // Wake the updater without waiting for its periodic idle check.
	h.requireRunResult(ErrRestart)
	if n := env.installs.Load(); n != 1 {
		t.Fatalf("installer calls = %d, want 1", n)
	}
}
