// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/sys/windows"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func holdWithoutDeleteSharing(t *testing.T, path string) *os.File {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("hold %s open: %v", path, err)
	}
	file := os.NewFile(uintptr(handle), path)
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func requireRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s should be removed, stat returned %v", path, err)
	}
}

func TestWindowsGuestSyncRetryFileToDirectory(t *testing.T) {
	testSyncRetryFileToDirectory(t, sandrunv1.GuestOs_GUEST_OS_WINDOWS, "top/-a with [space]", "top/-a with [space]", createWindowsJunction)
}

func createWindowsJunction(t *testing.T, link, target string) {
	t.Helper()
	// A junction is a directory reparse point and needs no symlink privilege.
	const script = `$ErrorActionPreference = 'Stop'
$parent = [System.IO.Path]::GetDirectoryName($env:SANDRUN_TEST_LINK)
$name = [System.IO.Path]::GetFileName($env:SANDRUN_TEST_LINK)
New-Item -ItemType Junction -Path $parent -Name $name -Value $env:SANDRUN_TEST_TARGET | Out-Null`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(script))
	cmd.Env = append(os.Environ(), "SANDRUN_TEST_LINK="+link, "SANDRUN_TEST_TARGET="+target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, out)
	}
}

func TestWindowsGuestSyncDirectoryAliasReplacement(t *testing.T) {
	const osys = sandrunv1.GuestOs_GUEST_OS_WINDOWS
	t.Run("default", func(t *testing.T) {
		testSyncDirectoryAliasReplacement(t, osys, "Assets", "assets", nil)
	})
	t.Run("exact", func(t *testing.T) {
		testSyncDirectoryAliasReplacement(t, osys, "Assets", "Assets", nil)
	})
	t.Run("case-sensitive", func(t *testing.T) {
		testSyncDirectoryAliasReplacement(t, osys, "Assets", "assets", func(t *testing.T, ws string) {
			if out, err := exec.Command("fsutil.exe", "file", "setCaseSensitiveInfo", ws, "enable").CombinedOutput(); err != nil {
				t.Skipf("enable case-sensitive NTFS lookup: %v: %s", err, out)
			}
			probe := filepath.Join(ws, "Probe")
			if err := os.Mkdir(probe, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(ws, "probe")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace does not distinguish case: %v", err)
			}
			if err := os.Remove(probe); err != nil {
				t.Fatal(err)
			}
		})
	})
}

func TestWindowsGuestSyncListPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"single deletion", []string{"\ufeffnote.txt"}},
		{"U+FEFF first and later", []string{"\ufefffirst.txt", "\ufefflater.txt"}},
		{"ordinary first", []string{"plain.txt", "\ufefflater.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_WINDOWS)
			ws := ds.GetWorkspacePath()
			dir := newRepo(t, nil, nil, "")
			// Seed an already synced workspace. A Windows tar.exe that cannot
			// decode a Unicode PAX path would otherwise block the deletion test.
			before := manifest{Version: manifestVersion, PathsValidated: true, Files: map[string]manifestEntry{}}
			for _, path := range tc.paths {
				writeWindowsFixtureFile(t, filepath.Join(ws, path))
				before.Files[path] = manifestEntry{Mode: modeFile}
			}
			writeWindowsFixtureFile(t, filepath.Join(ws, "note.txt"))
			body, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(guestSibling(ds, manifestSuffix), body, 0o644); err != nil {
				t.Fatal(err)
			}

			p := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			res, err := c.Sync(t.Context(), p)
			if err != nil || res.Deleted != len(tc.paths) || res.Uploaded != 0 {
				t.Fatalf("deletion sync = %+v, %v", res, err)
			}
			for _, path := range tc.paths {
				requireRemoved(t, filepath.Join(ws, path))
			}
			readGuest(t, ds, "note.txt")
			if files := guestManifest(t, ds).Files; len(files) != 0 {
				t.Fatalf("manifest contains deleted paths: %+v", files)
			}
		})
	}
}

