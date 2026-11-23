// SPDX-License-Identifier: FSL-1.1-ALv2

package scheduler

import (
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestRevokedPendingIdleIsTransientWithoutVersionRevoked checks that the
// agent-reported REVOKED_PENDING_IDLE state alone blocks placement as a
// transient condition, even when the control plane's manifest view has not
// marked the version revoked yet.
func TestRevokedPendingIdleIsTransientWithoutVersionRevoked(t *testing.T) {
	d := linuxDevice("d1")
	d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
	d.VersionRevoked = false
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("revoked pending idle without VersionRevoked: %v", err)
	}
	d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE
	if _, err := Place(linuxRequest(), []Device{d}, now); err != nil {
		t.Fatalf("up to date device: %v", err)
	}
}
