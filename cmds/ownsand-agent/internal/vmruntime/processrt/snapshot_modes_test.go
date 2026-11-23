// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package processrt

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/internal/fsutil"
)

// TestSnapshotAndRestorePreserveModesUnderUmask captures a sandbox root that
// holds a read-only directory and a group-writable file, restores a second
// sandbox from the snapshot under a restrictive umask, and checks that every
// copy keeps the modes of the source. Deleting both sandboxes and the
// snapshot must still succeed for a user that is not root.
func TestSnapshotAndRestorePreserveModesUnderUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	srcDir := longSandboxDir(t)
	restoredDir := longSandboxDir(t)
	snapDir := filepath.Join(t.TempDir(), "snapshot")
	// Registered before the runtime, so that it runs after the runtime
	// closed and killed the helpers: a failed test must not leave a
	// read-only tree that t.TempDir cannot remove.
	t.Cleanup(func() {
		for _, dir := range []string{srcDir, restoredDir, snapDir} {
			fsutil.RemoveAll(dir)
		}
	})
	rt := newRuntime(t)

	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.Create(context.Background(), vmruntime.Spec{SandboxID: id, Dir: srcDir, GuestOS: hostGuestOS, BaseImage: hostBaseImage})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(srcDir, rootDir)
	if err := os.MkdirAll(filepath.Join(root, "ro", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]fs.FileMode{"ro/nested/f": 0o444, "ro/g": 0o770, "h": 0o664} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ro/nested", "ro"} {
		if err := os.Chmod(filepath.Join(root, name), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]fs.FileMode{
		"ro": 0o555, "ro/nested": 0o555, "ro/nested/f": 0o444, "ro/g": 0o770, "h": 0o664,
	}
	requireModes := func(what, base string) {
		t.Helper()
		for name, mode := range want {
			info, err := os.Lstat(filepath.Join(base, name))
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if got := info.Mode().Perm(); got != mode {
				t.Errorf("%s: %s has mode %o, want %o", what, name, got, mode)
			}
		}
	}
	requireModes("source", root)

	if _, err := rt.Snapshot(context.Background(), v, snapDir); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	requireModes("snapshot", filepath.Join(snapDir, diskDir))

	restoredID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Create(context.Background(), vmruntime.Spec{
		SandboxID: restoredID, Dir: restoredDir, GuestOS: hostGuestOS, BaseImage: hostBaseImage,
		Snapshot: &vmruntime.SnapshotSource{SnapshotID: "snap", Dir: snapDir},
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	requireModes("restored sandbox", filepath.Join(restoredDir, rootDir))

	for _, sandboxID := range []string{restoredID, id} {
		if err := rt.Delete(context.Background(), sandboxID); err != nil {
			t.Fatalf("Delete %s: %v", sandboxID, err)
		}
	}
	for _, dir := range []string{srcDir, restoredDir} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("sandbox directory %s remains after Delete: %v", dir, err)
		}
	}
	if err := fsutil.RemoveAll(snapDir); err != nil {
		t.Fatalf("remove the snapshot: %v", err)
	}
}
