// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestSyncCaseOnlyReplacement(t *testing.T) {
	needPOSIX(t)
	testSyncDirectoryAliasReplacement(t, sandrunv1.GuestOs_GUEST_OS_MACOS, "Assets", "assets", nil)
}

func TestSyncNormalizationDirectoryReplacement(t *testing.T) {
	needPOSIX(t)
	testSyncDirectoryAliasReplacement(t, sandrunv1.GuestOs_GUEST_OS_MACOS, "caf\u00e9", "cafe\u0301", nil)
}

func testSyncDirectoryAliasReplacement(t *testing.T, osys sandrunv1.GuestOs, oldDir, incoming string, prepareWorkspace func(*testing.T, string)) {
	t.Helper()
	old := oldDir + "/nested/old.txt"
	guestOnly := oldDir + "/nested/guest.txt"
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
			ds := newSession(t, c, srv, org, osys)
			ws := ds.GetWorkspacePath()
			if prepareWorkspace != nil {
				prepareWorkspace(t, ws)
			}
			dir := newRepo(t, map[string]string{old: "old"}, nil, "")
			// Preserve the original spellings when Git lists the staged paths on macOS.
			git(t, dir, "config", "core.precomposeUnicode", "false")
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if _, err := c.Sync(ctx, params); err != nil {
				t.Fatal(err)
			}
			if readGuest(t, ds, old) != "old" || len(guestManifest(t, ds).Files) != 1 {
				t.Fatal("first sync did not install the tracked file")
			}
			info, err := os.Stat(filepath.Join(ws, incoming))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			aliases := err == nil && info.IsDir()
			t.Logf("guest aliases %q and %q: %t", oldDir, incoming, aliases)
			before, err := os.ReadFile(ws + manifestSuffix)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.guestFile {
				if err := os.WriteFile(filepath.Join(ws, guestOnly), []byte("guest"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			git(t, dir, "rm", "-q", "-r", oldDir)
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
			if !aliases || scenario.guestFile {
				if _, err := os.Stat(filepath.Join(ws, old)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("old tracked file was not removed: %v", err)
				}
			}
			if !aliases {
				if _, err := os.ReadDir(filepath.Join(ws, oldDir, "nested")); err != nil {
					t.Fatalf("distinct guest directory was not preserved: %v", err)
				}
			}
			requests := srv.ExecRequests()
			apply, cleanup := requests[len(requests)-2], requests[len(requests)-1]
			aliasList := apply.GetEnv()[guestEnvAliasPrunes]
			if oldDir == incoming {
				if aliasList != "" || apply.GetEnv()[guestEnvPrunes] == "" {
					t.Fatal("exact-spelling replacement should use the ordinary prune list")
				}
			} else if aliasList == "" || cleanup.GetEnv()[guestEnvAliasPrunes] != aliasList {
				t.Fatal("apply and cleanup did not receive the same alias prune list")
			}
			if _, err := os.Stat(ws + lockSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sync lock was not removed: %v", err)
			}
			if matches, err := filepath.Glob(ws + ".sandrun-sync-*"); err != nil || len(matches) != 0 {
				t.Fatalf("temporary files after sync: %v, glob error: %v", matches, err)
			}
		})
	}
}

func TestSplitSyncDeletionsCaseOnlyReplacement(t *testing.T) {
	deleted := []string{"Assets", "Assets/Subdir", "Exact", "Same-File", "unrelated"}
	changed := []localFile{{rel: "assets/subdir/new"}, {rel: "Exact/new"}, {rel: "same-file"}}
	exact := []string{"Exact"}
	for _, matchAliases := range []bool{false, true} {
		wantStrict := deleted[:2]
		var wantAliases []string
		if matchAliases {
			wantStrict = nil
			wantAliases = []string{"Assets", "assets", "Assets/Subdir", "assets/subdir"}
		}
		wantStrict = append(slices.Clone(wantStrict), "Same-File", "unrelated")
		strict, replaced, aliases := splitSyncDeletions(deleted, changed, matchAliases)
		if !slices.Equal(strict, wantStrict) || !slices.Equal(replaced, exact) || !slices.Equal(aliases, wantAliases) {
			t.Fatalf("splitSyncDeletions(matchAliases=%t) = %q, %q, %q; want %q, %q, %q", matchAliases, strict, replaced, aliases, wantStrict, exact, wantAliases)
		}
	}
}

func TestSyncCaseOnlyReplacementApplyAliasRecords(t *testing.T) {
	needPOSIX(t)
	root := t.TempDir()
	ws := filepath.Join(root, "work")
	oldDir := "-Assets\n'"
	oldFile := "-File\n'"
	if err := os.MkdirAll(filepath.Join(ws, oldDir), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(ws, oldDir, "guest.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, oldFile), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A spelling with ./ resolves to the same entry on case-sensitive volumes too.
	list := filepath.Join(root, "replace-alias")
	if err := os.WriteFile(list, pathList([]string{oldDir, "./" + oldDir, oldFile, "./" + oldFile}, false), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := guestCommand(sandrunv1.GuestOs_GUEST_OS_MACOS, opApply)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), guestEnvWorkspace + "=" + ws, guestEnvAliasReplace + "=" + list}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply alias replacements: %v: %s", err, out)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("guest-only file = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, oldFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("former file was not removed, stat returned %v", err)
	}
}
