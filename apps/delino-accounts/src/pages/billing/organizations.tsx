// SPDX-License-Identifier: Apache-2.0
import { Badge, BadgeVariant } from "delino-ui/badge";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  Loading,
  MorePages,
} from "../../components/ui.tsx";
import { formatDate, labels } from "../../format.ts";
import { useAllPages } from "../../paging.ts";
import {
  BillingStatus,
  BillingUsageService,
  type LinkedOrganization,
} from "@delino/accounts-protos/v1/usage_pb";

function statusBadge(org: LinkedOrganization) {
  const status = org.status?.status ?? BillingStatus.UNSPECIFIED;
  const reason = org.status
    ? labels.billingStatusReason[org.status.reason]
    : "";
  const text =
    reason === ""
      ? labels.billingStatus[status]
      : `${labels.billingStatus[status]}: ${reason}`;
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
  return (
    <section className="section" aria-labelledby="organizations-title">
      <h2 id="organizations-title">Linked organizations</h2>
      <p className="muted">
        This billing account pays for these organizations. Link or unlink an
        organization in its service, for example in the OwnSand console.
      </p>
      {orgs.isPending ? (
        <Loading label="Loading linked organizations" />
      ) : orgs.error ? (
        <ErrorNotice error={orgs.error} onRetry={orgs.refetch} />
      ) : orgs.items.length === 0 ? (
        <Empty title="No linked organizations" />
      ) : (
        <>
          <ul className="list" aria-label="Linked organizations">
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
                    {labels.service[org.service]} · Linked{" "}
                    {formatDate(org.linkTime)}
                  </span>
                </div>
                {statusBadge(org)}
              </li>
            ))}
          </ul>
          <MorePages pages={orgs} />
        </>
      )}
    </section>
  );
}
