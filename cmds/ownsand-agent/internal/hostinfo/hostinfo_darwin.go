//go:build darwin

package hostinfo

import (
	"encoding/binary"
	"syscall"
)

// MemoryMiB returns hw.memsize.
func MemoryMiB() int64 {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	// syscall.Sysctl drops a trailing zero byte of the 8-byte value.
	b := append([]byte(s), make([]byte, 8)...)[:8]
	return int64(binary.LittleEndian.Uint64(b) >> 20)
}

// FreeDiskBytes returns the bytes available to the agent in dir.
func FreeDiskBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
