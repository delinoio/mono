// SPDX-License-Identifier: FSL-1.1-ALv2

package authz

import (
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestMatrix checks the authorization matrix of the contract cell by cell.
func TestMatrix(t *testing.T) {
	type row struct{ owner, admin, member, apiKey bool }
	want := map[Action]row{
		ActionOwnerOnly:        {true, false, false, false},
		ActionManageMembers:    {true, true, false, false},
		ActionLeave:            {false, true, true, false},
		ActionManageDevices:    {true, true, false, false},
		ActionManageAPIKeys:    {true, true, false, false},
		ActionReadOrganization: {true, true, true, false},
		ActionReadDevices:      {true, true, true, true},
		ActionUseSandboxes:     {true, true, true, true},
	}
	if len(want) != len(AllActions()) {
		t.Fatalf("the test covers %d of %d actions", len(want), len(AllActions()))
	}
	for _, action := range AllActions() {
		w := want[action]
		got := row{
			RoleAllowed(ownsandv1.Role_ROLE_OWNER, action),
			RoleAllowed(ownsandv1.Role_ROLE_ADMIN, action),
			RoleAllowed(ownsandv1.Role_ROLE_MEMBER, action),
			APIKeyAllowed(action),
		}
		if got != w {
			t.Errorf("action %d: got %+v, want %+v", action, got, w)
		}
		if RoleAllowed(ownsandv1.Role_ROLE_UNSPECIFIED, action) {
			t.Errorf("action %d allowed for an unspecified role", action)
		}
	}
}
