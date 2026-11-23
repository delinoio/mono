// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// recordSyncDir replaces syncDir for one test and returns the paths that it
// was called with, in order. fail, when set, is returned by every call.
func recordSyncDir(t *testing.T, fail error) *[]string {
	t.Helper()
	var calls []string
	old := syncDir
	syncDir = func(path string) error {
		calls = append(calls, path)
		return fail
	}
	t.Cleanup(func() { syncDir = old })
	return &calls
}

func pendingIdentity() *Identity {
	return &Identity{PrivateKeySeed: []byte("0123456789abcdef0123456789abcdef")}
}

// The pending identity is on disk when the state directory is flushed, and
// an existing state directory needs no other flush.
func TestSaveIdentityDurableSyncsExistingStateDirectory(t *testing.T) {
	dir := t.TempDir()
	calls := recordSyncDir(t, nil)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Check the order by looking for the file inside the flush.
	var seenAtSync bool
	record := syncDir
	syncDir = func(path string) error {
		_, serr := os.Stat(filepath.Join(dir, IdentityFile))
		seenAtSync = serr == nil
		return record(path)
	}
	if err := s.SaveIdentityDurable(pendingIdentity()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*calls, []string{dir}) {
		t.Fatalf("synced directories = %v, want only %s", *calls, dir)
	}
	if !seenAtSync {
		t.Fatal("the identity file did not exist when the directory was flushed")
	}
	loaded, err := s.LoadIdentity()
	if err != nil || string(loaded.PrivateKeySeed) != string(pendingIdentity().PrivateKeySeed) {
		t.Fatalf("loaded identity = %+v, %v", loaded, err)
	}
}

// A state directory that Open created, with the ancestors that were missing,
// is reachable after a power loss only when the parent of each is flushed.
func TestSaveIdentityDurableSyncsCreatedAncestors(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "state")
	calls := recordSyncDir(t, nil)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentityDurable(pendingIdentity()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		dir,
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a"),
		root,
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("synced directories = %v, want %v", *calls, want)
	}
}

// A failed flush is an error, so that the caller does not go on.
func TestSaveIdentityDurableReturnsSyncFailure(t *testing.T) {
	sentinel := errors.New("flush failed")
	recordSyncDir(t, sentinel)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentityDurable(pendingIdentity()); !errors.Is(err, sentinel) {
		t.Fatalf("SaveIdentityDurable = %v, want the flush error", err)
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

// The file keeps its format and mode.
func TestSaveIdentityDurableKeepsFormat(t *testing.T) {
	recordSyncDir(t, nil)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := pendingIdentity()
	if err := s.SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(s.Path(IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentityDurable(id); err != nil {
		t.Fatal(err)
	}
	durable, err := os.ReadFile(s.Path(IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != string(durable) {
		t.Fatalf("durable file differs from SaveIdentity output:\n%s\nvs\n%s", durable, plain)
	}
}
