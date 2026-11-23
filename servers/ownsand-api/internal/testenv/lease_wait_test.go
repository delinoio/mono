// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
)

// TestWaitForLeaseBoundsAFailedAcquisition proves that a lease that is never
// acquired fails the wait with the lease error instead of blocking.
func TestWaitForLeaseBoundsAFailedAcquisition(t *testing.T) {
	// A lease that never runs is never held, like one whose acquisition
	// keeps failing.
	l := &lease.Lease{InstanceID: "never-acquired"}
	start := time.Now()
	err := waitForLease(l, 50*time.Millisecond)
	if err == nil {
		t.Fatal("waitForLease returned nil for a lease that is not held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "instance lease") {
		t.Fatalf("error = %q, want it to name the instance lease", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForLease took %s, want it bounded by its timeout", elapsed)
	}
}
