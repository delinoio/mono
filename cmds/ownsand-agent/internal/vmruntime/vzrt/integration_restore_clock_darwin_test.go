// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func (e *env) checkRestoreClock(t *testing.T, snapshotID string, guestOS ownsandv1.GuestOs) {
	t.Helper()
	snapshotDir, snapshot, ok := e.m.SnapshotContent(snapshotID)
	if !ok {
		t.Fatal("snapshot content missing")
	}
	if !snapshot.GetHasMemoryState() {
		t.Skip("snapshot has no memory state")
	}

	clockFailure := errors.New("injected clock RPC failure")
	stopFailure := errors.New("injected unconfirmed stop")
	for _, tc := range []struct {
		name        string
		cancel      bool
		wait        bool
		clockErr    error
		stopErr     error
		wantWarning bool
	}{
		{name: "parent-canceled", cancel: true, wait: true},
		{name: "parent-canceled-as-clock-succeeds", cancel: true},
		{name: "unconfirmed-stop-retry", cancel: true, wait: true, stopErr: stopFailure},
		{name: "clock-error-only", clockErr: clockFailure, wantWarning: true},
		{name: "clock-deadline-only", wait: true, wantWarning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fallback, clockWarning atomic.Bool
			logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
				ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
					if attr.Key == slog.MessageKey {
						msg := attr.Value.String()
						if strings.Contains(msg, "booting the snapshot disk cold") {
							fallback.Store(true)
						}
						if msg == "set guest clock after memory restore failed" {
							clockWarning.Store(true)
						}
					}
					return attr
				},
			}))
			vmsDir := t.TempDir()
			rt, err := vzrt.New(vzrt.Options{Kernel: os.Getenv(envKernel), VMsDir: vmsDir, Logger: logger})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := rt.Close(); err != nil {
					t.Error(err)
				}
			})
			id, err := uuidv7.NewString()
			if err != nil {
				t.Fatal(err)
			}
			resources := snapshot.GetResources()
			spec := vmruntime.Spec{
				SandboxID: id, Dir: filepath.Join(vmsDir, id), Resources: resources, GuestOS: guestOS,
				NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE, MemoryRestoreAllowed: true,
				Snapshot: &vmruntime.SnapshotSource{SnapshotID: snapshotID, Dir: snapshotDir, HasMemoryState: true, Resources: resources},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			got, err := rt.(*vzrt.Runtime).CreateWithRestoreClockForTest(ctx, spec, func(clockCtx context.Context, _ time.Time) error {
				calls++
				if tc.cancel {
					cancel()
				}
				if tc.wait {
					<-clockCtx.Done()
					return clockCtx.Err()
				}
				return tc.clockErr
			}, tc.stopErr)
			if calls != 1 || fallback.Load() {
				t.Fatalf("clock calls = %d, disk fallback = %t, create error = %v", calls, fallback.Load(), err)
			}
			if warned := clockWarning.Load(); warned != tc.wantWarning {
				t.Fatalf("clock warning = %t, want %t", warned, tc.wantWarning)
			}

			if !tc.cancel {
				if err != nil || got == nil || got.RestorePath() != ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_MEMORY_STATE {
					t.Fatalf("create = %v, %v, want MEMORY_STATE", got, err)
				}
				return
			}
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("create = %v, %v, want nil VM and parent cancellation", got, err)
			}
			if tc.stopErr != nil {
				if !errors.Is(err, tc.stopErr) || !errors.Is(err, vzrt.ErrVMMayRun) {
					t.Fatalf("create error = %v, want unconfirmed stop", err)
				}
				if ids, err := rt.Running(t.Context()); err != nil || !slices.Equal(ids, []string{id}) {
					t.Fatalf("retained VM IDs = %v, error = %v", ids, err)
				}
				if _, err := os.Stat(spec.Dir); err != nil {
					t.Fatalf("stat retained files: %v", err)
				}
				if err := rt.Delete(t.Context(), id); err != nil {
					t.Fatalf("retry cleanup: %v", err)
				}
			}
			if ids, err := rt.Running(t.Context()); err != nil || len(ids) != 0 {
				t.Fatalf("VM IDs after confirmed stop = %v, error = %v", ids, err)
			}
			if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat after confirmed stop = %v, want no files", err)
			}
		})
	}
}
