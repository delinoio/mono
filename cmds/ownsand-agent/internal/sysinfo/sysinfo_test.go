// SPDX-License-Identifier: FSL-1.1-ALv2

package sysinfo

import (
	"math"
	"path/filepath"
	"testing"
)

func TestHostFacts(t *testing.T) {
	if HostOS() == 0 || Arch() == 0 || LogicalCPUs() < 1 {
		t.Fatal("host facts missing")
	}
	if MemoryMiB() <= 0 {
		t.Fatal("memory unknown")
	}
	if free, err := FreeDiskBytes(t.TempDir()); err != nil || free <= 0 {
		t.Fatalf("free disk %d %v", free, err)
	}
}

func TestFreeDiskBytesProbeError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "child")
	if free, err := FreeDiskBytes(dir); err == nil || free != 0 {
		t.Fatalf("free disk for a missing directory = %d, %v, want zero and a probe error", free, err)
	}
}

func TestAvailBytes(t *testing.T) {
	tests := []struct {
		name   string
		bavail uint64
		bsize  int64
		frsize int64
		want   int64
	}{
		{"fragment size wins over a larger block size", 1000, 131072, 4096, 4096000},
		{"zero fragment size falls back to the block size", 1000, 4096, 0, 4096000},
		{"equal sizes", 1000, 4096, 4096, 4096000},
		{"no free blocks", 0, 131072, 4096, 0},
		{"last representable fragment count", (1 << 51) - 1, 131072, 4096, math.MaxInt64 - 4095},
		{"fragment count at the signed boundary", 1 << 51, 131072, 4096, math.MaxInt64},
		{"fragment count at the unsigned boundary", 1 << 52, 131072, 4096, math.MaxInt64},
		{"last representable block count", (1 << 51) - 1, 4096, 0, math.MaxInt64 - 4095},
		{"block count at the signed boundary", 1 << 51, 4096, 0, math.MaxInt64},
		{"block count at the unsigned boundary", 1 << 52, 4096, 0, math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := availBytes(tt.bavail, tt.bsize, tt.frsize); got != tt.want {
				t.Fatalf("availBytes(%d, %d, %d) = %d, want %d", tt.bavail, tt.bsize, tt.frsize, got, tt.want)
			}
		})
	}
}

func TestClampFreeBytes(t *testing.T) {
	// Windows supplies the caller-available count in bytes.
	for _, tc := range []struct {
		count uint64
		want  int64
	}{
		{0, 0},
		{123456789, 123456789},
		{math.MaxInt64, math.MaxInt64},
		{uint64(math.MaxInt64) + 1, math.MaxInt64},
		{math.MaxUint64, math.MaxInt64},
	} {
		if got := clampFreeBytes(tc.count, 1); got != tc.want {
			t.Errorf("clampFreeBytes(%d, 1) = %d, want %d", tc.count, got, tc.want)
		}
	}
}
