// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ReconcileStateReportForTest applies a full state report of the device
// without an agent channel, so a test controls when the transaction runs.
// It reports whether the device was already removed when the report took
// the device row lock. The report has no channel ID, so an export that a
// snapshot of the device still has in EXPORTING counts as owned by another
// channel; the tests that use this hook have no such export.
func (s *Service) ReconcileStateReportForTest(ctx context.Context, deviceID string, report *ownsandv1.StateReport) (removed bool, err error) {
	res, err := s.reconcileStateReport(ctx, deviceID, "", report)
	return res.removed, err
}
