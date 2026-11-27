// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package store

import (
	"bytes"
	"errors"
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
	want = append(want, ancestorPaths(root)...)
	if !slices.Equal(*calls, want) {
		t.Fatalf("synced directories = %v, want %v", *calls, want)
	}
}

// recordSyncDir replaces syncDir for one test and returns the paths that it
// was called with, in order. onSync, when set, runs after recording each call.
func recordSyncDir(t *testing.T, onSync func(string) error) *[]string {
	t.Helper()
	var calls []string
	old := syncDir
	syncDir = func(path string) error {
		calls = append(calls, path)
		if onSync != nil {
			return onSync(path)
		}
		return nil
	}
	t.Cleanup(func() { syncDir = old })
	return &calls
}

func ancestorPaths(dir string) []string {
	var paths []string
	for ; dir != "/"; dir = filepath.Dir(dir) {
		paths = append(paths, dir)
	}
	return append(paths, "/")
}

func TestSaveIdentityDurableSyncsAncestors(t *testing.T) {
	for _, name := range []string{"first opener", "second opener"} {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "parent", "child", "state")
			st, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if name == "second opener" {
				st, err = Open(dir)
				if err != nil {
					t.Fatal(err)
				}
			}

			unlock, err := st.LockEnrollment(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if _, err := st.LoadIdentity(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("LoadIdentity before save = %v, want ErrNotFound", err)
			}
			id := pendingIdentity()
			calls := recordSyncDir(t, func(path string) error {
				if path == dir {
					assertPendingIdentity(t, st, id)
				}
				return nil
			})
			if err := st.SaveIdentityDurable(id); err != nil {
				t.Fatal(err)
			}
			want := []string{dir, filepath.Join(root, "parent", "child"), filepath.Join(root, "parent")}
			want = append(want, ancestorPaths(root)...)
			if !slices.Equal(*calls, want) {
				t.Fatalf("synced directories = %v, want %v", *calls, want)
			}
		})
	}
}

func TestSaveIdentityDurableKeepsPendingKeyAfterAncestorSyncFailure(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "parent")
	dir := filepath.Join(parent, "state")
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	unlock, err := st.LockEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	sentinel := errors.New("flush failed")
	fail := true
	calls := recordSyncDir(t, func(path string) error {
		if fail && path == parent {
			return sentinel
		}
		return nil
	})
	id := pendingIdentity()
	if err := st.SaveIdentityDurable(id); !errors.Is(err, sentinel) {
		t.Fatalf("SaveIdentityDurable = %v, want the ancestor flush error", err)
	}
	if want := []string{dir, parent}; !slices.Equal(*calls, want) {
		t.Fatalf("synced directories before failure = %v, want %v", *calls, want)
	}
	assertPendingIdentity(t, st, id)

	fail = false
	*calls = nil
	if err := st.SyncPendingIdentityDirectories(); err != nil {
		t.Fatal(err)
	}
	want := append([]string{dir, parent}, ancestorPaths(root)...)
	if !slices.Equal(*calls, want) {
		t.Fatalf("synced directories on retry = %v, want %v", *calls, want)
	}
	assertPendingIdentity(t, st, id)
}

func assertPendingIdentity(t *testing.T, st *Store, want *Identity) {
	t.Helper()
	id, err := st.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if id.DeviceID != "" || !bytes.Equal(id.PrivateKeySeed, want.PrivateKeySeed) {
		t.Fatal("identity file does not contain the expected pending key")
	}
}

// A failed write is an error before any flush.
func TestSaveIdentityDurableDoesNotSyncAfterWriteFailure(t *testing.T) {
	calls := recordSyncDir(t, nil)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A directory at identity.json makes the atomic rename fail.
	if err := os.MkdirAll(filepath.Join(s.Path(IdentityFile), "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentityDurable(pendingIdentity()); err == nil {
		t.Fatal("SaveIdentityDurable succeeded with a blocked identity.json")
	}
	if len(*calls) != 0 {
		t.Fatalf("synced directories after a failed write = %v", *calls)
	}
}

func TestFailedCreateCleanupResyncsAfterMarkerRemoval(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("injected directory sync failure")
	var fail bool
	calls := recordSyncDir(t, func(string) error {
		if fail {
			return cause
		}
		return nil
	})
	if err := st.SaveSandboxCleanup("sandbox"); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := st.DeleteSandboxCleanup("sandbox"); !errors.Is(err, cause) {
		t.Fatalf("clear marker = %v, want a directory sync error", err)
	}
	fail = false
	if err := st.DeleteSandboxCleanup("sandbox"); err != nil {
		t.Fatal(err)
	}
	dir := st.Path(SandboxesDir)
	if want := []string{dir, dir, dir}; !slices.Equal(*calls, want) {
		t.Fatalf("directory sync calls = %v, want %v", *calls, want)
	}
}
