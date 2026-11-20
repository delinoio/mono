// SPDX-License-Identifier: Apache-2.0
import { Role } from "@delino/ownsand-sdk/gen/common_pb";

/**
 * Role-based visibility in the console. The console only hides actions that
 * the caller's role does not allow; the control plane enforces every
 * permission again (docs/servers-ownsand-api-foundation.md#authorization).
 */

/** Stable role identifiers of the project (`owner`, `admin`, `member`). */
export enum ConsoleRole {
  Owner = "owner",
  Admin = "admin",
  Member = "member",
}

/** Actions and views whose visibility depends on the caller's role. */
export enum Permission {
  ViewOrganization = "view-organization",
  ViewSandboxes = "view-sandboxes",
  ViewDevices = "view-devices",
  ViewMembers = "view-members",
  ViewAgentVersionPolicy = "view-agent-version-policy",
  ManageInvitations = "manage-invitations",
  ManageMembers = "manage-members",
  LeaveOrganization = "leave-organization",
  ManageEnrollmentTokens = "manage-enrollment-tokens",
  ManageDevices = "manage-devices",
  ManageApiKeys = "manage-api-keys",
  ChangeAgentVersionPolicy = "change-agent-version-policy",
  TransferOwnership = "transfer-ownership",
  DeleteOrganization = "delete-organization",
}

const MEMBER_PERMISSIONS: readonly Permission[] = [
  Permission.ViewOrganization,
  Permission.ViewSandboxes,
  Permission.ViewDevices,
  Permission.ViewMembers,
  Permission.ViewAgentVersionPolicy,
  Permission.LeaveOrganization,
];

const ADMIN_PERMISSIONS: readonly Permission[] = [
  ...MEMBER_PERMISSIONS,
  Permission.ManageInvitations,
  Permission.ManageMembers,
  Permission.ManageEnrollmentTokens,
  Permission.ManageDevices,
  Permission.ManageApiKeys,
  Permission.ChangeAgentVersionPolicy,
];

const OWNER_PERMISSIONS: readonly Permission[] = [
  // The owner cannot leave without transferring ownership first.
  ...ADMIN_PERMISSIONS.filter((p) => p !== Permission.LeaveOrganization),
  Permission.TransferOwnership,
  Permission.DeleteOrganization,
];

const PERMISSIONS: Readonly<Record<ConsoleRole, ReadonlySet<Permission>>> = {
  [ConsoleRole.Owner]: new Set(OWNER_PERMISSIONS),
  [ConsoleRole.Admin]: new Set(ADMIN_PERMISSIONS),
  [ConsoleRole.Member]: new Set(MEMBER_PERMISSIONS),
};

/** Maps the protobuf role; `ROLE_UNSPECIFIED` and unknown values map to `undefined`. */
export function fromProtoRole(role: Role | undefined): ConsoleRole | undefined {
  switch (role) {
    case Role.OWNER:
      return ConsoleRole.Owner;
    case Role.ADMIN:
      return ConsoleRole.Admin;
    case Role.MEMBER:
      return ConsoleRole.Member;
    default:
      return undefined;
  }
}

/** Maps a console role back to the protobuf role. */
export function toProtoRole(role: ConsoleRole): Role {
  switch (role) {
    case ConsoleRole.Owner:
      return Role.OWNER;
    case ConsoleRole.Admin:
      return Role.ADMIN;
    case ConsoleRole.Member:
      return Role.MEMBER;
  }
}

/** True when `role` allows `permission`. An unknown role allows nothing. */
export function can(
  role: ConsoleRole | undefined,
  permission: Permission,
): boolean {
  return role !== undefined && PERMISSIONS[role].has(permission);
}

/** Display label of a role. */
export function roleLabel(role: ConsoleRole | undefined): string {
  switch (role) {
    case ConsoleRole.Owner:
      return "Owner";
    case ConsoleRole.Admin:
      return "Admin";
    case ConsoleRole.Member:
      return "Member";
    default:
      return "Unknown";
  }
}

/**
 * True when the caller may change the role of, or remove, another member.
 * An admin cannot change or remove the owner, and nobody uses these actions
 * on their own membership (leaving is a separate action).
 */
export function canManageMember(
  callerRole: ConsoleRole | undefined,
  callerUserId: string | undefined,
  target: { readonly userId: string; readonly role: ConsoleRole | undefined },
): boolean {
  if (!can(callerRole, Permission.ManageMembers)) {
    return false;
  }
  if (callerUserId !== undefined && target.userId === callerUserId) {
    return false;
  }
  return target.role !== ConsoleRole.Owner;
}

/**
 * Roles that the caller may grant through invitations or role changes. Only
 * `TransferOwnership` makes someone the owner, so `owner` is never offered.
 */
export function grantableRoles(
  callerRole: ConsoleRole | undefined,
): ConsoleRole[] {
  return can(callerRole, Permission.ManageMembers)
    ? [ConsoleRole.Member, ConsoleRole.Admin]
    : [];
}
