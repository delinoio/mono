// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ResetDir needs no healthy layout: it deletes an identity and a regular
// file at images, which Open rejects, and keeps the directory itself.
func TestResetDirDeletesDamagedLayout(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{IdentityFile, ImagesDir} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a regular file at images")
	}
	if err := ResetDir(dir); err != nil {
		t.Fatalf("ResetDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries %v, error %v", entries, err)
	}
	if err := ResetDir(dir); err != nil {
		t.Fatalf("second ResetDir on an empty directory: %v", err)
	}
}

// An absent directory counts as empty and stays absent.
func TestResetDirAbsentDirectoryIsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if err := ResetDir(dir); err != nil {
		t.Fatalf("ResetDir: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ResetDir created the directory: %v", err)
	}
}

// An empty path and a path that is a regular file are invalid.
func TestResetDirRejectsInvalidDirectory(t *testing.T) {
	if err := ResetDir(""); err == nil {
		t.Error("ResetDir accepted an empty path")
	}
	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ResetDir(file); err == nil {
		t.Error("ResetDir accepted a regular file")
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("ResetDir changed the file: %v", err)
	}
}

// A failed deletion is reported, and the other entries are still deleted.
func TestResetDirReportsFailedDeletionAndContinues(t *testing.T) {
	dir := t.TempDir()
	stuck := filepath.Join(dir, "a-stuck")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The removal of a tree makes its read-only directories writable, so a
	// read-only directory no longer makes the removal fail; a seam does.
	orig := removeAll
	t.Cleanup(func() { removeAll = orig })
	removeAll = func(path string) error {
		if path == stuck {
			return errors.New("stuck entry")
		}
		return orig(path)
	}
	for _, name := range []string{IdentityFile, ImagesDir} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ResetDir(dir); err == nil {
		t.Fatal("ResetDir did not report the entry that it could not delete")
	}
	for _, name := range []string{IdentityFile, ImagesDir} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists: %v", name, err)
		}
	}
}
