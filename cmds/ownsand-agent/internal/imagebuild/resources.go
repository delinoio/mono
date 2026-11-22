// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"fmt"
	"math"
)

// Largest accepted resource values. The build converts the disk size from GiB
// and the memory size from MiB to a byte count in a signed 64-bit integer
// (and a uint64 for the macOS VM memory), and hands the vCPU count to the
// hypervisor as a signed 32-bit integer; a larger value would wrap in the
// conversion and silently build a different, smaller image or VM.
const (
	MaxDiskGiB   int64 = math.MaxInt64 >> 30
	MaxMemoryMiB int64 = math.MaxInt64 >> 20
	MaxCPUCount  int64 = math.MaxInt32
)

// ValidateResources checks the disk size (GiB), the vCPU count and the memory
// (MiB) of a build before any state is created. Zero selects the default of
// the guest OS. The messages name the CLI flags, which are the only callers.
func ValidateResources(diskGiB, cpus, memoryMiB int64) error {
	switch {
	case diskGiB < 0 || cpus < 0 || memoryMiB < 0:
		return fmt.Errorf("imagebuild: --disk-gib, --cpus and --memory-mib must not be negative")
	case diskGiB > MaxDiskGiB:
		return fmt.Errorf("imagebuild: --disk-gib %d is too large (at most %d)", diskGiB, MaxDiskGiB)
	case cpus > MaxCPUCount:
		return fmt.Errorf("imagebuild: --cpus %d is too large (at most %d)", cpus, MaxCPUCount)
	case memoryMiB > MaxMemoryMiB:
		return fmt.Errorf("imagebuild: --memory-mib %d is too large (at most %d)", memoryMiB, MaxMemoryMiB)
	}
	return nil
}
