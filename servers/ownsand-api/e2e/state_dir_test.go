// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemainingStatePaths covers the state-directory inspection of the
// device removal case: an empty directory has no paths, a leftover is
// reported with its path, and a directory that cannot be read is an error
// instead of an empty result. It needs no Hyper-V host.
func TestRemainingStatePaths(t *testing.T) {
	t.Run("empty directory", func(t *testing.T) {
		left, err := remainingStatePaths(t.TempDir())
		if err != nil || len(left) != 0 {
			t.Fatalf("remainingStatePaths = %v, %v, want no paths and no error", left, err)
		}
	})
	t.Run("leftover file", func(t *testing.T) {
		root := t.TempDir()
		leftover := filepath.Join(root, "device.key")
		if err := os.WriteFile(leftover, []byte("key"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		left, err := remainingStatePaths(root)
		if err != nil {
			t.Fatalf("remainingStatePaths: %v", err)
		}
		if len(left) != 1 || left[0] != leftover {
			t.Fatalf("remainingStatePaths = %v, want [%s]", left, leftover)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "never-created")
		left, err := remainingStatePaths(root)
		if err == nil {
			t.Fatalf("remainingStatePaths = %v, nil, want an error", left)
		}
		if !strings.Contains(err.Error(), root) {
			t.Fatalf("error %q does not name the path %s", err, root)
		}
	})
}
