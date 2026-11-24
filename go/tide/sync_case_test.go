// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

func TestSyncCaseOnlyReplacement(t *testing.T) {
	needPOSIX(t)
	const (
		old       = "Assets/nested/old.txt"
		guestOnly = "Assets/nested/guest.txt"
		incoming  = "assets"
	)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	for _, scenario := range []struct {
		name      string
		guestFile bool
	}{
		{name: "replace"},
		{name: "guest-only-file", guestFile: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_MACOS)
			dir := newRepo(t, map[string]string{old: "old"}, nil, "")
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if _, err := c.Sync(ctx, params); err != nil {
				t.Fatal(err)
			}
			ws := ds.GetWorkspacePath()
			info, err := os.Stat(filepath.Join(ws, incoming))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			aliases := err == nil && info.IsDir()
			t.Logf("guest aliases Assets and assets: %t", aliases)
			before, err := os.ReadFile(ws + manifestSuffix)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.guestFile {
				if err := os.WriteFile(filepath.Join(ws, guestOnly), []byte("guest"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			git(t, dir, "rm", "-q", "-r", "Assets")
			if err := os.WriteFile(filepath.Join(dir, incoming), []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", incoming)

			res, err := c.Sync(ctx, params)
			if aliases && scenario.guestFile {
				if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "guest apply command") {
					t.Fatalf("sync = %+v, %v, want guest apply error", res, err)
				}
				after, err := os.ReadFile(ws + manifestSuffix)
				if err != nil || !bytes.Equal(after, before) {
					t.Fatalf("manifest changed after failed apply: %v", err)
				}
			} else {
				if err != nil || res.Uploaded != 1 || res.Deleted != 1 {
					t.Fatalf("sync = %+v, %v, want one upload and one deletion", res, err)
				}
				info, err := os.Lstat(filepath.Join(ws, incoming))
				if err != nil || !info.Mode().IsRegular() || readGuest(t, ds, incoming) != "new" {
					t.Fatalf("replacement is not a regular file with the expected contents: %v", err)
				}
				m := guestManifest(t, ds)
				if len(m.Files) != 1 || m.Files[incoming].Mode != modeFile {
					t.Fatalf("manifest after replacement = %+v", m)
				}
			}
			if scenario.guestFile && readGuest(t, ds, guestOnly) != "guest" {
				t.Fatal("guest-only file contents changed")
			}
			if !aliases {
				entries, err := os.ReadDir(filepath.Join(ws, "Assets/nested"))
				if err != nil {
					t.Fatalf("distinct guest directory was not preserved: %v", err)
				}
				if _, err := os.Stat(filepath.Join(ws, old)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("old tracked file in distinct guest directory: %v (entries: %v)", err, entries)
				}
			}
			if matches, _ := filepath.Glob(ws + ".tide-sync-*"); len(matches) != 0 {
				t.Fatalf("temporary files left: %v", matches)
			}
		})
	}
}
