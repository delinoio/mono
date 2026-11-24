// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A snapshot disk that was copied from a read-only directory keeps the
// read-only mode, and deleting the snapshot must still work.
func TestDeleteSnapshotRemovesReadOnlyContent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot("snap", map[string]string{"id": "snap"}); err != nil {
		t.Fatal(err)
	}
	dir, err := s.SnapshotDir("snap")
	if err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(dir, "content", "disk", "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "f"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	if err := s.DeleteSnapshot("snap"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("snapshot directory still exists: %v", err)
	}
}
