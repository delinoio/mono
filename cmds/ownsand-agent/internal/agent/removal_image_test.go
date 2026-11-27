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

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

func TestRemovalWaitsForImageBuild(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		name := "wait"
		if interrupt {
			name = "interrupted-then-retried"
		}
		t.Run(name, func(t *testing.T) {
			cp := testcp.New(t)
			st, id := enrollDevice(t, cp)
			buildUnlock, err := st.LockImageBuild(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer buildUnlock()
			imageDir := st.Path(store.ImagesDir, images.MacOSDir)
			imageLock, err := images.TryLock(imageDir)
			if err != nil {
				t.Fatal(err)
			}
			releaseImage := sync.OnceFunc(func() {
				if err := imageLock.Release(); err != nil {
					t.Error(err)
				}
			})
			defer releaseImage()
			lockPath := images.LockPath(imageDir)
			before, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			confirm := confirmRemovalGate{entered: make(chan struct{}), resume: make(chan struct{})}
			a := newAgent(t, cp, st, id, func(o *Options) {
				confirm.AgentServiceClient = o.Control.Agent
				o.Control.Agent = &confirm
			})
			t.Cleanup(a.shutdown)
			cp.MarkRemoved(id.DeviceID)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			removed := make(chan error, 1)
			go func() { removed <- a.Run(ctx) }()
			checkWaiting := func() {
				t.Helper()
				state := waitForRemovalNotice(t, st)
				if state.Removal.CleanupDone || cp.Confirms() != 0 {
					t.Fatalf("removal advanced while the image build was held: %+v, confirmations %d", state.Removal, cp.Confirms())
				}
				if after, err := os.Stat(lockPath); err != nil || !os.SameFile(before, after) {
					t.Fatalf("active image lock was unlinked or replaced: %v", err)
				}
				if lock, err := images.TryLock(imageDir); !errors.Is(err, images.ErrLocked) {
					if lock != nil {
						lock.Release()
					}
					t.Fatalf("second destination lock: %v, want ErrLocked", err)
				}
				if unlock, err := st.LockImageBuild(t.Context()); !errors.Is(err, store.ErrRemovalPending) {
					if unlock != nil {
						unlock()
					}
					t.Fatalf("second builder admission: %v, want ErrRemovalPending", err)
				}
				select {
				case <-confirm.entered:
					t.Fatal("removal tried to confirm while the build was held")
				default:
				}
			}
			checkWaiting()
			if interrupt {
				cancel()
				select {
				case err := <-removed:
					if !errors.Is(err, context.Canceled) || !errors.Is(err, store.ErrEnrollmentBusy) {
						t.Fatalf("interrupted removal: %v, want a canceled lock wait", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("removal did not stop waiting after cancellation")
				}
				checkWaiting()
				if _, err := st.LoadIdentity(); err != nil {
					t.Fatalf("interrupted removal lost the identity: %v", err)
				}
			}

			// The admitted builder can reach staging even after the notice.
			// Its writes still precede the wipe, including on a removal retry.
			stage := images.BuildDir(imageDir)
			if err := os.Mkdir(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, "disk.img"), []byte("build data"), 0o600); err != nil {
				t.Fatal(err)
			}
			releaseImage()
			buildUnlock()
			if interrupt {
				retry := newAgent(t, cp, st, id, func(o *Options) {
					o.Control.Agent = &confirm
				})
				retryCtx, stop := context.WithTimeout(t.Context(), 15*time.Second)
				defer stop()
				go func() { removed <- retry.Run(retryCtx) }()
			}
			waitClosed(t, confirm.entered, "removal confirmation")
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("image stage at confirmation: %v, want absent", err)
			}
			if unlock, err := st.LockImageBuild(t.Context()); !errors.Is(err, store.ErrEnrollmentBusy) {
				if unlock != nil {
					unlock()
				}
				t.Fatalf("builder admission during confirmation: %v, want ErrEnrollmentBusy", err)
			}
			close(confirm.resume)
			select {
			case err := <-removed:
				if !errors.Is(err, ErrRemoved) {
					t.Fatalf("completed removal: %v, want ErrRemoved", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("removal did not finish after the build returned")
			}
			if got := cp.Confirms(); got != 1 {
				t.Fatalf("removal confirmations = %d, want 1", got)
			}
			requireEmptyStateDir(t, st)
		})
	}
}
