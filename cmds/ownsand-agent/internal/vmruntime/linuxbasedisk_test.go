// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckLinuxBaseDiskSize(t *testing.T) {
	const gib = 1 << 30
	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		bytes        int64
		requestedGiB int64
		wantError    string
	}{
		{"smaller", 2 * gib, 1, "needs at least 2 GiB of disk (requested 1 GiB)"},
		{"equal", 2 * gib, 2, ""},
		{"larger", 2 * gib, 3, ""},
		{"partial GiB rounds up", 2*gib + 1, 2, "needs at least 3 GiB of disk (requested 2 GiB)"},
		{"maximum valid request", 2*gib + 1, (1 << 33) - 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Truncate(path, tc.bytes); err != nil {
				t.Fatal(err)
			}
			err := CheckLinuxBaseDiskSize(path, tc.requestedGiB)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("disk check = %v, want ErrUnsupported with %q", err, tc.wantError)
			}
		})
	}
}
