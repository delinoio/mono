// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func checkLinuxRootfsGrowth(out string) error {
	// Allow for ext4 overhead on the 2 GiB disk, as in the Hyper-V test.
	const minKiB = 1536 * 1024
	lines := strings.SplitN(out, "\n", 4)
	if len(lines) < 3 {
		return fmt.Errorf("missing df row: %q", out)
	}
	fields := strings.Fields(lines[2])
	if len(fields) < 2 {
		return fmt.Errorf("missing root file system capacity in df row: %q", out)
	}
	if blocks, err := strconv.ParseInt(fields[1], 10, 64); err != nil || blocks < minKiB {
		return fmt.Errorf("root file system did not report at least %d KiB: %q", minKiB, out)
	}
	return nil
}

func TestLinuxRootfsGrowth(t *testing.T) {
	const greeting = "hello\nLinux aarch64"
	for _, tc := range []struct {
		name string
		out  string
		pass bool
	}{
		{name: "missing row", out: greeting},
		{name: "missing capacity", out: greeting + "\n/dev/vda\nlo\n"},
		{name: "nonnumeric capacity", out: greeting + "\n/dev/vda unknown 200000 810000 20% /\nlo\n"},
		{name: "ungrown capacity", out: greeting + "\n/dev/vda 1010000 200000 810000 20% /\nlo\n"},
		{name: "just below minimum", out: greeting + "\n/dev/vda 1572863 200000 1372863 13% /\nlo\n"},
		{name: "at minimum", out: greeting + "\n/dev/vda 1572864 200000 1372864 13% /\nlo\n", pass: true},
		{name: "above minimum", out: greeting + "\n/dev/vda 2000000 200000 1800000 10% /\nlo\n", pass: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLinuxRootfsGrowth(tc.out)
			if tc.pass {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), strconv.Quote(tc.out)) {
				t.Fatalf("expected an error with observed output %q, got %v", tc.out, err)
			}
		})
	}
}
