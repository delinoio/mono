// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

func TestWindowsGuestSyncRelativeLinks(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_WINDOWS)
	dir := newRepo(t, map[string]string{"src/main.txt": "payload"}, nil, "")
	git(t, dir, "config", "core.symlinks", "true")
	if err := os.Mkdir(filepath.Join(dir, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := []struct{ path, target string }{
		{"links/main.txt", `..\src\main.txt`},
		{"links/src", `..\src`},
		{"links/missing", `..\missing.txt`},
		{"links/absolute", filepath.Join(ds.GetWorkspacePath(), "src", "main.txt")},
	}
	for _, link := range links {
		if err := os.Symlink(link.target, filepath.Join(dir, link.path)); errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("Windows account cannot create symbolic links")
		} else if err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "--", "links")
	git(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "links")

	params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
	res, err := c.Sync(t.Context(), params)
	if err != nil || res.Uploaded != len(links)+1 || len(res.Warnings) != 0 {
		t.Fatalf("first sync = %+v, %v", res, err)
	}
	m := guestManifest(t, ds)
	if m.Version != 1 || len(m.Files) != len(links)+1 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, link := range links {
		sourceTarget, err := os.Readlink(filepath.Join(dir, link.path))
		if err != nil {
			t.Fatal(err)
		}
		if target, err := os.Readlink(filepath.Join(ds.GetWorkspacePath(), link.path)); err != nil || target != link.target {
			t.Errorf("%s target = %q, %v, want %q", link.path, target, err, link.target)
		}
		want := manifestEntry{Size: int64(len(sourceTarget)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(sourceTarget))), Mode: modeSymlink}
		if got := m.Files[link.path]; got != want {
			t.Errorf("manifest entry for %s = %+v, want %+v", link.path, got, want)
		}
	}
	for _, path := range []string{"links/main.txt", "links/src/main.txt", "links/absolute"} {
		if got := readGuest(t, ds, path); got != "payload" {
			t.Errorf("%s contains %q", path, got)
		}
	}

	res, err = c.Sync(t.Context(), params)
	if err != nil || res.Uploaded != 0 || res.Skipped != len(links)+1 || res.BytesSent != 0 || len(res.Warnings) != 0 {
		t.Fatalf("unchanged sync = %+v, %v", res, err)
	}

	// Retarget a directory link whose old target is still populated.
	oldLink := filepath.Join(dir, "links", "src")
	if err := os.Remove(oldLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(`..\src\main.txt`, oldLink); err != nil {
		t.Fatal(err)
	}
	res, err = c.Sync(t.Context(), params)
	if err != nil || res.Uploaded != 1 || len(res.Warnings) != 0 {
		t.Fatalf("retargeted sync = %+v, %v", res, err)
	}
	for _, path := range []string{"links/src", "src/main.txt"} {
		if got := readGuest(t, ds, path); got != "payload" {
			t.Errorf("%s contains %q after retargeting", path, got)
		}
	}
}

func TestWindowsGuestLinkPermissionFailureUTF8(t *testing.T) {
	if os.Getenv("SANDRUN_TEST_WINDOWS_LINK_DENIED") != "1" {
		t.Skip("run explicitly in a code page 437 console as a user without symbolic link permission")
	}
	if cp, err := windows.GetConsoleOutputCP(); err != nil || cp != 437 {
		t.Fatalf("console output code page = %d, %v, want 437", cp, err)
	}
	ws := t.TempDir()
	target := filepath.Join(ws, "target")
	if err := os.WriteFile(target, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ws, "probe")); !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		t.Fatalf("symbolic link permission probe = %v, want ERROR_PRIVILEGE_NOT_HELD", err)
	}
	list := filepath.Join(t.TempDir(), "links")
	if err := os.WriteFile(list, []byte("dir\\café\t"+target+"\nascii\ttarget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := guestCommand(sandrunv1.GuestOs_GUEST_OS_WINDOWS, opApply)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
	cmd.Env = append(os.Environ(), guestEnvWorkspace+"="+ws, guestEnvLinks+"="+list)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	want := linkFailedPrefix + "dir\\café\r\n" + linkFailedPrefix + "ascii\r\n"
	if err != nil || string(out) != want {
		t.Fatalf("apply = %v, stdout hex %x, stderr %q, want stdout hex %x", err, out, stderr.String(), want)
	}
	t.Logf("code page 437, apply exited 0, stdout hex %x", out)
}

func TestWindowsGuestSyncRetryFileToDirectory(t *testing.T) {
	testSyncRetryFileToDirectory(t, sandrunv1.GuestOs_GUEST_OS_WINDOWS, "top/-a with [space]", func(t *testing.T, link, target string) {
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
	})
}

func TestWindowsGuestCleanup(t *testing.T) {
	for _, name := range []string{guestEnvArchive, guestEnvDeletes, guestEnvReplaceFiles, guestEnvPrunes, guestEnvLinks, guestEnvLock, guestEnvDLArchive} {
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

	for _, blocked := range []string{guestEnvArchive, guestEnvLock, guestEnvDLArchive} {
		t.Run(blocked, func(t *testing.T) {
			root := t.TempDir()
			op := opCleanup
			env := map[string]string{}
			if blocked == guestEnvDLArchive {
				op = opRemoveArchive
				env[guestEnvDLArchive] = filepath.Join(root, "download [archive].tar")
			} else {
				for _, key := range []string{guestEnvArchive, guestEnvDeletes, guestEnvReplaceFiles, guestEnvPrunes, guestEnvLinks} {
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
