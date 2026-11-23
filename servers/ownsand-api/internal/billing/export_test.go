// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import "time"

// TestReportTimer is the idle timer of the reporter as a test controls it.
type TestReportTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// SetReportTimerForTest replaces the factory of the reporter's idle timer.
// Call it before Run; the seam exists only in test builds.
func SetReportTimerForTest(m *Metering, newTimer func(time.Duration) TestReportTimer) {
	m.newTimer = func(d time.Duration) reportTimer { return newTimer(d) }
}
