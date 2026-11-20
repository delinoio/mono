//go:build !linux && !darwin && !windows

package hostinfo

// MemoryMiB is unknown on other host OSes.
func MemoryMiB() int64 { return 0 }

// FreeDiskBytes is unknown on other host OSes.
func FreeDiskBytes(string) int64 { return 0 }
