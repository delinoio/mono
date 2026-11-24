// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
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

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
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

func TestWindowsGuestCleanup(t *testing.T) {
	for _, name := range []string{guestEnvArchive, guestEnvDeletes, guestEnvPrunes, guestEnvLinks, guestEnvLock, guestEnvDLArchive} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, op guestOp, env map[string]string, wantCode int) {
		t.Helper()
		args, err := guestCommand(tidev1.GuestOs_GUEST_OS_WINDOWS, op)
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
				for _, key := range []string{guestEnvArchive, guestEnvDeletes, guestEnvPrunes, guestEnvLinks} {
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
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_WINDOWS)
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
				if !strings.Contains(logs.String(), "level=DEBUG msg=\"tide sync clean-up failed\"") {
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
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_WINDOWS)
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
	if !strings.Contains(logs.String(), "level=DEBUG msg=\"tide download clean-up failed\"") {
		t.Errorf("missing cleanup debug message in %q", logs.String())
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("held archive should remain: %v", err)
	}
}
