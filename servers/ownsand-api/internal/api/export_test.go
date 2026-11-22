// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"

	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
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

// WatcherCount reports the number of live sandbox change registrations. It is
// exported for the external api_test package, which cannot reach the unexported
// watch registry and which testenv (importing api) prevents an internal test
// from using.
func (s *Service) WatcherCount() int { return s.watch.count() }

// SetAfterConfigLimitsReadForTest sets a hook that runs after a configuration
// send read the device limits and before it queues the message; nil removes
// it. A test pauses a sender there to open the window between the read and the
// queueing.
func (s *Service) SetAfterConfigLimitsReadForTest(fn func(deviceID string)) {
	if fn == nil {
		s.afterConfigLimitsRead.Store(nil)
		return
	}
	s.afterConfigLimitsRead.Store(&fn)
}

// SendConfigurationForTest runs the configuration send that the channel
// handler runs when the channel opens.
func (s *Service) SendConfigurationForTest(ctx context.Context, conn *hub.Conn) error {
	return s.sendConfiguration(ctx, conn)
}

// SessionCount reports the number of open or pending data path sessions on
// the lease holder, so an api_test test can check that a rejected OpenSession
// leaves none.
func (p *DataPlane) SessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// ReauthorizeForTest runs the authorization that OpenSession and
// AuthorizeRelay repeat after they register a pending session or grant, with
// the data path action. A test uses it where the registered call cannot reach
// that step, for example after the organization and its sandboxes are gone.
func (s *Service) ReauthorizeForTest(ctx context.Context, orgID string) error {
	_, _, err := s.reauthorize(ctx, s.DB.Pool, orgID, authz.ActionUseSandboxes)
	return err
}
