// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guest

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func setSystemClock(t time.Time) error {
	tv, err := timevalFor(t)
	if err != nil {
		return err
	}
	return unix.Settimeofday(&tv)
}

// timevalFor converts t to the unix.Timeval of this platform. The seconds and
// the sub-second part are converted separately, because t.UnixNano overflows
// int64 for times after 2262-04-11. The microseconds round up so the clock
// never lands before the requested instant, and 1,000,000 microseconds carry
// into the seconds. It returns an error, before any clock update, when a field
// of the platform Timeval (for example the int32 fields of 32-bit Linux) cannot
// hold the value.
func timevalFor(t time.Time) (unix.Timeval, error) {
	sec, usec := splitTimeval(t)
	var tv unix.Timeval
	if !fitsInteger(&tv.Sec, sec) {
		return unix.Timeval{}, fmt.Errorf("clock time %s does not fit the seconds field of this platform's timeval", t.UTC().Format(time.RFC3339Nano))
	}
	if !fitsInteger(&tv.Usec, usec) {
		return unix.Timeval{}, fmt.Errorf("clock time %s does not fit the microseconds field of this platform's timeval", t.UTC().Format(time.RFC3339Nano))
	}
	return tv, nil
}

// splitTimeval returns the Unix seconds of t and its sub-second part in
// microseconds, rounded up and carried into the seconds.
func splitTimeval(t time.Time) (sec, usec int64) {
	sec = t.Unix()
	usec = int64(t.Nanosecond()+999) / 1000
	if usec == 1_000_000 {
		sec++
		usec = 0
	}
	return sec, usec
}

// fitsInteger stores v in *dst and reports whether the destination type holds
// it without truncation. It leaves *dst unchanged when v does not fit.
func fitsInteger[T ~int32 | ~int64](dst *T, v int64) bool {
	converted := T(v)
	if int64(converted) != v {
		return false
	}
	*dst = converted
	return true
}
