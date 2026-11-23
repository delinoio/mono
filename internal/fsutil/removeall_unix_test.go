// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// makeReadOnlyTree builds root/a/b/f with the directories a and b read-only
// and registers a cleanup that makes them writable again, so t.TempDir can
// delete the tree even when a test fails.
func makeReadOnlyTree(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "f"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "a", "b"), filepath.Join(root, "a"), root} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { makeDirsWritable(root) })
}

func TestRemoveAllDeletesReadOnlyDirectories(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "tree")
	makeReadOnlyTree(t, root)
	if err := RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tree still exists: %v", err)
	}
}

func TestRemoveAllMissingPathIsNotAnError(t *testing.T) {
	if err := RemoveAll(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
}

func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(outside, 0o755) })
	root := filepath.Join(parent, "tree")
	makeReadOnlyTree(t, root)
	// A link inside the read-only directory b would need a writable b, so
	// replace b's content before the tree turns read-only.
	if err := os.Chmod(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a", "b", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a", "b"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatalf("link target was removed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o555 {
		t.Fatalf("link target mode changed to %o", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("link target content was removed: %v", err)
	}
}

func TestRemoveAllStillReportsAnUndeletableTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove entries of a read-only directory")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "tree")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The read-only parent blocks the removal of root itself, which no
	// change inside the tree can fix.
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })
	err := RemoveAll(root)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("RemoveAll error = %v, want a permission error", err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("tree was removed: %v", err)
	}
}