func TestWindowsGuestApplyReplacementAndPruneListPaths(t *testing.T) {
	for _, tc := range []struct{ name, first string }{
		{"U+FEFF first", "\ufefffirst"},
		{"ordinary first", "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			var replacements, prunes []string
			for _, prefix := range []string{tc.first, "\ufefflater"} {
				config, prune := prefix+"-config", prefix+"-prune"
				replacements = append(replacements, config)
				prunes = append(prunes, prune)
				writeWindowsFixtureFile(t, filepath.Join(ws, config))
				if err := os.Mkdir(filepath.Join(ws, prune), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			lists := map[string][]byte{
				guestEnvReplaceFiles: pathList(replacements, true),
				guestEnvPrunes:       pathList(prunes, true),
			}
			applyWindowsSyncLists(t, ws, lists)
			for _, path := range prunes {
				requireRemoved(t, filepath.Join(ws, path))
			}
			for _, path := range replacements {
				// Materialize the directory that an intervening extraction
				// would create before the manifest write fails.
				if err := os.Mkdir(filepath.Join(ws, path), 0o755); err != nil {
					t.Fatalf("replacement did not remove %q: %v", path, err)
				}
				writeWindowsFixtureFile(t, filepath.Join(ws, path, "guest.txt"))
			}
			applyWindowsSyncLists(t, ws, lists)
			for _, path := range replacements {
				if got, err := os.ReadFile(filepath.Join(ws, path, "guest.txt")); err != nil || string(got) != "fixture" {
					t.Errorf("file under %q after retry = %q, %v", path, got, err)
				}
			}
		})
	}
}

func TestWindowsGuestApplyLinkListPaths(t *testing.T) {
	for _, tc := range []struct{ name, first string }{
		{"U+FEFF first", "\ufefffirst"},
		{"ordinary first", "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			target := filepath.Join(ws, "target.txt")
			writeWindowsFixtureFile(t, target)
			names := []string{tc.first, "\ufefflater"}
			links := make([]localFile, 0, len(names))
			for _, name := range names {
				// Apply removes this exact file before it attempts the link.
				// The removal is observable even without symlink privilege.
				writeWindowsFixtureFile(t, filepath.Join(ws, name))
				links = append(links, localSymlink(name, target))
			}
			out := applyWindowsSyncLists(t, ws, map[string][]byte{guestEnvLinks: windowsLinkList(links)})
			failed := 0
			for _, name := range names {
				got, err := os.Readlink(filepath.Join(ws, name))
				switch {
				case errors.Is(err, os.ErrNotExist):
					failed++
				case err != nil || got != target:
					t.Errorf("guest link %q target = %q, %v, want %q", name, got, err, target)
				}
			}
			if reported := bytes.Count(out, []byte("SANDRUN-LINK-FAILED ")); reported != failed {
				t.Fatalf("found %d link failure diagnostics for %d absent paths: %s", reported, failed, out)
			}
			t.Logf("created %d symbolic links and reported %d failures after removing the original paths", len(names)-failed, failed)
		})
	}
}

func writeWindowsFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// applyWindowsSyncLists runs the production PowerShell reader without
// extracting an archive, so the result depends on the list encoding.
func applyWindowsSyncLists(t *testing.T, workspace string, lists map[string][]byte) []byte {
	t.Helper()
	args, err := guestCommand(sandrunv1.GuestOs_GUEST_OS_WINDOWS, opApply)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
	cmd.Env = append(os.Environ(), guestEnvWorkspace+"="+workspace,
		guestEnvStaleManifest+"=", guestEnvArchive+"=")
	inputs := t.TempDir()
	for _, key := range []string{guestEnvDeletes, guestEnvReplaceFiles, guestEnvPrunes, guestEnvAliasPrunes, guestEnvLinks} {
		path := ""
		if data, ok := lists[key]; ok {
			path = filepath.Join(inputs, key)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd.Env = append(cmd.Env, key+"="+path)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Windows apply failed: %v: %s", err, out)
	}
	return out
}

func TestWindowsGuestAliasPruneIdentity(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "work")
	target := filepath.Join(root, "target")
	kept := []string{
		filepath.Join(ws, "old", "nested"),
		filepath.Join(ws, "distinct"),
		filepath.Join(ws, "assets"),
		filepath.Join(target, "nested"),
	}
	for _, p := range kept {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	junction := filepath.Join(ws, "assets", "junction")
	createWindowsJunction(t, junction, target)
	same := filepath.Join(ws, "same")
	if err := os.MkdirAll(filepath.Join(same, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"old", "distinct", "old/nested",
		"old", "missing", "old/nested",
		"assets", "./assets", "assets/junction/nested",
		"assets", "./assets", "assets/junction",
		"same", "./same", "same/nested",
		"same", "./same", "same",
	}
	applyWindowsSyncLists(t, ws, map[string][]byte{guestEnvAliasPrunes: pathList(paths, true)})
	for _, p := range append(kept, junction) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("alias pruning should preserve %s: %v", p, err)
		}
	}
	requireRemoved(t, same)
}

