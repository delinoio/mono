// SPDX-License-Identifier: Apache-2.0
import { BillingAccountRole } from "./gen/delino/accounts/v1/billing_account_pb.ts";

/**
 * Actions that the pages show or hide by the caller's role, according to the
 * role table of docs/servers-accounts-api-foundation.md. The API server still
 * enforces every permission; this only keeps the pages from offering actions
 * that would fail.
 */
export enum Permission {
  /** Delete the billing account. */
  DeleteAccount = "delete-account",
  /** Make another member the owner. */
  TransferOwnership = "transfer-ownership",
  /** Subscription, manual and automatic top-up and alerts. */
  ManagePayments = "manage-payments",
  /** Invitations, roles and removal of admins and viewers, renaming. */
  ManageMembers = "manage-members",
  /** Invoices and invoice links; they carry the payer's address. */
  ReadInvoices = "read-invoices",
  /** Remove oneself from the billing account. */
  Leave = "leave",
}

const OWNER_ADMIN: ReadonlySet<BillingAccountRole> = new Set([
  BillingAccountRole.OWNER,
  BillingAccountRole.ADMIN,
]);

/** Whether a role allows an action. An unknown role allows nothing. */
export function can(
  role: BillingAccountRole | undefined,
  permission: Permission,
): boolean {
  if (role === undefined) return false;
  switch (permission) {
    case Permission.DeleteAccount:
    case Permission.TransferOwnership:
      return role === BillingAccountRole.OWNER;
    case Permission.ManagePayments:
    case Permission.ManageMembers:
    case Permission.ReadInvoices:
      return OWNER_ADMIN.has(role);
    case Permission.Leave:
      return (
        role === BillingAccountRole.ADMIN || role === BillingAccountRole.VIEWER
      );
  }
}

/**
 * Whether the caller may change the role of, or remove, a member: the owner
 * and admins manage admins and viewers. Nobody manages the owner row.
 */
export function canManageMember(
  callerRole: BillingAccountRole | undefined,
  memberRole: BillingAccountRole,
): boolean {
  return (
    can(callerRole, Permission.ManageMembers) &&
    (memberRole === BillingAccountRole.ADMIN ||
      memberRole === BillingAccountRole.VIEWER)
  );
}
