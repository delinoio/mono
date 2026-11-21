// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package guest

import (
	"errors"
	"time"
)

// setSystemClock is not available on this guest OS.
func setSystemClock(time.Time) error {
	return errors.ErrUnsupported
}
