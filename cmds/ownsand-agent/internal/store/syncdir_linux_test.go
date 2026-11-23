// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package store

import (
	"path/filepath"
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
