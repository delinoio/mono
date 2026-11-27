// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package sandboxes

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestRestartRemovesProcessRuntimeFilesOfLostSandboxes checks that a Manager
// that starts with the process runtime removes the sandbox directory that an
// earlier agent process left behind, before it commits the terminal state.
// The runtime has no tracked sandbox after a restart, so it must find the
// directory through the sandbox directories.
func TestRestartRemovesProcessRuntimeFilesOfLostSandboxes(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	guest := filepath.Join(t.TempDir(), "ownsand-guest")
	if err := os.WriteFile(guest, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := processrt.NewWithOptions(processrt.Options{GuestBinary: guest, VMsDir: e.st.Path(store.VMsDir), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.st.Path(store.VMsDir, id)
	for _, sub := range []string{"root", "guest-state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "root", "user-file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The agent restarts with the process runtime, which tracks no sandbox.
	e.m.Close()
	m, err := New(context.Background(), Options{Store: e.st, Runtime: rt, Now: e.clk.Now, FreeDisk: func() (int64, error) { return 1 << 50, nil }})
	if err != nil {
		t.Fatal(err)
	}
	m.SetOutbox(e.out)
	m.SetExecTracker(e.ex)
	e.m = m
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("sandbox directory after the restart: %v; want it removed", err)
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || endReason(e.m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("lost sandbox %v %v", s, endReason(e.m, id))
	}
}

// restartCleanupRuntime injects a transient Delete failure into the real
// process runtime, which owns the actual VM files.
type restartCleanupRuntime struct {
	vmruntime.Runtime
	failDeletes int
	deleteCalls int
}

func (r *restartCleanupRuntime) Delete(ctx context.Context, id string) error {
	r.deleteCalls++
	if r.deleteCalls <= r.failDeletes {
		return errors.New("injected Delete failure")
	}
	return r.Runtime.Delete(ctx, id)
}

func TestRestartRemovesFailedCreateFilesWithoutRuntimeInventory(t *testing.T) {
	for _, tc := range []struct {
		name              string
		acknowledged      bool
		retryByRestart    bool
		failStartupDelete bool
	}{
		{name: "terminal-retained"},
		{name: "terminal-acknowledged", acknowledged: true},
		{name: "retry-by-tick", acknowledged: true, failStartupDelete: true},
		{name: "retry-by-restart", acknowledged: true, failStartupDelete: true, retryByRestart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := retainedFailedCreate(t, e, tc.acknowledged)
			dir := e.st.Path(store.VMsDir, id)
			unrelated := e.st.Path(store.VMsDir, "unrelated")
			for _, path := range []string{dir, unrelated} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("keep unless targeted"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			guest := filepath.Join(t.TempDir(), "ownsand-guest")
			if err := os.WriteFile(guest, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			native, err := processrt.NewWithOptions(processrt.Options{GuestBinary: guest, VMsDir: e.st.Path(store.VMsDir), Logger: slog.Default()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = native.Close() })
			rt := &restartCleanupRuntime{Runtime: native}
			if tc.failStartupDelete {
				rt.failDeletes = 1
			}
			restart := func() *Manager {
				t.Helper()
				m, err := New(t.Context(), Options{Store: e.st, Runtime: rt, Now: e.clk.Now})
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			e.m.Close()
			m := restart()
			defer func() { m.Close() }()
			if tc.failStartupDelete {
				if _, err := os.Stat(filepath.Join(dir, "sentinel")); err != nil {
					t.Fatalf("failed Delete lost the sentinel: %v", err)
				}
				ids, err := e.st.LoadSandboxCleanups()
				if err != nil || len(ids) != 1 || ids[0] != id || rt.deleteCalls != 1 {
					t.Fatalf("after first attempt, markers = %v, error = %v, calls = %d", ids, err, rt.deleteCalls)
				}
				if tc.retryByRestart {
					m.Close()
					m = restart()
				} else {
					m.Tick(t.Context(), false)
					m.bg.Wait()
				}
			}
			m.Tick(t.Context(), false)
			m.bg.Wait()
			if want := 1 + rt.failDeletes; rt.deleteCalls != want {
				t.Fatalf("Delete calls = %d, want %d", rt.deleteCalls, want)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("sandbox directory after cleanup = %v, want absent", err)
			}
			if _, err := os.Stat(filepath.Join(unrelated, "sentinel")); err != nil {
				t.Fatalf("unrelated sandbox file removed: %v", err)
			}
			if ids, err := e.st.LoadSandboxCleanups(); err != nil || len(ids) != 0 {
				t.Fatalf("markers after cleanup = %v, %v", ids, err)
			}
			state, found := m.State(id)
			if tc.acknowledged {
				if found {
					t.Fatal("the acknowledged report reappeared")
				}
			} else if state != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || endReason(m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
				t.Fatalf("terminal report after cleanup = %v/%v", state, endReason(m, id))
			}
		})
	}
}
