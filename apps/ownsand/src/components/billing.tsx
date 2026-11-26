// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import {
  BillingPhase,
  BillingService,
  type OrganizationBilling,
} from "@delino/ownsand-protos/v1/billing_pb";
import { BillingReason, Role } from "@delino/ownsand-protos/v1/common_pb";
import { ACCOUNTS_URL } from "../config.ts";
import { formatTime } from "../format.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import { showsFullError } from "./ui.tsx";

/** Why the organization is blocked, in plain words. */
function reasonText(billing: OrganizationBilling): string {
  const unpaid =
    "the linked billing account has no active subscription, or its balance is $0.00 or less";
  switch (billing.reason) {
    case BillingReason.ACCOUNT_REQUIRED:
      return "No billing account is linked to this organization.";
    case BillingReason.BLOCKED:
      return `Payment is missing: ${unpaid}.`;
    case BillingReason.SUSPENDED:
      return billing.linked
        ? `Payment has been missing for 3 days: ${unpaid}.`
        : "No billing account has been linked for 3 days.";
    default:
      return "Billing does not allow new resources.";
  }
}

const BLOCKED_ACTIONS =
  "creating sandboxes, snapshots, templates and enrollment tokens, resuming sandboxes, exporting snapshots, and enrolling a device even with an existing enrollment token";

/**
 * The billing banner of every organization page. Every member sees it while
 * the organization is `BLOCKED` or `SUSPENDED`. It reads `GetBillingStatus`,
 * which the console reads again after any call fails with a billing reason
 * (`watchBillingErrors` in `src/api.ts`).
 */
export function BillingBanner({
  organizationId,
  role,
}: {
  organizationId: string;
  role: Role;
}) {
  const status = useQuery(BillingService.method.getBillingStatus, {
    organizationId,
  });
  const billing = status.data?.billing;
  if (
    showsFullError(status) ||
    billing === undefined ||
    (billing.phase !== BillingPhase.BLOCKED &&
      billing.phase !== BillingPhase.SUSPENDED)
  ) {
    return null;
  }
  const suspended = billing.phase === BillingPhase.SUSPENDED;
  return (
    <section
      className="billing-banner"
      data-phase={suspended ? "suspended" : "blocked"}
      aria-labelledby="billing-banner-title"
    >
      <h2 id="billing-banner-title">
        {suspended
          ? "This organization is suspended"
          : "This organization is blocked"}
      </h2>
      <p>{reasonText(billing)}</p>
      {suspended ? (
        <p>
          Running sandboxes are paused. OwnSand rejects {BLOCKED_ACTIONS}, and
          also exec, file transfers, sessions and relay connections. Reads and
          deletions still work.
          {billing.suspendTime
            ? ` Suspended since ${formatTime(billing.suspendTime)}.`
            : null}
        </p>
      ) : (
        <p>
          OwnSand rejects {BLOCKED_ACTIONS}. Running sandboxes, exec, reads and
          deletions still work.
          {billing.suspendTime
            ? ` Unless billing is fixed, the organization is suspended on ${formatTime(billing.suspendTime)} and its running sandboxes are paused.`
            : null}
        </p>
      )}
      <p className="billing-banner-actions">
        {billing.linked ? null : role === Role.OWNER ? (
          <Link to={routePath({ id: RouteId.Settings, organizationId })}>
            Link a billing account
          </Link>
        ) : (
          <span>The organization owner must link a billing account.</span>
        )}
        <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
          Manage billing at accounts.delino.io
        </a>
      </p>
    </section>
  );
}
