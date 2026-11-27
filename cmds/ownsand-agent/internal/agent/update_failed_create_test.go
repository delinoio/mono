// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type cleanupUpdateLog struct {
	slog.Handler
	failed, complete chan struct{}
}

func (h cleanupUpdateLog) Handle(ctx context.Context, record slog.Record) error {
	switch record.Message {
	case "sandbox cleanup incomplete":
		close(h.failed)
	case "sandbox cleanup complete":
		close(h.complete)
	}
	return h.Handler.Handle(ctx, record)
}

func TestUpdateWaitsForRetainedFailedCreate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		revoked []string
		pending ownsandv1.AgentUpdateState
	}{
		{name: "ordinary", pending: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE},
		{name: "revoked", revoked: []string{"1.0.0"}, pending: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			cp := testcp.New(t)
			st, id := enrollDevice(t, cp)
			env := newUpdateEnv(t, false, tc.revoked...)
			env.install = func(path string) error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if string(data) != "new agent binary" {
					return fmt.Errorf("unexpected verified artifact contents: %q", data)
				}
				return nil
			}
			log := cleanupUpdateLog{
				Handler: quietLogger().Handler(),
				failed:  make(chan struct{}), complete: make(chan struct{}),
			}
			stopStarted, stopResume := make(chan struct{}), make(chan struct{})
			var rt *retainedCreateRuntime
			a := newAgent(t, cp, st, id, func(o *Options) {
				env.tweak(o)
				o.Logger = slog.New(log)
				rt = &retainedCreateRuntime{
					Runtime: o.Runtime, failDeletes: 1,
					firstDelete: func() {
						close(stopStarted)
						<-stopResume
					},
				}
				o.Runtime = rt
			})
			t.Cleanup(a.shutdown)
			resumeStop := sync.OnceFunc(func() { close(stopResume) })
			t.Cleanup(resumeStop)

			sandboxID, err := uuidv7.NewString()
			if err != nil {
				t.Fatal(err)
			}
			a.Sandboxes.SetLimits(limits)
			entry, err := a.Sandboxes.Create(ctx, createCommand(sandboxID).GetCreateSandbox())
			if err == nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED ||
				entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
				t.Fatalf("create = (%v, %v), want FAILED/CREATE_FAILED", entry, err)
			}
			// Stage recovery after the create because accepting the revocation
			// closes the gate to new creates.
			const target = "1.1.0"
			if err := a.updater.Handle(ctx, &ownsandv1.UpdateAgentCommand{TargetVersion: target, RevocationRecovery: len(tc.revoked) > 0}); err != nil {
				t.Fatal(err)
			}
			assertPending := func(stage string) {
				t.Helper()
				installed, err := a.updater.TryInstall()
				state, version := a.updater.State()
				if installed || err != nil || env.installs.Load() != 0 || state != tc.pending || version != target {
					t.Fatalf("%s: TryInstall = (%t, %v), calls = %d, state = (%v, %q), want deferred in %v",
						stage, installed, err, env.installs.Load(), state, version, tc.pending)
				}
				select {
				case <-a.restart:
					t.Fatalf("%s: update requested restart", stage)
				default:
				}
				if running, err := rt.Running(ctx); err != nil || !slices.Equal(running, []string{sandboxID}) {
					t.Fatalf("%s: runtime inventory = (%v, %v), want retained sandbox", stage, running, err)
				}
				t.Logf("%s: %v, installer calls = 0, sandbox still running", stage, state)
			}
			assertPending("create failed")
			a.Sandboxes.Tick(ctx, false)
			waitClosed(t, stopStarted, "first runtime Delete")
			assertPending("stop retry in flight")

			a.Sandboxes.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: sandboxID, Revision: entry.GetRevision()})
			if got := a.Sandboxes.StateEntry(sandboxID); got != nil {
				t.Fatalf("terminal acknowledgement kept the sandbox record: %v", got)
			}
			assertPending("terminal record acknowledged")
			resumeStop()
			waitClosed(t, log.failed, "failed retry to finish")
			assertPending("stop retry failed")

			a.Sandboxes.Tick(ctx, false)
			waitClosed(t, log.complete, "successful retry to finish")
			if running, err := rt.Running(ctx); err != nil || len(running) != 0 || rt.calls() != 2 {
				t.Fatalf("after cleanup: runtime inventory = (%v, %v), Delete calls = %d, want none running after two attempts", running, err, rt.calls())
			}
			if installed, err := a.updater.TryInstall(); !installed || err != nil {
				t.Fatalf("install after cleanup = (%t, %v), want (true, nil)", installed, err)
			}
			if installed, err := a.updater.TryInstall(); installed || err != nil || env.installs.Load() != 1 {
				t.Fatalf("repeated check = (%t, %v), installer calls = %d, want one install total", installed, err, env.installs.Load())
			}
			waitClosed(t, a.restart, "restart request after installation")
			t.Log("Successful cleanup removed the retained sandbox. The next install check used the verified artifact once and requested a restart.")
		})
	}
}
