// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package update

import (
	"crypto/ed25519"
	"errors"
	"io/fs"
	"os"
	"sync/atomic"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestRevocationSaveFailureKeepsCreateGate(t *testing.T) {
	for _, tc := range []struct {
		name            string
		startRevoked    bool
		manifestRevoked bool
		target          string
		wantTarget      string
	}{
		{"save first revocation", false, true, "1.1.0", "1.1.0"},
		{"change recovery target", true, true, "1.2.0", "1.1.0"},
		{"clear revocation", true, false, "1.2.0", "1.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revoking := newRelease(t, nil)
			clearing := newRelease(t, func(m *Manifest, _ *[]byte) { m.RevokedVersions = nil })
			u, _, _ := newUpdater(t, revoking, "0.9.0", &atomic.Bool{}, revoking.pub, clearing.pub)
			if tc.startRevoked {
				if err := update(u, "1.1.0", true); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.manifestRevoked {
				u.opts.ManifestURL = clearing.srv.URL + "/manifest.json"
				u.opts.Keys = []ed25519.PublicKey{clearing.pub}
			}

			// Reads still work, but the store cannot create its temporary file.
			dir := u.opts.Store.Dir()
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Error(err)
				}
			})
			if f, err := os.CreateTemp(dir, ".permission-probe-*"); err == nil {
				f.Close()
				os.Remove(f.Name())
				t.Skip("test process can write to a read-only directory")
			} else if !errors.Is(err, fs.ErrPermission) {
				t.Fatal(err)
			}

			if err := update(u, tc.target, tc.manifestRevoked); !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("update did not return the store error: %v", err)
			}
			requireRevokedCreate(t, u)
			requireUpdateState(t, u, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, tc.wantTarget)
			requireUpdateEntries(t, u.opts.Store)
			if tc.startRevoked {
				after, err := New(u.opts)
				if err != nil {
					t.Fatal(err)
				}
				requireRevokedCreate(t, after)
				requireUpdateState(t, after, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, "1.1.0")
			}
		})
	}
}
