// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
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
			signature := newGateTransport(t, gate)
			signature.path = "/manifest.json.sig"
			u.opts.HTTP = &http.Client{Transport: signature}

			done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0", RevocationRecovery: tc.running == "0.9.0"}, u.NextOrder())
			signature.awaitStartedOrEnd(t, done)
			requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING, "1.1.0")
			signature.open()
			gate.awaitStartedOrEnd(t, done)
			requireUpdateState(t, u, tc.want, "1.1.0")
			if tc.running == "0.9.0" {
				requireRevokedCreate(t, u)
			}
			gate.open()
			if err := awaitDone(t, done); err != nil {
				t.Fatalf("download finished with %v", err)
			}
		})
	}
}

func TestRetryKeepsVerifiedRevocation(t *testing.T) {
	for _, tc := range []struct{ path, failure string }{
		{"/manifest.json", "update: download manifest: update: download returned HTTP 503"},
		{"/manifest.json.sig", "update: download manifest signature: update: download returned HTTP 503"},
	} {
		t.Run(tc.path[1:], func(t *testing.T) {
			r := newRelease(t, nil)
			busy := &atomic.Bool{}
			busy.Store(true)
			u, _, _ := newUpdater(t, r, "0.9.0", busy)
			base := r.srv.Client().Transport
			u.opts.HTTP = &http.Client{Transport: &statusTransport{base: base, path: "/agent", status: http.StatusNotFound}}
			if err := update(u, "1.1.0", true); err == nil || err.Error() != "update: download artifact: update: download returned HTTP 404" {
				t.Fatalf("initial update: %v", err)
			}
			revoked := ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
			requireUpdateState(t, u, revoked, "1.1.0")

			failure := &statusTransport{base: base, path: tc.path, status: http.StatusServiceUnavailable}
			gate := newGateTransport(t, failure)
			gate.path = tc.path
			u.opts.HTTP = &http.Client{Transport: gate}
			done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.2.0", RevocationRecovery: true}, u.NextOrder())
			gate.awaitStartedOrEnd(t, done)
			requireUpdateState(t, u, revoked, "1.2.0")
			requireRevokedCreate(t, u)
			gate.open()
			err := awaitDone(t, done)
			if err == nil || err.Error() != tc.failure {
				t.Fatalf("retry lost the original HTTP error: %v", err)
			}
			requireUpdateState(t, u, revoked, "1.2.0")
			requireRevokedCreate(t, u)
		})
	}
}

func TestVerifiedRetryClearsRevocationAfterSupersedingOlderManifest(t *testing.T) {
	old := newRelease(t, nil)
	current := newRelease(t, func(m *Manifest, _ *[]byte) { m.RevokedVersions = nil })
	var latest atomic.Pointer[release]
	latest.Store(old)
	signatureStarted := make(chan struct{})
	releaseSignature := make(chan struct{})
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r := latest.Load()
		if r == old && req.URL.Path == "/manifest.json.sig" {
			close(signatureStarted)
			select {
			case <-releaseSignature:
			case <-req.Context().Done():
				return
			}
		}
		http.Redirect(w, req, r.srv.URL+req.URL.Path, http.StatusFound)
	}))
	t.Cleanup(manifestServer.Close)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, _, _ := newUpdater(t, old, "0.9.0", busy, old.pub, current.pub)
	if err := update(u, "1.1.0", true); err != nil {
		t.Fatal(err)
	}
	u.opts.ManifestURL = manifestServer.URL + "/manifest.json"
	artifact := newGateTransport(t, manifestServer.Client().Transport)
	u.opts.HTTP = &http.Client{Transport: artifact}

	stale, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.2.0", RevocationRecovery: true}, u.NextOrder())
	if err := waitStarted(signatureStarted, stale, gateTimeout); err != nil {
		t.Fatal(err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, "1.2.0")
	latest.Store(current)
	retry, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, u.NextOrder())
	artifact.awaitStartedOrEnd(t, retry)
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING, "1.1.0")
	release, err := u.BeginCreate()
	if err != nil {
		t.Fatalf("create after revocation cleared: %v", err)
	}
	release()
	close(releaseSignature)
	if err := awaitDone(t, stale); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("superseded update: %v", err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING, "1.1.0")
	artifact.open()
	if err := awaitDone(t, retry); err != nil {
		t.Fatal(err)
	}
	requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE, "1.1.0")
	release, err = u.BeginCreate()
	if err != nil {
		t.Fatalf("create after the older manifest completed: %v", err)
	}
	release()
}

func requireUpdateState(t *testing.T, u *Updater, want ownsandv1.AgentUpdateState, wantTarget string) {
	t.Helper()
	if state, target := u.State(); state != want || target != wantTarget {
		t.Fatalf("update state %v %q, want %v %q", state, target, want, wantTarget)
	}
}

func requireRevokedCreate(t *testing.T, u *Updater) {
	t.Helper()
	if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("create on a revoked version: %v", err)
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
