// SPDX-License-Identifier: FSL-1.1-ALv2

package authz

import (
	"testing"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

const (
	owner  = sandrunv1.Role_ROLE_OWNER
	admin  = sandrunv1.Role_ROLE_ADMIN
	member = sandrunv1.Role_ROLE_MEMBER
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
		if RoleAllowed(sandrunv1.Role_ROLE_UNSPECIFIED, a) {
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
		sandrunv1connect.DevSessionServiceListDevSessionProfilesProcedure,
		sandrunv1connect.DevSessionServiceCreateDevSessionProcedure,
		sandrunv1connect.DevSessionServiceGetDevSessionProcedure,
		sandrunv1connect.DevSessionServiceListDevSessionsProcedure,
		sandrunv1connect.DevSessionServiceDeleteDevSessionProcedure,
		sandrunv1connect.DevSessionServiceExecDevSessionProcedure,
		sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure,
		sandrunv1connect.DevSessionServiceReadDevSessionFileProcedure,
		sandrunv1connect.DevSessionServiceWriteDevSessionFileProcedure,
	}
	rejected := []string{
		sandrunv1connect.OrganizationServiceCreateOrganizationProcedure,
		sandrunv1connect.OrganizationServiceGetOrganizationProcedure,
		sandrunv1connect.OrganizationServiceListOrganizationsProcedure,
		sandrunv1connect.OrganizationServiceDeleteOrganizationProcedure,
		sandrunv1connect.OrganizationServiceTransferOwnershipProcedure,
		sandrunv1connect.OrganizationServiceUpdateOrganizationSettingsProcedure,
		sandrunv1connect.MemberServiceCreateInvitationProcedure,
		sandrunv1connect.MemberServiceListInvitationsProcedure,
		sandrunv1connect.MemberServiceRevokeInvitationProcedure,
		sandrunv1connect.MemberServiceAcceptInvitationProcedure,
		sandrunv1connect.MemberServiceListMembersProcedure,
		sandrunv1connect.MemberServiceUpdateMemberRoleProcedure,
		sandrunv1connect.MemberServiceRemoveMemberProcedure,
		sandrunv1connect.ApiKeyServiceCreateApiKeyProcedure,
		sandrunv1connect.ApiKeyServiceListApiKeysProcedure,
		sandrunv1connect.ApiKeyServiceRevokeApiKeyProcedure,
		sandrunv1connect.AuditServiceListAuditEventsProcedure,
		sandrunv1connect.OwnSandLinkServiceSetOwnSandLinkProcedure,
		sandrunv1connect.OwnSandLinkServiceGetOwnSandLinkProcedure,
		sandrunv1connect.OwnSandLinkServiceRemoveOwnSandLinkProcedure,
		sandrunv1connect.GitHubServiceCreateGitHubInstallUrlProcedure,
		sandrunv1connect.GitHubServiceLinkGitHubInstallationProcedure,
		sandrunv1connect.GitHubServiceListGitHubInstallationsProcedure,
		sandrunv1connect.GitHubServiceListGitHubRunnerGroupsProcedure,
		sandrunv1connect.GitHubServiceUpdateGitHubInstallationProcedure,
		sandrunv1connect.GitHubServiceUnlinkGitHubInstallationProcedure,
		sandrunv1connect.AgentIntegrationServiceCreateAgentIntegrationProcedure,
		sandrunv1connect.AgentIntegrationServiceGetAgentIntegrationProcedure,
		sandrunv1connect.AgentIntegrationServiceListAgentIntegrationsProcedure,
		sandrunv1connect.AgentIntegrationServiceUpdateAgentIntegrationProcedure,
		sandrunv1connect.AgentIntegrationServiceDeleteAgentIntegrationProcedure,
		sandrunv1connect.RunnerProfileServiceCreateRunnerProfileProcedure,
		sandrunv1connect.RunnerProfileServiceGetRunnerProfileProcedure,
		sandrunv1connect.RunnerProfileServiceListRunnerProfilesProcedure,
		sandrunv1connect.RunnerProfileServiceUpdateRunnerProfileProcedure,
		sandrunv1connect.RunnerProfileServiceDeleteRunnerProfileProcedure,
		sandrunv1connect.WorkServiceListRequestsProcedure,
		sandrunv1connect.WorkServiceGetRequestProcedure,
		sandrunv1connect.WorkServiceCancelRequestProcedure,
		sandrunv1connect.WorkServiceListRunnersProcedure,
		sandrunv1connect.WorkServiceGetRunnerProcedure,
		sandrunv1connect.WorkServiceDeleteRunnerProcedure,
		sandrunv1connect.WorkServiceGetCapacityProcedure,
		sandrunv1connect.WorkServiceWatchQueueProcedure,
		// Look-alike paths.
		"/delino.sandrun.v1.DevSessionServiceX/CreateDevSession",
		"/delino.sandrun.v2.DevSessionService/CreateDevSession",
		"delino.sandrun.v1.DevSessionService/CreateDevSession",
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
		role          sandrunv1.Role
		caller, owner string
		want          bool
	}{
		{owner, "u1", "u2", true},
		{admin, "u1", "u2", true},
		{member, "u1", "u2", false},
		{member, "u1", "u1", true},
		{sandrunv1.Role_ROLE_UNSPECIFIED, "u1", "u1", false},
	} {
		if got := CanDeleteDevSession(tc.role, tc.caller, tc.owner); got != tc.want {
			t.Errorf("CanDeleteDevSession(%v, %s, %s) = %v", tc.role, tc.caller, tc.owner, got)
		}
	}
}
