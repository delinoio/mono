// SPDX-License-Identifier: Apache-2.0

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
