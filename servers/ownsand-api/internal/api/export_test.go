// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ReconcileStateReportForTest applies a full state report of the device
// without an agent channel, so a test controls when the transaction runs.
// It reports whether the device was already removed when the report took
// the device row lock.
func (s *Service) ReconcileStateReportForTest(ctx context.Context, deviceID string, report *ownsandv1.StateReport) (removed bool, err error) {
	res, err := s.reconcileStateReport(ctx, deviceID, report)
	return res.removed, err
}

// WatcherCount reports the number of live sandbox change registrations. It is
// exported for the external api_test package, which cannot reach the unexported
// watch registry and which testenv (importing api) prevents an internal test
// from using.
func (s *Service) WatcherCount() int { return s.watch.count() }
