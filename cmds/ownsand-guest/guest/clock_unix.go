// SPDX-License-Identifier: Apache-2.0

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
