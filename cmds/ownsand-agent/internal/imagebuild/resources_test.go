// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestValidateResources(t *testing.T) {
	// The oversized values are built at run time: the constants of the
	// limits are 64-bit and must not be converted to a 32-bit int in source.
	tests := []struct {
		name                  string
		diskGiB, cpus, memMiB int64
		wantFlag              string // empty: accepted
	}{
		{name: "defaults"},
		{name: "supported", diskGiB: 64, cpus: 4, memMiB: 8192},
		{name: "largest disk", diskGiB: MaxDiskGiB},
		{name: "largest cpus", cpus: math.MaxInt32},
		{name: "largest memory", memMiB: MaxMemoryMiB},
		{name: "disk one over", diskGiB: MaxDiskGiB + 1, wantFlag: "--disk-gib"},
		{name: "disk wraps to zero", diskGiB: 1 << 34, wantFlag: "--disk-gib"},
		{name: "disk wraps to 64 GiB", diskGiB: 1<<34 + 64, wantFlag: "--disk-gib"},
		{name: "disk max int64", diskGiB: math.MaxInt64, wantFlag: "--disk-gib"},
		{name: "cpus one over", cpus: math.MaxInt32 + 1, wantFlag: "--cpus"},
		{name: "cpus wraps to zero", cpus: 1 << 32, wantFlag: "--cpus"},
		{name: "cpus wraps to four", cpus: 1<<32 + 4, wantFlag: "--cpus"},
		{name: "memory one over", memMiB: MaxMemoryMiB + 1, wantFlag: "--memory-mib"},
		{name: "memory wraps to zero", memMiB: 1 << 44, wantFlag: "--memory-mib"},
		{name: "memory max int64", memMiB: math.MaxInt64, wantFlag: "--memory-mib"},
		{name: "negative disk", diskGiB: -1, wantFlag: "must not be negative"},
		{name: "negative cpus", cpus: -1, wantFlag: "must not be negative"},
		{name: "negative memory", memMiB: -1, wantFlag: "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateResources(tc.diskGiB, tc.cpus, tc.memMiB)
			if tc.wantFlag == "" {
				if err != nil {
					t.Fatalf("ValidateResources: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantFlag) {
				t.Fatalf("ValidateResources error = %v, want %q", err, tc.wantFlag)
			}
		})
	}
	if got, want := strconv.FormatInt(MaxDiskGiB, 10), "8589934591"; got != want {
		t.Fatalf("MaxDiskGiB = %s, want %s", got, want)
	}
	if got, want := MaxDiskGiB<<30, int64(math.MaxInt64)&^(1<<30-1); got != want {
		t.Fatalf("MaxDiskGiB<<30 = %d, want %d", got, want)
	}
	if got := MaxMemoryMiB << 20; got <= 0 {
		t.Fatalf("MaxMemoryMiB<<20 = %d overflows", got)
	}
}

// TestBuildRejectsOversizedResources checks the programmatic entry point: the
// check runs before the guest OS is dispatched, so no host is needed. The vCPU
// count is an int32 there and cannot exceed its limit.
func TestBuildRejectsOversizedResources(t *testing.T) {
	for _, guest := range []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_MACOS, ownsandv1.GuestOs_GUEST_OS_WINDOWS} {
		dir := t.TempDir()
		for name, opts := range map[string]Options{
			"--disk-gib":   {DiskGiB: MaxDiskGiB + 1},
			"--memory-mib": {MemoryMiB: MaxMemoryMiB + 1},
		} {
			opts.ImagesDir = dir
			opts.GuestOS = guest
			err := Build(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%v %s: Build error = %v", guest, name, err)
			}
		}
	}
}
