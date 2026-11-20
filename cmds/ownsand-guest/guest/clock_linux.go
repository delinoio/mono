//go:build linux

package guest

import (
	"syscall"
	"time"
)

// setSystemClock sets the guest wall clock. The guest helper runs as root.
func setSystemClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}
