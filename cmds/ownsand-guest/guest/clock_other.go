//go:build !linux

package guest

import (
	"errors"
	"time"
)

// setSystemClock is not implemented for macOS and Windows guests yet.
func setSystemClock(time.Time) error {
	return errors.New("setting the guest clock is not implemented on this guest OS")
}
