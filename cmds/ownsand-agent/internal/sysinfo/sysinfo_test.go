// SPDX-License-Identifier: FSL-1.1-ALv2

package sysinfo

import "testing"

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := availBytes(tt.bavail, tt.bsize, tt.frsize); got != tt.want {
				t.Fatalf("availBytes(%d, %d, %d) = %d, want %d", tt.bavail, tt.bsize, tt.frsize, got, tt.want)
			}
		})
	}
}
