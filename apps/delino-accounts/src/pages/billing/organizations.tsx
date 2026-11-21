// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { Badge, BadgeVariant } from "delino-ui/badge";
import { LIST_PAGE_SIZE } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import { Empty, ErrorNotice, Loading } from "../../components/ui.tsx";
import { formatDate, labels } from "../../format.ts";
import {
  BillingStatus,
  BillingUsageService,
  type LinkedOrganization,
} from "../../gen/delino/accounts/v1/usage_pb.ts";

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
  const orgs = useQuery(BillingUsageService.method.listLinkedOrganizations, {
    billingAccountId,
    pageSize: LIST_PAGE_SIZE,
  });
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
        <ErrorNotice error={orgs.error} onRetry={() => void orgs.refetch()} />
      ) : orgs.data.linkedOrganizations.length === 0 ? (
        <Empty title="No linked organizations" />
      ) : (
        <ul className="list" aria-label="Linked organizations">
          {orgs.data.linkedOrganizations.map((org) => (
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
      )}
    </section>
  );
}
