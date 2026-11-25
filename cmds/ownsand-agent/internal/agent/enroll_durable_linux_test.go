// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

func TestEnrollStopsBeforeCallWhenPendingKeyIsNotDurable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a directory without the read permission")
	}
	for _, tc := range []struct {
		name   string
		failed string
		reopen bool
	}{
		{name: "state directory", failed: "parent/child/state"},
		{name: "previously created ancestor", failed: "parent", reopen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := testcp.New(t)
			cp.ConsumeEnrollToken()
			client, err := cpclient.New(cp.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			dir := filepath.Join(root, "parent", "child", "state")
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			blocked := filepath.Join(root, tc.failed)
			// Write and search access allow the identity write. Flushing
			// the directory entries also requires read access.
			if err := os.Chmod(blocked, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(blocked, 0o700) })
			enroll := func() (*store.Identity, error) {
				return Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
			}
			requireSyncFailure := func() {
				t.Helper()
				_, err := enroll()
				var pathErr *fs.PathError
				if !errors.As(err, &pathErr) || !errors.Is(err, fs.ErrPermission) || pathErr.Op != "open" || pathErr.Path != blocked {
					t.Errorf("Enroll error = %v, want permission failure opening %s", err, blocked)
				}
				if calls := len(cp.EnrollPublicKeys()); calls != 0 {
					t.Fatalf("%d Enroll calls reached the control plane while %s could not be flushed", calls, blocked)
				}
			}
			requireSyncFailure()
			pendingKey := requirePendingKey(t, st)
			if tc.reopen {
				// A later enrollment process has no record of which
				// directories the first Store created.
				st, err = store.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			requireSyncFailure()
			if err := os.Chmod(blocked, 0o700); err != nil {
				t.Fatal(err)
			}
			id, err := enroll()
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if id.DeviceID == "" {
				t.Fatal("retry returned no device ID")
			}
			if keys := cp.EnrollPublicKeys(); len(keys) != 1 || !bytes.Equal(keys[0], pendingKey) {
				t.Fatal("want one Enroll call with the original pending public key")
			}
		})
	}
}

// TestEnrollKeyIsOnDiskWhenCallArrives checks that the pending key is
// stored when the Enroll call reaches the control plane, and that a retry
// after a lost response sends the same key (the token is single-use for its
// first key).
func TestEnrollKeyIsOnDiskWhenCallArrives(t *testing.T) {
	cp := testcp.New(t)
	cp.ConsumeEnrollToken()
	cp.DropNextEnrollResponse()
	hold := cp.HoldNextEnroll()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := enrollAsync(t, context.Background(), st, client)
	if err := waitForEnrollArrival(hold, first); err != nil {
		t.Fatal(err)
	}
	onDisk := requirePendingKey(t, st)
	hold.Release()
	if r := <-first; r.err == nil {
		t.Fatal("enroll succeeded although the response was dropped")
	}
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	keys := cp.EnrollPublicKeys()
	if len(keys) != 2 || string(keys[0]) != string(onDisk) || string(keys[1]) != string(onDisk) {
		t.Fatalf("Enroll calls used keys %x, want two calls with the stored key %x", keys, onDisk)
	}
}
