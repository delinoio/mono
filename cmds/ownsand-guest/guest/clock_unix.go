// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guest

import (
	"time"

	"golang.org/x/sys/unix"
)

func setSystemClock(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Settimeofday(&tv)
}
