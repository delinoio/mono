// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import {
  BillingPhase,
  BillingService,
  type OrganizationBilling,
} from "@delino/ownsand-protos/v1/billing_pb";
import { BillingReason, Role } from "@delino/ownsand-protos/v1/common_pb";
import { ACCOUNTS_URL } from "../config.ts";
import { formatTime, formatUsdMicros } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import type { Text } from "../text.ts";
import { showsFullError } from "./ui.tsx";

/** The host name of the billing site, which link texts show. */
export const ACCOUNTS_HOST = new URL(ACCOUNTS_URL).host;

/** Why the organization is blocked, in plain words. */
function reasonText(billing: OrganizationBilling, text: Text): string {
  // The threshold balance of an unpaid billing account.
  const amount = formatUsdMicros(0n, text.lang);
  switch (billing.reason) {
    case BillingReason.ACCOUNT_REQUIRED:
      return text.t("billingBanner.reason.accountRequired");
    case BillingReason.BLOCKED:
      return text.t("billingBanner.reason.blocked", { amount });
    case BillingReason.SUSPENDED:
      return billing.linked
        ? text.t("billingBanner.reason.suspendedUnpaid", { amount })
        : text.t("billingBanner.reason.suspendedUnlinked");
    default:
      return text.t("billingBanner.reason.other");
  }
}

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
  const i18n = useI18n();
  const { t, lang } = i18n;
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
          ? t("billingBanner.suspendedTitle")
          : t("billingBanner.blockedTitle")}
      </h2>
      <p>{reasonText(billing, i18n)}</p>
      {suspended ? (
        <p>
          {t("billingBanner.suspendedEffects")}
          {billing.suspendTime
            ? ` ${t("billingBanner.suspendedSince", {
                time: formatTime(billing.suspendTime, lang),
              })}`
            : null}
        </p>
      ) : (
        <p>
          {t("billingBanner.blockedEffects")}
          {billing.suspendTime
            ? ` ${t("billingBanner.suspendOn", {
                time: formatTime(billing.suspendTime, lang),
              })}`
            : null}
        </p>
      )}
      <p className="billing-banner-actions">
        {billing.linked ? null : role === Role.OWNER ? (
          <Link to={routePath({ id: RouteId.Settings, organizationId })}>
            {t("billingBanner.linkAccount")}
          </Link>
        ) : (
          <span>{t("billingBanner.ownerMustLink")}</span>
        )}
        <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
          {t("billingBanner.manageBilling", { site: ACCOUNTS_HOST })}
        </a>
      </p>
    </section>
  );
}
