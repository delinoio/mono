// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux && !darwin

package guest

import (
	"errors"
	"time"
)

// setSystemClock is not implemented on Windows guests yet (it needs
// SetSystemTime with the SE_SYSTEMTIME_NAME privilege). Implement it with
// the Windows guest image.
func setSystemClock(time.Time) error {
	return errors.ErrUnsupported
}
