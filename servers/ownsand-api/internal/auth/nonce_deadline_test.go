// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"testing"
	"time"
)

// TestNonceExpiredDeadline checks that the expiry instant itself is expired.
func TestNonceExpiredDeadline(t *testing.T) {
	expiry := time.Unix(1_800_000_060, 0)
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"one millisecond before", expiry.Add(-time.Millisecond), false},
		{"at the deadline", expiry, true},
		{"one millisecond after", expiry.Add(time.Millisecond), true},
	} {
		if got := NonceExpired(tc.now, expiry); got != tc.want {
			t.Errorf("%s: NonceExpired = %v, want %v", tc.name, got, tc.want)
		}
	}
}
