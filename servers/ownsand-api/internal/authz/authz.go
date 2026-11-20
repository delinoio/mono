// SPDX-License-Identifier: FSL-1.1-ALv2

// Package authz holds the authorization matrix of the control plane. The
// owning contract is the Authorization section of
// docs/servers-ownsand-api-foundation.md.
package authz

import ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"

// Action is one row of the authorization matrix.
type Action int

const (
	ActionUnspecified Action = iota
	// ActionOwnerOnly: delete the organization, transfer ownership.
	ActionOwnerOnly
	// ActionManageMembers: invitations, member roles, removal of another
	// member.
	ActionManageMembers
	// ActionLeave: remove the caller's own membership.
	ActionLeave
	// ActionManageDevices: enrollment tokens, device removal, device
	// limits, agent version policy changes.
	ActionManageDevices
	// ActionManageAPIKeys: API keys.
	ActionManageAPIKeys
	// ActionReadOrganization: read the organization, list members.
	ActionReadOrganization
	// ActionReadDevices: list and read devices and the agent version policy.
	ActionReadDevices
	// ActionUseSandboxes: sandboxes, exec, files, pause, resume, snapshots,
	// templates, data path sessions, relay authorization and relay RPCs.
	ActionUseSandboxes
)

// AllActions lists every action, for exhaustive tests.
func AllActions() []Action {
	return []Action{
		ActionOwnerOnly, ActionManageMembers, ActionLeave, ActionManageDevices, ActionManageAPIKeys,
		ActionReadOrganization, ActionReadDevices, ActionUseSandboxes,
	}
}

// RoleAllowed reports whether a member with role may perform action.
func RoleAllowed(role ownsandv1.Role, action Action) bool {
	switch action {
	case ActionOwnerOnly:
		return role == ownsandv1.Role_ROLE_OWNER
	case ActionManageMembers, ActionManageDevices, ActionManageAPIKeys:
		return role == ownsandv1.Role_ROLE_OWNER || role == ownsandv1.Role_ROLE_ADMIN
	case ActionLeave:
		// The owner must transfer ownership before leaving.
		return role == ownsandv1.Role_ROLE_ADMIN || role == ownsandv1.Role_ROLE_MEMBER
	case ActionReadOrganization, ActionReadDevices, ActionUseSandboxes:
		return role == ownsandv1.Role_ROLE_OWNER || role == ownsandv1.Role_ROLE_ADMIN || role == ownsandv1.Role_ROLE_MEMBER
	}
	return false
}

// APIKeyAllowed reports whether an API key of the organization may perform
// action.
func APIKeyAllowed(action Action) bool {
	return action == ActionReadDevices || action == ActionUseSandboxes
}