func TestWindowsGuestCleanup(t *testing.T) {
	for _, name := range []string{guestEnvArchive, guestEnvDeletes, guestEnvReplaceFiles, guestEnvPrunes, guestEnvAliasPrunes, guestEnvLinks, guestEnvLock, guestEnvDLArchive} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, op guestOp, env map[string]string, wantCode int) {
		t.Helper()
		args, err := guestCommand(sandrunv1.GuestOs_GUEST_OS_WINDOWS, op)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantCode {
			t.Fatalf("%v should exit %d, got %v: %s", op, wantCode, err, out)
		}
	}

	for _, blocked := range []string{guestEnvArchive, guestEnvAliasPrunes, guestEnvLock, guestEnvDLArchive} {
		t.Run(blocked, func(t *testing.T) {
			root := t.TempDir()
			op := opCleanup
			env := map[string]string{}
			if blocked == guestEnvDLArchive {
				op = opRemoveArchive
				env[guestEnvDLArchive] = filepath.Join(root, "download [archive].tar")
			} else {
				for _, key := range []string{guestEnvArchive, guestEnvDeletes, guestEnvReplaceFiles, guestEnvPrunes, guestEnvAliasPrunes, guestEnvLinks} {
					env[key] = filepath.Join(root, key)
				}
				env[guestEnvLock] = filepath.Join(root, "lock")
				if err := os.Mkdir(env[guestEnvLock], 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for k, path := range env {
				if k != guestEnvLock {
					if err := os.WriteFile(path, nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			heldPath := env[blocked]
			if blocked == guestEnvLock {
				heldPath = filepath.Join(heldPath, "held")
				if err := os.WriteFile(heldPath, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			held := holdWithoutDeleteSharing(t, heldPath)
			run(t, op, env, 1)
			if _, err := os.Stat(heldPath); err != nil {
				t.Fatalf("held path should remain: %v", err)
			}
			for key, path := range env {
				if key != blocked {
					requireRemoved(t, path)
				}
			}

			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			run(t, op, env, 0)
			requireRemoved(t, env[blocked])
			run(t, op, env, 0) // Paths already absent.
			run(t, op, nil, 0) // Paths unset.
		})
	}
}

func TestWindowsGuestCleanupSyncResults(t *testing.T) {
	for _, scenario := range []string{"committed sync", "manifest write fails"} {
		t.Run(scenario, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_WINDOWS)
			dir := newRepo(t, map[string]string{"a.txt": "a"}, nil, "")
			var logs bytes.Buffer
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			lock := guestSibling(ds, lockSuffix)
			var held *os.File
			params := SyncParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir,
				Progress: func(p TransferProgress) {
					if p.Phase != TransferApplying {
						return
					}
					path := filepath.Join(lock, "held")
					if err := os.WriteFile(path, nil, 0o644); err != nil {
						t.Fatal(err)
					}
					held = holdWithoutDeleteSharing(t, path)
					if scenario == "manifest write fails" {
						srv.Fail(writeProc, connect.NewError(connect.CodePermissionDenied, errors.New("manifest write denied")))
					}
				},
			}
			ctx := context.Background()
			res, err := c.Sync(ctx, params)
			if scenario == "manifest write fails" {
				var connectErr *connect.Error
				if res != nil || !errors.As(err, &connectErr) || connectErr.Code() != connect.CodePermissionDenied || connectErr.Message() != "manifest write denied" {
					t.Fatalf("sync = %+v, %v, want manifest write error", res, err)
				}
				requireRemoved(t, guestSibling(ds, manifestSuffix))
				if !strings.Contains(logs.String(), "level=DEBUG msg=\"sandrun sync clean-up failed\"") {
					t.Errorf("missing cleanup debug message in %q", logs.String())
				}
			} else {
				const warning = "the guest could not remove the sync archive or release the sync lock; the next sync may need force"
				if err != nil || res == nil || res.Uploaded != 1 || !slices.Contains(res.Warnings, warning) {
					t.Fatalf("sync = %+v, %v, want cleanup warning", res, err)
				}
				if m := guestManifest(t, ds); len(m.Files) != 1 || m.Files["a.txt"].Size != 1 {
					t.Fatalf("committed manifest = %+v", m)
				}
			}
			if readGuest(t, ds, "a.txt") != "a" {
				t.Fatal("sync did not apply the file")
			}
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			params.Progress = nil
			if _, err := c.Sync(ctx, params); !errors.Is(err, ErrSyncLocked) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("retry with stale lock = %v, want ErrSyncLocked", err)
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			params.Force = true
			res, err = c.Sync(ctx, params)
			if err != nil || res == nil || res.Uploaded != 1 || len(res.Warnings) != 0 || readGuest(t, ds, "a.txt") != "new" {
				t.Fatalf("forced sync = %+v, %v", res, err)
			}
			requireRemoved(t, lock)
		})
	}
}

func TestWindowsGuestCleanupDownloadResult(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_WINDOWS)
	if err := os.WriteFile(filepath.Join(ds.GetWorkspacePath(), "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dest := filepath.Join(t.TempDir(), "download")
	var archive string
	res, err := c.Download(context.Background(), DownloadParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: ".", LocalPath: dest,
		Progress: func(p TransferProgress) {
			if p.Phase == TransferApplying {
				archive = srv.ExecRequests()[0].GetEnv()[guestEnvDLArchive]
				holdWithoutDeleteSharing(t, archive)
			}
		},
	})
	if err != nil || res == nil || !res.Directory || res.Files != 1 {
		t.Fatalf("download = %+v, %v", res, err)
	}
	if content, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(content) != "a" {
		t.Fatalf("downloaded file = %q, %v", content, err)
	}
	if !strings.Contains(logs.String(), "level=DEBUG msg=\"sandrun download clean-up failed\"") {
		t.Errorf("missing cleanup debug message in %q", logs.String())
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("held archive should remain: %v", err)
	}
}
