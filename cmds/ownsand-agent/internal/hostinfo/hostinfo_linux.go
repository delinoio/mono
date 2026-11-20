//go:build linux

package hostinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// MemoryMiB returns the total memory from /proc/meminfo.
func MemoryMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, _ := strconv.ParseInt(fields[1], 10, 64)
			return kib / 1024
		}
	}
	return 0
}

// FreeDiskBytes returns the bytes available to the agent in dir.
func FreeDiskBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
