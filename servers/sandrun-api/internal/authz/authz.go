// SPDX-License-Identifier: FSL-1.1-ALv2

// Package authz holds the authorization matrix of sandrun-api. The owning
// contract is the Authorization section of
// docs/servers-sandrun-api-foundation.md. Every layer of sandrun-api authorizes
// with these actions through api.Service.Authorize, so a role rule exists in
// one place only.
package authz

import (
	"strings"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// Action is one row of the authorization matrix.
type Action int

const (
	ActionUnspecified Action = iota
	// ActionOwnerOnly: delete the organization, transfer ownership.
	ActionOwnerOnly
	// ActionManageSettings: organization settings (the per-user dev session
	// limit).
	ActionManageSettings
	// ActionManageMembers: invitations, member roles, removal of another
	// member.
	ActionManageMembers
	// ActionLeave: remove the caller's own membership.
	ActionLeave
	// ActionManageIntegrations: the OwnSand link, GitHub installations,
	// agent integrations and their credentials.
	ActionManageIntegrations
	// ActionManageRunnerProfiles: create, change and delete runner profiles.
	ActionManageRunnerProfiles
	// ActionDeleteRunner: delete a runner.
	ActionDeleteRunner
	// ActionCancelRequest: cancel a request.
	ActionCancelRequest
	// ActionRead: read the organization, members, the link and integration
	// state (never credentials), runner profiles, requests, runners,
	// capacity and audit events.
	ActionRead
	// ActionManageOwnAPIKeys: create, list and revoke the caller's own
	// personal API keys. A personal API key can never do this.
	ActionManageOwnAPIKeys
	// ActionUseDevSessions: create a dev session, list dev session profiles
	// and read the metadata of every dev session of the organization.
	ActionUseDevSessions
	// ActionOperateDevSession: run commands and transfer files in a dev
	// session. The role check passes for every member; the caller must also
	// be the session owner (CanOperateDevSession).
	ActionOperateDevSession
	// ActionDeleteDevSession: delete a dev session. The role check passes
	// for every member; a member can delete only an own session
	// (CanDeleteDevSession).
	ActionDeleteDevSession
)

// AllActions lists every action, for exhaustive tests.
func AllActions() []Action {
	return []Action{
		ActionOwnerOnly, ActionManageSettings, ActionManageMembers, ActionLeave,
		ActionManageIntegrations, ActionManageRunnerProfiles, ActionDeleteRunner, ActionCancelRequest,
		ActionRead, ActionManageOwnAPIKeys,
		ActionUseDevSessions, ActionOperateDevSession, ActionDeleteDevSession,
	}
}

// RoleAllowed reports whether a member with role may perform action.
func RoleAllowed(role sandrunv1.Role, action Action) bool {
	owner := role == sandrunv1.Role_ROLE_OWNER
	admin := role == sandrunv1.Role_ROLE_ADMIN
	member := role == sandrunv1.Role_ROLE_MEMBER
	switch action {
	case ActionOwnerOnly:
		return owner
	case ActionManageSettings, ActionManageMembers, ActionManageIntegrations,
		ActionManageRunnerProfiles, ActionDeleteRunner, ActionCancelRequest:
		return owner || admin
	case ActionLeave:
		// The owner must transfer ownership before leaving.
		return admin || member
	case ActionRead, ActionManageOwnAPIKeys, ActionUseDevSessions,
		ActionOperateDevSession, ActionDeleteDevSession:
		return owner || admin || member
	}
	return false
}

// APIKeyAllowed reports whether a personal API key may perform action (in
// addition to the role check of its user). A personal API key can call only
// the dev session rows of the matrix.
func APIKeyAllowed(action Action) bool {
	switch action {
	case ActionUseDevSessions, ActionOperateDevSession, ActionDeleteDevSession:
		return true
	}
	return false
}

// APIKeyAcceptedForProcedure reports whether an RPC accepts a personal API
// key: only the RPCs of delino.sandrun.v1.DevSessionService do. The
// authentication interceptor rejects a personal API key on every other RPC
// with permission_denied.
func APIKeyAcceptedForProcedure(procedure string) bool {
	return strings.HasPrefix(procedure, "/"+sandrunv1connect.DevSessionServiceName+"/")
}

// CanOperateDevSession reports whether the caller may run commands and
// transfer files in a dev session of ownerUserID: only the session owner,
// whatever the organization role.
func CanOperateDevSession(callerUserID, ownerUserID string) bool {
	return callerUserID != "" && callerUserID == ownerUserID
}

// CanDeleteDevSession reports whether a caller with role may delete a dev
// session of ownerUserID: an owner or admin any session, a member only an
// own session.
func CanDeleteDevSession(role sandrunv1.Role, callerUserID, ownerUserID string) bool {
	switch role {
	case sandrunv1.Role_ROLE_OWNER, sandrunv1.Role_ROLE_ADMIN:
		return true
	case sandrunv1.Role_ROLE_MEMBER:
		return CanOperateDevSession(callerUserID, ownerUserID)
	}
	return false
}
