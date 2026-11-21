// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"

// Role is the role of a member in a billing account, as the database stores
// it.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Proto returns the protobuf value of the role.
func (r Role) Proto() accountsv1.BillingAccountRole {
	switch r {
	case RoleOwner:
		return accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER
	case RoleAdmin:
		return accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN
	case RoleViewer:
		return accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER
	default:
		return accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_UNSPECIFIED
	}
}

// grantableRole returns the role of an invitation or a role change: ADMIN or
// VIEWER. OWNER and UNSPECIFIED are not grantable.
func grantableRole(p accountsv1.BillingAccountRole) (Role, bool) {
	switch p {
	case accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN:
		return RoleAdmin, true
	case accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER:
		return RoleViewer, true
	default:
		return "", false
	}
}

// Permission is an action of the role table in
// docs/servers-accounts-api-foundation.md (Billing accounts and members).
type Permission int

const (
	// PermissionRead reads the account, its members, balance and settings.
	PermissionRead Permission = iota + 1
	// PermissionManage manages members, invitations, the name, payments and
	// organization links.
	PermissionManage
	// PermissionOwn deletes the account and transfers ownership.
	PermissionOwn
)

// Allows reports whether the role may perform the action.
func (r Role) Allows(p Permission) bool {
	switch p {
	case PermissionRead:
		return r == RoleOwner || r == RoleAdmin || r == RoleViewer
	case PermissionManage:
		return r == RoleOwner || r == RoleAdmin
	case PermissionOwn:
		return r == RoleOwner
	default:
		return false
	}
}
