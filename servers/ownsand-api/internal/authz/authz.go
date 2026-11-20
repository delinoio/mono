// SPDX-License-Identifier: FSL-1.1-ALv2

// Package authz holds the OwnSand authorization matrix and the access rule
// of every RPC. It is pure logic: the handlers load the caller's membership
// and pass it in. Contract: docs/servers-ownsand-api-foundation.md#authorization.
package authz

import (
	"errors"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

// Action is one row of the authorization matrix.
type Action int

const (
	ActionUnspecified Action = iota
	// Delete the organization, transfer ownership.
	ActionDeleteOrganization
	ActionTransferOwnership
	// Invitations, member roles, removal of another member.
	ActionManageMembers
	// Remove the caller's own membership.
	ActionLeaveOrganization
	// Enrollment tokens, device removal, device limits, agent version policy changes.
	ActionManageDevices
	// API keys.
	ActionManageAPIKeys
	// Read the organization, list members.
	ActionReadOrganization
	// List and read devices and the agent version policy.
	ActionReadDevices
	// Sandboxes, exec, files, pause, resume, snapshots, templates, data path
	// sessions, relay authorization and relay RPCs.
	ActionUseSandboxes
)

// Access is how a procedure authenticates its caller.
type Access int

const (
	AccessUnspecified Access = iota
	// Takes an organization_id; a user or API key credential; the matrix applies.
	AccessOrganization
	// Takes no organization_id; only a Logto access token, no membership needed.
	AccessUserOnly
	// AgentService RPCs that authenticate with the request body (no bearer).
	AccessAgentUnauthenticated
	// The agent channel and the agent relay connection: only an osd_ credential.
	AccessDevice
)

// Rule is the access rule of one procedure.
type Rule struct {
	Access Access
	Action Action
}

// Procedures maps every Connect procedure that the server serves to its rule.
// MemberService.RemoveMember is listed with ActionManageMembers; the handler
// uses ActionLeaveOrganization when the target is the caller.
var Procedures = map[string]Rule{
	ownsandv1connect.OrganizationServiceCreateOrganizationProcedure: {AccessUserOnly, ActionUnspecified},
	ownsandv1connect.OrganizationServiceListOrganizationsProcedure:  {AccessUserOnly, ActionUnspecified},
	ownsandv1connect.OrganizationServiceGetOrganizationProcedure:    {AccessOrganization, ActionReadOrganization},
	ownsandv1connect.OrganizationServiceDeleteOrganizationProcedure: {AccessOrganization, ActionDeleteOrganization},
	ownsandv1connect.OrganizationServiceTransferOwnershipProcedure:  {AccessOrganization, ActionTransferOwnership},

	ownsandv1connect.MemberServiceCreateInvitationProcedure: {AccessOrganization, ActionManageMembers},
	ownsandv1connect.MemberServiceListInvitationsProcedure:  {AccessOrganization, ActionManageMembers},
	ownsandv1connect.MemberServiceRevokeInvitationProcedure: {AccessOrganization, ActionManageMembers},
	ownsandv1connect.MemberServiceAcceptInvitationProcedure: {AccessUserOnly, ActionUnspecified},
	ownsandv1connect.MemberServiceListMembersProcedure:      {AccessOrganization, ActionReadOrganization},
	ownsandv1connect.MemberServiceUpdateMemberRoleProcedure: {AccessOrganization, ActionManageMembers},
	ownsandv1connect.MemberServiceRemoveMemberProcedure:     {AccessOrganization, ActionManageMembers},

	ownsandv1connect.ApiKeyServiceCreateApiKeyProcedure: {AccessOrganization, ActionManageAPIKeys},
	ownsandv1connect.ApiKeyServiceListApiKeysProcedure:  {AccessOrganization, ActionManageAPIKeys},
	ownsandv1connect.ApiKeyServiceRevokeApiKeyProcedure: {AccessOrganization, ActionManageAPIKeys},

	ownsandv1connect.DeviceServiceCreateEnrollmentTokenProcedure:    {AccessOrganization, ActionManageDevices},
	ownsandv1connect.DeviceServiceListEnrollmentTokensProcedure:     {AccessOrganization, ActionManageDevices},
	ownsandv1connect.DeviceServiceRevokeEnrollmentTokenProcedure:    {AccessOrganization, ActionManageDevices},
	ownsandv1connect.DeviceServiceListDevicesProcedure:              {AccessOrganization, ActionReadDevices},
	ownsandv1connect.DeviceServiceGetDeviceProcedure:                {AccessOrganization, ActionReadDevices},
	ownsandv1connect.DeviceServiceUpdateDeviceLimitsProcedure:       {AccessOrganization, ActionManageDevices},
	ownsandv1connect.DeviceServiceRemoveDeviceProcedure:             {AccessOrganization, ActionManageDevices},
	ownsandv1connect.DeviceServiceGetAgentVersionPolicyProcedure:    {AccessOrganization, ActionReadDevices},
	ownsandv1connect.DeviceServiceUpdateAgentVersionPolicyProcedure: {AccessOrganization, ActionManageDevices},

	ownsandv1connect.SandboxServiceCreateSandboxProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SandboxServiceGetSandboxProcedure:    {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SandboxServiceListSandboxesProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SandboxServiceDeleteSandboxProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SandboxServicePauseSandboxProcedure:  {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SandboxServiceResumeSandboxProcedure: {AccessOrganization, ActionUseSandboxes},

	ownsandv1connect.SnapshotServiceCreateSnapshotProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceGetSnapshotProcedure:    {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceListSnapshotsProcedure:  {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceDeleteSnapshotProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceExportSnapshotProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceCreateTemplateProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceListTemplatesProcedure:  {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.SnapshotServiceDeleteTemplateProcedure: {AccessOrganization, ActionUseSandboxes},

	ownsandv1connect.DataPathServiceGetIceServersProcedure:  {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.DataPathServiceOpenSessionProcedure:    {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.DataPathServiceAuthorizeRelayProcedure: {AccessOrganization, ActionUseSandboxes},

	ownsandv1connect.RelayServiceExecProcedure:       {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.RelayServiceAttachExecProcedure: {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.RelayServiceReadFileProcedure:   {AccessOrganization, ActionUseSandboxes},
	ownsandv1connect.RelayServiceWriteFileProcedure:  {AccessOrganization, ActionUseSandboxes},

	ownsandv1connect.AgentServiceEnrollProcedure:          {AccessAgentUnauthenticated, ActionUnspecified},
	ownsandv1connect.AgentServiceCreateChallengeProcedure: {AccessAgentUnauthenticated, ActionUnspecified},
	ownsandv1connect.AgentServiceAuthenticateProcedure:    {AccessAgentUnauthenticated, ActionUnspecified},
	ownsandv1connect.AgentServiceConfirmRemovalProcedure:  {AccessAgentUnauthenticated, ActionUnspecified},
	ownsandv1connect.AgentServiceChannelProcedure:         {AccessDevice, ActionUnspecified},

	ownsandv1connect.RelayAgentServiceConnectProcedure: {AccessDevice, ActionUnspecified},
}

// RoleAllows reports whether a member role may perform the action.
func RoleAllows(role ownsandv1.Role, action Action) bool {
	switch action {
	case ActionDeleteOrganization, ActionTransferOwnership:
		return role == ownsandv1.Role_ROLE_OWNER
	case ActionManageMembers, ActionManageDevices, ActionManageAPIKeys:
		return role == ownsandv1.Role_ROLE_OWNER || role == ownsandv1.Role_ROLE_ADMIN
	case ActionLeaveOrganization:
		// The owner must transfer ownership before leaving.
		return role == ownsandv1.Role_ROLE_ADMIN || role == ownsandv1.Role_ROLE_MEMBER
	case ActionReadOrganization, ActionReadDevices, ActionUseSandboxes:
		return role == ownsandv1.Role_ROLE_OWNER || role == ownsandv1.Role_ROLE_ADMIN || role == ownsandv1.Role_ROLE_MEMBER
	default:
		return false
	}
}

// APIKeyAllows reports whether an API key of the organization may perform the
// action.
func APIKeyAllows(action Action) bool {
	return action == ActionReadDevices || action == ActionUseSandboxes
}

// Errors of Decide. A caller outside the organization gets not_found so that
// it cannot discover organizations or their entities.
var (
	ErrNotFound         = connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
	ErrPermissionDenied = connect.NewError(connect.CodePermissionDenied, errors.New("the caller is not allowed to perform this action"))
)

// Membership is the caller's membership in the requested organization, or
// nil when the user has none.
type Membership struct {
	MembershipID string
	Role         ownsandv1.Role
}

// Decide authorizes an action on organizationID. membership is the user's
// membership in that organization (nil when absent; ignored for API keys).
func Decide(p auth.Principal, organizationID string, membership *Membership, action Action) error {
	switch p.Kind {
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER:
		if membership == nil {
			return ErrNotFound
		}
		if !RoleAllows(membership.Role, action) {
			return ErrPermissionDenied
		}
		return nil
	case ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY:
		// The key's organization is checked first: a key of another
		// organization gets not_found even for actions keys may never do.
		if p.OrganizationID != organizationID {
			return ErrNotFound
		}
		if !APIKeyAllows(action) {
			return ErrPermissionDenied
		}
		return nil
	default:
		return ErrPermissionDenied
	}
}

// CheckAccess applies the procedure's access rule to the principal kind,
// before any handler code runs. ok is false for an unknown procedure.
func CheckAccess(procedure string, kind ownsandv1.PrincipalKind) (Rule, error) {
	rule, ok := Procedures[procedure]
	if !ok {
		return Rule{}, connect.NewError(connect.CodeUnimplemented, errors.New("unknown procedure"))
	}
	switch rule.Access {
	case AccessAgentUnauthenticated:
		return rule, nil
	case AccessDevice:
		if kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE {
			return rule, ErrPermissionDenied
		}
	case AccessUserOnly:
		if kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER {
			return rule, ErrPermissionDenied
		}
	case AccessOrganization:
		if kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER && kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY {
			return rule, ErrPermissionDenied
		}
	default:
		return rule, ErrPermissionDenied
	}
	return rule, nil
}
