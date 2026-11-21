// SPDX-License-Identifier: Apache-2.0

package hcsrt

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNotFoundOrStopped(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		// HcsOpenComputeSystem of computecore.dll returns 0x8037010e for an
		// unknown compute system ID.
		{&hcsError{op: "HcsOpenComputeSystem", hr: 0x8037010e}, true},
		{&hcsError{op: "HcsTerminateComputeSystem", hr: 0x80370110}, true},
		{&hcsError{op: "HcsOpenComputeSystem", hr: 0xc037010e}, true},
		{&hcsError{op: "HcsTerminateComputeSystem", hr: 0xc0370110}, true},
		{fmt.Errorf("wrapped: %w", &hcsError{op: "HcsOpenComputeSystem", hr: 0x8037010e}), true},
		{&hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}, false},
		{errors.New("other"), false},
	} {
		if got := isNotFoundOrStopped(tc.err); got != tc.want {
			t.Errorf("isNotFoundOrStopped(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
