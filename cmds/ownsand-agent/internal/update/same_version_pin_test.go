// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestSameVersionPin(t *testing.T) {
	for _, tc := range []struct {
		name        string
		revoked     bool
		downloading bool
	}{
		{name: "queued recovery", revoked: true},
		{name: "downloading recovery", revoked: true, downloading: true},
		{name: "unrevoked running version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, func(m *Manifest, _ *[]byte) {
				if tc.revoked {
					m.RevokedVersions = append(m.RevokedVersions, "1.0.0")
				}
			})
			busy := &atomic.Bool{}
			busy.Store(true)
			u, inst, restarts := newUpdater(t, r, "1.0.0", busy)
			ctx := context.Background()
			pin := &ownsandv1.UpdateAgentCommand{TargetVersion: "v1.0.0"}
			staleOrder := u.NextOrder()

			var finishDownload func()
			if tc.downloading {
				gate := newGateTransport(t, r.srv.Client().Transport)
				u.opts.HTTP = &http.Client{Transport: gate}
				done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, u.NextOrder())
				gate.awaitStartedOrEnd(t, done)
				finishDownload = func() {
					gate.open()
					if err := awaitDone(t, done); err != nil {
						t.Fatalf("finish recovery download: %v", err)
					}
				}
			} else if err := update(u, "1.1.0", false); err != nil {
				t.Fatalf("queue update: %v", err)
			}

			if err := u.HandleOrdered(ctx, pin, staleOrder); !errors.Is(err, ErrSuperseded) {
				t.Fatalf("stale pin: %v, want %v", err, ErrSuperseded)
			}
			var wantPinErr error
			wantState, wantTarget := ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, ""
			if tc.revoked {
				wantPinErr = ErrRunningRevoked
				wantState, wantTarget = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, "1.1.0"
			}
			if err := u.Handle(ctx, pin); !errors.Is(err, wantPinErr) {
				t.Fatalf("same-version pin: %v, want %v", err, wantPinErr)
			}
			if state, target := u.State(); state != wantState || target != wantTarget {
				t.Fatalf("after pin: state %v, target %q, want %v, %q", state, target, wantState, wantTarget)
			}
			if tc.revoked {
				if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
					t.Fatalf("create after pin: %v, want %v", err, ErrRunningRevoked)
				}
			}
			if finishDownload != nil {
				finishDownload()
				if state, target := u.State(); state != wantState || target != wantTarget {
					t.Fatalf("after download: state %v, target %q, want %v, %q", state, target, wantState, wantTarget)
				}
			}

			busy.Store(false)
			if installed, err := u.TryInstall(); err != nil || installed != tc.revoked {
				t.Fatalf("idle installation: %v, %v, want %v, nil", installed, err, tc.revoked)
			}
			if installed, err := u.TryInstall(); err != nil || installed {
				t.Fatalf("second installation check: %v, %v", installed, err)
			}
			wantInstalls := int32(0)
			if tc.revoked {
				wantInstalls = 1
				if err := u.HandleOrdered(ctx, pin, staleOrder); !errors.Is(err, ErrInstalling) {
					t.Fatalf("pin after installation started: %v, want %v", err, ErrInstalling)
				}
			}
			if installed, restarted := inst.installed.Load(), restarts.Load(); installed != wantInstalls || restarted != wantInstalls {
				t.Fatalf("installs %d, restarts %d, want %d each", installed, restarted, wantInstalls)
			}
			u.FenceForRemoval()
			if err := u.HandleOrdered(ctx, pin, staleOrder); !errors.Is(err, ErrRemoving) {
				t.Fatalf("pin during removal: %v, want %v", err, ErrRemoving)
			}
		})
	}
}
