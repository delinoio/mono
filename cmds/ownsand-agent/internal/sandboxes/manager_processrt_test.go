// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package sandboxes

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
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
