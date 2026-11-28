// SPDX-License-Identifier: Apache-2.0
import { Badge, BadgeVariant } from "delino-ui/badge";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  RefetchNotice,
  Loading,
  MorePages,
} from "../../components/ui.tsx";
import { formatDate, labels, serviceName } from "../../format.ts";
import { useI18n } from "../../i18n.ts";
import { useAllPages } from "../../paging.ts";
import {
  BillingStatus,
  BillingUsageService,
  type LinkedOrganization,
} from "@delino/accounts-protos/v1/usage_pb";

function StatusBadge({ org }: { org: LinkedOrganization }) {
  const { t } = useI18n();
  const status = org.status?.status ?? BillingStatus.UNSPECIFIED;
  const reason = org.status
    ? labels.billingStatusReason[org.status.reason]
    : undefined;
  const text =
    reason === undefined
      ? t(labels.billingStatus[status])
      : t("organizations.statusWithReason", {
          status: t(labels.billingStatus[status]),
          reason: t(reason),
        });
  return (
    <Badge
      variant={
        status === BillingStatus.ALLOWED
          ? BadgeVariant.Accent
          : status === BillingStatus.UNSPECIFIED
            ? BadgeVariant.Neutral
            : BadgeVariant.Danger
      }
    >
      {text}
    </Badge>
  );
}

/** The organizations tab: linked organizations, read-only. */
export function OrganizationsSection() {
  const { billingAccountId } = useBillingAccount();
  const orgs = useAllPages(
    BillingUsageService.method.listLinkedOrganizations,
    { billingAccountId },
    (page) => ({
      items: page.linkedOrganizations,
      nextPageToken: page.nextPageToken,
    }),
    (org) => `${org.service}/${org.organizationId}`,
  );
  const i18n = useI18n();
  const { lang, t } = i18n;
  return (
    <section className="section" aria-labelledby="organizations-title">
      <h2 id="organizations-title">{t("organizations.title")}</h2>
      <p className="muted">{t("organizations.description")}</p>
      <RefetchNotice error={orgs.refreshError} onRetry={orgs.refetch} />
      {orgs.isPending ? (
        <Loading label={t("organizations.loading")} />
      ) : orgs.error ? (
        <ErrorNotice error={orgs.error} onRetry={orgs.refetch} />
      ) : orgs.items.length === 0 ? (
        <Empty title={t("organizations.empty")} />
      ) : (
        <>
          <ul className="list" aria-label={t("organizations.title")}>
            {orgs.items.map((org) => (
              <li
                key={`${org.service}/${org.organizationId}`}
                className="list-row"
              >
                <div className="list-main">
                  <span className="list-title">
                    {org.organizationName !== ""
                      ? org.organizationName
                      : org.organizationId}
                  </span>
                  <span className="muted">
                    {t("organizations.details", {
                      service: serviceName(org.service, i18n),
                      date: formatDate(org.linkTime, lang),
                    })}
                  </span>
                </div>
                <StatusBadge org={org} />
              </li>
            ))}
          </ul>
          <MorePages pages={orgs} />
        </>
      )}
    </section>
  );
}
