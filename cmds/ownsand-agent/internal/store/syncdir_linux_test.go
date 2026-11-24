// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package store

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The real flush works on a directory and fails on a missing one.
func TestSyncDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := syncDirectory(dir); err != nil {
		t.Fatalf("syncDirectory(existing) = %v", err)
	}
	if err := syncDirectory(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("syncDirectory(missing) succeeded")
	}
}

func TestSyncPendingIdentityDirectoriesFollowsResolvedPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if err := os.Mkdir("target", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", "alias"); err != nil {
		t.Fatal(err)
	}
	st, err := Open("alias/state")
	if err != nil {
		t.Fatal(err)
	}
	calls := recordSyncDir(t, nil)
	if err := st.SyncPendingIdentityDirectories(); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "target", "state"), filepath.Join(root, "target")}
	for dir := root; dir != "/"; dir = filepath.Dir(dir) {
		want = append(want, dir)
	}
	want = append(want, "/")
	if !slices.Equal(*calls, want) {
		t.Fatalf("synced directories = %v, want %v", *calls, want)
	}
}
