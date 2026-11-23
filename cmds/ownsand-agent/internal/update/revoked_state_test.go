// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestStateWhileDownloadFollowsRunningRevocation checks the state that the
// heartbeat reports while the artifact download is pending: a verified
// manifest that revokes the running version reports REVOKED_PENDING_IDLE with
// the target, and a running version that is not revoked keeps DOWNLOADING.
func TestStateWhileDownloadFollowsRunningRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, running string
		want          ownsandv1.AgentUpdateState
	}{
		{"revoked running version", "0.9.0", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE},
		{"running version not revoked", "1.0.0", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, nil)
			busy := &atomic.Bool{}
			busy.Store(true)
			u, _, _ := newUpdater(t, r, tc.running, busy)
			gate := newGateTransport(t, r.srv.Client().Transport)
			u.opts.HTTP = &http.Client{Transport: gate}

			order := u.NextOrder()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				done <- u.HandleOrdered(ctx, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0", RevocationRecovery: tc.running == "0.9.0"}, order)
			}()
			gate.awaitStarted(t)
			if s, target := u.State(); s != tc.want || target != "1.1.0" {
				t.Fatalf("state during the download %v %q, want %v", s, target, tc.want)
			}
			gate.open()
			if err := awaitDone(t, done); err != nil {
				t.Fatalf("download finished with %v", err)
			}
		})
	}
}

// TestInvalidSignatureCannotSetRevocation checks that a manifest which does
// not verify neither marks the running version revoked nor changes the state
// to REVOKED_PENDING_IDLE, even if its unsigned content lists the running
// version as revoked.
func TestInvalidSignatureCannotSetRevocation(t *testing.T) {
	r := newRelease(t, nil)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The updater trusts only a key that did not sign the manifest.
	u, _, _ := newUpdater(t, r, "0.9.0", &atomic.Bool{}, otherPub)
	if err := update(u, "1.1.0", true); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("update with an unverifiable manifest: %v", err)
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
		t.Fatalf("state %v", s)
	}
	release, err := u.BeginCreate()
	if err != nil {
		t.Fatalf("create after an unverifiable manifest: %v", err)
	}
	release()
}

// TestFailedInstallReportsRevokedStateWithTarget checks that a failed
// installation while the running version is revoked reports
// REVOKED_PENDING_IDLE with the failed target, returns the install error and
// removes the artifact, and that the unrevoked twin still reports FAILED.
func TestFailedInstallReportsRevokedStateWithTarget(t *testing.T) {
	for _, tc := range []struct {
		name, running string
		want          ownsandv1.AgentUpdateState
	}{
		{"revoked running version", "0.9.0", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE},
		{"running version not revoked", "1.0.0", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, nil)
			busy := &atomic.Bool{}
			busy.Store(true)
			u, inst, restarts := newUpdater(t, r, tc.running, busy)
			installErr := errors.New("read-only executable directory")
			inst.err = installErr
			if err := update(u, "1.1.0", tc.running == "0.9.0"); err != nil {
				t.Fatal(err)
			}
			busy.Store(false)
			ok, err := u.TryInstall()
			if ok || !errors.Is(err, installErr) {
				t.Fatalf("install should return the install error: %v", ok)
			}
			if restarts.Load() != 0 {
				t.Fatal("restarted after a failed install")
			}
			if s, target := u.State(); s != tc.want || target != "1.1.0" {
				t.Fatalf("state %v %q, want %v with the failed target", s, target, tc.want)
			}
			names, _ := filepath.Glob(filepath.Join(u.opts.Store.Path(store.UpdateDir), "*"))
			if len(names) != 0 {
				t.Fatalf("the failed installation left %v", names)
			}
		})
	}
}
