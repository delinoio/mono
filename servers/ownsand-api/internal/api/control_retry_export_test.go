// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import "context"

// SetLoadDeleteTargetForTest replaces the sandbox lookup of the delete event
// handler and returns the function it replaced, so a test can fail the
// lookup of a delete event once and let a later poll succeed. A caller
// installs it before the delete request that writes the event, so the
// replacement is ordered before the event processor's read of it.
func (c *Control) SetLoadDeleteTargetForTest(fn func(ctx context.Context, orgID, sandboxID string) (*sandboxRow, error)) (previous func(ctx context.Context, orgID, sandboxID string) (*sandboxRow, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, c.loadDeleteTarget = c.loadDeleteTarget, fn
	return previous
}

// SandboxRowForTest names the sandbox row type for tests of package api_test,
// which cannot name the unexported row type.
type SandboxRowForTest = sandboxRow
