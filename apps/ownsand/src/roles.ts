// SPDX-License-Identifier: Apache-2.0
import { Role } from "ownsand-sdk/gen/common_pb";

/** Console actions that depend on the caller's role. */
export enum Permission {
  ReadOrganization = "read_organization",
  ManageInvitations = "manage_invitations",
  ManageMembers = "manage_members",
  LeaveOrganization = "leave_organization",
  ManageDevices = "manage_devices",
  ManageEnrollmentTokens = "manage_enrollment_tokens",
  ManageApiKeys = "manage_api_keys",
  ChangeAgentVersionPolicy = "change_agent_version_policy",
  TransferOwnership = "transfer_ownership",
  DeleteOrganization = "delete_organization",
  UseSandboxes = "use_sandboxes",
  ViewBilling = "view_billing",
  ManageBillingLink = "manage_billing_link",
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
    case Permission.UseSandboxes:
      return ANY.has(role);
    case Permission.LeaveOrganization:
      return role === Role.ADMIN || role === Role.MEMBER;
    case Permission.TransferOwnership:
    case Permission.DeleteOrganization:
    case Permission.ManageBillingLink:
      return role === Role.OWNER;
    case Permission.ManageInvitations:
    case Permission.ManageMembers:
    case Permission.ManageDevices:
    case Permission.ManageEnrollmentTokens:
    case Permission.ManageApiKeys:
    case Permission.ChangeAgentVersionPolicy:
    case Permission.ViewBilling:
      return ADMIN.has(role);
  }
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
