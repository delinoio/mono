// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsReadSnapshotCleanupDenial(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		held string
		deny string
	}{
		{name: "denial only", deny: "a-denied"},
		{name: "handle first", held: "a-held", deny: "z-denied"},
		{name: "denial first", held: "z-held", deny: "a-denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			old := filepath.Join(state, "reads", "snapshots-old")
			nested := filepath.Join(old, "nested")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			denied := filepath.Join(nested, tc.deny)
			if err := os.WriteFile(denied, []byte("synthetic snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
			var held *os.File
			if tc.held != "" {
				held = holdReadStartupFile(t, filepath.Join(nested, tc.held))
			}
			restore := denyReadCleanupDeletion(t, denied)
			if held != nil {
				if err := os.Remove(held.Name()); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
					t.Fatalf("independent handle fixture: %v, want ERROR_SHARING_VIOLATION", err)
				}
			}

			var logs bytes.Buffer
			cfg := Config{StateDir: state, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			assertReadStartupCleanupFailure(t, cfg, windows.ERROR_ACCESS_DENIED, "snapshots-old")
			if logs.Len() != 0 {
				t.Fatalf("unexpected warning on fatal cleanup: %s", &logs)
			}
			if held != nil {
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
			}
			restore()

			startAfterReadCleanupFailure(t, cfg, old)
		})
	}
}

func TestWindowsReadSnapshotCleanupRootDenial(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		heldFile bool
	}{
		{name: "empty root"},
		{name: "root with held snapshot", heldFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			root := filepath.Join(state, "reads")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			var held *os.File
			var entries []string
			if tc.heldFile {
				held = holdReadStartupFile(t, filepath.Join(root, "snapshots-old", "held"))
				entries = []string{"snapshots-old"}
			}
			restore := denyReadCleanupDeletion(t, root)

			cfg := Config{StateDir: state}
			assertReadStartupCleanupFailure(t, cfg, windows.ERROR_ACCESS_DENIED, entries...)
			if held != nil {
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
			}
			restore()
			startAfterReadCleanupFailure(t, cfg, filepath.Join(root, "snapshots-old"))
		})
	}
}

func TestWindowsReadSnapshotCleanupKeepsJunctionTarget(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	old := filepath.Join(state, "reads", "snapshots-old")
	held := holdReadStartupFile(t, filepath.Join(old, "held"))
	target := t.TempDir()
	marker := filepath.Join(target, "keep")
	if err := os.WriteFile(marker, []byte("outside tree"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(old, "junction")
	out, err := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create junction: %v\n%s", err, out)
	}

	// Keep the junction present through the initial RemoveAll so the
	// fallback must inspect it too.
	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })

	srv, err := New(Config{StateDir: state})
	if err != nil {
		t.Fatalf("start with held snapshot file and junction: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if got, err := os.ReadFile(marker); err != nil || string(got) != "outside tree" {
		t.Fatalf("junction target marker = %q, %v", got, err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertReadStartupCleanupFailure(t *testing.T, cfg Config, cause error, wantEntries ...string) {
	t.Helper()
	srv, err := New(cfg)
	if srv != nil {
		_ = srv.Close()
	}
	if !errors.Is(err, cause) || srv != nil {
		t.Fatalf("start: server returned = %t, error = %v, want %v", srv != nil, err, cause)
	}
	root := filepath.Join(cfg.StateDir, "reads")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, wantEntries) {
		t.Fatalf("reads after failed start: %v, want %v", names, wantEntries)
	}
	lock, err := lockStateDir(cfg.StateDir)
	if err != nil {
		t.Fatalf("take state lock after failed start: %v", err)
	}
	lock.release()
}

func startAfterReadCleanupFailure(t *testing.T, cfg Config, old string) {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("start after removing the deletion block: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old snapshot directory after restart: %v, want not exist", err)
	}
	if info, err := os.Stat(srv.reads.dir); err != nil || !info.IsDir() {
		t.Fatalf("fresh snapshot directory: %v", err)
	}
}

// Windows permits deletion via either DELETE on the entry or
// FILE_DELETE_CHILD on its parent. Deny both, and prove the denial
// before letting the helper touch the fixture.
func denyReadCleanupDeletion(t *testing.T, path string) func() {
	t.Helper()
	const fileDeleteChild windows.ACCESS_MASK = 0x40
	restoreParent := denyReadCleanupAccess(t, filepath.Dir(path), fileDeleteChild)
	restoreEntry := denyReadCleanupAccess(t, path, windows.DELETE)
	if err := os.Remove(path); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("native denied deletion of %s: %v, want ERROR_ACCESS_DENIED (%d)", filepath.Base(path), err, windows.ERROR_ACCESS_DENIED)
	}
	t.Logf("native denied deletion: ERROR_ACCESS_DENIED (%d)", windows.ERROR_ACCESS_DENIED)
	return func() {
		restoreEntry()
		restoreParent()
	}
}

func denyReadCleanupAccess(t *testing.T, path string, access windows.ACCESS_MASK) func() {
	t.Helper()
	path = extendedPath(path)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: access,
		AccessMode:        windows.DENY_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, original)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
		if control&windows.SE_DACL_PROTECTED != 0 {
			flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags, nil, nil, original, nil); err != nil {
			t.Errorf("restore DACL: %v", err)
		}
	})
	t.Cleanup(restore)
	return restore
}
