// SPDX-License-Identifier: FSL-1.1-ALv2

package authz

import (
	"testing"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

const (
	owner  = tidev1.Role_ROLE_OWNER
	admin  = tidev1.Role_ROLE_ADMIN
	member = tidev1.Role_ROLE_MEMBER
)

// TestMatrix checks every row of the contract's authorization matrix.
func TestMatrix(t *testing.T) {
	type row struct {
		owner, admin, member, apiKey bool
	}
	want := map[Action]row{
		ActionOwnerOnly:            {true, false, false, false},
		ActionManageSettings:       {true, true, false, false},
		ActionManageMembers:        {true, true, false, false},
		ActionLeave:                {false, true, true, false},
		ActionManageIntegrations:   {true, true, false, false},
		ActionManageRunnerProfiles: {true, true, false, false},
		ActionDeleteRunner:         {true, true, false, false},
		ActionCancelRequest:        {true, true, false, false},
		ActionRead:                 {true, true, true, false},
		ActionManageOwnAPIKeys:     {true, true, true, false},
		ActionUseDevSessions:       {true, true, true, true},
		ActionOperateDevSession:    {true, true, true, true},
		ActionDeleteDevSession:     {true, true, true, true},
	}
	if len(want) != len(AllActions()) {
		t.Fatalf("the test covers %d actions, AllActions has %d", len(want), len(AllActions()))
	}
	for _, a := range AllActions() {
		w, ok := want[a]
		if !ok {
			t.Fatalf("action %d has no expected row", a)
		}
		got := row{RoleAllowed(owner, a), RoleAllowed(admin, a), RoleAllowed(member, a), APIKeyAllowed(a)}
		if got != w {
			t.Errorf("action %d: got %+v, want %+v", a, got, w)
		}
		if RoleAllowed(tidev1.Role_ROLE_UNSPECIFIED, a) {
			t.Errorf("action %d allowed for an unspecified role", a)
		}
	}
	if RoleAllowed(owner, ActionUnspecified) || APIKeyAllowed(ActionUnspecified) {
		t.Fatal("unspecified action allowed")
	}
}

// TestAPIKeyProcedures checks that a personal API key is accepted only on
// DevSessionService RPCs.
func TestAPIKeyProcedures(t *testing.T) {
	accepted := []string{
		tidev1connect.DevSessionServiceListDevSessionProfilesProcedure,
		tidev1connect.DevSessionServiceCreateDevSessionProcedure,
		tidev1connect.DevSessionServiceGetDevSessionProcedure,
		tidev1connect.DevSessionServiceListDevSessionsProcedure,
		tidev1connect.DevSessionServiceDeleteDevSessionProcedure,
		tidev1connect.DevSessionServiceExecDevSessionProcedure,
		tidev1connect.DevSessionServiceAttachDevSessionExecProcedure,
		tidev1connect.DevSessionServiceReadDevSessionFileProcedure,
		tidev1connect.DevSessionServiceWriteDevSessionFileProcedure,
	}
	rejected := []string{
		tidev1connect.OrganizationServiceCreateOrganizationProcedure,
		tidev1connect.OrganizationServiceGetOrganizationProcedure,
		tidev1connect.OrganizationServiceListOrganizationsProcedure,
		tidev1connect.OrganizationServiceDeleteOrganizationProcedure,
		tidev1connect.OrganizationServiceTransferOwnershipProcedure,
		tidev1connect.OrganizationServiceUpdateOrganizationSettingsProcedure,
		tidev1connect.MemberServiceCreateInvitationProcedure,
		tidev1connect.MemberServiceListInvitationsProcedure,
		tidev1connect.MemberServiceRevokeInvitationProcedure,
		tidev1connect.MemberServiceAcceptInvitationProcedure,
		tidev1connect.MemberServiceListMembersProcedure,
		tidev1connect.MemberServiceUpdateMemberRoleProcedure,
		tidev1connect.MemberServiceRemoveMemberProcedure,
		tidev1connect.ApiKeyServiceCreateApiKeyProcedure,
		tidev1connect.ApiKeyServiceListApiKeysProcedure,
		tidev1connect.ApiKeyServiceRevokeApiKeyProcedure,
		tidev1connect.AuditServiceListAuditEventsProcedure,
		tidev1connect.OwnSandLinkServiceSetOwnSandLinkProcedure,
		tidev1connect.OwnSandLinkServiceGetOwnSandLinkProcedure,
		tidev1connect.OwnSandLinkServiceRemoveOwnSandLinkProcedure,
		tidev1connect.GitHubServiceCreateGitHubInstallUrlProcedure,
		tidev1connect.GitHubServiceLinkGitHubInstallationProcedure,
		tidev1connect.GitHubServiceListGitHubInstallationsProcedure,
		tidev1connect.GitHubServiceListGitHubRunnerGroupsProcedure,
		tidev1connect.GitHubServiceUpdateGitHubInstallationProcedure,
		tidev1connect.GitHubServiceUnlinkGitHubInstallationProcedure,
		tidev1connect.AgentIntegrationServiceCreateAgentIntegrationProcedure,
		tidev1connect.AgentIntegrationServiceGetAgentIntegrationProcedure,
		tidev1connect.AgentIntegrationServiceListAgentIntegrationsProcedure,
		tidev1connect.AgentIntegrationServiceUpdateAgentIntegrationProcedure,
		tidev1connect.AgentIntegrationServiceDeleteAgentIntegrationProcedure,
		tidev1connect.RunnerProfileServiceCreateRunnerProfileProcedure,
		tidev1connect.RunnerProfileServiceGetRunnerProfileProcedure,
		tidev1connect.RunnerProfileServiceListRunnerProfilesProcedure,
		tidev1connect.RunnerProfileServiceUpdateRunnerProfileProcedure,
		tidev1connect.RunnerProfileServiceDeleteRunnerProfileProcedure,
		tidev1connect.WorkServiceListRequestsProcedure,
		tidev1connect.WorkServiceGetRequestProcedure,
		tidev1connect.WorkServiceCancelRequestProcedure,
		tidev1connect.WorkServiceListRunnersProcedure,
		tidev1connect.WorkServiceGetRunnerProcedure,
		tidev1connect.WorkServiceDeleteRunnerProcedure,
		tidev1connect.WorkServiceGetCapacityProcedure,
		tidev1connect.WorkServiceWatchQueueProcedure,
		// Look-alike paths.
		"/delino.tide.v1.DevSessionServiceX/CreateDevSession",
		"/delino.tide.v2.DevSessionService/CreateDevSession",
		"delino.tide.v1.DevSessionService/CreateDevSession",
		"",
	}
	for _, p := range accepted {
		if !APIKeyAcceptedForProcedure(p) {
			t.Errorf("%s: personal API key rejected", p)
		}
	}
	for _, p := range rejected {
		if APIKeyAcceptedForProcedure(p) {
			t.Errorf("%s: personal API key accepted", p)
		}
	}
}

func TestDevSessionOwnerChecks(t *testing.T) {
	if !CanOperateDevSession("u1", "u1") || CanOperateDevSession("u1", "u2") || CanOperateDevSession("", "") {
		t.Fatal("operate: only the session owner")
	}
	for _, tc := range []struct {
		role          tidev1.Role
		caller, owner string
		want          bool
	}{
		{owner, "u1", "u2", true},
		{admin, "u1", "u2", true},
		{member, "u1", "u2", false},
		{member, "u1", "u1", true},
		{tidev1.Role_ROLE_UNSPECIFIED, "u1", "u1", false},
	} {
		if got := CanDeleteDevSession(tc.role, tc.caller, tc.owner); got != tc.want {
			t.Errorf("CanDeleteDevSession(%v, %s, %s) = %v", tc.role, tc.caller, tc.owner, got)
		}
	}
}
