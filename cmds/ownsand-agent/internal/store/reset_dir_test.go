// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err != nil || len(entries) != 1 || entries[0].Name() != EnrollmentLockFile {
		t.Fatalf("entries %v, error %v", entries, err)
	}
	if err := ResetDir(dir); err != nil {
		t.Fatalf("second ResetDir: %v", err)
	}
}

func TestResetDirKeepsHeldLockAndStateUntilRetry(t *testing.T) {
	dir := t.TempDir()
	unlock := lockEnrollment(t, openStore(t, dir))
	defer unlock()
	lockPath := filepath.Join(dir, EnrollmentLockFile)
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{IdentityFile, AgentFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := resetDir(ctx, dir); !errors.Is(err, ErrEnrollmentBusy) {
		t.Fatalf("reset while enrollment holds the lock: %v, want ErrEnrollmentBusy", err)
	}
	for _, name := range []string{IdentityFile, AgentFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("busy reset changed %s: %v", name, err)
		}
	}
	unlock()
	if err := ResetDir(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("reset changed the enrollment lock: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != EnrollmentLockFile {
		t.Fatalf("entries %v, error %v", entries, err)
	}
}

func TestResetDirHandlesEnrollmentLockCasing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, strings.ToUpper(EnrollmentLockFile))
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, EnrollmentLockFile))
	caseSensitive := errors.Is(err, os.ErrNotExist)
	if err != nil && !caseSensitive {
		t.Fatal(err)
	}
	if err := ResetDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v, error %v", entries, err)
	}
	wantName := filepath.Base(path)
	if caseSensitive {
		wantName = EnrollmentLockFile
	}
	if got := entries[0].Name(); got != wantName {
		t.Fatalf("retained lock %q, want %q", got, wantName)
	}
}

func TestResetDirRejectsMalformedEnrollmentLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, EnrollmentLockFile), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(dir, IdentityFile)
	if err := os.WriteFile(identity, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ResetDir(dir); err == nil {
		t.Fatal("reset accepted a directory as the enrollment lock")
	}
	if _, err := os.Stat(identity); err != nil {
		t.Fatalf("reset changed the identity: %v", err)
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
