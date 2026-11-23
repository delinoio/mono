// SPDX-License-Identifier: Apache-2.0
import { Role } from "@delino/tide-protos/v1/common_pb";

/** Console actions that depend on the caller's role. */
export enum Permission {
  /** Read the organization, members, link state, profiles, work and audit. */
  ReadOrganization = "read_organization",
  ManageInvitations = "manage_invitations",
  ManageMembers = "manage_members",
  LeaveOrganization = "leave_organization",
  /** The OwnSand link, GitHub installations and agent integrations. */
  ManageIntegrations = "manage_integrations",
  ManageRunnerProfiles = "manage_runner_profiles",
  CancelRequests = "cancel_requests",
  DeleteRunners = "delete_runners",
  /** Delete every dev session of the organization, not only one's own. */
  DeleteAnyDevSession = "delete_any_dev_session",
  /** Own personal API keys. */
  ManageOwnApiKeys = "manage_own_api_keys",
  /** The per-user dev session limit. */
  ChangeSettings = "change_settings",
  TransferOwnership = "transfer_ownership",
  DeleteOrganization = "delete_organization",
}

const ADMIN: ReadonlySet<Role> = new Set([Role.OWNER, Role.ADMIN]);
const ANY: ReadonlySet<Role> = new Set([Role.OWNER, Role.ADMIN, Role.MEMBER]);

/**
 * The authorization matrix of the control plane contract, used to hide
 * actions. The control plane still enforces every permission.
 */
export function can(role: Role | undefined, permission: Permission): boolean {
  if (role === undefined) return false;
  switch (permission) {
    case Permission.ReadOrganization:
    case Permission.ManageOwnApiKeys:
      return ANY.has(role);
    case Permission.LeaveOrganization:
      return role === Role.ADMIN || role === Role.MEMBER;
    case Permission.TransferOwnership:
    case Permission.DeleteOrganization:
      return role === Role.OWNER;
    case Permission.ManageInvitations:
    case Permission.ManageMembers:
    case Permission.ManageIntegrations:
    case Permission.ManageRunnerProfiles:
    case Permission.CancelRequests:
    case Permission.DeleteRunners:
    case Permission.DeleteAnyDevSession:
    case Permission.ChangeSettings:
      return ADMIN.has(role);
  }
}

/**
 * Whether the caller may delete a dev session: the session owner (the user
 * who created it) always, and an `owner` or `admin` of the organization for
 * every session.
 */
export function canDeleteDevSession(
  callerRole: Role | undefined,
  callerUserId: string | undefined,
  session: { ownerUserId: string },
): boolean {
  if (callerUserId !== undefined && session.ownerUserId === callerUserId) {
    return callerRole !== undefined;
  }
  return can(callerRole, Permission.DeleteAnyDevSession);
}

/**
 * Whether the caller may change the role of, or remove, another member. An
 * admin cannot change or remove the owner, and nobody changes their own
 * role here (leaving is a separate action).
 */
export function canManageMember(
  callerRole: Role | undefined,
  callerUserId: string | undefined,
  target: { userId: string; role: Role },
): boolean {
  if (!can(callerRole, Permission.ManageMembers)) return false;
  if (target.userId === callerUserId) return false;
  return target.role !== Role.OWNER;
}
